package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestThroughSeqAt(t *testing.T) {
	mined := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	at := func(seq int, offset time.Duration) backfillMessage {
		return backfillMessage{Sequence: seq, Timestamp: mined.Add(offset), HasTime: true}
	}
	tests := []struct {
		name     string
		messages []backfillMessage
		want     int
		wantOK   bool
	}{
		{"all before", []backfillMessage{at(3, -2*time.Hour), at(5, -time.Hour)}, 5, true},
		{"stops at first later message", []backfillMessage{at(3, -time.Hour), at(5, time.Hour), at(7, -time.Minute)}, 3, true},
		{"untimed rows do not stop the scan", []backfillMessage{at(3, -time.Hour), {Sequence: 4}, at(5, -time.Minute), at(6, time.Hour)}, 5, true},
		{"trailing untimed row is not counted as seen", []backfillMessage{at(3, -time.Hour), {Sequence: 4}, at(6, time.Hour)}, 3, true},
		{"nothing before", []backfillMessage{at(3, time.Hour)}, 0, false},
		{"empty", nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := throughSeqAt(tt.messages, mined)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("got (%d, %v), want (%d, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestBackfillMark(t *testing.T) {
	shown := seqTurns(strings.Repeat("x", 100), strings.Repeat("x", 100), strings.Repeat("x", 100),
		strings.Repeat("x", 100), strings.Repeat("x", 100), strings.Repeat("x", 100))
	tests := []struct {
		name     string
		capChars int
		msgCount int
		headSeq  int
		want     mineMark
	}{
		{"envelope, fits", 100000, 6, 0, mineMark{ThroughSeq: 60}},
		{"envelope, middle dropped", 600, 6, 0, mineMark{ThroughSeq: 20, Partial: true}},
		{"tools normal reads all", 0, 200, 0, mineMark{ThroughSeq: 60}},
		{"tools large reads the first 50", 0, 400, 30, mineMark{ThroughSeq: 30, Partial: true}},
		{"tools large mined before message 50", 0, 400, 90, mineMark{ThroughSeq: 60}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backfillMark(shown, 60, tt.capChars, tt.msgCount, tt.headSeq); got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestBackfillCap(t *testing.T) {
	envelope := SessionMineConfig{Mode: "envelope", TranscriptMaxChars: 120000}
	tests := []struct {
		name     string
		flagCap  int
		flagMode string
		cfg      SessionMineConfig
		want     int
	}{
		{"tools", 0, "", SessionMineConfig{Mode: "tools", TranscriptMaxChars: 24000}, 0},
		{"envelope uses the old default", 0, "", envelope, 24000},
		{"envelope small cap", 0, "", SessionMineConfig{Mode: "envelope", TranscriptMaxChars: 10000}, 10000},
		{"flag cap wins", 120000, "", SessionMineConfig{Mode: "envelope", TranscriptMaxChars: 24000}, 120000},
		{"--mode tools in an envelope KB", 120000, "tools", envelope, 0},
		{"--mode envelope in a tools KB", 0, "envelope", SessionMineConfig{Mode: "tools", TranscriptMaxChars: 24000}, 24000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backfillCap(tt.flagCap, tt.flagMode, tt.cfg); got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLegacyMinedEntries(t *testing.T) {
	processed := map[string]any{
		"bare":     "2026-09-01T00:00:00Z",
		"object":   map[string]any{"extracted": "2026-09-02T00:00:00Z", "project": "p"},
		"skipped":  map[string]any{"extracted": "2026-09-01T00:00:00Z", "skipped": true},
		"marked":   map[string]any{"extracted": "2026-09-01T00:00:00Z", "through_seq": float64(9)},
		"badstamp": "yesterday",
	}
	ids, minedAt, unparsable := legacyMinedEntries(processed)
	if strings.Join(ids, ",") != "bare,object" || unparsable != 1 {
		t.Fatalf("ids=%v unparsable=%d", ids, unparsable)
	}
	if !minedAt["object"].Equal(time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("object time %v", minedAt["object"])
	}
	before := minedBeforeOnly(ids, minedAt, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))
	if strings.Join(before, ",") != "bare" {
		t.Fatalf("mined-before: %v", before)
	}
}

func TestWithBackfilledMarkKeepsFields(t *testing.T) {
	mined := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	got := decodeEntry(t, withBackfilledMark(map[string]any{"extracted": "2026-09-01T00:00:00Z", "project": "p"}, mined, mineMark{ThroughSeq: 7, Partial: true}))
	m, ok := got.(map[string]any)
	if !ok || m["project"] != "p" || m["extracted"] != "2026-09-01T00:00:00Z" {
		t.Fatalf("fields lost: %#v", got)
	}
	if seq, _ := entryThroughSeq(got); seq != 7 || !entryPartial(got) {
		t.Fatalf("mark not added: %#v", got)
	}
	bare := decodeEntry(t, withBackfilledMark("2026-09-01T00:00:00Z", mined, mineMark{ThroughSeq: 7}))
	if seq, _ := entryThroughSeq(bare); seq != 7 || entryExtracted(bare) != "2026-09-01T00:00:00Z" {
		t.Fatalf("bare entry: %#v", bare)
	}
}

func TestSessionsBackfillWatermarksRun(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	root := sessionsTestKB(t, dbPath)
	mined := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	stamp := func(d time.Duration) string { return mined.Add(d).Format("2006-01-02 15:04:05.999999999 -0700 MST") }

	row := insertFixtureSession(t, db, "grew", "/p", 3, stamp(-time.Hour), stamp(time.Hour), "")
	for _, m := range []struct {
		seq  int
		text string
		at   time.Duration
	}{{3, "question", -time.Hour}, {5, "answer", -30 * time.Minute}, {8, "later follow-up", time.Hour}} {
		//nolint:noctx // test fixture
		if _, err := db.Exec(`INSERT INTO messages (session_id, type, text_content, sequence, timestamp) VALUES (?, 'user', ?, ?, ?)`,
			row, m.text, m.seq, stamp(m.at)); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(root, "wiki", "_sessions_log.json")
	initial := `{"processed": {"grew": "2026-09-01T12:00:00Z", "gone": "2026-09-01T12:00:00Z", "done": {"extracted": "2026-09-01T12:00:00Z", "through_seq": 4}}}`
	if err := os.WriteFile(logPath, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := (&SessionsBackfillWatermarksCmd{DryRun: true}).Run(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(logPath); string(data) != initial {
		t.Fatalf("dry run wrote the log: %s", data)
	}

	if err := (&SessionsBackfillWatermarksCmd{}).Run(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Processed map[string]any `json:"processed"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if seq, _ := entryThroughSeq(doc.Processed["grew"]); seq != 5 {
		t.Errorf("grew: through %d, want 5 (the follow-up came after the mine): %s", seq, data)
	}
	if entryExtracted(doc.Processed["grew"]) != "2026-09-01T12:00:00Z" {
		t.Errorf("grew: processed time changed: %s", data)
	}
	if s, ok := doc.Processed["gone"].(string); !ok || s != "2026-09-01T12:00:00Z" {
		t.Errorf("gone (no ccrider rows) must be left as is: %#v", doc.Processed["gone"])
	}
	if seq, _ := entryThroughSeq(doc.Processed["done"]); seq != 4 {
		t.Errorf("existing watermark changed: %s", data)
	}
}

// --margin moves the time rule back: the answer stamped 30 minutes
// before the processed time counts as unread under a 45-minute margin.
func TestSessionsBackfillWatermarksMargin(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	root := sessionsTestKB(t, dbPath)
	mined := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	stamp := func(d time.Duration) string { return mined.Add(d).Format("2006-01-02 15:04:05.999999999 -0700 MST") }
	row := insertFixtureSession(t, db, "s", "/p", 2, stamp(-time.Hour), stamp(-30*time.Minute), "")
	for _, m := range []struct {
		seq int
		at  time.Duration
	}{{3, -time.Hour}, {5, -30 * time.Minute}} {
		//nolint:noctx // test fixture
		if _, err := db.Exec(`INSERT INTO messages (session_id, type, text_content, sequence, timestamp) VALUES (?, 'user', 'text', ?, ?)`,
			row, m.seq, stamp(m.at)); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(root, "wiki", "_sessions_log.json")
	if err := os.WriteFile(logPath, []byte(`{"processed": {"s": "2026-09-01T12:00:00Z"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (&SessionsBackfillWatermarksCmd{Margin: 45 * time.Minute}).Run(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(logPath)
	var doc struct {
		Processed map[string]any `json:"processed"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if seq, _ := entryThroughSeq(doc.Processed["s"]); seq != 3 {
		t.Fatalf("through %d, want 3 (message 5 falls inside the margin): %s", seq, data)
	}
}

func TestEstimateRemineQueue(t *testing.T) {
	cfg := SessionMineConfig{RemineMinNewChars: 1000, TranscriptMaxChars: 400, RemineMaxPerRun: 2}
	results := []backfillResult{
		{ID: "big", Mark: mineMark{ThroughSeq: 5}, Remaining: 1000},                   // 3 calls
		{ID: "partial", Mark: mineMark{ThroughSeq: 5, Partial: true}, Remaining: 100}, // 1 call
		{ID: "small", Mark: mineMark{ThroughSeq: 5}, Remaining: 999},                  // below min, not partial
		{ID: "done", Mark: mineMark{ThroughSeq: 5}},                                   // nothing past the mark
		{ID: "ignored", Mark: mineMark{ThroughSeq: 5}, Remaining: 5000},               // out of scope
	}
	q := estimateRemineQueue(cfg, results, func(ids []string) []string {
		var kept []string
		for _, id := range ids {
			if id != "ignored" {
				kept = append(kept, id)
			}
		}
		return kept
	})
	if q.partial != 1 || q.withRemainder != 4 || q.remainingChars != 7099 {
		t.Errorf("counts: %+v", q)
	}
	if len(q.queued) != 2 || q.queuedChars != 1100 || q.calls != 4 || q.runs != 2 {
		t.Errorf("queue: %+v, want big+partial, 1100 chars, 4 calls, 2 runs", q)
	}
}
