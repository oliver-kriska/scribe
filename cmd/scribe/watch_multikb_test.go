package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestProcessedByAllKBs(t *testing.T) {
	a := map[string]bool{"x": true, "y": true}
	b := map[string]bool{"x": true}

	// x is in both → handled everywhere → skip.
	if !processedByAllKBs([]map[string]bool{a, b}, "x") {
		t.Error("x is processed by both KBs, want true")
	}
	// y is only in A → B still wants it → don't skip.
	if processedByAllKBs([]map[string]bool{a, b}, "y") {
		t.Error("y is unprocessed by B, want false (still a candidate)")
	}
	// unknown id → nobody processed it.
	if processedByAllKBs([]map[string]bool{a, b}, "z") {
		t.Error("z processed by nobody, want false")
	}
	// no KBs → never drop for lack of a registry.
	if processedByAllKBs(nil, "x") {
		t.Error("empty set must report false")
	}
}

// TestWatchScan_MultiKBQueuing is the #26 headline: one watcher feeds the
// shared pending queue for every registered KB. A session already mined by
// one KB but not another must still be queued (the old single-KB watcher,
// keyed on the default KB's log alone, would have starved the other KB).
func TestWatchScan_MultiKBQueuing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // isolates pendingSessionsFile()
	db, dbPath := newCcriderDB(t)

	kbA, kbB := t.TempDir(), t.TempDir()
	writeSessionsProcessed(t, kbA, "sess-both", "sess-onlyA")
	writeSessionsProcessed(t, kbB, "sess-both")

	upd := time.Now().UTC().Format("2006-01-02 15:04:05")
	for _, id := range []string{"sess-both", "sess-onlyA", "sess-none"} {
		rid := insertFixtureSession(t, db, id, "/p/"+id, 20, upd, upd, "did work")
		setSessionProvider(t, db, id, "codex")
		insertFixtureMessage(t, db, rid, "user", "decided to refactor the design", false)
	}

	// MinScore 0 so the dedup decision, not FTS scoring, is what's under test.
	w := &WatchCmd{MinMessages: 1, MinScore: 0, LookbackMin: 60, Provider: "codex"}
	w.scan([]string{kbA, kbB}, dbPath)

	got := peekPendingSessions()
	// sess-both: every KB mined it → skipped.
	if slices.Contains(got, "sess-both") {
		t.Error("sess-both was queued though every KB already processed it")
	}
	// sess-onlyA: KB-B hasn't mined it → must be queued.
	if !slices.Contains(got, "sess-onlyA") {
		t.Errorf("sess-onlyA not queued — KB-B still needs it; got %v", got)
	}
	// sess-none: nobody mined it → queued.
	if !slices.Contains(got, "sess-none") {
		t.Errorf("sess-none not queued; got %v", got)
	}
}

// writeSessionsProcessed writes <kb>/wiki/_sessions_log.json marking the
// given session IDs processed, in the {"processed": {...}} shape
// loadProcessedSessionIDs reads.
func writeSessionsProcessed(t *testing.T, kb string, ids ...string) {
	t.Helper()
	dir := filepath.Join(kb, "wiki")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	processed := map[string]bool{}
	for _, id := range ids {
		processed[id] = true
	}
	data, err := json.Marshal(map[string]any{"processed": processed})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "_sessions_log.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func setSessionProvider(t *testing.T, db *sql.DB, sessionID, provider string) {
	t.Helper()
	//nolint:noctx // test fixture
	if _, err := db.Exec(`UPDATE sessions SET provider = ? WHERE session_id = ?`, provider, sessionID); err != nil {
		t.Fatal(err)
	}
}
