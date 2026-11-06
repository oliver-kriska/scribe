package main

import (
	"database/sql"
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
//     processed time. Scanning in sequence order and stopping at the first
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
	Mode string `enum:",tools,envelope" default:"" help:"Mode the old mines ran in (tools|envelope). Default: the current session_mine.mode. Run between scheduled syncs: the log write is not locked against another process."`
	// MinedBefore lets a KB whose cap changed over time backfill in
	// passes: the older entries with the old cap, then the rest.
	MinedBefore string `name:"mined-before" help:"Only entries processed before this date (YYYY-MM-DD, UTC)."`
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

// deriveBackfill computes the watermark for one legacy session.
// found=false when ccrider has no messages for it.
func deriveBackfill(db *sql.DB, dbPath, sessionID string, minedAt time.Time, capChars int) (backfillResult, bool, error) {
	messages, err := scanBackfillMessages(db, sessionID)
	if err != nil {
		return backfillResult{}, false, err
	}
	if len(messages) == 0 {
		return backfillResult{}, false, nil
	}
	through, ok := throughSeqAt(messages, minedAt)
	if !ok {
		// Every message is stamped after the mine — clocks disagree or the
		// file was rewritten. Treat nothing as seen: re-mine it all.
		through = 0
	}

	turns, err := fetchSessionTranscript(dbPath, sessionID)
	if err != nil {
		return backfillResult{}, false, err
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
			return backfillResult{}, false, nil
		}
	}
	remaining := 0
	for _, t := range turnsAfter(turns, mark.ThroughSeq) {
		remaining += len(renderTurn(t))
	}
	return backfillResult{ID: sessionID, Mark: mark, Remaining: remaining}, true, nil
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
	notInDB := 0
	for _, id := range ids {
		r, found, err := deriveBackfill(db, cfg.CcriderDB, id, minedAt[id], capChars)
		if err != nil {
			return err
		}
		if !found {
			notInDB++
			continue
		}
		results = append(results, r)
	}

	partial, withRemainder, remainingChars := 0, 0, 0
	for _, r := range results {
		if r.Mark.Partial {
			partial++
		}
		if r.Remaining > 0 {
			withRemainder++
			remainingChars += r.Remaining
		}
	}
	capNote := "none (tools mode: whole session, or first 50 messages over 300)"
	if capChars > 0 {
		capNote = fmt.Sprintf("%d chars", capChars)
	}
	fmt.Printf("Legacy mined entries: %d (assumed cap: %s)\n", len(ids), capNote)
	fmt.Printf("  watermark derived : %d\n", len(results))
	fmt.Printf("  partial           : %d (unread content the session held at mine time)\n", partial)
	fmt.Printf("  content past mark : %d sessions, ~%d chars\n", withRemainder, remainingChars)
	if notInDB > 0 {
		fmt.Printf("  not markable      : %d (no messages in ccrider, or no sequence to mark before the first; left as they are)\n", notInDB)
	}
	if unparsable > 0 {
		fmt.Printf("  bad timestamp     : %d (left as they are)\n", unparsable)
	}

	if c.DryRun {
		sort.Slice(results, func(i, j int) bool { return results[i].Remaining > results[j].Remaining })
		fmt.Println("\nLargest remainders:")
		for _, r := range results[:min(10, len(results))] {
			fmt.Printf("  %s  through %d  partial=%v  ~%d chars\n", r.ID, r.Mark.ThroughSeq, r.Mark.Partial, r.Remaining)
		}
		return nil
	}

	written := 0
	err = updateSessionsLog(filepath.Join(root, "wiki", "_sessions_log.json"), func(data map[string]any) {
		processed, _ := data["processed"].(map[string]any)
		if processed == nil {
			return
		}
		for _, r := range results {
			entry, ok := processed[r.ID]
			// Re-check under the lock: a sync may have mined it since.
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
	if err != nil {
		return fmt.Errorf("write sessions log: %w", err)
	}
	fmt.Printf("\nWrote watermarks for %d sessions. The remine pass of `sync --sessions` mines what lies past them, %d per run (session_mine.remine_max_per_run).\n",
		written, cfg.SessionMine.RemineMaxPerRun)
	return nil
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
