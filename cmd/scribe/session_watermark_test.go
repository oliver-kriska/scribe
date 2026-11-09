package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// insertSeqMessage adds a text message with an explicit sequence.
func insertSeqMessage(t *testing.T, db *sql.DB, sessionRowID int64, msgType, text string, sequence int) {
	t.Helper()
	//nolint:noctx // test fixture
	if _, err := db.Exec(`INSERT INTO messages (session_id, type, text_content, sequence) VALUES (?, ?, ?, ?)`,
		sessionRowID, msgType, text, sequence); err != nil {
		t.Fatal(err)
	}
}

// decodeEntry round-trips an entry through JSON, as the log file does.
func decodeEntry(t *testing.T, entry any) any {
	t.Helper()
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSessionsLogEntry(t *testing.T) {
	const ts = "2026-11-06T00:00:00Z"
	tests := []struct {
		name     string
		existing any
		through  int
		wantSeq  int
		wantBare bool
	}{
		{name: "unknown watermark keeps legacy string", existing: nil, through: 0, wantBare: true},
		{name: "new watermark", existing: nil, through: 12, wantSeq: 12},
		{name: "legacy string gains a watermark", existing: "2026-01-01T00:00:00Z", through: 7, wantSeq: 7},
		{name: "never lowers", existing: map[string]any{"extracted": ts, "through_seq": float64(40)}, through: 9, wantSeq: 40},
		{name: "unknown keeps existing", existing: map[string]any{"extracted": ts, "through_seq": float64(40)}, through: 0, wantSeq: 40},
		{name: "raises", existing: map[string]any{"extracted": ts, "through_seq": float64(40)}, through: 90, wantSeq: 90},
		{name: "partial kept when the higher entry wins", existing: map[string]any{"extracted": ts, "through_seq": float64(40), "partial": true}, through: 9, wantSeq: 40},
		{name: "skip entry replaced on mine", existing: map[string]any{"extracted": ts, "skipped": true}, through: 5, wantSeq: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodeEntry(t, sessionsLogEntry(tt.existing, ts, mineMark{ThroughSeq: tt.through}))
			if tt.wantBare {
				if s, ok := got.(string); !ok || s != ts {
					t.Fatalf("got %#v, want bare %q", got, ts)
				}
				return
			}
			seq, ok := entryThroughSeq(got)
			if !ok || seq != tt.wantSeq {
				t.Fatalf("through_seq = %d (%v), want %d", seq, ok, tt.wantSeq)
			}
			if entryIsSkipped(got) {
				t.Fatalf("mined entry still marked skipped: %#v", got)
			}
		})
	}
}

func TestSessionsLogEntryPartial(t *testing.T) {
	got := decodeEntry(t, sessionsLogEntry(nil, "2026-11-06T00:00:00Z", mineMark{ThroughSeq: 5, Partial: true}))
	if !entryPartial(got) {
		t.Fatalf("partial mark not stored: %#v", got)
	}
	got = decodeEntry(t, sessionsLogEntry(got, "2026-11-07T00:00:00Z", mineMark{ThroughSeq: 9}))
	if entryPartial(got) {
		t.Fatalf("a complete mine past the old watermark must clear partial: %#v", got)
	}
}

func seqTurns(texts ...string) []sessionTurn {
	turns := make([]sessionTurn, 0, len(texts))
	for i, text := range texts {
		turns = append(turns, sessionTurn{Role: "user", Text: text, Sequence: (i + 1) * 10})
	}
	return turns
}

func TestRenderedThroughSeq(t *testing.T) {
	small := seqTurns("a", "b", "c")
	if got := renderedThroughSeq(small, 10000); got != (mineMark{ThroughSeq: 30}) {
		t.Fatalf("fits whole: got %+v, want 30", got)
	}
	if got := renderedThroughSeq(nil, 10000); got != (mineMark{}) {
		t.Fatalf("empty: got %+v, want 0", got)
	}
	// 10 turns of ~110 rendered bytes; cap 600 → head half 300 → 2 turns.
	big := seqTurns(strings.Repeat("x", 100), strings.Repeat("x", 100), strings.Repeat("x", 100),
		strings.Repeat("x", 100), strings.Repeat("x", 100), strings.Repeat("x", 100),
		strings.Repeat("x", 100), strings.Repeat("x", 100), strings.Repeat("x", 100), strings.Repeat("x", 100))
	if got := renderedThroughSeq(big, 600); got != (mineMark{ThroughSeq: 20, Partial: true}) {
		t.Fatalf("middle dropped: got %+v, want 20 (end of head half)", got)
	}
	// The watermark never passes what renderTranscriptForPrompt shows.
	head := renderTranscriptForPrompt(big, 600)
	if !strings.Contains(head, "truncated") {
		t.Fatalf("expected the renderer to truncate this fixture")
	}
}

func TestRenderTranscriptPrefix(t *testing.T) {
	turns := seqTurns(strings.Repeat("a", 100), strings.Repeat("b", 100), strings.Repeat("c", 100))
	all, mark := renderTranscriptPrefix(turns, 0)
	if mark != (mineMark{ThroughSeq: 30}) || !strings.Contains(all, "ccc") {
		t.Fatalf("no cap: mark=%+v", mark)
	}
	part, mark := renderTranscriptPrefix(turns, 250)
	if mark != (mineMark{ThroughSeq: 20, Partial: true}) || strings.Contains(part, "ccc") || !strings.Contains(part, "bbb") {
		t.Fatalf("cap 250: mark=%+v, rendered %q", mark, part)
	}
	if strings.Contains(part, "truncated") {
		t.Fatalf("prefix render must never drop a middle")
	}
	cut, mark := renderTranscriptPrefix(turns, 40)
	if mark.ThroughSeq != 10 || len(cut) > 40 {
		t.Fatalf("oversized first turn: mark=%+v len=%d — must still make progress within the cap", mark, len(cut))
	}
	if _, mark := renderTranscriptPrefix(nil, 100); mark != (mineMark{}) {
		t.Fatalf("empty: mark=%+v", mark)
	}
}

func TestTurnsAfter(t *testing.T) {
	turns := seqTurns("a", "b", "c")
	got := turnsAfter(turns, 10)
	if len(got) != 2 || got[0].Sequence != 20 {
		t.Fatalf("got %+v", got)
	}
	if len(turnsAfter(turns, 30)) != 0 {
		t.Fatalf("nothing after the last turn")
	}
}

func TestShouldRemine(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	base := remineCandidate{ThroughSeq: 100, MaxSeq: 500, UpdatedAt: now.Add(-10 * time.Hour), NewChars: 50000}
	quiet := 6 * time.Hour
	tests := []struct {
		name string
		edit func(c remineCandidate) remineCandidate
		want bool
	}{
		{"grown and quiet", func(c remineCandidate) remineCandidate { return c }, true},
		{"no watermark", func(c remineCandidate) remineCandidate { c.ThroughSeq = 0; return c }, false},
		{"too little new", func(c remineCandidate) remineCandidate { c.NewChars = 100; return c }, false},
		{"still active", func(c remineCandidate) remineCandidate { c.UpdatedAt = now.Add(-time.Hour); return c }, false},
		// Mined after it went quiet with the middle dropped: no update
		// since, but the rest is still unread.
		{"quiet long before the mine", func(c remineCandidate) remineCandidate { c.UpdatedAt = now.Add(-30 * 24 * time.Hour); return c }, true},
		{"partial remainder under threshold", func(c remineCandidate) remineCandidate { c.Partial = true; c.NewChars = 100; return c }, true},
		{"partial with nothing left", func(c remineCandidate) remineCandidate { c.Partial = true; c.NewChars = 0; return c }, false},
		{"partial still active", func(c remineCandidate) remineCandidate { c.Partial = true; c.UpdatedAt = now; return c }, false},
		{"source rewritten", func(c remineCandidate) remineCandidate { c.MaxSeq = 50; return c }, false},
		{"exact threshold", func(c remineCandidate) remineCandidate { c.NewChars = 20000; return c }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldRemine(tt.edit(base), 20000, quiet, now); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFindGrownSessions(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	old := now.Add(-24 * time.Hour).Format("2006-01-02 15:04:05.999999999 -0700 MST")

	grownRow := insertFixtureSession(t, db, "grown", "/p", 3, old, old, "")
	insertSeqMessage(t, db, grownRow, "user", "start", 1)
	insertSeqMessage(t, db, grownRow, "assistant", strings.Repeat("n", 300), 2)
	insertSeqMessage(t, db, grownRow, "user", strings.Repeat("m", 300), 3)

	smallRow := insertFixtureSession(t, db, "small", "/p", 2, old, old, "")
	insertSeqMessage(t, db, smallRow, "user", "start", 1)
	insertSeqMessage(t, db, smallRow, "assistant", "tiny", 2)

	processed := map[string]any{
		"grown":   map[string]any{"extracted": now.Add(-72 * time.Hour).Format(time.RFC3339), "through_seq": float64(1)},
		"small":   map[string]any{"extracted": now.Add(-72 * time.Hour).Format(time.RFC3339), "through_seq": float64(1)},
		"legacy":  now.Add(-72 * time.Hour).Format(time.RFC3339),
		"missing": map[string]any{"extracted": now.Add(-72 * time.Hour).Format(time.RFC3339), "through_seq": float64(1)},
	}
	cfg := SessionMineConfig{RemineMinNewChars: 500, RemineQuietHours: 6}
	got, err := findGrownSessions(dbPath, processed, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "grown" || got[0].NewChars != 600 || got[0].ThroughSeq != 1 {
		t.Fatalf("got %+v, want only 'grown' with 600 new chars", got)
	}
}

func TestEnvelopeTranscriptRemine(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wiki", "decisions"), 0o755); err != nil {
		t.Fatal(err)
	}
	row := insertFixtureSession(t, db, "s1", "/p", 3, "", "", "")
	insertSeqMessage(t, db, row, "user", "FRAMING question", 5)
	insertSeqMessage(t, db, row, "assistant", "OLD answer", 9)
	insertSeqMessage(t, db, row, "user", "NEW follow-up", 14)

	// First mine: whole transcript, watermark at the last turn.
	text, cont, mark := envelopeTranscript(root, dbPath, "s1", 10000)
	if mark != (mineMark{ThroughSeq: 14}) || !strings.Contains(text, "OLD answer") || cont != "" {
		t.Fatalf("first mine: mark=%+v text=%q cont=%q", mark, text, cont)
	}

	// Re-mine: watermark at 9 → only the delta, behind the header that
	// names the existing page.
	page := "---\ntitle: X\nsources: [\"session:s1\"]\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(root, "wiki", "decisions", "x.md"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	recordSessionProcessed(root, "s1", mineMark{ThroughSeq: 9})
	text, cont, mark = envelopeTranscript(root, dbPath, "s1", 10000)
	if mark != (mineMark{ThroughSeq: 14}) {
		t.Fatalf("remine mark=%+v, want through 14", mark)
	}
	// The instructions travel in the continuation block, never inside
	// the transcript (which the prompt fences off as data).
	for _, want := range []string{"Continuation of an earlier mine", "FRAMING question", "x.md"} {
		if !strings.Contains(cont, want) {
			t.Errorf("continuation missing %q:\n%s", want, cont)
		}
	}
	if !strings.Contains(text, "NEW follow-up") || strings.Contains(text, "Continuation") {
		t.Errorf("remine transcript should be the bare delta:\n%s", text)
	}
	if strings.Contains(text, "OLD answer") {
		t.Errorf("remine transcript repeats mined content:\n%s", text)
	}

	// Nothing new past the watermark → empty, watermark unchanged.
	recordSessionProcessed(root, "s1", mineMark{ThroughSeq: 14})
	if text, _, mark = envelopeTranscript(root, dbPath, "s1", 10000); text != "" || mark.ThroughSeq != 14 {
		t.Fatalf("no delta: text=%q mark=%+v", text, mark)
	}
}

func TestWithHigherWatermark(t *testing.T) {
	raw := func(v any) json.RawMessage {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	winner := raw(map[string]any{"extracted": "2026-11-06T00:00:00Z", "through_seq": 10})
	other := raw(map[string]any{"extracted": "2026-11-01T00:00:00Z", "through_seq": 50})
	var got any
	_ = json.Unmarshal(withHigherWatermark(winner, other), &got)
	if seq, _ := entryThroughSeq(got); seq != 50 || entryExtracted(got) != "2026-11-06T00:00:00Z" {
		t.Fatalf("object winner: got %#v", got)
	}
	_ = json.Unmarshal(withHigherWatermark(raw("2026-11-06T00:00:00Z"), other), &got)
	if seq, _ := entryThroughSeq(got); seq != 50 {
		t.Fatalf("string winner: got %#v", got)
	}
	if string(withHigherWatermark(other, winner)) != string(other) {
		t.Fatalf("lower other must leave winner untouched")
	}

	merged := mergeSessionsLogContent(
		[]byte(`{"processed":{"s":{"extracted":"2026-11-06T00:00:00Z","through_seq":10}}}`),
		[]byte(`{"processed":{"s":{"extracted":"2026-11-01T00:00:00Z","through_seq":50}}}`))
	var doc struct {
		Processed map[string]any `json:"processed"`
	}
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatal(err)
	}
	if seq, _ := entryThroughSeq(doc.Processed["s"]); seq != 50 {
		t.Fatalf("merge lowered the watermark: %s", merged)
	}
}

func TestPagesFromSessionExactID(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "wiki", "decisions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pages := map[string]string{
		"mine.md":  "---\ntitle: A\nsources: [\"session:a-1\"]\n---\n",
		"other.md": "---\ntitle: B\nsources: [\"session:a-10\"]\n---\n",
		"body.md":  "---\ntitle: C\n---\nmentions session:a-1 only in the body\n",
	}
	for name, content := range pages {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := pagesFromSession(root, "a-1")
	if len(got) != 1 || !strings.HasSuffix(got[0], "mine.md") {
		t.Fatalf("got %v, want only mine.md", got)
	}
}

func TestRemineOrderRotatesDaily(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	first := func(day string) string {
		best := ids[0]
		for _, id := range ids[1:] {
			if remineOrderKey(id, day) < remineOrderKey(best, day) {
				best = id
			}
		}
		return best
	}
	day := "2026-11-06"
	if remineOrderKey("a", day) != remineOrderKey("a", strings.Clone(day)) {
		t.Fatal("order must be stable within a day")
	}
	seen := map[string]bool{}
	for d := 1; d <= 28; d++ {
		seen[first(time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC).Format("2006-01-02"))] = true
	}
	if len(seen) < 2 {
		t.Fatal("the first slot must rotate across days, or one failing session starves the lane")
	}
}

// The continuation block must land outside the transcript markers in
// every session prompt: inside them, the input-safety rule makes the
// model treat it as data.
func TestContinuationSlotOutsideTranscriptMarkers(t *testing.T) {
	for _, name := range []string{"session-mine-anthropic", "session-mine-ollama", "session-extract-anthropic", "session-extract-ollama"} {
		raw, err := promptFS.ReadFile("prompts/" + name + ".md")
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
		slot, begin := strings.Index(s, "{{CONTINUATION}}"), strings.Index(s, "<<<TRANSCRIPT_BEGIN>>>")
		if slot < 0 || begin < 0 || slot > begin {
			t.Errorf("%s: {{CONTINUATION}} at %d, transcript begin at %d; want the slot before the markers", name, slot, begin)
		}
	}
}

// A first turn longer than the head half (a resumed session opening with
// a long summary) must still give a non-zero partial watermark; a zero
// one records a bare timestamp, which the remine lane never picks up.
func TestRenderedThroughSeqOversizedFirstTurn(t *testing.T) {
	turns := []sessionTurn{
		{Role: "user", Text: strings.Repeat("s", 5000), Sequence: 3},
		{Role: "assistant", Text: "mid", Sequence: 7},
		{Role: "user", Text: strings.Repeat("t", 5000), Sequence: 11},
	}
	got := renderedThroughSeq(turns, 2000)
	if got != (mineMark{ThroughSeq: 3, Partial: true}) {
		t.Fatalf("got %+v, want through 3, partial", got)
	}
}

// A re-mine that reaches no further than the stored watermark (nothing
// new, a fetch error) must keep the partial flag.
func TestSessionsLogEntryKeepsPartialOnEqualMark(t *testing.T) {
	existing := map[string]any{"extracted": "2026-11-01T00:00:00Z", "through_seq": float64(9), "partial": true}
	got := sessionsLogEntry(existing, "2026-11-09T00:00:00Z", mineMark{ThroughSeq: 9})
	if seq, _ := entryThroughSeq(got); seq != 9 || !entryPartial(got) {
		t.Fatalf("got %#v, want through 9 still partial", got)
	}
}
