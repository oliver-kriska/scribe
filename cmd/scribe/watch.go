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
	root, err := kbDir()
	if err != nil {
		return err
	}
	cfg := loadConfig(root)
	dbPath := cfg.CcriderDB
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

	logMsg("watch", "watching %s (debounce=%s, provider=%s, lookback=%dmin)",
		walPath, w.Debounce, w.Provider, w.LookbackMin)

	// Scan once at startup to catch anything that landed between runs.
	w.scan(root, dbPath)

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
			w.scan(root, dbPath)

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
// queue or processed log, scores them, and appends qualifying ones to the
// pending file.
//
// All errors are logged and swallowed — this runs as a long-lived watcher,
// and a transient DB lock should never kill the process.
func (w *WatchCmd) scan(root, dbPath string) {
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

	sessionsLog := filepath.Join(root, "wiki", "_sessions_log.json")
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
	rows.Close()

	if len(candidates) == 0 {
		if w.Verbose {
			logMsg("watch", "no fresh candidates")
		}
		return
	}

	queued := 0
	for _, c := range candidates {
		if isSessionProcessed(sessionsLog, c.id) {
			if w.Verbose {
				logMsg("watch", "skip %s (already processed)", c.id)
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

		if err := appendPending(pendingFile, c.id, score); err != nil {
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

// appendPending writes one entry to the pending-sessions.txt file, creating
// the parent directory if needed. Format matches the SessionEnd hook so
// sync.go reads both writers uniformly.
func appendPending(path, sessionID string, score int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\t%d\t%s\n", sessionID, score, time.Now().UTC().Format(time.RFC3339))
	return err
}
