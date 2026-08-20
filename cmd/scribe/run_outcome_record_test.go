package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// decodeDegradedRecord drives the real writeRunRecord and reads back the
// JSONL line that `scribe doctor` later parses.
func decodeDegradedRecord(t *testing.T, root string) struct {
	Command   string            `json:"command"`
	Status    string            `json:"status"`
	Error     string            `json:"error"`
	Degraded  []string          `json:"degraded"`
	DegErrors map[string]string `json:"degraded_errors"`
} {
	t.Helper()
	var rec struct {
		Command   string            `json:"command"`
		Status    string            `json:"status"`
		Error     string            `json:"error"`
		Degraded  []string          `json:"degraded"`
		DegErrors map[string]string `json:"degraded_errors"`
	}
	if err := json.Unmarshal([]byte(readSingleRunRecord(t, root)), &rec); err != nil {
		t.Fatalf("run record is not valid JSON: %v", err)
	}
	return rec
}

func TestWriteRunRecord_DegradedStatusAndFields(t *testing.T) {
	root := testKB(t, "")
	t.Setenv("SCRIBE_KB", root)
	resetRunOutcome()
	t.Cleanup(resetRunOutcome)

	logPhaseFailure("sync", "absorb", errors.New("ollama refused"))
	logPhaseDegraded("sync", "push", "push failed (offline?)")

	// runErr is nil: the command completed and exits 0.
	writeRunRecord("sync", time.Now(), nil)

	rec := decodeDegradedRecord(t, root)
	if rec.Status != "degraded" {
		t.Fatalf("status = %q, want degraded", rec.Status)
	}
	if !slices.Equal(rec.Degraded, []string{"absorb", "push"}) {
		t.Fatalf("degraded = %v, want [absorb push]", rec.Degraded)
	}
	if rec.DegErrors["absorb"] != "ollama refused" {
		t.Errorf("absorb message = %q", rec.DegErrors["absorb"])
	}
	if rec.Error != "" {
		t.Errorf("a degraded run has no top-level error, got %q", rec.Error)
	}
}

func TestWriteRunRecord_CleanRunStaysOK(t *testing.T) {
	root := testKB(t, "")
	t.Setenv("SCRIBE_KB", root)
	resetRunOutcome()
	t.Cleanup(resetRunOutcome)

	writeRunRecord("sync", time.Now(), nil)

	rec := decodeDegradedRecord(t, root)
	if rec.Status != "ok" {
		t.Fatalf("status = %q, want ok", rec.Status)
	}
	if rec.Degraded != nil {
		t.Errorf("clean run must not carry degraded fields, got %v", rec.Degraded)
	}
}

// A hard error outranks a degraded phase: the command failed outright.
func TestWriteRunRecord_ErrorOutranksDegraded(t *testing.T) {
	root := testKB(t, "")
	t.Setenv("SCRIBE_KB", root)
	resetRunOutcome()
	t.Cleanup(resetRunOutcome)

	logPhaseFailure("sync", "absorb", errors.New("ollama refused"))
	writeRunRecord("sync", time.Now(), errors.New("lock held"))

	rec := decodeDegradedRecord(t, root)
	if rec.Status != "error" {
		t.Fatalf("status = %q, want error", rec.Status)
	}
	if rec.Error != "lock held" {
		t.Errorf("error = %q", rec.Error)
	}
}

func TestRecordDegraded_TruncatesLongMessages(t *testing.T) {
	resetRunOutcome()
	t.Cleanup(resetRunOutcome)

	// Run records are read back line-by-line by a 1 MB-capped scanner; an
	// LLM error wrapping a response body would poison the whole day file.
	recordDegradedMsg("absorb", strings.Repeat("x", 5000))

	_, msgs := degradedPhases()
	if got := len(msgs["absorb"]); got != degradedMsgMax {
		t.Fatalf("message length = %d, want %d", got, degradedMsgMax)
	}
}

// The two doctor readers are the reason the status exists; both must
// handle it, and in opposite directions.
func writeRunsFile(t *testing.T, root, day, content string) {
	t.Helper()
	dir := filepath.Join(root, "output", "runs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRunRecords_DegradedCountsAsRan(t *testing.T) {
	root := t.TempDir()
	writeRunsFile(t, root, "2026-08-19",
		`{"command":"sync","status":"degraded","timestamp":"2026-08-19T08:00:00Z","degraded":["absorb"]}`+"\n"+
			`{"command":"dream","status":"error","timestamp":"2026-08-19T09:00:00Z"}`+"\n")

	got, err := loadRunRecords(root)
	if err != nil {
		t.Fatalf("loadRunRecords: %v", err)
	}
	// The job fired — freshness must not report cron as stale.
	if _, ok := got["sync"]; !ok {
		t.Fatal("a degraded run must count as proof the command ran")
	}
	// A hard error still must not.
	if _, ok := got["dream"]; ok {
		t.Error("an error record must not count for freshness")
	}
}

func TestLoadRunErrors_SurfacesDegraded(t *testing.T) {
	root := t.TempDir()
	writeRunsFile(t, root, "2026-08-19",
		`{"command":"sync","status":"degraded","timestamp":"2026-08-19T08:00:00Z",`+
			`"degraded":["absorb","push"],"degraded_errors":{"absorb":"ollama refused"}}`+"\n")

	got, err := loadRunErrors(root, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("loadRunErrors: %v", err)
	}
	e, ok := got["sync"]
	if !ok {
		t.Fatal("a degraded run must surface in the errors section, or it is invisible everywhere")
	}
	if !strings.Contains(e.Msg, "absorb") || !strings.Contains(e.Msg, "ollama refused") {
		t.Fatalf("message should name the phase and its error, got %q", e.Msg)
	}
}

func TestLoadRunErrors_DegradedWithNoPhasesDoesNotDangle(t *testing.T) {
	root := t.TempDir()
	writeRunsFile(t, root, "2026-08-19",
		`{"command":"sync","status":"degraded","timestamp":"2026-08-19T08:00:00Z"}`+"\n")

	got, err := loadRunErrors(root, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("loadRunErrors: %v", err)
	}
	if msg := got["sync"].Msg; strings.HasSuffix(msg, "in ") || strings.HasSuffix(msg, ", ") {
		t.Fatalf("message dangles with no phases: %q", msg)
	}
}
