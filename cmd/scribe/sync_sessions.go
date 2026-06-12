// sync_sessions.go — sync Phase 2.5: mine Claude Code sessions from
// ccrider's FTS5 index (triage, pre-filter, batched envelope mining).
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// sessionFilterStats captures the per-session numbers the pre-filter looks at.
type sessionFilterStats struct {
	UserMsgs    int
	TotalChars  int
	ProjectPath string
	Found       bool
}

// filterVerdict returns "" (keep) or a reason string (skip).
// Keep in sync with preFilterSessions decision logic — this is the single
// source of truth for the pre-filter rule set.
func (st sessionFilterStats) filterVerdict() string {
	if !st.Found {
		return "" // keep on lookup error
	}
	// Skip truly empty sessions (<500 chars or no user turn at all).
	if st.UserMsgs < 1 || st.TotalChars < 500 {
		return "empty"
	}
	// Drop short back-and-forth with no depth: need at least 3 user
	// messages OR a rich one-shot assistant response (>=2000 chars).
	if st.UserMsgs < 3 && st.TotalChars < 2000 {
		return "thin"
	}
	return ""
}

// querySessionStats hits the ccrider DB for a single session.
func querySessionStats(db *sql.DB, sid string) sessionFilterStats {
	var st sessionFilterStats
	//nolint:noctx // CLI top-level
	err := db.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN type = 'user' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(LENGTH(text_content)), 0),
			COALESCE(MAX(s.project_path), '')
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE s.session_id = ?`, sid).Scan(&st.UserMsgs, &st.TotalChars, &st.ProjectPath)
	st.Found = (err == nil)
	return st
}

// projectScopeAllowed reports whether a session whose ccrider project_path
// is projectPath belongs in THIS KB. It enforces the same two gates the
// project-discovery lane already applies — the ignore list and the sources
// filter — so a project the user excluded from `sources` or `scribe
// projects ignore`d can't leak in through the parallel session-mining path.
//
// Deliberately permissive on the unknown cases: an undiscovered-but-allowed
// project passes (knowledge can be captured before formal enrollment — only
// ignored and source-excluded projects are dropped), and an empty
// projectPath (no provenance recorded) passes rather than over-filtering.
// isIgnored also folds in the shallow-path / within-a-KB / TCC guards, which
// is why this is safe to use on the large lane that skips sessionInKB.
func projectScopeAllowed(cfg *ScribeConfig, manifest *Manifest, projectPath string) bool {
	if projectPath == "" {
		return true
	}
	if manifest != nil && manifest.isIgnored(projectPath) {
		return false
	}
	return sourceAllowed(cfg, projectPath)
}

// filterSessionsByScope drops sessions whose project is ignored or
// source-excluded for this KB. It is the scope half of preFilterSessions,
// pulled out for the large-session lane (which skips the mechanical/thin
// gate but must still honor sources/ignore). Fails open on DB-open error,
// matching preFilterSessions.
func filterSessionsByScope(root, dbPath string, sessionIDs []string) []string {
	if len(sessionIDs) == 0 {
		return sessionIDs
	}
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		return sessionIDs
	}
	defer db.Close()

	manifest, _ := loadManifest(root)
	cfg := loadConfig(root)
	kept := sessionIDs[:0]
	dropped := 0
	for _, sid := range sessionIDs {
		stats := querySessionStats(db, sid)
		if stats.Found && !projectScopeAllowed(cfg, manifest, stats.ProjectPath) {
			dropped++
			continue
		}
		kept = append(kept, sid)
	}
	if dropped > 0 {
		logMsg("sync", "pre-filter: dropped %d large session(s) from out-of-scope projects (ignored or excluded by sources)", dropped)
	}
	return kept
}

// preFilterSessions removes sessions that are too mechanical to be worth
// extracting, and — independently — sessions that were spent inside the KB
// itself (mining those re-emits the wiki's own content). Queries ccrider DB
// directly for message stats + cwd. Returns filtered list + skipped IDs.
//
// The KB drop here is the chokepoint every *normal* session ID flows through
// (both triage picks and hook-queued pending IDs are merged before this
// call), so it also neutralizes any KB sessions queued before the hook-side
// guard shipped. Triage's SQL exclusion covers the large-session path that
// bypasses this filter.
func preFilterSessions(root, dbPath string, sessionIDs []string) (kept, skipped []string) {
	if len(sessionIDs) == 0 {
		return sessionIDs, nil
	}

	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		// If we can't open DB, keep all sessions (no filtering).
		return sessionIDs, nil
	}
	defer db.Close()

	// Approval gate: sessions spent in a pending (not yet approved)
	// project are dropped silently — not marked processed — so triage can
	// re-surface them once the user approves the project. Manifest load
	// failure fails open (no approval filtering), matching the DB-open
	// behavior above.
	manifest, _ := loadManifest(root)
	cfg := loadConfig(root)

	kbSkipped := 0
	scopeSkipped := 0
	pendingSkipped := 0
	for _, sid := range sessionIDs {
		stats := querySessionStats(db, sid)
		if !stats.Found {
			kept = append(kept, sid) // On error, keep the session.
			continue
		}
		if sessionInKB(root, stats.ProjectPath) {
			// Drop silently (not into `skipped`): the caller marks
			// skipped IDs as mechanically processed, which would be the
			// wrong reason. KB sessions can't resurface anyway — triage
			// now excludes them and the hook no longer queues them.
			kbSkipped++
			continue
		}
		if !projectScopeAllowed(cfg, manifest, stats.ProjectPath) {
			// Out-of-scope for this KB (ignored or excluded by sources).
			// Drop silently like KB sessions: marking them processed would
			// lie, and they must stay re-mineable if the KB's scope later
			// widens to include the project.
			scopeSkipped++
			continue
		}
		if manifest != nil && stats.ProjectPath != "" {
			// entryForPath rather than a keyed lookup: a session run
			// inside a pending project's WORKTREE must inherit the
			// pending status (the worktree's basename is not a manifest
			// key), and a basename collision must not let one project's
			// approval state govern another's sessions.
			if entry := manifest.entryForPath(stats.ProjectPath); entry != nil && !entry.IsApproved() {
				pendingSkipped++
				continue
			}
		}
		if stats.filterVerdict() != "" {
			skipped = append(skipped, sid)
			continue
		}
		kept = append(kept, sid)
	}
	if kbSkipped > 0 {
		logMsg("sync", "pre-filter: dropped %d session(s) run inside the KB (never mine the KB into itself)", kbSkipped)
	}
	if scopeSkipped > 0 {
		logMsg("sync", "pre-filter: dropped %d session(s) from out-of-scope projects (ignored or excluded by sources)", scopeSkipped)
	}
	if pendingSkipped > 0 {
		logMsg("sync", "pre-filter: deferred %d session(s) from pending project(s) — approve via `scribe projects review`", pendingSkipped)
	}
	return kept, skipped
}

// relatedSession is a nearby session surfaced to the extractor so it can
// reference or deduplicate against sibling work from the same project.
type relatedSession struct {
	SessionID string
	Date      string
	Summary   string
}

// queryRelatedSessions returns up to `limit` sessions from the same project
// whose updated_at is within `daysWindow` of the target session. The target
// session itself is excluded. Best effort — returns nil on any DB error.
func queryRelatedSessions(dbPath, sessionID string, daysWindow, limit int) []relatedSession {
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		return nil
	}
	defer db.Close()

	// Fetch the target session's project + updated_at first.
	var projectPath string
	var updatedAt string
	//nolint:noctx // CLI top-level
	err = db.QueryRow(
		`SELECT project_path, updated_at FROM sessions WHERE session_id = ?`,
		sessionID,
	).Scan(&projectPath, &updatedAt)
	if err != nil || projectPath == "" {
		return nil
	}

	//nolint:noctx // CLI top-level; sync orchestrates its own cancellation via errgroup
	rows, err := db.Query(`
		SELECT session_id, date(updated_at),
		       COALESCE(NULLIF(llm_summary, ''), COALESCE(summary, ''))
		FROM sessions
		WHERE project_path = ?
		  AND session_id != ?
		  AND ABS(JULIANDAY(updated_at) - JULIANDAY(?)) < ?
		ORDER BY updated_at DESC
		LIMIT ?`,
		projectPath, sessionID, updatedAt, daysWindow, limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []relatedSession
	for rows.Next() {
		var rs relatedSession
		if err := rows.Scan(&rs.SessionID, &rs.Date, &rs.Summary); err == nil {
			if len(rs.Summary) > 140 {
				rs.Summary = rs.Summary[:140] + "…"
			}
			out = append(out, rs)
		}
	}
	if rows.Err() != nil {
		// Best-effort contract: a truncated iteration means the related
		// list can't be trusted — drop it rather than hint from partials.
		return nil
	}
	return out
}

// formatRelatedSessions turns a slice of related sessions into the bullet
// list injected into the prompt as the {{RELATED_SESSIONS}} variable.
func formatRelatedSessions(related []relatedSession) string {
	if len(related) == 0 {
		return "(none — this is the first or only recent session for this project)"
	}
	var sb strings.Builder
	for _, r := range related {
		summary := strings.ReplaceAll(r.Summary, "\n", " ")
		if summary == "" {
			summary = "(no summary)"
		}
		fmt.Fprintf(&sb, "- %s (%s): %s\n", r.SessionID, r.Date, summary)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// triageSessionIDs calls scribe triage and returns session IDs.
func (s *SyncCmd) triageSessionIDs(top int, extraArgs ...string) []string {
	scribeExe, _ := os.Executable()
	if scribeExe == "" {
		scribeExe = "scribe"
	}
	args := make([]string, 0, 6+len(extraArgs))
	args = append(args, "triage", "--ids", "--top", fmt.Sprintf("%d", top), "--sort", s.SessionSort)
	args = append(args, extraArgs...)
	idsOutput, err := runCmdErr("", scribeExe, args...)
	if err != nil {
		return nil
	}
	var ids []string
	for line := range strings.SplitSeq(idsOutput, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			ids = append(ids, line)
		}
	}
	return ids
}

// sessionResult captures the outcome of a single session extraction.
type sessionResult struct {
	sessionID   string
	success     bool
	rateLimited bool
	err         error
}

// recordBatchOutcome appends one entry to wiki/_extraction_outcomes.json
// summarizing what the last checkpoint commit contained. This is approximate
// (all sessions in a parallel checkpoint share credit) but gives Phase 4
// threshold-tuning a data feed without racing on per-goroutine writes.
// Called AFTER a successful commitAndPush — reads HEAD~1..HEAD shortstat.
func recordBatchOutcome(root, label string, sessionIDs []string) {
	if len(sessionIDs) == 0 {
		return
	}
	files, added, deleted := gitDiffShortstat(root, "HEAD~1", "HEAD")
	sha := gitSHA(root)
	outcomesPath := filepath.Join(root, "wiki", "_extraction_outcomes.json")
	if !fileExists(outcomesPath) {
		if err := os.WriteFile(outcomesPath, []byte(`{"entries": []}`+"\n"), 0o644); err != nil {
			logMsg("sync", "init %s: %v", outcomesPath, err)
			return
		}
	}
	if err := updateJSONFile(outcomesPath, func(data map[string]any) {
		entries, _ := data["entries"].([]any)
		entry := map[string]any{
			"timestamp":     time.Now().UTC().Format(time.RFC3339),
			"label":         label,
			"commit_sha":    sha,
			"session_ids":   sessionIDs,
			"files_changed": files,
			"lines_added":   added,
			"lines_deleted": deleted,
		}
		data["entries"] = append(entries, entry)
	}); err != nil {
		logMsg("sync", "warn: could not update outcomes.json: %v", err)
	}
}

// mineSessionBatches processes session IDs with bounded parallelism.
// Returns total mined and whether a rate limit was hit.
//
// Phase 4C: when cfg.SessionMine.Mode == "envelope" (auto-flipped for
// non-anthropic providers), each session goes through the Go
// orchestrator path: Go fetches the transcript, the LLM emits one
// EnvelopeV2, scribe applies it. Otherwise the legacy tools path runs
// (claude -p with ccrider MCP).
func (s *SyncCmd) mineSessionBatches(root string, sessionIDs []string, parallel int, timeout time.Duration, promptName string, label string) (int, bool) {
	tools := []string{
		"Read", "Write", "Edit", "Glob", "Grep",
		"mcp__ccrider__get_session_messages", "mcp__ccrider__generate_session_anchor",
	}

	cfg := loadConfig(root)
	checkpointInterval := cfg.Sync.CheckpointInterval
	if checkpointInterval <= 0 {
		checkpointInterval = 5
	}

	dbPath := cfg.CcriderDB

	// Phase 4C envelope dispatch: when configured for envelope mode,
	// route the whole batch through the Go orchestrator. The legacy
	// tools path stays in place so users can A/B with a config flip
	// rather than a deploy.
	if strings.EqualFold(cfg.SessionMine.Mode, "envelope") {
		return s.mineSessionBatchesEnvelope(root, sessionIDs, parallel, label, cfg)
	}

	if parallel <= 1 {
		// Serial mode for large sessions (avoids concurrent wiki writes).
		return s.mineSessionsSerial(root, sessionIDs, timeout, promptName, label, tools, checkpointInterval)
	}

	// Parallel mode: bounded concurrency via semaphore channel.
	sem := make(chan struct{}, parallel)
	results := make(chan sessionResult, len(sessionIDs))
	var wg gosync.WaitGroup

	logMsg("sync", "%s: processing %d sessions (parallel=%d)", label, len(sessionIDs), parallel)

	for i, sid := range sessionIDs {
		wg.Add(1)
		go func(idx int, sessionID string) {
			defer wg.Done()
			sem <- struct{}{}        // acquire
			defer func() { <-sem }() // release

			logMsg("sync", "%s [%d/%d] extracting %s", label, idx+1, len(sessionIDs), sessionID)

			related := queryRelatedSessions(dbPath, sessionID, 7, 10)
			vars := map[string]string{
				"KB_DIR":           root,
				"SESSION_ID_LIST":  sessionID,
				"RELATED_SESSIONS": formatRelatedSessions(related),
			}
			prompt, err := loadPrompt(promptName, vars)
			if err != nil {
				results <- sessionResult{sessionID, false, false, err}
				return
			}

			ctx := context.Background()
			_, err = runClaude(withOpLabel(ctx, "session-mine"), root, prompt, s.Model, tools, timeout)
			if err != nil {
				// Treat the daily-budget ceiling like a rate-limit so
				// the session-mine batch stops gracefully and the next
				// day's cron picks up where this run left off.
				rl := errors.Is(err, ErrRateLimit) || errors.Is(err, ErrDailyBudgetExhausted)
				results <- sessionResult{sessionID, false, rl, err}
				return
			}
			results <- sessionResult{sessionID, true, false, nil}
		}(i, sid)
	}

	// Close results channel when all goroutines complete.
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results and checkpoint periodically.
	totalMined := 0
	rateLimited := false
	sinceCheckpoint := 0
	var batchIDs []string // session IDs rolled into the next checkpoint commit

	for r := range results {
		if r.rateLimited {
			logMsg("sync", "%s: rate limited on %s — will resume next run", label, r.sessionID)
			rateLimited = true
			// Don't break — let in-flight goroutines finish, just stop launching new ones would be ideal
			// but with the channel approach, already-launched goroutines will complete.
			continue
		}
		if r.err != nil {
			logMsg("sync", "%s: %s failed: %v", label, r.sessionID, r.err)
			continue
		}

		totalMined++
		sinceCheckpoint++
		batchIDs = append(batchIDs, r.sessionID)
		logMsg("sync", "%s: %s complete (%d/%d mined)", label, r.sessionID, totalMined, len(sessionIDs))

		// Checkpoint after every N successful extractions.
		if sinceCheckpoint >= checkpointInterval && totalMined < len(sessionIDs) {
			sinceCheckpoint = 0
			if err := s.rebuildAndReindex(root); err != nil {
				logMsg("sync", "checkpoint reindex error: %v", err)
			}
			committed, err := s.commitAndPush(root, fmt.Sprintf("sync: %s checkpoint (%d sessions)", label, totalMined))
			if err != nil {
				logMsg("sync", "checkpoint commit error: %v", err)
			} else if committed {
				// Only attribute the batch when a commit actually
				// happened — on the debounce/nothing-staged paths the
				// shortstat would describe the PREVIOUS commit. Unrecorded
				// IDs stay in batchIDs and ride into the next checkpoint.
				recordBatchOutcome(root, label, batchIDs)
				batchIDs = nil
			}
		}
	}

	return totalMined, rateLimited
}

// mineSessionsSerial processes sessions one at a time (for large sessions).
func (s *SyncCmd) mineSessionsSerial(root string, sessionIDs []string, timeout time.Duration, promptName string, label string, tools []string, checkpointInterval int) (int, bool) {
	totalMined := 0
	cfg := loadConfig(root)
	var batchIDs []string

	for i, sid := range sessionIDs {
		logMsg("sync", "%s [%d/%d] extracting %s", label, i+1, len(sessionIDs), sid)

		related := queryRelatedSessions(cfg.CcriderDB, sid, 7, 10)
		vars := map[string]string{
			"KB_DIR":           root,
			"SESSION_ID":       sid,
			"SESSION_ID_LIST":  sid,
			"MESSAGE_COUNT":    "large",
			"RELATED_SESSIONS": formatRelatedSessions(related),
		}
		prompt, err := loadPrompt(promptName, vars)
		if err != nil {
			logMsg("sync", "load prompt error: %v", err)
			continue
		}

		ctx := context.Background()
		_, err = runClaude(withOpLabel(ctx, "session-mine-batch"), root, prompt, s.Model, tools, timeout)
		if err != nil {
			if errors.Is(err, ErrRateLimit) {
				logMsg("sync", "%s: rate limited — will resume next run (%d mined)", label, totalMined)
				return totalMined, true
			}
			logMsg("sync", "%s: %s failed: %v", label, sid, err)
			continue
		}

		totalMined++
		batchIDs = append(batchIDs, sid)
		logMsg("sync", "%s [%d/%d] complete (%d mined)", label, i+1, len(sessionIDs), totalMined)

		// Checkpoint.
		if totalMined%checkpointInterval == 0 && i < len(sessionIDs)-1 {
			if err := s.rebuildAndReindex(root); err != nil {
				logMsg("sync", "checkpoint reindex error: %v", err)
			}
			committed, err := s.commitAndPush(root, fmt.Sprintf("sync: %s checkpoint (%d sessions)", label, totalMined))
			if err != nil {
				logMsg("sync", "checkpoint commit error: %v", err)
			} else if committed {
				recordBatchOutcome(root, label, batchIDs)
				batchIDs = nil
			}
		}
	}
	return totalMined, false
}

// largeSessionBudget is the separate large-session (>300 msgs) lane cap.
// It rides ON TOP of SessionsMax rather than inside it: the floor of 1
// keeps deep sessions from starving behind a busy normal queue, at the
// cost of `--sessions-max N` admitting up to N+max(1,N/3) total.
func (s *SyncCmd) largeSessionBudget() int {
	if s.SkipLarge {
		return 0
	}
	return max(1, s.SessionsMax/3)
}

// mineSessions runs session mining: triage via FTS5 then extract via LLM.
// Two passes: normal sessions (<=300 msgs, batches of 3) then large sessions (>300 msgs, one at a time).
func (s *SyncCmd) mineSessions(root string) (int, error) {
	logMsg("sync", "session mining (triage + extract, %d normal + %d large)...", s.SessionsMax, s.largeSessionBudget())

	// Ensure ccrider DB is fresh before triaging.
	if out := runCmd("", "ccrider", "sync"); out != "" {
		logMsg("sync", "ccrider sync: %s", lastLine(out))
	}

	if s.DryRun {
		// Peek at the hook queue without clearing it. Lets `sync --dry-run`
		// show the near-real-time capture pipeline is working without
		// actually consuming the queue (that belongs to the real sync run).
		if peeked := peekPendingSessions(); len(peeked) > 0 {
			logMsg("sync", "DRY RUN -- hook queue: %d pending session(s): %s",
				len(peeked), strings.Join(peeked, ", "))
		}
		logMsg("sync", "DRY RUN -- triage results:")
		scribeExe, _ := os.Executable()
		if scribeExe == "" {
			scribeExe = "scribe"
		}
		out, _ := runCmdErr("", scribeExe, "triage", "--top", fmt.Sprintf("%d", s.SessionsMax), "--sort", s.SessionSort)
		if out != "" {
			fmt.Println(out)
		}
		return 0, nil
	}

	// Ensure _sessions_log.json exists.
	sessionsLog := filepath.Join(root, "wiki", "_sessions_log.json")
	if !fileExists(sessionsLog) {
		if err := os.WriteFile(sessionsLog, []byte(`{"processed": {}, "last_scan": null}`+"\n"), 0o644); err != nil {
			return 0, fmt.Errorf("init sessions log: %w", err)
		}
	}

	totalMined := 0

	// Drain the hook queue first. The SessionEnd hook (see hook.go) drops
	// high-value session IDs here as they happen, so draining before the
	// normal triage gives those sessions priority over whatever the FTS5
	// scorer would surface next. Already-processed pending IDs are filtered
	// out below; the file itself is cleared on read so IDs are not reused.
	pendingIDs, err := readAndClearPendingSessions()
	if err != nil {
		logMsg("sync", "pending queue read error (continuing): %v", err)
	}
	if len(pendingIDs) > 0 {
		// Drop anything already extracted before. A hook might enqueue a
		// session that a previous sync already absorbed — don't waste a
		// slot on it.
		processedSet := make(map[string]bool)
		for _, id := range loadProcessedSessionIDs(filepath.Join(root, "wiki", "_sessions_log.json")) {
			processedSet[id] = true
		}
		filtered := pendingIDs[:0]
		for _, id := range pendingIDs {
			if !processedSet[id] {
				filtered = append(filtered, id)
			}
		}
		pendingIDs = filtered
		if len(pendingIDs) > 0 {
			logMsg("sync", "hook queue: %d pending session(s) to prioritize", len(pendingIDs))
		}
	}

	// Pass 1: Normal sessions (<=300 messages) — batches of 3, 10min timeout.
	// Over-fetch 3x the budget: the pre-filter below drops KB, pending-
	// project, and mechanical sessions AFTER triage, so trimming to the
	// budget first would let a noisy unapproved project's sessions occupy
	// every slot and starve approved projects indefinitely (they rank
	// N+1 forever). The trim to SessionsMax happens after the filter.
	normalIDs := s.triageSessionIDs(s.SessionsMax*3, "--message-limit", "300")

	// Merge pending IDs ahead of triage picks so high-value sessions
	// jump the queue.
	if len(pendingIDs) > 0 {
		seen := make(map[string]bool, len(pendingIDs)+len(normalIDs))
		merged := make([]string, 0, len(pendingIDs)+len(normalIDs))
		for _, id := range pendingIDs {
			if !seen[id] {
				seen[id] = true
				merged = append(merged, id)
			}
		}
		for _, id := range normalIDs {
			if !seen[id] {
				seen[id] = true
				merged = append(merged, id)
			}
		}
		normalIDs = merged
	}

	// Pre-filter: skip mechanical sessions with too few user messages or content.
	cfg := loadConfig(root)
	if len(normalIDs) > 0 {
		kept, skipped := preFilterSessions(root, cfg.CcriderDB, normalIDs)
		if len(skipped) > 0 {
			logMsg("sync", "pre-filter: skipped %d mechanical sessions (<%d user msgs or <500 chars)", len(skipped), 3)
			// Mark skipped sessions so they're not re-triaged.
			sessionsLog := filepath.Join(root, "wiki", "_sessions_log.json")
			if err := updateJSONFile(sessionsLog, func(data map[string]any) {
				processed, _ := data["processed"].(map[string]any)
				if processed == nil {
					processed = make(map[string]any)
					data["processed"] = processed
				}
				for _, sid := range skipped {
					processed[sid] = map[string]any{
						"extracted": time.Now().UTC().Format(time.RFC3339),
						"skipped":   true,
						"reason":    "mechanical (low user messages or content)",
					}
				}
			}); err != nil {
				logMsg("sync", "warn: could not update _sessions_log.json: %v", err)
			}
		}
		normalIDs = kept
	}

	// Trim to the slot budget AFTER filtering, so dropped sessions never
	// consume extraction slots and the parallel extractor stays bounded.
	if len(normalIDs) > s.SessionsMax {
		normalIDs = normalIDs[:s.SessionsMax]
	}

	if len(normalIDs) > 0 {
		logMsg("sync", "triage found %d normal sessions (<=300 msgs)", len(normalIDs))
		mined, rateLimited := s.mineSessionBatches(root, normalIDs, cfg.Sync.ParallelExtractions, 10*time.Minute, "session-extract.md", "session")
		totalMined += mined
		if rateLimited {
			logMsg("sync", "rate limited during normal session mining — skipping large sessions")
			s.updateScanTimestamp(sessionsLog)
			logMsg("sync", "session mining complete (%d sessions mined)", totalMined)
			return totalMined, nil
		}
	} else {
		logMsg("sync", "no normal sessions to mine")
	}

	// Pass 2: Large sessions (>300 messages) — one at a time, 20min timeout.
	if largeMax := s.largeSessionBudget(); largeMax > 0 {
		largeIDs := s.triageSessionIDs(largeMax, "--min-messages", "301")
		// The large lane bypasses preFilterSessions (large sessions are
		// exempt from the mechanical/thin gate), so the scope guard must be
		// re-applied here or ignored/source-excluded projects leak in.
		largeIDs = filterSessionsByScope(root, cfg.CcriderDB, largeIDs)
		if len(largeIDs) > 0 {
			logMsg("sync", "triage found %d large sessions (>300 msgs)", len(largeIDs))
			mined, _ := s.mineSessionBatches(root, largeIDs, 1, 20*time.Minute, "session-extract-large.md", "large-session")
			totalMined += mined
		}
	}

	s.updateScanTimestamp(sessionsLog)
	logMsg("sync", "session mining complete (%d sessions mined)", totalMined)
	return totalMined, nil
}

// updateScanTimestamp updates the last_scan field in _sessions_log.json.
func (s *SyncCmd) updateScanTimestamp(sessionsLog string) {
	if err := updateJSONFile(sessionsLog, func(data map[string]any) {
		data["last_scan"] = time.Now().UTC().Format(time.RFC3339)
	}); err != nil {
		logMsg("sync", "warn: could not update last_scan in _sessions_log.json: %v", err)
	}
}
