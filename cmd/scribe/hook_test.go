package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
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

	t.Run("4-column lines (current format) match first field only", func(t *testing.T) {
		// Regression guard for parsePendingLine now that a 4th column
		// (timestamp, after score+msgCount) exists.
		dir := t.TempDir()
		path := filepath.Join(dir, "pending.txt")
		content := "session-a\t42\t100\t2026-06-01T10:00:00Z\nsession-b\t7\t50\t2026-06-02T10:00:00Z\n"
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if !pendingContainsID(path, "session-a") {
			t.Error("session-a not matched (4-column tab field extraction broken)")
		}
		if pendingContainsID(path, "42") || pendingContainsID(path, "100") {
			t.Error("matched on score or msgCount field instead of id field")
		}
	})
}

// TestParsePendingEntry is table-driven over the format rows documented in
// docs/issue-22-priority-lanes-plan.md §2.2: the current 4-column shape and
// the 3 legacy shapes a pre-upgrade queue file can still contain.
func TestParsePendingEntry(t *testing.T) {
	ts := "2026-06-01T10:00:00Z"
	wantTime, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name             string
		line             string
		wantOK           bool
		wantID           string
		wantScore        int
		wantHasScore     bool
		wantMsgCount     int
		wantHasEnqueued  bool
		wantEnqueuedAt   time.Time
		wantLegacyUnkAge bool
	}{
		{
			name: "4-column current format", line: "s1\t85\t120\t" + ts,
			wantOK: true, wantID: "s1", wantScore: 85, wantHasScore: true,
			wantMsgCount: 120, wantHasEnqueued: true, wantEnqueuedAt: wantTime,
		},
		{
			name: "3-column defensive shape (id, score, msgCount)", line: "s2\t60\t45",
			wantOK: true, wantID: "s2", wantScore: 60, wantHasScore: true,
			wantMsgCount: 45, wantHasEnqueued: false,
		},
		{
			name: "3-column legacy shape (id, score, timestamp)", line: "s3\t70\t" + ts,
			wantOK: true, wantID: "s3", wantScore: 70, wantHasScore: true,
			wantMsgCount: -1, wantHasEnqueued: true, wantEnqueuedAt: wantTime,
		},
		{
			name: "2-column (id, score)", line: "s4\t50",
			wantOK: true, wantID: "s4", wantScore: 50, wantHasScore: true,
			wantMsgCount: -1, wantHasEnqueued: false,
		},
		{
			name: "1-column bare ID (oldest legacy)", line: "s5",
			wantOK: true, wantID: "s5", wantScore: 0, wantHasScore: false,
			wantMsgCount: -1, wantHasEnqueued: false, wantLegacyUnkAge: true,
		},
		{
			name: "blank line", line: "",
			wantOK: false,
		},
		{
			name: "whitespace-only line", line: "   ",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, ok := parsePendingEntry(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if e.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", e.ID, tc.wantID)
			}
			if e.Score != tc.wantScore {
				t.Errorf("Score = %d, want %d", e.Score, tc.wantScore)
			}
			if e.HasScore != tc.wantHasScore {
				t.Errorf("HasScore = %v, want %v", e.HasScore, tc.wantHasScore)
			}
			if e.MsgCount != tc.wantMsgCount {
				t.Errorf("MsgCount = %d, want %d", e.MsgCount, tc.wantMsgCount)
			}
			if e.HasEnqueuedAt != tc.wantHasEnqueued {
				t.Errorf("HasEnqueuedAt = %v, want %v", e.HasEnqueuedAt, tc.wantHasEnqueued)
			}
			if tc.wantHasEnqueued && !e.EnqueuedAt.Equal(tc.wantEnqueuedAt) {
				t.Errorf("EnqueuedAt = %v, want %v", e.EnqueuedAt, tc.wantEnqueuedAt)
			}
			if e.LegacyUnknownAge != tc.wantLegacyUnkAge {
				t.Errorf("LegacyUnknownAge = %v, want %v", e.LegacyUnknownAge, tc.wantLegacyUnkAge)
			}
		})
	}
}

// TestParsePendingLineStillWorks pins parsePendingLine's ID-only contract
// across every line shape parsePendingEntry understands, now that a 4th
// column exists — a regression guard for callers (pendingContainsID, dedup
// loops) that only ever need the ID.
func TestParsePendingLineStillWorks(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{"s1\t85\t120\t2026-06-01T10:00:00Z", "s1"},
		{"s2\t60\t45", "s2"},
		{"s3\t70\t2026-06-01T10:00:00Z", "s3"},
		{"s4\t50", "s4"},
		{"s5", "s5"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := parsePendingLine(tc.line); got != tc.want {
			t.Errorf("parsePendingLine(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
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

	// Create the directory + file. The hook writes one ID per line, with
	// the current 4-column format (id, score, msgCount, timestamp); older
	// lines can still be 3-column, 2-column, or bare-ID legacy shapes.
	scribeDir := filepath.Join(tmp, "scribe")
	if err := os.MkdirAll(scribeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(scribeDir, "pending-sessions.txt")
	content := "session-a\t80\t150\t2026-06-01T10:00:00Z\nsession-b\nsession-a\t99\t200\t2026-06-02T10:00:00Z\n\nsession-c\n"
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

// TestPeekAndDrainPendingEntries mirrors TestPeekAndDrainPendingSessions
// but asserts the full pendingEntry fields (score/msgCount/age) survive
// the round trip, across a mix of the 4-column current format, the
// 3-column legacy timestamp shape, and a bare-ID legacy line.
func TestPeekAndDrainPendingEntries(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	if entries := peekPendingEntries(); entries != nil {
		t.Errorf("expected nil for missing file, got %v", entries)
	}

	scribeDir := filepath.Join(tmp, "scribe")
	if err := os.MkdirAll(scribeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(scribeDir, "pending-sessions.txt")
	// s1: current 4-column format. s2: legacy 3-column (id, score, ts).
	// s3: bare-ID oldest legacy shape.
	content := "s1\t95\t120\t2026-06-01T10:00:00Z\ns2\t60\t2026-06-02T10:00:00Z\ns3\n"
	if err := os.WriteFile(pendingPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	byID := func(entries []pendingEntry) map[string]pendingEntry {
		m := make(map[string]pendingEntry, len(entries))
		for _, e := range entries {
			m[e.ID] = e
		}
		return m
	}

	assertEntries := func(t *testing.T, entries []pendingEntry) {
		t.Helper()
		if len(entries) != 3 {
			t.Fatalf("got %d entries, want 3: %+v", len(entries), entries)
		}
		m := byID(entries)
		s1 := m["s1"]
		if !s1.HasScore || s1.Score != 95 || s1.MsgCount != 120 || !s1.HasEnqueuedAt {
			t.Errorf("s1 = %+v, want score=95 hasScore msgCount=120 hasEnqueuedAt", s1)
		}
		s2 := m["s2"]
		if !s2.HasScore || s2.Score != 60 || s2.MsgCount != -1 || !s2.HasEnqueuedAt {
			t.Errorf("s2 = %+v, want score=60 hasScore msgCount=-1 hasEnqueuedAt", s2)
		}
		s3 := m["s3"]
		if s3.HasScore || s3.MsgCount != -1 || s3.HasEnqueuedAt || !s3.LegacyUnknownAge {
			t.Errorf("s3 = %+v, want zero score/msgCount=-1/no enqueuedAt/LegacyUnknownAge=true", s3)
		}
	}

	// peekPendingEntries: full fields, no file change.
	peeked := peekPendingEntries()
	assertEntries(t, peeked)
	if _, err := os.Stat(pendingPath); err != nil {
		t.Error("peek consumed the file — should not have")
	}

	// readAndClearPendingEntries: same entries, file gone.
	drained, err := readAndClearPendingEntries()
	if err != nil {
		t.Fatal(err)
	}
	assertEntries(t, drained)
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Error("drain did not remove the file")
	}

	// Second drain on the empty queue: (nil, nil), no panic.
	drained2, err := readAndClearPendingEntries()
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
