package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestPendingContainsID covers the pending-queue dedup primitive.
// The hook calls it on every fire so the same session ID doesn't
// land in the queue twice. A bug here causes runaway queue growth.
func TestPendingContainsID(t *testing.T) {
	t.Run("missing file returns false", func(t *testing.T) {
		if pendingContainsID("/nonexistent/path.txt", "any-id") {
			t.Error("got true, want false")
		}
	})

	t.Run("plain id lines", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "pending.txt")
		content := "session-a\nsession-b\nsession-c\n"
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if !pendingContainsID(path, "session-b") {
			t.Error("session-b not found")
		}
		if pendingContainsID(path, "session-z") {
			t.Error("session-z found (shouldn't be)")
		}
	})

	t.Run("tab-separated lines match first field only", func(t *testing.T) {
		// The hook writes "session-id\tscore\tmsg-count" so we must match
		// only the first tab-separated field.
		dir := t.TempDir()
		path := filepath.Join(dir, "pending.txt")
		content := "session-a\t42\t100\nsession-b\t7\t50\n"
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if !pendingContainsID(path, "session-a") {
			t.Error("session-a not matched (tab field extraction broken)")
		}
		// Must not match on the score value.
		if pendingContainsID(path, "42") {
			t.Error("matched on score field instead of id field")
		}
	})
}

// TestIsSessionProcessed bridges the hook's decision logic to the
// canonical processed map. Reuses loadProcessedSessionIDs so this is
// primarily a smoke test that the two stay wired together.
func TestIsSessionProcessed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "_sessions_log.json")
	content := `{"processed": {"session-a": true, "session-b": true}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isSessionProcessed(path, "session-a") {
		t.Error("session-a should be marked processed")
	}
	if isSessionProcessed(path, "session-missing") {
		t.Error("session-missing should not be processed")
	}
	if isSessionProcessed("/nonexistent/log.json", "session-a") {
		t.Error("missing file should return false")
	}
}

// TestPeekAndDrainPendingSessions covers the queue lifecycle: writer
// side (the hook) puts IDs in, reader side (sync) drains them.
// peek should not consume; readAndClear should consume.
// Uses XDG_CONFIG_HOME override to redirect pendingSessionsFile() to
// a tempdir so tests don't clobber real state.
func TestPeekAndDrainPendingSessions(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	// scribe dir doesn't exist yet — peek must tolerate that.
	if ids := peekPendingSessions(); ids != nil {
		t.Errorf("expected nil for missing file, got %v", ids)
	}

	// Create the directory + file. The hook writes one ID per line,
	// optionally with \tscore\tcount suffix.
	scribeDir := filepath.Join(tmp, "scribe")
	if err := os.MkdirAll(scribeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(scribeDir, "pending-sessions.txt")
	content := "session-a\t80\t150\nsession-b\nsession-a\t99\t200\n\nsession-c\n"
	if err := os.WriteFile(pendingPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// peekPendingSessions: should return unique IDs, no file change.
	peeked := peekPendingSessions()
	sort.Strings(peeked)
	wantIDs := []string{"session-a", "session-b", "session-c"}
	if !equalStrings(peeked, wantIDs) {
		t.Errorf("peek = %v, want %v", peeked, wantIDs)
	}
	if _, err := os.Stat(pendingPath); err != nil {
		t.Error("peek consumed the file — should not have")
	}

	// readAndClearPendingSessions: same unique set, but file gone.
	drained, err := readAndClearPendingSessions()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(drained)
	if !equalStrings(drained, wantIDs) {
		t.Errorf("drain = %v, want %v", drained, wantIDs)
	}
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Error("drain did not remove the file")
	}

	// Second drain on the empty queue: (nil, nil), no panic.
	drained2, err := readAndClearPendingSessions()
	if err != nil {
		t.Errorf("second drain errored: %v", err)
	}
	if drained2 != nil {
		t.Errorf("second drain = %v, want nil", drained2)
	}
}

// TestPendingSessionsFileRespectsXDG locks in the env-var fallback
// so refactors don't accidentally break the XDG config layout.
func TestPendingSessionsFileRespectsXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/fake/xdg")
	got := pendingSessionsFile()
	want := "/fake/xdg/scribe/pending-sessions.txt"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Empty XDG falls back to $HOME/.config/scribe/...
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/fake/home")
	got = pendingSessionsFile()
	want = "/fake/home/.config/scribe/pending-sessions.txt"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
