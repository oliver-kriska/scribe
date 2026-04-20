package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// HookCmd groups Claude Code lifecycle hooks. Currently only SessionEnd,
// but this leaves room for PreToolUse/PostToolUse hooks later.
//
// Install the SessionEnd hook by adding this to ~/.claude/settings.json:
//
//	{
//	  "hooks": {
//	    "SessionEnd": [{
//	      "command": "scribe hook session-end"
//	    }]
//	  }
//	}
//
// See `scribe hook session-end --help` for tuning flags.
type HookCmd struct {
	SessionEnd SessionEndHookCmd `cmd:"session-end" help:"Claude Code SessionEnd hook — queue high-value sessions for next sync."`
}

// SessionEndHookCmd runs when a Claude Code session closes. It is the
// near-real-time capture path: instead of waiting up to 2h for cron to
// triage, sessions that clear a low bar get appended to a pending queue
// that `scribe sync --sessions` drains first next cycle.
//
// The hook must be fast. Claude Code blocks briefly while hooks run, and
// a heavy user opens/closes 5-10 empty sessions per minute in normal work,
// so every hook invocation has a hard deadline of 2 seconds and does zero
// LLM calls. Only SQLite reads + a file append.
//
// The selection filter has two stages, both cheap:
//  1. message_count < MinMessages (default 10) drops ~90% of noise outright.
//  2. An FTS5 score below MinScore (default 50) drops mechanical sessions
//     that had length but no decision/research/learning content.
//
// A session that clears both goes in ~/.config/scribe/pending-sessions.txt
// with file-level dedup. `scribe sync --sessions` reads the file before
// running normal triage and prepends these IDs (they get priority over the
// normal top-N triage selection), then clears the file on success.
type SessionEndHookCmd struct {
	MinMessages int  `help:"Skip sessions with fewer than this many messages." default:"10" name:"min-messages"`
	MinScore    int  `help:"Skip sessions whose FTS5 triage score falls below this." default:"50" name:"min-score"`
	Verbose     bool `help:"Log hook decisions to stderr (otherwise silent)."`
}

// Run is the hook entry point. It is deliberately error-tolerant: any
// problem is logged in verbose mode and silently swallowed otherwise.
// A failing hook must never block Claude Code from exiting cleanly.
func (c *SessionEndHookCmd) Run() error {
	// Hard deadline — if anything in this hook takes longer than 2s,
	// bail without writing. Hooks that stall annoy the user.
	deadline := time.Now().Add(2 * time.Second)

	// 1. Try to read session_id from Claude Code's hook payload on stdin.
	//    Format: {"hook_event_name": "SessionEnd", "session_id": "...", ...}
	sessionID := readSessionIDFromStdin()

	// 2. Nudge ccrider to sync before we read the DB. The session that
	//    just ended might still be lag-behind in the DB file. Quick and
	//    capped — if ccrider is missing or slow, we proceed anyway with
	//    whatever data is already on disk.
	runBestEffort("ccrider", []string{"sync"}, 1500*time.Millisecond)

	// 3. Open the ccrider DB read-only.
	root, err := kbDir()
	if err != nil {
		return c.skip("no KB root: %v", err)
	}
	cfg := loadConfig(root)
	if _, err := os.Stat(cfg.CcriderDB); err != nil {
		return c.skip("ccrider db missing: %v", err)
	}
	db, err := sql.Open("sqlite3", cfg.CcriderDB+"?mode=ro")
	if err != nil {
		return c.skip("open db: %v", err)
	}
	defer db.Close()

	// 4. If stdin didn't give us a session_id (hook triggered manually,
	//    or older Claude Code that doesn't pipe the payload), fall back
	//    to the most recently updated session in the DB.
	if sessionID == "" {
		sessionID = queryMostRecentSessionID(db)
	}
	if sessionID == "" {
		return c.skip("no session id resolvable")
	}

	if time.Now().After(deadline) {
		return c.skip("deadline before message count")
	}

	// 5. Cheap first cut: message count filter.
	msgCount, ok := queryMessageCount(db, sessionID)
	if !ok {
		return c.skip("session %s not in db yet", sessionID)
	}
	if msgCount < c.MinMessages {
		return c.skip("session %s has %d messages (< %d)", sessionID, msgCount, c.MinMessages)
	}

	// 6. Skip if already processed by a previous extraction — no point
	//    re-queueing something the LLM has already chewed on.
	sessionsLog := filepath.Join(root, "wiki", "_sessions_log.json")
	if isSessionProcessed(sessionsLog, sessionID) {
		return c.skip("session %s already processed", sessionID)
	}

	// 7. Skip if it's already waiting in the pending queue from an
	//    earlier hook invocation this cycle.
	pendingFile := pendingSessionsFile()
	if pendingContainsID(pendingFile, sessionID) {
		return c.skip("session %s already pending", sessionID)
	}

	if time.Now().After(deadline) {
		return c.skip("deadline before scoring")
	}

	// 8. FTS5 scoring pass. Same keyword categories and weights as
	//    `scribe triage` — keep them in sync so hook queue and cron
	//    selection agree on what "high value" means.
	score := quickScoreSession(db, sessionID)
	if score < c.MinScore {
		return c.skip("session %s scored %d (< %d)", sessionID, score, c.MinScore)
	}

	// 9. Append to pending-sessions.txt. Format:
	//    sessionID<TAB>score<TAB>ISO8601-UTC
	//    The extra columns are informational; sync.go only reads column 1.
	if err := os.MkdirAll(filepath.Dir(pendingFile), 0o755); err != nil {
		return c.skip("mkdir pending dir: %v", err)
	}
	f, err := os.OpenFile(pendingFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return c.skip("open pending file: %v", err)
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\t%d\t%s\n", sessionID, score, time.Now().UTC().Format(time.RFC3339))

	if c.Verbose {
		fmt.Fprintf(os.Stderr, "scribe hook: queued %s (score %d, msgs %d)\n", sessionID, score, msgCount)
	}
	return nil
}

// skip is the hook's one-stop "decide not to queue" path. It returns nil
// unconditionally because hook errors must never propagate back to Claude
// Code — a failed hook is a silent no-op, not a blocker.
func (c *SessionEndHookCmd) skip(format string, args ...any) error {
	if c.Verbose {
		fmt.Fprintf(os.Stderr, "scribe hook: skip: "+format+"\n", args...)
	}
	return nil
}

// readSessionIDFromStdin parses Claude Code's hook payload JSON from stdin.
// Returns "" if stdin is a TTY, empty, or doesn't parse — the caller falls
// back to querying the DB for the most recent session in that case.
func readSessionIDFromStdin() string {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return ""
	}
	// No pipe attached (interactive terminal) — nothing to read.
	if (fi.Mode() & os.ModeCharDevice) != 0 {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 64*1024))
	if err != nil || len(data) == 0 {
		return ""
	}
	var payload struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.SessionID)
}

// runBestEffort launches a command and kills it if it doesn't finish within
// timeout. Any error (missing binary, non-zero exit, kill) is swallowed.
// The hook must never get stuck on ccrider hiccups.
func runBestEffort(name string, args []string, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	_ = cmd.Run()
}

// queryMostRecentSessionID returns the session_id of the most recently
// updated non-bootstrap session. Used as a fallback when the hook didn't
// get a session_id from stdin. Filters out the "You are working in..."
// system-prompt sessions that clutter the DB.
func queryMostRecentSessionID(db *sql.DB) string {
	var sid string
	//nolint:noctx // hook path — fast, no cancellation
	_ = db.QueryRow(`
		SELECT session_id FROM sessions
		WHERE message_count > 0
		  AND COALESCE(summary, '') NOT LIKE 'You are working in%'
		ORDER BY updated_at DESC
		LIMIT 1`).Scan(&sid)
	return sid
}

// queryMessageCount returns (count, true) on success, (0, false) if the
// session isn't in the DB yet.
func queryMessageCount(db *sql.DB, sessionID string) (int, bool) {
	var n int
	//nolint:noctx // hook path — fast, no cancellation
	err := db.QueryRow(
		`SELECT message_count FROM sessions WHERE session_id = ?`,
		sessionID,
	).Scan(&n)
	if err != nil {
		return 0, false
	}
	return n, true
}

// quickScoreSession computes a single-session knowledge-density score
// mirroring the `scribe triage` weights. Weights stay in lockstep with
// runScoring in triage.go so the hook and the cron pipeline agree on
// what "high value" means.
//
// Uses one COUNT(*) subquery per keyword category. Six subqueries, each
// filtered by session_id, each riding the FTS5 index. In practice this
// runs in a few milliseconds per session.
func quickScoreSession(db *sql.DB, sessionID string) int {
	// (query, weight) pairs. Any addition here must also land in
	// triage.go:runScoring or the two scorers will drift.
	categories := []struct {
		match  string
		weight int
	}{
		{"decided OR chose OR tradeoff OR alternative", 3},
		{"architecture OR \"design pattern\" OR strategy OR refactor", 2},
		{"research OR paper OR benchmark OR measured OR compared", 3},
		{"learned OR realized OR mistake OR lesson OR insight", 2},
		{"evaluated OR verdict OR recommend OR comparison", 2},
		{"analysis OR investigation OR \"root cause\" OR audit", 1},
	}
	total := 0
	for _, cat := range categories {
		var hits int
		//nolint:noctx // hook path — fast, no cancellation
		err := db.QueryRow(`
			SELECT COUNT(*)
			FROM messages_fts f
			JOIN messages m ON m.id = f.rowid
			JOIN sessions s ON s.id = m.session_id
			WHERE s.session_id = ?
			  AND messages_fts MATCH ?`,
			sessionID, cat.match,
		).Scan(&hits)
		if err != nil {
			continue
		}
		total += hits * cat.weight
	}
	// Also pick up code-pattern hits (same weight 1 as triage).
	var codeHits int
	//nolint:noctx // hook path — fast, no cancellation
	_ = db.QueryRow(`
		SELECT COUNT(*)
		FROM messages_fts_code f
		JOIN messages m ON m.id = f.rowid
		JOIN sessions s ON s.id = m.session_id
		WHERE s.session_id = ?
		  AND messages_fts_code MATCH 'GenServer OR LiveView OR Oban OR Ecto OR Phoenix OR migration OR Supervisor OR PubSub OR Endpoint OR Router'`,
		sessionID,
	).Scan(&codeHits)
	total += codeHits
	return total
}

// pendingSessionsFile is the handoff point between the SessionEnd hook
// (writer) and `scribe sync --sessions` (reader). Kept in XDG config so
// it's scoped to the user and survives KB-directory moves.
func pendingSessionsFile() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "scribe", "pending-sessions.txt")
	}
	return filepath.Join(os.Getenv("HOME"), ".config", "scribe", "pending-sessions.txt")
}

// parsePendingLine returns the session ID from a "<id>\t<score>\t<ts>" line.
// Blank or malformed lines return "".
func parsePendingLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	if tab := strings.IndexByte(line, '\t'); tab > 0 {
		return line[:tab]
	}
	return line
}

// pendingContainsID scans the pending file for a line whose first
// tab-separated field matches sessionID. Returns false on any error
// (file not found is the common case — means the queue is empty).
func pendingContainsID(path, sessionID string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if parsePendingLine(sc.Text()) == sessionID {
			return true
		}
	}
	return false
}

// isSessionProcessed returns true if sessionID is already in
// _sessions_log.json's processed map. Reuses the same loader as the
// triage pipeline to stay consistent.
func isSessionProcessed(sessionsLog, sessionID string) bool {
	return slices.Contains(loadProcessedSessionIDs(sessionsLog), sessionID)
}

// peekPendingSessions reads the pending-sessions file without clearing it.
// Used by `sync --dry-run` for observability. Returns nil on any error.
func peekPendingSessions() []string {
	path := pendingSessionsFile()
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var ids []string
	seen := make(map[string]bool)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		id := parsePendingLine(sc.Text())
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// readAndClearPendingSessions is called by sync.go to drain the hook queue.
//
// Atomic rename prevents a race: if we read-then-delete, an appender running
// between the last scanner read and the unlink loses its write (the append
// opens O_APPEND, writes, closes — then the reader unlinks the inode and
// a subsequent drain never sees it). Rename claims the current file as ours;
// any concurrent appender creates a fresh file that the next drain picks up.
func readAndClearPendingSessions() ([]string, error) {
	path := pendingSessionsFile()
	claim := path + ".reading"

	// Recover from a prior crash: a leftover .reading file is ours to process.
	if _, err := os.Stat(claim); err != nil {
		if err := os.Rename(path, claim); err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("claim pending file: %w", err)
		}
	}

	f, err := os.Open(claim)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open pending file: %w", err)
	}
	var ids []string
	seen := make(map[string]bool)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		id := parsePendingLine(sc.Text())
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	f.Close()

	if err := os.Remove(claim); err != nil && !os.IsNotExist(err) {
		return ids, fmt.Errorf("clear pending file: %w", err)
	}
	return ids, nil
}
