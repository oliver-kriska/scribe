package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppendPending is a contract test between watch.go and hook.go:
// both writers must produce lines that readAndClearPendingSessions (in
// hook.go) can parse back. If the formats drift, Codex sessions queued
// by the watcher silently disappear from the next sync.
func TestAppendPending(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "pending-sessions.txt")

	if err := appendPending(path, "sess-alpha", 120); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := appendPending(path, "sess-beta", 85); err != nil {
		t.Fatalf("second append: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), string(data))
	}

	// Each line: sessionID<TAB>score<TAB>iso8601
	for i, line := range lines {
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			t.Errorf("line %d has %d tab-separated fields, want 3: %q", i, len(parts), line)
		}
	}
	if !strings.HasPrefix(lines[0], "sess-alpha\t120\t") {
		t.Errorf("first line malformed: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "sess-beta\t85\t") {
		t.Errorf("second line malformed: %q", lines[1])
	}
}

// TestAppendPendingReaderCompat is the round-trip: watcher writes,
// pendingContainsID (hook.go) reads. This is the handoff seam between
// the watch goroutine and the sync pipeline.
func TestAppendPendingReaderCompat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pending-sessions.txt")

	if err := appendPending(path, "sess-gamma", 200); err != nil {
		t.Fatal(err)
	}
	if !pendingContainsID(path, "sess-gamma") {
		t.Error("pendingContainsID did not find sess-gamma after appendPending")
	}
	if pendingContainsID(path, "sess-missing") {
		t.Error("pendingContainsID returned true for missing id")
	}

	// And manual tab-split to catch field-count drift.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan()
	parts := strings.Split(sc.Text(), "\t")
	if len(parts) < 1 || parts[0] != "sess-gamma" {
		t.Errorf("first field not the session id: %q", parts)
	}
}
