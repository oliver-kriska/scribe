package main

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// session_backfill.go — `scribe sessions backfill-watermarks` (issue #146).
//
// Entries written before watermarks existed carry no through_seq, so the
// remine lane never selects them (an upgrade must not start re-mining old
// history on its own). This opt-in command gives each such entry the
// watermark its mine most likely reached, so the lane can read the rest:
//
//  1. Time: the sequence just before the first message stamped after the
//     processed time minus --margin. The processed time is stamped when
//     the mine finished, and ccrider indexes on its own schedule, so the
//     messages the mine actually read can end well before it; the margin
//     covers both. Scanning in sequence order and stopping at the first
//     later message keeps a stray out-of-order timestamp from pulling in
//     messages that were really added after the mine.
//  2. What the prompt showed: with a cap (envelope mode), a transcript
//     larger than the cap lost its middle, so the watermark drops to the
//     end of the head half (renderedThroughSeq) and the entry is partial.
//     In tools mode a session over largeSessionMessages went through the
//     large prompt, which reads the first 50 messages; the watermark drops
//     to message 50, partial.
//
// Re-reading content an old mine did see (the tail of a head/tail render)
// is accepted duplication; marking unseen content as mined would be loss,
// so every rule errs low.

// modeTools / modeEnvelope are session_mine.mode values (also --mode).
const (
	modeTools    = "tools"
	modeEnvelope = "envelope"
)

type SessionsBackfillWatermarksCmd struct {
	DryRun bool `help:"Report what would change without writing." short:"n"`
	Cap    int  `help:"Transcript cap (chars) the old envelope mines used. Default: min(session_mine.transcript_max_chars, 24000)."`
	// Mode lets a KB that switched session_mine.mode backfill the entries
	// from before the switch with the rule that mined them.
	Mode string `enum:",tools,envelope" default:"" help:"Mode the old mines ran in (tools|envelope). Default: the current session_mine.mode."`
	// MinedBefore lets a KB whose cap changed over time backfill in
	// passes: the older entries with the old cap, then the rest.
	MinedBefore string `name:"mined-before" help:"Only entries processed before this date (YYYY-MM-DD, UTC)."`
	// Margin is subtracted from each processed time before the time rule
	// runs: re-reading a little is duplication, over-claiming is loss.
	Margin time.Duration `default:"2h" help:"Treat messages stamped within this long before an entry's processed time as unread (the mine ran, and ccrider indexed, before the time was stamped)."`
}

// backfillMessage is one row of the per-session scan.
type backfillMessage struct {
	Sequence  int
	Timestamp time.Time
	HasTime   bool
}

// backfillResult is the watermark derived for one legacy entry.
type backfillResult struct {
	ID        string
	Mark      mineMark
	Remaining int // rendered chars past the watermark
}

// throughSeqAt returns the sequence of the last message stamped at or
// before minedAt, scanning in sequence order and stopping at the first
// message stamped after it. An untimed message neither stops the scan nor
// counts as seen on its own: it is covered only when a later stamped
// message is (it then sat between two seen messages). ok=false when
// nothing preceded the mine.
func throughSeqAt(messages []backfillMessage, minedAt time.Time) (int, bool) {
	through := 0
	for _, m := range messages {
		if !m.HasTime {
			continue
		}
		if m.Timestamp.After(minedAt) {
			break
		}
		through = m.Sequence
	}
	return through, through > 0
}

// backfillMark applies the "what the prompt showed" rule to the turns up
// to the time-based watermark. cap>0 is an envelope mine; cap==0 is tools
// mode, where messageCount decides between the normal and large prompt.
func backfillMark(shown []sessionTurn, through, capChars, messageCount int, headSeq int) mineMark {
	if capChars > 0 {
		return renderedThroughSeq(shown, capChars)
	}
	if messageCount >= largeSessionMinMessages && headSeq > 0 && headSeq < through {
		return mineMark{ThroughSeq: headSeq, Partial: true}
	}
	return mineMark{ThroughSeq: through}
}

// backfillCap resolves the cap the old mines are assumed to have used:
// 0 for tools mode (the prompt reads the session), else --cap or the
// smaller of today's cap and the old default.
func backfillCap(flagCap int, flagMode string, cfg SessionMineConfig) int {
	mode := flagMode
	if mode == "" {
		mode = cfg.Mode
	}
	if !strings.EqualFold(mode, modeEnvelope) {
		return 0
	}
	if flagCap > 0 {
		return flagCap
	}
	return min(cfg.TranscriptMaxChars, defaultTranscriptMaxChars)
}

// legacyMinedEntries returns processed IDs (sorted) that were mined, not
// skipped, and carry no watermark, with their processed time.
func legacyMinedEntries(processed map[string]any) (ids []string, minedAt map[string]time.Time, unparsable int) {
	minedAt = map[string]time.Time{}
	for id, entry := range processed {
		if entryIsSkipped(entry) {
			continue
		}
		if _, ok := entryThroughSeq(entry); ok {
			continue
		}
		stamp, _ := entry.(string)
		if stamp == "" {
			stamp = entryExtracted(entry)
		}
		t, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			unparsable++
			continue
		}
		ids = append(ids, id)
		minedAt[id] = t
	}
	sort.Strings(ids)
	return ids, minedAt, unparsable
}

// backfillOutcome says why deriveBackfill did or did not produce a mark.
type backfillOutcome int

const (
	backfillMarked     backfillOutcome = iota
	backfillNoMessages                 // ccrider has no messages for the session
	backfillUnmarkable                 // nothing counts as seen and no sequence sits below the first message
)

// deriveBackfill computes the watermark for one legacy session.
func deriveBackfill(db *sql.DB, dbPath, sessionID string, minedAt time.Time, capChars int) (backfillResult, backfillOutcome, error) {
	messages, err := scanBackfillMessages(db, sessionID)
	if err != nil {
		return backfillResult{}, backfillNoMessages, err
	}
	if len(messages) == 0 {
		return backfillResult{}, backfillNoMessages, nil
	}
	through, ok := throughSeqAt(messages, minedAt)
	if !ok {
		// Every message is stamped after the mine — clocks disagree or the
		// file was rewritten. Treat nothing as seen: re-mine it all.
		through = 0
	}

	turns, err := fetchSessionTranscript(dbPath, sessionID)
	if err != nil {
		return backfillResult{}, backfillNoMessages, err
	}
	shown := make([]sessionTurn, 0, len(turns))
	for _, t := range turns {
		if t.Sequence <= through {
			shown = append(shown, t)
		}
	}
	headSeq := 0
	if len(messages) >= toolsLargeHeadMessages {
		headSeq = messages[toolsLargeHeadMessages-1].Sequence
	}
	// The prompt was picked by the size the session had at mine time.
	countAtMine := 0
	for _, m := range messages {
		if m.Sequence <= through {
			countAtMine++
		}
	}
	mark := backfillMark(shown, through, capChars, countAtMine, headSeq)
	if mark.ThroughSeq <= 0 {
		// Nothing counts as seen. A zero watermark would be stored as
		// "unknown", so use the value just below the first message; when
		// that is not positive the entry cannot be marked and is left.
		mark = mineMark{ThroughSeq: messages[0].Sequence - 1, Partial: true}
		if mark.ThroughSeq <= 0 {
			return backfillResult{}, backfillUnmarkable, nil
		}
	}
	remaining := 0
	for _, t := range turnsAfter(turns, mark.ThroughSeq) {
		remaining += len(renderTurn(t))
	}
	return backfillResult{ID: sessionID, Mark: mark, Remaining: remaining}, backfillMarked, nil
}

func (c *SessionsBackfillWatermarksCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	cfg := loadConfig(root)
	view, err := loadSessionsLogView(root)
	if err != nil {
		return err
	}
	if c.Margin < 0 {
		return errors.New("--margin must not be negative")
	}
	ids, minedAt, unparsable := legacyMinedEntries(view.processed)
	if c.MinedBefore != "" {
		cutoff, err := time.Parse("2006-01-02", c.MinedBefore)
		if err != nil {
			return fmt.Errorf("--mined-before: want YYYY-MM-DD: %w", err)
		}
		ids = minedBeforeOnly(ids, minedAt, cutoff)
	}
	if len(ids) == 0 {
		fmt.Println("No mined entries without a watermark.")
		return nil
	}
	capChars := backfillCap(c.Cap, c.Mode, cfg.SessionMine)

	db, err := openSQLiteRO(cfg.CcriderDB)
	if err != nil {
		return fmt.Errorf("open ccrider db: %w", err)
	}
	defer db.Close()

	var results []backfillResult
	noMessages, unmarkable, failed := 0, 0, 0
	for _, id := range ids {
		r, outcome, err := deriveBackfill(db, cfg.CcriderDB, id, minedAt[id].Add(-c.Margin), capChars)
		if err != nil {
			// One unreadable session must not cost the others their mark;
			// it stays unmarked and a later run can retry it.
			logMsg("sessions", "backfill %s: %v", id, err)
			failed++
			continue
		}
		switch outcome {
		case backfillMarked:
			results = append(results, r)
		case backfillNoMessages:
			noMessages++
		case backfillUnmarkable:
			unmarkable++
		}
	}

	q := estimateRemineQueue(cfg.SessionMine, results, func(ids []string) []string {
		return filterSessionsByScope(root, cfg.CcriderDB, ids)
	})
	capNote := "none (tools mode: whole session, or first 50 messages over 300)"
	if capChars > 0 {
		capNote = fmt.Sprintf("%d chars", capChars)
	}
	fmt.Printf("Legacy mined entries: %d (assumed cap: %s, margin: %s)\n", len(ids), capNote, c.Margin)
	fmt.Printf("  watermark derived : %d (%d partial: unread content the session held at mine time)\n", len(results), q.partial)
	fmt.Printf("  content past mark : %d sessions, ~%d chars\n", q.withRemainder, q.remainingChars)
	fmt.Printf("  remine queue      : %d sessions, ~%d chars (in scope, and partial or past remine_min_new_chars)\n", len(q.queued), q.queuedChars)
	if len(q.queued) > 0 {
		fmt.Printf("  remine cost       : ~%d LLM calls at transcript_max_chars %d, ~%d sync runs at remine_max_per_run %d\n",
			q.calls, cfg.SessionMine.TranscriptMaxChars, q.runs, cfg.SessionMine.RemineMaxPerRun)
	}
	if noMessages > 0 {
		fmt.Printf("  not in ccrider    : %d (no messages; left as they are)\n", noMessages)
	}
	if unmarkable > 0 {
		fmt.Printf("  not markable      : %d (no message predates the mine, and none sits below the first sequence; left as they are)\n", unmarkable)
	}
	if failed > 0 {
		fmt.Printf("  read errors       : %d (see stderr; left as they are)\n", failed)
	}
	if unparsable > 0 {
		fmt.Printf("  bad timestamp     : %d (left as they are)\n", unparsable)
	}

	if c.DryRun {
		sort.Slice(results, func(i, j int) bool { return results[i].Remaining > results[j].Remaining })
		if len(results) > 0 {
			fmt.Println("\nLargest remainders:")
		}
		for _, r := range results[:min(10, len(results))] {
			fmt.Printf("  %s  through %d  partial=%v  ~%d chars\n", r.ID, r.Mark.ThroughSeq, r.Mark.Partial, r.Remaining)
		}
		return nil
	}
	if len(results) == 0 {
		fmt.Println("\nNothing to write.")
		return nil
	}

	written := 0
	// Under the sync lock: a running sync rewrites the log as it mines,
	// and the in-process mutex in updateSessionsLog does not reach it.
	err = withSyncLock(root, func() error {
		return updateSessionsLog(filepath.Join(root, "wiki", "_sessions_log.json"), func(data map[string]any) {
			processed, _ := data["processed"].(map[string]any)
			if processed == nil {
				return
			}
			for _, r := range results {
				entry, ok := processed[r.ID]
				// Re-check: a sync may have mined it since the scan.
				if !ok || entryIsSkipped(entry) {
					continue
				}
				if _, has := entryThroughSeq(entry); has {
					continue
				}
				processed[r.ID] = withBackfilledMark(entry, minedAt[r.ID], r.Mark)
				written++
			}
		})
	})
	if err != nil {
		return fmt.Errorf("write sessions log: %w", err)
	}
	fmt.Printf("\nWrote watermarks for %d sessions. The remine pass of `sync --sessions` mines what lies past them, %d per run (session_mine.remine_max_per_run).\n",
		written, cfg.SessionMine.RemineMaxPerRun)
	return nil
}

// remineQueueEstimate is what a backfill hands the remine lane.
type remineQueueEstimate struct {
	partial, withRemainder, remainingChars int
	queued                                 []string
	queuedChars                            int
	calls, runs                            int
}

// estimateRemineQueue counts the backfilled sessions the remine lane
// would actually pick (shouldRemine's size rule, then scope, which is
// filterSessionsByScope in production) and what draining them costs: a
// re-mine reads one cap-sized prefix of the remainder per call.
func estimateRemineQueue(cfg SessionMineConfig, results []backfillResult, scope func([]string) []string) remineQueueEstimate {
	var q remineQueueEstimate
	chars := map[string]int{}
	var eligible []string
	for _, r := range results {
		if r.Mark.Partial {
			q.partial++
		}
		if r.Remaining <= 0 {
			continue
		}
		q.withRemainder++
		q.remainingChars += r.Remaining
		if r.Mark.Partial || r.Remaining >= cfg.RemineMinNewChars {
			eligible = append(eligible, r.ID)
			chars[r.ID] = r.Remaining
		}
	}
	if len(eligible) > 0 {
		q.queued = scope(eligible)
	}
	perCall := max(cfg.TranscriptMaxChars, 1)
	for _, id := range q.queued {
		q.queuedChars += chars[id]
		q.calls += (chars[id] + perCall - 1) / perCall
	}
	if perRun := cfg.RemineMaxPerRun; perRun > 0 {
		q.runs = (q.calls + perRun - 1) / perRun
	}
	return q
}

// withBackfilledMark returns a copy of a legacy entry with the derived
// watermark added; fields an older scribe stored on the object survive.
func withBackfilledMark(entry any, minedAt time.Time, mark mineMark) any {
	built, ok := sessionsLogEntry(nil, minedAt.UTC().Format(time.RFC3339), mark).(map[string]any)
	if !ok {
		return entry
	}
	old, isMap := entry.(map[string]any)
	if !isMap {
		return built
	}
	copied := make(map[string]any, len(old)+len(built))
	for k, v := range old {
		copied[k] = v
	}
	for k, v := range built {
		if k == sessionsLogExtractedKey {
			continue // keep the original processed time
		}
		copied[k] = v
	}
	return copied
}

// minedBeforeOnly keeps the IDs processed strictly before cutoff.
func minedBeforeOnly(ids []string, minedAt map[string]time.Time, cutoff time.Time) []string {
	kept := make([]string, 0, len(ids))
	for _, id := range ids {
		if minedAt[id].Before(cutoff) {
			kept = append(kept, id)
		}
	}
	return kept
}

// scanBackfillMessages reads (sequence, timestamp) for a session in
// sequence order.
func scanBackfillMessages(db *sql.DB, sessionID string) ([]backfillMessage, error) {
	//nolint:noctx // CLI top-level
	rows, err := db.Query(`
		SELECT COALESCE(m.sequence, 0), COALESCE(m.timestamp, '')
		FROM messages m JOIN sessions s ON s.id = m.session_id
		WHERE s.session_id = ?
		ORDER BY m.sequence ASC, m.id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", sessionID, err)
	}
	defer rows.Close()
	var messages []backfillMessage
	for rows.Next() {
		var seq int
		var stamp string
		if err := rows.Scan(&seq, &stamp); err != nil {
			return nil, fmt.Errorf("scan %s: %w", sessionID, err)
		}
		t, ok := parseCcriderTime(stamp)
		messages = append(messages, backfillMessage{Sequence: seq, Timestamp: t, HasTime: ok})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", sessionID, err)
	}
	return messages, nil
}
