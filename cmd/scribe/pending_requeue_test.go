package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAppendPendingEntryPreservesEnqueuedAt: the shared writer keeps an
// entry's original enqueue time (what age promotion keys off) and stamps
// now only when the entry never had one.
func TestAppendPendingEntryPreservesEnqueuedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q", "pending-sessions.txt")
	old := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	if err := appendPendingEntry(path, pendingEntry{ID: "s-old", Score: 77, HasScore: true, MsgCount: 42, EnqueuedAt: old, HasEnqueuedAt: true}); err != nil {
		t.Fatal(err)
	}
	if err := appendPendingEntry(path, pendingEntry{ID: "s-new", Score: 55, HasScore: true, MsgCount: -1}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %q", string(data))
	}
	e1, _ := parsePendingEntry(lines[0])
	if e1.ID != "s-old" || e1.Score != 77 || e1.MsgCount != 42 || !e1.HasEnqueuedAt || !e1.EnqueuedAt.Equal(old) {
		t.Errorf("round trip lost fields: %+v", e1)
	}
	e2, _ := parsePendingEntry(lines[1])
	if e2.ID != "s-new" || e2.MsgCount != -1 || !e2.HasEnqueuedAt || time.Since(e2.EnqueuedAt) > time.Minute {
		t.Errorf("entry without a timestamp must be stamped now: %+v", e2)
	}
}

// TestRequeueUnprocessedPending pins the drain contract: entries the run
// did not process go back with their original timestamps, processed ones
// do not, stale ones are dropped, duplicates collapse.
func TestRequeueUnprocessedPending(t *testing.T) {
	isolateUserConfig(t)
	path := pendingSessionsFile()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	entries := []pendingEntry{
		{ID: "mined", Score: 95, HasScore: true, MsgCount: 10, EnqueuedAt: now.Add(-1 * day), HasEnqueuedAt: true},
		{ID: "waiting", Score: 60, HasScore: true, MsgCount: 20, EnqueuedAt: now.Add(-5 * day), HasEnqueuedAt: true},
		{ID: "waiting", Score: 60, HasScore: true, MsgCount: 20, EnqueuedAt: now.Add(-5 * day), HasEnqueuedAt: true},
		{ID: "ancient", Score: 60, HasScore: true, MsgCount: 20, EnqueuedAt: now.Add(-40 * day), HasEnqueuedAt: true},
		{ID: "legacy", Score: 0, LegacyUnknownAge: true, MsgCount: -1},
	}
	processed := map[string]bool{"mined": true}
	kept, dropped := requeueUnprocessedPending(path, entries, processed, 28*day, now)
	if kept != 2 || dropped != 1 {
		t.Fatalf("kept=%d dropped=%d, want 2/1", kept, dropped)
	}
	got := peekPendingEntries()
	if len(got) != 2 {
		t.Fatalf("queue should hold waiting+legacy, got %+v", got)
	}
	byID := map[string]pendingEntry{}
	for _, e := range got {
		byID[e.ID] = e
	}
	if w, ok := byID["waiting"]; !ok || !w.EnqueuedAt.Equal(now.Add(-5*day)) || w.Score != 60 {
		t.Errorf("waiting must keep its original enqueue time and score: %+v", w)
	}
	if _, ok := byID["legacy"]; !ok {
		t.Error("legacy entry without age must be re-queued (stamped now)")
	}
	if _, ok := byID["mined"]; ok {
		t.Error("processed entry must not be re-queued")
	}
	if _, ok := byID["ancient"]; ok {
		t.Error("entry older than maxAge must be dropped")
	}

	// A second drain round-trips through the real reader.
	drained, err := readAndClearPendingEntries()
	if err != nil || len(drained) != 2 {
		t.Fatalf("drain: %v %+v", err, drained)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("drain must clear the file")
	}
}
