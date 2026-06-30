package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newCcriderDB creates an on-disk SQLite fixture using the minimal
// ccrider schema in testdata/ccrider_schema.sql. Tests must never touch
// the real ~/.config/ccrider/sessions.db.
func newCcriderDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schema, err := os.ReadFile(filepath.Join("testdata", "ccrider_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(schema)); err != nil { //nolint:noctx // test fixture
		t.Fatalf("apply schema: %v", err)
	}
	return db, path
}

// insertFixtureSession adds a sessions row and returns its rowid.
func insertFixtureSession(t *testing.T, db *sql.DB, sessionID, projectPath string, msgCount int, createdAt, updatedAt, summary string) int64 {
	t.Helper()
	//nolint:noctx // test fixture
	res, err := db.Exec(`INSERT INTO sessions (session_id, project_path, message_count, created_at, updated_at, summary)
		VALUES (?, ?, ?, ?, ?, ?)`, sessionID, projectPath, msgCount, createdAt, updatedAt, summary)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// insertFixtureMessage adds a messages row, mirrored into the FTS tables.
func insertFixtureMessage(t *testing.T, db *sql.DB, sessionRowID int64, msgType, text string, code bool) {
	t.Helper()
	//nolint:noctx // test fixture
	res, err := db.Exec(`INSERT INTO messages (session_id, type, text_content) VALUES (?, ?, ?)`,
		sessionRowID, msgType, text)
	if err != nil {
		t.Fatal(err)
	}
	rowid, _ := res.LastInsertId()
	if _, err := db.Exec(`INSERT INTO messages_fts (rowid, text_content) VALUES (?, ?)`, rowid, text); err != nil { //nolint:noctx // test fixture
		t.Fatal(err)
	}
	if code {
		if _, err := db.Exec(`INSERT INTO messages_fts_code (rowid, text_content) VALUES (?, ?)`, rowid, text); err != nil { //nolint:noctx // test fixture
			t.Fatal(err)
		}
	}
}

// sessionsTestKB scaffolds a KB whose scribe.yaml points ccrider_db at
// the fixture database.
func sessionsTestKB(t *testing.T, ccriderDB string) string {
	t.Helper()
	root := t.TempDir()
	yaml := "domains: [acme]\n"
	if ccriderDB != "" {
		yaml += "ccrider_db: " + ccriderDB + "\n"
	}
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCRIBE_KB", root)
	return root
}

func TestFilterVerdict(t *testing.T) {
	tests := []struct {
		name  string
		stats sessionFilterStats
		want  string
	}{
		{"lookup error keeps", sessionFilterStats{Found: false}, ""},
		{"no user turns", sessionFilterStats{Found: true, UserMsgs: 0, TotalChars: 9000}, "empty"},
		{"under 500 chars", sessionFilterStats{Found: true, UserMsgs: 5, TotalChars: 499}, "empty"},
		{"short and shallow", sessionFilterStats{Found: true, UserMsgs: 2, TotalChars: 1999}, "thin"},
		{"short but rich one-shot", sessionFilterStats{Found: true, UserMsgs: 1, TotalChars: 2000}, ""},
		{"three user turns", sessionFilterStats{Found: true, UserMsgs: 3, TotalChars: 600}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.stats.filterVerdict(); got != tt.want {
				t.Errorf("filterVerdict() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQuerySessionStats(t *testing.T) {
	db, _ := newCcriderDB(t)
	sid := insertFixtureSession(t, db, "rich", "/p/alpha", 10, "2026-06-01T10:00:00", "2026-06-01T12:00:00", "did things")
	insertFixtureMessage(t, db, sid, "user", strings.Repeat("q", 300), false)
	insertFixtureMessage(t, db, sid, "user", strings.Repeat("w", 300), false)
	insertFixtureMessage(t, db, sid, "assistant", strings.Repeat("a", 1500), false)

	st := querySessionStats(db, "rich")
	if !st.Found {
		t.Fatal("Found = false")
	}
	if st.UserMsgs != 2 {
		t.Errorf("UserMsgs = %d, want 2", st.UserMsgs)
	}
	if st.TotalChars != 2100 {
		t.Errorf("TotalChars = %d, want 2100", st.TotalChars)
	}
	if st.ProjectPath != "/p/alpha" {
		t.Errorf("ProjectPath = %q", st.ProjectPath)
	}

	// Aggregate queries always return one row: an unknown session reads
	// as Found with zeros, i.e. verdict "empty" — not a lookup error.
	unknown := querySessionStats(db, "nope")
	if !unknown.Found || unknown.filterVerdict() != "empty" {
		t.Errorf("unknown session: %+v verdict=%q", unknown, unknown.filterVerdict())
	}
}

func TestPreFilterSessions(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	root := sessionsTestKB(t, dbPath)
	pendingProj := filepath.Join(t.TempDir(), "pending-proj")

	seed := func(sessionID, project string, userMsgs int, charsPerMsg int) {
		rowid := insertFixtureSession(t, db, sessionID, project, userMsgs+1,
			"2026-06-01T10:00:00", "2026-06-01T12:00:00", "s")
		for i := 0; i < userMsgs; i++ {
			insertFixtureMessage(t, db, rowid, "user", strings.Repeat("x", charsPerMsg), false)
		}
	}
	// Project path must clear isIgnored's depth floor (>=4 non-empty
	// segments) or the scope gate drops these sessions as too-shallow
	// system paths before the content verdict is ever reached.
	const proj = "/home/dev/projects/alpha"
	seed("rich", proj, 3, 800)
	seed("thin", proj, 2, 400)
	seed("empty", proj, 0, 0)
	seed("inkb", root, 3, 800)
	seed("pending", pendingProj, 3, 800)

	manifest := `{"projects": {"pending-proj": {"path": ` + jsonQuote(pendingProj) + `, "status": "pending"}}}`
	writeKBFile(t, root, "scripts/projects.json", manifest)

	kept, skipped := preFilterSessions(root, dbPath,
		[]string{"rich", "thin", "empty", "inkb", "pending"})

	if len(kept) != 1 || kept[0] != "rich" {
		t.Errorf("kept = %v, want [rich]", kept)
	}
	// thin + empty are marked skipped (recorded as processed); KB and
	// pending-project sessions are dropped silently so they can resurface.
	if len(skipped) != 2 {
		t.Errorf("skipped = %v, want [thin empty] in some order", skipped)
	}
	for _, s := range skipped {
		if s == "inkb" || s == "pending" {
			t.Errorf("%s must be dropped silently, not marked skipped", s)
		}
	}
}

func TestPreFilterSessions_MissingDBKeepsAll(t *testing.T) {
	root := sessionsTestKB(t, "")
	ids := []string{"a", "b"}
	kept, skipped := preFilterSessions(root, filepath.Join(t.TempDir(), "no.db"), ids)
	if len(kept) != 2 || len(skipped) != 0 {
		t.Errorf("kept=%v skipped=%v, want all kept on DB error", kept, skipped)
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestQueryRelatedSessions(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	longSummary := strings.Repeat("s", 150)
	insertFixtureSession(t, db, "target", "/p/alpha", 10, "2026-06-01T10:00:00", "2026-06-10T12:00:00", "target work")
	insertFixtureSession(t, db, "near", "/p/alpha", 10, "2026-06-01T10:00:00", "2026-06-09T12:00:00", longSummary)
	insertFixtureSession(t, db, "far", "/p/alpha", 10, "2026-04-01T10:00:00", "2026-04-01T12:00:00", "ancient")
	insertFixtureSession(t, db, "other", "/p/beta", 10, "2026-06-01T10:00:00", "2026-06-10T11:00:00", "different project")
	db.Close() // queryRelatedSessions opens its own handle

	related := queryRelatedSessions(dbPath, "target", 7, 10)
	if len(related) != 1 {
		t.Fatalf("related = %+v, want exactly the near sibling", related)
	}
	r := related[0]
	if r.SessionID != "near" {
		t.Errorf("SessionID = %q", r.SessionID)
	}
	if len(r.Summary) != 140+len("…") || !strings.HasSuffix(r.Summary, "…") {
		t.Errorf("summary not truncated to 140+ellipsis: %d chars", len(r.Summary))
	}

	if got := queryRelatedSessions(filepath.Join(t.TempDir(), "no.db"), "target", 7, 10); got != nil {
		t.Errorf("missing DB should return nil, got %v", got)
	}
	if got := queryRelatedSessions(dbPath, "unknown", 7, 10); got != nil {
		t.Errorf("unknown target should return nil, got %v", got)
	}
}

func TestFormatRelatedSessions(t *testing.T) {
	if got := formatRelatedSessions(nil); !strings.Contains(got, "none") {
		t.Errorf("empty list = %q", got)
	}
	got := formatRelatedSessions([]relatedSession{
		{SessionID: "s1", Date: "2026-06-09", Summary: "line one\nline two"},
		{SessionID: "s2", Date: "2026-06-08", Summary: ""},
	})
	if !strings.Contains(got, "- s1 (2026-06-09): line one line two") {
		t.Errorf("newlines not flattened: %q", got)
	}
	if !strings.Contains(got, "- s2 (2026-06-08): (no summary)") {
		t.Errorf("missing-summary placeholder absent: %q", got)
	}
	if strings.HasSuffix(got, "\n") {
		t.Errorf("trailing newline not trimmed: %q", got)
	}
}

func TestLargeSessionBudget(t *testing.T) {
	tests := []struct {
		max  int
		skip bool
		want int
	}{
		{9, false, 3},
		{1, false, 1}, // floor of 1
		{0, false, 1},
		{30, true, 0}, // --skip-large
	}
	for _, tt := range tests {
		s := &SyncCmd{SessionsMax: tt.max, SkipLarge: tt.skip}
		if got := s.largeSessionBudget(); got != tt.want {
			t.Errorf("largeSessionBudget(max=%d skip=%v) = %d, want %d", tt.max, tt.skip, got, tt.want)
		}
	}
}

// --- sessions.go: log view + subcommands ---

const sessionsLogFixture = `{
  "processed": {
    "s1": {"extracted": "2026-06-01T10:00:00Z", "project": "alpha"},
    "s2": {"extracted": "2026-06-02T10:00:00Z", "project": "alpha"},
    "s3": {"skipped": true, "reason": "mechanical", "extracted": "2026-06-03T00:00:00Z"}
  },
  "last_scan": "2026-06-03T12:00:00Z"
}`

func TestSessionsLogView(t *testing.T) {
	root := sessionsTestKB(t, "")
	writeKBFile(t, root, "wiki/_sessions_log.json", sessionsLogFixture)

	view, err := loadSessionsLogView(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.processed) != 3 {
		t.Fatalf("processed = %d entries, want 3", len(view.processed))
	}
	if !entryIsSkipped(view.processed["s3"]) {
		t.Error("s3 should be skipped")
	}
	if entryIsSkipped(view.processed["s1"]) {
		t.Error("s1 should not be skipped")
	}
	if entryProject(view.processed["s1"]) != "alpha" {
		t.Errorf("project = %q", entryProject(view.processed["s1"]))
	}
	if entryExtracted(view.processed["s2"]) != "2026-06-02T10:00:00Z" {
		t.Errorf("extracted = %q", entryExtracted(view.processed["s2"]))
	}
	// Non-map entries are tolerated.
	if entryIsSkipped("garbage") || entryProject(42) != "" || entryExtracted(nil) != "" {
		t.Error("non-map entries must read as zero values")
	}

	delete(view.processed, "s3")
	if err := view.save(); err != nil {
		t.Fatal(err)
	}
	again, err := loadSessionsLogView(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.processed["s3"]; ok {
		t.Error("save did not persist the deletion")
	}
	if lastScan, _ := again.raw["last_scan"].(string); lastScan != "2026-06-03T12:00:00Z" {
		t.Errorf("save dropped sibling keys: last_scan = %q", lastScan)
	}
}

func TestSortedCountKeys(t *testing.T) {
	got := sortedCountKeys(map[string]int{"b": 2, "a": 2, "z": 9, "m": 1})
	want := []string{"z", "a", "b", "m"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("order[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestSessionsStatsCmd_JSON(t *testing.T) {
	root := sessionsTestKB(t, "")
	writeKBFile(t, root, "wiki/_sessions_log.json", sessionsLogFixture)

	var err error
	out := captureLintStdout(t, func() {
		err = (&SessionsStatsCmd{JSON: true}).Run()
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Total        int            `json:"total"`
		Extracted    int            `json:"extracted"`
		Skipped      int            `json:"skipped"`
		ByProject    map[string]int `json:"by_project"`
		FirstExtract string         `json:"first_extract"`
		LastExtract  string         `json:"last_extract"`
		LastScan     string         `json:"last_scan"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("bad JSON output: %v\n%s", err, out)
	}
	if payload.Total != 3 || payload.Extracted != 2 || payload.Skipped != 1 {
		t.Errorf("counts = %+v", payload)
	}
	if payload.ByProject["alpha"] != 2 {
		t.Errorf("by_project = %v", payload.ByProject)
	}
	if payload.FirstExtract != "2026-06-01T10:00:00Z" || payload.LastExtract != "2026-06-02T10:00:00Z" {
		t.Errorf("extract range = %q..%q", payload.FirstExtract, payload.LastExtract)
	}
	if payload.LastScan != "2026-06-03T12:00:00Z" {
		t.Errorf("last_scan = %q", payload.LastScan)
	}
}

func TestSessionsUnskipCmd(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	// "wasrich" now passes the filter; "stillempty" doesn't.
	rich := insertFixtureSession(t, db, "wasrich", "/p/alpha", 10, "2026-06-01T10:00:00", "2026-06-01T12:00:00", "s")
	for i := 0; i < 3; i++ {
		insertFixtureMessage(t, db, rich, "user", strings.Repeat("x", 800), false)
	}
	insertFixtureSession(t, db, "stillempty", "/p/alpha", 2, "2026-06-01T10:00:00", "2026-06-01T12:00:00", "s")

	root := sessionsTestKB(t, dbPath)
	logJSON := `{"processed": {
		"wasrich": {"skipped": true, "reason": "mechanical"},
		"stillempty": {"skipped": true, "reason": "mechanical"},
		"extracted-one": {"extracted": "2026-06-01T00:00:00Z"}
	}, "last_scan": null}`
	writeKBFile(t, root, "wiki/_sessions_log.json", logJSON)

	t.Run("dry run reports without writing", func(t *testing.T) {
		var err error
		out := captureLintStdout(t, func() {
			err = (&SessionsUnskipCmd{DryRun: true}).Run()
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "would unskip : 1") {
			t.Errorf("expected 1 unskippable:\n%s", out)
		}
		view, _ := loadSessionsLogView(root)
		if len(view.processed) != 3 {
			t.Errorf("dry run modified the log: %d entries", len(view.processed))
		}
	})

	t.Run("real run removes only passing entries", func(t *testing.T) {
		var err error
		captureLintStdout(t, func() {
			err = (&SessionsUnskipCmd{}).Run()
		})
		if err != nil {
			t.Fatal(err)
		}
		view, _ := loadSessionsLogView(root)
		if _, ok := view.processed["wasrich"]; ok {
			t.Error("wasrich should have been unskipped")
		}
		if _, ok := view.processed["stillempty"]; !ok {
			t.Error("stillempty must stay skipped")
		}
		if _, ok := view.processed["extracted-one"]; !ok {
			t.Error("extracted entries must never be touched")
		}
	})

	t.Run("all flag removes every skipped entry", func(t *testing.T) {
		writeKBFile(t, root, "wiki/_sessions_log.json", logJSON)
		var err error
		captureLintStdout(t, func() {
			err = (&SessionsUnskipCmd{All: true}).Run()
		})
		if err != nil {
			t.Fatal(err)
		}
		view, _ := loadSessionsLogView(root)
		if len(view.processed) != 1 {
			t.Errorf("want only the extracted entry left, got %v", view.processed)
		}
	})
}

func TestSessionsInspectCmd_JSON(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	rich := insertFixtureSession(t, db, "abc-123", "/p/alpha", 12, "2026-06-01T10:00:00", "2026-06-01T12:00:00", "built a thing")
	for i := 0; i < 3; i++ {
		insertFixtureMessage(t, db, rich, "user", strings.Repeat("x", 800), false)
	}

	root := sessionsTestKB(t, dbPath)
	writeKBFile(t, root, "wiki/_sessions_log.json",
		`{"processed": {"skipped-one": {"skipped": true}}, "last_scan": null}`)

	var err error
	out := captureLintStdout(t, func() {
		err = (&SessionsInspectCmd{ID: "abc-123", JSON: true}).Run()
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out)
	}
	if payload["in_db"] != true || payload["in_log"] != false {
		t.Errorf("in_db/in_log = %v/%v", payload["in_db"], payload["in_log"])
	}
	if payload["filter_verdict"] != "keep" {
		t.Errorf("filter_verdict = %v", payload["filter_verdict"])
	}
	if payload["project_path"] != "/p/alpha" {
		t.Errorf("project_path = %v", payload["project_path"])
	}
	if payload["message_count"] != float64(12) {
		t.Errorf("message_count = %v", payload["message_count"])
	}
}

// --- hook.go DB query helpers (same fixture schema) ---

func TestHookQueryHelpers(t *testing.T) {
	db, _ := newCcriderDB(t)
	insertFixtureSession(t, db, "older", "/p/alpha", 8, "2026-06-01T10:00:00", "2026-06-05T10:00:00", "real work")
	insertFixtureSession(t, db, "newest", "/p/beta", 4, "2026-06-01T10:00:00", "2026-06-09T10:00:00", "more work")
	insertFixtureSession(t, db, "bootstrap", "/p/beta", 9, "2026-06-01T10:00:00", "2026-06-10T10:00:00", "You are working in a directory")
	insertFixtureSession(t, db, "empty-one", "/p/beta", 0, "2026-06-01T10:00:00", "2026-06-11T10:00:00", "x")

	t.Run("queryMostRecentSessionID skips bootstrap and empty", func(t *testing.T) {
		if got := queryMostRecentSessionID(db); got != "newest" {
			t.Errorf("got %q, want newest", got)
		}
	})

	t.Run("queryMessageCount", func(t *testing.T) {
		if n, ok := queryMessageCount(db, "older"); !ok || n != 8 {
			t.Errorf("got (%d, %v)", n, ok)
		}
		if _, ok := queryMessageCount(db, "ghost"); ok {
			t.Error("unknown session should not be found")
		}
	})

	t.Run("querySessionProjectPath", func(t *testing.T) {
		if got := querySessionProjectPath(db, "older"); got != "/p/alpha" {
			t.Errorf("got %q", got)
		}
		if got := querySessionProjectPath(db, "ghost"); got != "" {
			t.Errorf("unknown session = %q, want empty", got)
		}
	})
}

func TestQuickScoreSession(t *testing.T) {
	db, _ := newCcriderDB(t)
	sid := insertFixtureSession(t, db, "scored", "/p/alpha", 5, "2026-06-01T10:00:00", "2026-06-01T12:00:00", "s")
	// One decision hit (weight 3), one research hit (weight 3), one code
	// pattern hit (weight 1) → 7. A message matching a category twice
	// still counts once per row.
	insertFixtureMessage(t, db, sid, "user", "we decided to ship it", false)
	insertFixtureMessage(t, db, sid, "assistant", "research notes from the paper", false)
	insertFixtureMessage(t, db, sid, "assistant", "defmodule MyApp.Worker uses GenServer", true)

	// The code message also lands in messages_fts; make sure it carries
	// no keyword from the six prose categories (it doesn't), so the
	// expected total stays 3 + 3 + 1.
	if got := quickScoreSession(db, "scored"); got != 7 {
		t.Errorf("score = %d, want 7", got)
	}

	if got := quickScoreSession(db, "ghost"); got != 0 {
		t.Errorf("unknown session score = %d, want 0", got)
	}
}
