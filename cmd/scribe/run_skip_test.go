package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWriteRunRecord_SkippedRun: a lock-busy or lease-lost run exits 0
// but must not be recorded as "ok" — doctor's freshness check and
// dreamRunHistory read that status as "the job ran".
func TestWriteRunRecord_SkippedRun(t *testing.T) {
	root := testKB(t, "")
	t.Setenv("SCRIBE_KB", root)
	globalRoot = ""
	resetRunOutcome()
	t.Cleanup(resetRunOutcome)

	writeRunRecord("dream", time.Now(), fmt.Errorf("dream: %w", skipRun("another dream cycle is running")))

	var rec map[string]any
	if err := json.Unmarshal([]byte(readSingleRunRecord(t, root)), &rec); err != nil {
		t.Fatalf("run record is not valid JSON: %v", err)
	}
	if rec["status"] != "skipped" {
		t.Errorf("status = %v, want skipped", rec["status"])
	}
	if rec["skip_reason"] != "another dream cycle is running" {
		t.Errorf("skip_reason = %v", rec["skip_reason"])
	}
	if _, has := rec["error"]; has {
		t.Errorf("a skip is not an error: %v", rec["error"])
	}
}

func TestRunSkipReason_Wrapped(t *testing.T) {
	if r, ok := runSkipReason(fmt.Errorf("outer: %w", skipRun("lease held"))); !ok || r != "lease held" {
		t.Errorf("wrapped skip not recognized: %q %v", r, ok)
	}
	if _, ok := runSkipReason(errors.New("plain failure")); ok {
		t.Error("plain error reported as skip")
	}
	if _, ok := runSkipReason(nil); ok {
		t.Error("nil reported as skip")
	}
}

// TestLogPhaseOutcome_SkipDegradesPhase: mining that was rate-limited
// before any work used to return nil and the sync recorded "ok".
func TestLogPhaseOutcome_SkipDegradesPhase(t *testing.T) {
	resetRunOutcome()
	t.Cleanup(resetRunOutcome)
	logPhaseOutcome("sync", "session mining", nil)
	if names, _ := degradedPhases(); len(names) != 0 {
		t.Fatalf("nil must be a no-op, got %v", names)
	}
	logPhaseOutcome("sync", "session mining", skipRun("rate limited before any session was mined"))
	names, msgs := degradedPhases()
	if len(names) != 1 || names[0] != "session mining" {
		t.Fatalf("degraded phases = %v", names)
	}
	if !strings.Contains(msgs["session mining"], "skipped: rate limited") {
		t.Errorf("message = %q", msgs["session mining"])
	}
}

// TestLoadRunRecords_HotDoesNotRefreshFullDream: the daily `dream --hot`
// record refreshed the weekly "dream" freshness key, so a weekly dream
// that had stopped firing stayed green for as long as the hot pass ran.
// A skipped record refreshes nothing.
func TestLoadRunRecords_HotDoesNotRefreshFullDream(t *testing.T) {
	root := t.TempDir()
	runsDir := filepath.Join(root, "output", "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"command":"dream","status":"ok","timestamp":"2026-07-01T02:00:00Z","args":["dream"]}`,
		`{"command":"dream","status":"ok","timestamp":"2026-07-05T03:10:00Z","args":["dream","--hot"],"mode":"hot"}`,
		`{"command":"dream","status":"skipped","timestamp":"2026-07-06T02:00:00Z","args":["dream"],"skip_reason":"another dream cycle is running"}`,
		`{"command":"sync","status":"skipped","timestamp":"2026-07-06T04:00:00Z","args":["sync"],"skip_reason":"another scribe sync is running"}`,
		`{"command":"sync","status":"ok","timestamp":"2026-07-06T03:00:00Z","args":["sync","--sessions"]}`,
	}
	if err := os.WriteFile(filepath.Join(runsDir, "2026-07-06.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadRunRecords(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := mustTime(t, "2026-07-01T02:00:00Z"); !got["dream"].Equal(want) {
		t.Errorf("dream = %v, want %v (hot and skipped records must not refresh it)", got["dream"], want)
	}
	if want := mustTime(t, "2026-07-05T03:10:00Z"); !got["dream --hot"].Equal(want) {
		t.Errorf("dream --hot = %v, want %v", got["dream --hot"], want)
	}
	// --sessions is an inclusive mode: that run extracted projects too.
	if want := mustTime(t, "2026-07-06T03:00:00Z"); !got["sync"].Equal(want) {
		t.Errorf("sync = %v, want %v (skipped record must not win)", got["sync"], want)
	}
}

func TestFormatRunLine_ShowsSkipReason(t *testing.T) {
	line := `{"command":"sync","status":"skipped","timestamp":"2026-09-03T02:00:00Z","skip_reason":"another scribe sync is running"}`
	if got := formatRunLine(line); !strings.Contains(got, "[skipped: another scribe sync is running]") {
		t.Errorf("formatRunLine = %q", got)
	}
}
