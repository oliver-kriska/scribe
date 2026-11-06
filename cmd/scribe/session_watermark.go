package main

import (
	"database/sql"
	"fmt"
	"hash/fnv"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// session_watermark.go — "mined through here" for a session (issue #146).
//
// A session used to be mined once: its ID went into
// wiki/_sessions_log.json and triage excluded it forever. Two kinds of
// content were then never read: messages added after the mine (sessions
// are admitted while still open — a desktop session, a resume and a
// compaction all keep the same ID), and the part of a long transcript
// the prompt never showed (envelope mode drops the middle; the tools-mode
// large prompt reads the first and last 50 messages).
//
// The watermark is the ccrider `sequence` of the last message the model
// was actually shown. `sequence` is the JSONL line index — unique per
// session, in file order, stable across re-imports, with gaps. Timestamps
// are not used: they are out of order in real sessions. Everything after
// the watermark is "not yet mined", whatever the reason, so one mechanism
// (the remine lane in mineGrownSessions) covers both gaps.
//
// Entry shapes in `processed` (all must keep loading):
//
//	"2026-11-06T00:00:00Z"                              legacy, no watermark
//	{"extracted": ts, "skipped": true, "reason": …}     mechanical skip
//	{"extracted": ts, "through_seq": 812}               mined through 812
//
// Legacy entries are never re-selected — a scribe upgrade must not start
// spending LLM calls on sessions mined before it. `scribe sessions
// backfill-watermarks` (a separate, opt-in step) can give them one.

const (
	sessionsLogExtractedKey  = "extracted"
	sessionsLogThroughSeqKey = "through_seq"
	// sessionsLogPartialKey marks an entry whose mine stopped before the
	// end of what the session held at the time (a dropped middle, a
	// capped prefix, the tools-mode large prompt). The rest is old
	// content, so it is re-mined without waiting for new growth.
	sessionsLogPartialKey = "partial"
	// remineLabel is the batch label for re-mines; it maps to the
	// session-mine prompt family (promptBaseForSessionLabel) and shows
	// up as its own row in `scribe cost`.
	remineLabel = "session-mine-remine"
	// remineContextMaxChars bounds the framing block prepended to a
	// delta transcript (first user turn + pages already written).
	remineContextMaxChars = 4000
)

// mineMark is what one mine reached: the watermark, and whether content
// the session already held lies past it.
type mineMark struct {
	ThroughSeq int
	Partial    bool
}

// entryThroughSeq returns the watermark stored in a processed entry.
func entryThroughSeq(entry any) (int, bool) {
	m, ok := entry.(map[string]any)
	if !ok {
		return 0, false
	}
	// encoding/json decodes numbers into float64.
	f, ok := m[sessionsLogThroughSeqKey].(float64)
	if !ok || f <= 0 {
		return 0, false
	}
	return int(f), true
}

// entryPartial reports whether a processed entry is marked partial.
func entryPartial(entry any) bool {
	m, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	partial, _ := m[sessionsLogPartialKey].(bool)
	return partial
}

// sessionsLogEntry builds the value to store for a mined session. It
// never lowers a watermark the existing entry already carries: two runs
// (or two machines) can finish out of order, and the entry with the
// higher watermark is kept whole. A zero ThroughSeq means "not known" and
// keeps the legacy bare-timestamp shape unless the existing entry already
// has a watermark.
func sessionsLogEntry(existing any, timestamp string, mark mineMark) any {
	if prev, ok := entryThroughSeq(existing); ok && prev > mark.ThroughSeq {
		return existing
	}
	if mark.ThroughSeq <= 0 {
		return timestamp
	}
	entry := map[string]any{
		sessionsLogExtractedKey:  timestamp,
		sessionsLogThroughSeqKey: mark.ThroughSeq,
	}
	if mark.Partial {
		entry[sessionsLogPartialKey] = true
	}
	return entry
}

// lastSequence returns the sequence of the last turn, or 0 for none.
func lastSequence(turns []sessionTurn) int {
	if len(turns) == 0 {
		return 0
	}
	return turns[len(turns)-1].Sequence
}

// turnsAfter returns the turns whose sequence is past the watermark.
func turnsAfter(turns []sessionTurn, throughSeq int) []sessionTurn {
	after := make([]sessionTurn, 0, len(turns))
	for _, t := range turns {
		if t.Sequence > throughSeq {
			after = append(after, t)
		}
	}
	return after
}

// renderedThroughSeq returns the watermark for a transcript rendered by
// renderTranscriptForPrompt with maxChars: the last turn when it fits
// whole, else the last turn that ends inside the head half — the dropped
// middle and the tail stay "not yet mined" (re-reading the tail on a
// later delta is accepted duplication; skipping the middle is loss).
func renderedThroughSeq(turns []sessionTurn, maxChars int) mineMark {
	total := 0
	for _, t := range turns {
		total += len(renderTurn(t))
	}
	if maxChars <= 0 || total <= maxChars {
		return mineMark{ThroughSeq: lastSequence(turns)}
	}
	half := max(maxChars/2, 200)
	through, end := 0, 0
	for _, t := range turns {
		end += len(renderTurn(t))
		if end > half {
			break
		}
		through = t.Sequence
	}
	return mineMark{ThroughSeq: through, Partial: true}
}

// renderTranscriptPrefix renders turns in order until the next whole turn
// would pass maxChars, and returns the sequence of the last turn shown.
// Unlike renderTranscriptForPrompt it never drops a middle: whatever does
// not fit stays past the watermark for the next run. A first turn larger
// than maxChars is cut to fit and still counts as shown: the alternative
// is re-sending the same oversized turn every run and never advancing.
func renderTranscriptPrefix(turns []sessionTurn, maxChars int) (string, mineMark) {
	var sb strings.Builder
	through := 0
	for _, t := range turns {
		piece := renderTurn(t)
		if maxChars > 0 && sb.Len()+len(piece) > maxChars {
			if sb.Len() == 0 {
				sb.WriteString(truncateBytes(piece, maxChars))
				through = t.Sequence
			}
			break
		}
		sb.WriteString(piece)
		through = t.Sequence
	}
	return sb.String(), mineMark{ThroughSeq: through, Partial: through < lastSequence(turns)}
}

// remineContext is the framing block put in front of a delta transcript:
// what the session was about, and which pages earlier mines of it wrote,
// so the model appends to them instead of re-creating them (create on an
// existing path is refused under entityWriterApplyOptions).
func remineContext(firstUserTurn string, pages []string) string {
	var sb strings.Builder
	sb.WriteString("[CONTINUATION] This session was already mined up to an earlier point. ")
	sb.WriteString("The transcript below is only what came after. Extract only knowledge that is new in it. ")
	sb.WriteString("For a topic already covered by a page listed here, use `append` on that page, not `create`. ")
	sb.WriteString("Add a rolling-memory entry only for a fact not already recorded.\n")
	if firstUserTurn != "" {
		sb.WriteString("\nThe session began with:\n")
		sb.WriteString(truncateBytes(firstUserTurn, remineContextMaxChars/2))
		sb.WriteString("\n")
	}
	if len(pages) > 0 {
		sb.WriteString("\nPages already written from this session (these files exist):\n")
		for _, p := range pages {
			sb.WriteString("- " + p + "\n")
		}
	}
	return truncateBytes(sb.String(), remineContextMaxChars) + "\n[END CONTINUATION]\n\n"
}

// firstUserText returns the text of the session's first user turn.
func firstUserText(turns []sessionTurn) string {
	for _, t := range turns {
		if t.Role == "user" && t.Text != "" {
			return t.Text
		}
	}
	return ""
}

// pagesFromSession lists KB-relative paths of articles whose frontmatter
// cites session:<id>. Walks the wiki once; the remine lane runs it for a
// handful of sessions per sync, never in a hot path.
func pagesFromSession(root, sessionID string) []string {
	// Bounded on the right so session "a-1" does not match "session:a-10".
	cites := regexp.MustCompile(`session:` + regexp.QuoteMeta(sessionID) + `(?:[^A-Za-z0-9_-]|$)`)
	var pages []string
	if err := walkArticles(root, func(path string, content []byte) error {
		if cites.Match(frontmatterBlock(content)) {
			if rel, err := filepath.Rel(root, path); err == nil {
				pages = append(pages, rel)
			}
		}
		return nil
	}); err != nil {
		// A partial list only weakens the append-not-create hint; the
		// apply step still refuses to overwrite an existing page.
		logPhaseDegraded("sync", "session remine", "listing pages for session %s: %v", sessionID, err)
	}
	sort.Strings(pages)
	return pages
}

// frontmatterBlock returns the leading YAML frontmatter, or nil.
func frontmatterBlock(content []byte) []byte {
	s := string(content)
	if !strings.HasPrefix(s, "---\n") {
		return nil
	}
	end := strings.Index(s[4:], "\n---")
	if end < 0 {
		return nil
	}
	return content[:4+end]
}

// grownSession is a mined session with unmined content past its watermark.
type grownSession struct {
	ID         string
	ThroughSeq int
	NewChars   int
}

// remineCandidate is a watermarked processed entry plus the ccrider facts
// findGrownSessions needs to decide whether it has grown enough.
type remineCandidate struct {
	ThroughSeq int
	Partial    bool
	UpdatedAt  time.Time
	NewChars   int
	MaxSeq     int
}

// shouldRemine is the one predicate for "mined, but there is more to
// mine": a watermark, content past it, and quiet long enough that the
// session is probably finished. New growth must reach minNewChars; a
// partial entry's remainder is old content and needs none. updated_at is
// deliberately not compared with the mine time: a session mined after it
// went quiet with its middle dropped has not changed since, and still
// needs the rest read. A max sequence below the watermark means the
// source file was rewritten — never re-select on that.
func shouldRemine(c remineCandidate, minNewChars int, quiet time.Duration, now time.Time) bool {
	if c.ThroughSeq <= 0 || c.MaxSeq < c.ThroughSeq || c.NewChars <= 0 {
		return false
	}
	if now.Sub(c.UpdatedAt) < quiet {
		return false
	}
	return c.Partial || c.NewChars >= minNewChars
}

// findGrownSessions returns watermarked sessions that pass shouldRemine,
// in an order that rotates daily (remineOrderKey). Cheap filter first
// (quiet long enough, from the sessions row), then one indexed
// per-session sum only for the survivors. The size estimate matches
// renderTranscriptForPrompt: text whole, tool payloads capped at 1200
// bytes.
func findGrownSessions(dbPath string, processed map[string]any, cfg SessionMineConfig, now time.Time) ([]grownSession, error) {
	db, err := openSQLiteRO(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open ccrider db: %w", err)
	}
	defer db.Close()

	quiet := time.Duration(cfg.RemineQuietHours) * time.Hour
	var grown []grownSession
	for id, entry := range processed {
		through, ok := entryThroughSeq(entry)
		if !ok {
			continue
		}
		c, found, err := queryRemineCandidate(db, id, through, func(updatedAt time.Time) bool {
			// Cheap pre-check on the sessions row: skip the message scan
			// for sessions still active.
			return now.Sub(updatedAt) >= quiet
		})
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		c.Partial = entryPartial(entry)
		if c.MaxSeq < through {
			logPhaseDegraded("sync", "session remine",
				"session %s: ccrider max sequence %d is below the mined watermark %d (source rewritten?) — not re-mining", id, c.MaxSeq, through)
			continue
		}
		if shouldRemine(c, cfg.RemineMinNewChars, quiet, now) {
			grown = append(grown, grownSession{ID: id, ThroughSeq: through, NewChars: c.NewChars})
		}
	}
	day := now.UTC().Format("2006-01-02")
	sort.Slice(grown, func(i, j int) bool {
		return remineOrderKey(grown[i].ID, day) < remineOrderKey(grown[j].ID, day)
	})
	return grown, nil
}

// remineOrderKey orders remine candidates. A failed re-mine leaves the
// watermark where it was, so a fixed order (e.g. most new content first)
// would hand the same failing session the first slot every run and starve
// the lane. A per-day hash keeps the order stable within a day and
// reshuffles it the next.
func remineOrderKey(sessionID, day string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(day + "\x00" + sessionID))
	return h.Sum64()
}

// queryRemineCandidate reads updated_at, max sequence and the new-content
// estimate for one session. found=false when ccrider no longer has it,
// or when worthSizing rejects its updated_at.
// worthSizing gates the message scan on updated_at.
func queryRemineCandidate(db *sql.DB, sessionID string, throughSeq int, worthSizing func(updatedAt time.Time) bool) (remineCandidate, bool, error) {
	var updated sql.NullString
	var pk int64
	//nolint:noctx // CLI top-level
	err := db.QueryRow(`SELECT id, updated_at FROM sessions WHERE session_id = ?`, sessionID).Scan(&pk, &updated)
	if err == sql.ErrNoRows {
		return remineCandidate{}, false, nil
	}
	if err != nil {
		return remineCandidate{}, false, fmt.Errorf("query session %s: %w", sessionID, err)
	}
	updatedAt, ok := parseCcriderTime(updated.String)
	if !ok || !worthSizing(updatedAt) {
		return remineCandidate{}, false, nil
	}
	var newChars, maxSeq sql.NullInt64
	//nolint:noctx // CLI top-level
	err = db.QueryRow(`
		SELECT
			SUM(CASE WHEN sequence > ? THEN
				CASE WHEN length(COALESCE(text_content, '')) > 0 THEN length(text_content)
				     ELSE MIN(length(COALESCE(content, '')), 1200) END
			ELSE 0 END),
			MAX(sequence)
		FROM messages WHERE session_id = ?`, throughSeq, pk).Scan(&newChars, &maxSeq)
	if err != nil {
		return remineCandidate{}, false, fmt.Errorf("size session %s: %w", sessionID, err)
	}
	return remineCandidate{
		ThroughSeq: throughSeq,
		UpdatedAt:  updatedAt,
		NewChars:   int(newChars.Int64),
		MaxSeq:     int(maxSeq.Int64),
	}, true, nil
}

// mineGrownSessions is the remine lane: after the normal and large passes,
// mine up to RemineMaxPerRun grown sessions through the envelope path.
// The envelope path is used whatever session_mine.mode says, because only
// it can start at an offset; mineSessionEnvelope sees the watermark and
// mines the delta.
func (s *SyncCmd) mineGrownSessions(root string, cfg *ScribeConfig) (int, bool) {
	if !cfg.SessionMine.remineEnabled() || cfg.SessionMine.RemineMaxPerRun <= 0 {
		return 0, false
	}
	view, err := loadSessionsLogView(root)
	if err != nil {
		logPhaseDegraded("sync", "session remine", "could not read sessions log: %v", err)
		return 0, false
	}
	grown, err := findGrownSessions(cfg.CcriderDB, view.processed, cfg.SessionMine, time.Now())
	if err != nil {
		logPhaseDegraded("sync", "session remine", "%v", err)
		return 0, false
	}
	ids := make([]string, 0, len(grown))
	for _, g := range grown {
		ids = append(ids, g.ID)
	}
	// Same scope rules as first mines: a project that was un-approved or
	// ignored since does not get re-mined.
	ids = filterSessionsByScope(root, cfg.CcriderDB, ids)
	if len(ids) > cfg.SessionMine.RemineMaxPerRun {
		ids = ids[:cfg.SessionMine.RemineMaxPerRun]
	}
	setRunStat("remine_candidates", len(grown))
	if len(ids) == 0 {
		return 0, false
	}
	logMsg("sync", "remine: %d grown session(s) since last mine, mining %d", len(grown), len(ids))
	remineCfg := *cfg
	if !strings.EqualFold(cfg.SessionMine.Mode, "envelope") && s.Model != "" {
		// Tools-mode installs pick the model with `sync --model`; the
		// re-mine must use the same one, not session_mine.model.
		remineCfg.SessionMine.Model = s.Model
	}
	return s.mineSessionBatchesEnvelope(root, ids, 1, remineLabel, &remineCfg)
}

// largeSessionPrompt is the tools-mode prompt that reads only the first
// and last toolsLargeHeadMessages messages of a session.
const (
	largeSessionPrompt     = "session-extract-large.md"
	toolsLargeHeadMessages = 50
)

// toolsModeWatermarks snapshots, per session, what a tools-mode prompt is
// guaranteed to read: the whole session for the normal prompt, the first
// 50 messages for the large one (it reads the last 50 too, but the middle
// before them is not read, so the watermark cannot pass it). A lookup
// error leaves the session out of the map — recorded with no watermark,
// exactly as before this change.
func toolsModeWatermarks(dbPath string, sessionIDs []string, promptName string) map[string]mineMark {
	marks := make(map[string]mineMark, len(sessionIDs))
	db, err := openSQLiteRO(dbPath)
	if err != nil {
		logPhaseDegraded("sync", "session watermark", "open ccrider db: %v — sessions in this batch get no watermark and will not be re-mined", err)
		return marks
	}
	defer db.Close()
	query := `SELECT MAX(m.sequence) FROM messages m JOIN sessions s ON s.id = m.session_id WHERE s.session_id = ?`
	args := func(id string) []any { return []any{id} }
	if promptName == largeSessionPrompt {
		query = `SELECT m.sequence FROM messages m JOIN sessions s ON s.id = m.session_id
			WHERE s.session_id = ? ORDER BY m.sequence LIMIT 1 OFFSET ?`
		args = func(id string) []any { return []any{id, toolsLargeHeadMessages - 1} }
	}
	for _, id := range sessionIDs {
		var seq sql.NullInt64
		//nolint:noctx // CLI top-level
		if err := db.QueryRow(query, args(id)...).Scan(&seq); err == nil && seq.Valid {
			// The large prompt stops reading the head at message 50, so
			// the rest of what the session held is unread: partial.
			marks[id] = mineMark{ThroughSeq: int(seq.Int64), Partial: promptName == largeSessionPrompt}
		}
	}
	return marks
}
