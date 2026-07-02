package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	_ "github.com/mattn/go-sqlite3"
)

// WatchCmd is the fsnotify-driven near-real-time session capture path.
//
// The SessionEnd hook (hook.go) only fires for Claude Code sessions — ccrider
// imports Codex sessions too, but Codex has no equivalent hook, so Codex
// sessions would only land in the pending queue on the next cron tick. This
// watcher closes that gap: it monitors ccrider's SQLite WAL file, and when
// the DB changes it looks for freshly-updated Codex sessions and scores them
// with the same FTS5 logic the SessionEnd hook uses.
//
// Why the WAL file and not the DB itself: ccrider writes in WAL mode, so
// sessions.db only changes on checkpoint. sessions.db-wal changes on every
// commit, which is what we actually want to react to.
//
// Why fsnotify instead of polling: polling every 30s on an 800MB SQLite file
// isn't free, and we want the latency of a real file-system notification so
// a Codex session ending shows up in the pending queue within a minute.
//
// Debounce logic: ccrider can write the WAL in bursts (multi-statement
// transactions). We collect events for Debounce seconds of quiet before
// running the scoring pass, so a burst of 50 writes triggers one scan.
type WatchCmd struct {
	Debounce    time.Duration `help:"Quiet window after a write burst before scanning." default:"30s"`
	MinMessages int           `help:"Skip sessions with fewer than this many messages." default:"10" name:"min-messages"`
	MinScore    int           `help:"Skip sessions whose FTS5 triage score falls below this." default:"50" name:"min-score"`
	LookbackMin int           `help:"Only consider sessions whose updated_at is within this many minutes." default:"10" name:"lookback-min"`
	Provider    string        `help:"Session provider to watch (codex | claude | any)." default:"codex"`
	Verbose     bool          `help:"Log scan decisions to stderr."`
}

func (w *WatchCmd) Run() error {
	// Multi-KB (issue #26): one watcher serves every registered KB. The
	// ccrider DB and the pending queue are both machine-global, so a single
	// fsnotify watch feeds all KBs — what differs per KB is only the
	// processed-session dedup, handled in scan. Fall back to the default KB
	// when no registry exists (single-KB installs migrate untouched).
	roots := registeredKBs()
	if len(roots) == 0 {
		root, err := kbDir()
		if err != nil {
			return err
		}
		roots = []string{root}
	}
	// ccrider's DB is machine-global; resolve its path from the first KB's
	// config (every KB points at the same sessions.db).
	dbPath := loadConfig(roots[0]).CcriderDB
	if dbPath == "" {
		return fmt.Errorf("no ccrider_db configured in %s", roots[0])
	}
	walPath := dbPath + "-wal"

	// We watch the directory (not the file directly) because the WAL file
	// can be recreated on checkpoint — watching the file by name means
	// losing the watch when SQLite rotates the WAL. Directory watches
	// survive rotation.
	watchDir := filepath.Dir(dbPath)
	if _, err := os.Stat(watchDir); err != nil {
		return fmt.Errorf("ccrider dir not found: %w", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify: %w", err)
	}
	defer watcher.Close()

	if err := watcher.Add(watchDir); err != nil {
		return fmt.Errorf("add watch %s: %w", watchDir, err)
	}

	logMsg("watch", "watching %s for %d KB(s) (debounce=%s, provider=%s, lookback=%dmin)",
		walPath, len(roots), w.Debounce, w.Provider, w.LookbackMin)

	// Scan once at startup to catch anything that landed between runs.
	w.scan(roots, dbPath)

	var debounceTimer *time.Timer
	// Use a channel so the timer goroutine communicates back to the main loop.
	fireCh := make(chan struct{}, 1)

	for {
		select {
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			// Only react to writes/creates on the WAL or DB file. Ignore
			// everything else (lock files, temp files, etc).
			base := filepath.Base(ev.Name)
			dbBase := filepath.Base(dbPath)
			if base != dbBase && base != dbBase+"-wal" && base != dbBase+"-shm" {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}

			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(w.Debounce, func() {
				select {
				case fireCh <- struct{}{}:
				default:
				}
			})

		case <-fireCh:
			w.scan(roots, dbPath)

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			logMsg("watch", "watcher error: %v", err)
		}
	}
}

// scan opens ccrider's DB, finds sessions updated in the last LookbackMin
// minutes that match the target provider and aren't already in the pending
// queue, scores them, and appends qualifying ones to the (machine-global)
// pending file. A session is treated as already handled — and skipped —
// only when EVERY registered KB has processed it; if even one KB hasn't,
// it still reaches the queue so that KB's next sync can mine it. Per-KB
// scope filtering happens at sync time (preFilterSessions), so the watcher
// deliberately over-queues rather than replicating that logic here.
//
// All errors are logged and swallowed — this runs as a long-lived watcher,
// and a transient DB lock should never kill the process.
func (w *WatchCmd) scan(roots []string, dbPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := os.Stat(dbPath); err != nil {
		if w.Verbose {
			logMsg("watch", "db missing: %v", err)
		}
		return
	}

	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		logMsg("watch", "open db: %v", err)
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	providerClause := ""
	args := []any{w.LookbackMin, w.MinMessages}
	switch w.Provider {
	case "any":
		// no filter
	case "":
		providerClause = "AND s.provider = ?"
		args = append(args, "codex")
	default:
		providerClause = "AND s.provider = ?"
		args = append(args, w.Provider)
	}

	query := fmt.Sprintf(`
		SELECT s.session_id, s.message_count, COALESCE(s.provider, 'claude')
		FROM sessions s
		WHERE s.updated_at >= datetime('now', '-' || ? || ' minutes')
		  AND s.message_count >= ?
		  AND COALESCE(s.summary, '') NOT LIKE 'You are working in%%'
		  %s
		ORDER BY s.updated_at DESC
		LIMIT 20`, providerClause)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		logMsg("watch", "query: %v", err)
		return
	}
	defer rows.Close()

	// Preload each KB's processed-session set once per scan, so a candidate
	// can be checked against every KB without re-reading any log per row.
	processedSets := make([]map[string]bool, len(roots))
	for i, r := range roots {
		ids := loadProcessedSessionIDs(filepath.Join(r, "wiki", "_sessions_log.json"))
		set := make(map[string]bool, len(ids))
		for _, id := range ids {
			set[id] = true
		}
		processedSets[i] = set
	}
	pendingFile := pendingSessionsFile()

	type candidate struct {
		id       string
		msgs     int
		provider string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.msgs, &c.provider); err != nil {
			continue
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		logMsg("watch", "iterate sessions: %v", err)
		return
	}
	rows.Close()

	if len(candidates) == 0 {
		if w.Verbose {
			logMsg("watch", "no fresh candidates")
		}
		return
	}

	queued := 0
	for _, c := range candidates {
		if processedByAllKBs(processedSets, c.id) {
			if w.Verbose {
				logMsg("watch", "skip %s (already processed by all %d KB(s))", c.id, len(roots))
			}
			continue
		}
		if pendingContainsID(pendingFile, c.id) {
			if w.Verbose {
				logMsg("watch", "skip %s (already pending)", c.id)
			}
			continue
		}

		score := quickScoreSession(db, c.id)
		if score < w.MinScore {
			if w.Verbose {
				logMsg("watch", "skip %s (%s, score %d < %d)", c.id, c.provider, score, w.MinScore)
			}
			continue
		}

		if err := appendPending(pendingFile, c.id, score, c.msgs); err != nil {
			logMsg("watch", "append pending: %v", err)
			continue
		}
		queued++
		logMsg("watch", "queued %s (%s, score %d, msgs %d)", c.id, c.provider, score, c.msgs)
	}

	if w.Verbose && queued == 0 {
		logMsg("watch", "scan complete, 0 queued from %d candidates", len(candidates))
	}
}

// processedByAllKBs reports whether every registered KB has already mined
// this session — the only case where the watcher can safely drop it. As
// long as one KB still hasn't processed it, the session reaches the shared
// pending queue so that KB's next sync gets its chance. Empty set (no KBs)
// reports false so nothing is dropped for lack of a registry.
func processedByAllKBs(sets []map[string]bool, id string) bool {
	if len(sets) == 0 {
		return false
	}
	for _, s := range sets {
		if !s[id] {
			return false
		}
	}
	return true
}

// appendPending writes one entry to the pending-sessions.txt file, creating
// the parent directory if needed. Format matches the SessionEnd hook (4
// columns: sessionID, score, msgCount, ISO8601-UTC — see
// docs/issue-22-priority-lanes-plan.md §2.2) so sync.go reads both writers
// uniformly.
func appendPending(path, sessionID string, score, msgCount int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\t%d\t%d\t%s\n", sessionID, score, msgCount, time.Now().UTC().Format(time.RFC3339))
	return err
}
