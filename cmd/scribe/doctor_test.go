package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadRunRecords covers the critical path — doctor's freshness check is
// only as good as this loader. The three things it must not get wrong:
// (a) picking the newest ok record per command, (b) ignoring error records,
// (c) splitting `sync` vs `sync --sessions` into distinct keys.
func TestLoadRunRecords(t *testing.T) {
	root := t.TempDir()
	runsDir := filepath.Join(root, "output", "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Two JSONL files across two dates. Newest ok lines should win.
	day1 := filepath.Join(runsDir, "2026-04-09.jsonl")
	day2 := filepath.Join(runsDir, "2026-04-10.jsonl")

	day1Content := []string{
		`{"command":"sync","status":"ok","timestamp":"2026-04-09T10:00:00Z","args":["sync"]}`,
		`{"command":"sync","status":"error","timestamp":"2026-04-09T12:00:00Z","args":["sync"]}`,
		`{"command":"lint","status":"ok","timestamp":"2026-04-09T12:30:00Z","args":["lint"]}`,
	}
	day2Content := []string{
		`{"command":"sync","status":"ok","timestamp":"2026-04-10T08:00:00Z","args":["sync","--sessions","--sessions-max","3"]}`,
		`{"command":"sync","status":"ok","timestamp":"2026-04-10T06:00:00Z","args":["sync"]}`,
		`{"command":"dream","status":"error","timestamp":"2026-04-10T02:00:00Z","args":["dream"]}`,
		`garbage line that should be skipped`,
		`{"command":"ingest drain","status":"ok","timestamp":"2026-04-10T09:30:00Z","args":["ingest","drain"]}`,
	}
	if err := os.WriteFile(day1, []byte(joinLines(day1Content)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(day2, []byte(joinLines(day2Content)), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := loadRunRecords(root)
	if err != nil {
		t.Fatalf("loadRunRecords: %v", err)
	}

	// Newest ok `sync` is 2026-04-10T08:00:00Z (the one with --sessions args).
	syncTime := mustTime(t, "2026-04-10T08:00:00Z")
	if !got["sync"].Equal(syncTime) {
		t.Errorf("sync newest: got %v, want %v", got["sync"], syncTime)
	}
	// `sync --sessions` should only see the 08:00 record (the other sync had no --sessions flag).
	if !got["sync --sessions"].Equal(syncTime) {
		t.Errorf("sync --sessions: got %v, want %v", got["sync --sessions"], syncTime)
	}
	// `lint` — only day1 had it.
	lintTime := mustTime(t, "2026-04-09T12:30:00Z")
	if !got["lint"].Equal(lintTime) {
		t.Errorf("lint: got %v, want %v", got["lint"], lintTime)
	}
	// `dream` had only an error — must not appear.
	if _, ok := got["dream"]; ok {
		t.Errorf("dream should not appear (only error records): %v", got["dream"])
	}
	// `ingest drain` — args do not contain any --flag, so only the base key should exist.
	drainTime := mustTime(t, "2026-04-10T09:30:00Z")
	if !got["ingest drain"].Equal(drainTime) {
		t.Errorf("ingest drain: got %v, want %v", got["ingest drain"], drainTime)
	}
}

func TestLoadRunRecords_MissingDir(t *testing.T) {
	// Fresh checkout with no output/runs yet must not error.
	root := t.TempDir()
	got, err := loadRunRecords(root)
	if err != nil {
		t.Fatalf("expected nil error on missing dir, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %d entries", len(got))
	}
}

func TestClassifyFreshness(t *testing.T) {
	now := mustTime(t, "2026-04-10T12:00:00Z")
	cases := []struct {
		name    string
		lastOk  time.Time
		gap     time.Duration
		want    checkStatus
		wantSub string // substring the detail must contain
	}{
		{"never ran", time.Time{}, 6 * time.Hour, statusWarn, "never run"},
		{"fresh — within gap", now.Add(-1 * time.Hour), 6 * time.Hour, statusOK, "last ok 1h ago"},
		{"right at edge", now.Add(-6 * time.Hour), 6 * time.Hour, statusOK, "last ok 6h ago"},
		{"stale — over gap", now.Add(-7 * time.Hour), 6 * time.Hour, statusWarn, "expected ≤ 6h"},
		{"very stale — days", now.Add(-72 * time.Hour), 48 * time.Hour, statusWarn, "3d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, detail := classifyFreshness(tc.lastOk, now, tc.gap)
			if status != tc.want {
				t.Errorf("status: got %q, want %q", status, tc.want)
			}
			if !strings.Contains(detail, tc.wantSub) {
				t.Errorf("detail: got %q, want substring %q", detail, tc.wantSub)
			}
		})
	}
}

// TestCheckState_Parsers verifies that each state file probe correctly
// classifies a corrupt fixture as FAIL and a valid fixture as OK. The full
// checkState function is exercised against a real tmp KB root.
func TestCheckState_Parsers(t *testing.T) {
	root := t.TempDir()
	// Minimal KB layout.
	mustMkdir(t, filepath.Join(root, "scripts"))
	mustMkdir(t, filepath.Join(root, "wiki"))

	// Valid projects.json manifest.
	manifest := `{"projects":{"foo":{"path":"/tmp/foo","domain":"general","last_sha":"","last_extracted":"","last_md_scan":""}},"domain_aliases":{},"ignored_paths":[]}`
	mustWrite(t, filepath.Join(root, "scripts", "projects.json"), manifest)
	// Valid imessage-state.
	mustWrite(t, filepath.Join(root, "scripts", "imessage-state.json"), `{"last_capture":null,"captured_urls":[],"captured_count":0}`)
	// Corrupt sessions log — should FAIL.
	mustWrite(t, filepath.Join(root, "wiki", "_sessions_log.json"), `{not valid json`)
	// Valid backlinks.
	mustWrite(t, filepath.Join(root, "wiki", "_backlinks.json"), `{"Foo":["Bar"]}`)
	// Non-empty index.md and log.md.
	mustWrite(t, filepath.Join(root, "wiki", "_index.md"), "# Index\n\n- item\n")
	mustWrite(t, filepath.Join(root, "log.md"), "## 2026-04-10 init\n")

	results := checkState(root)

	findStatus := func(name string) checkStatus {
		for _, ck := range results {
			if ck.Name == name {
				return ck.Status
			}
		}
		return ""
	}

	cases := []struct {
		name string
		want checkStatus
	}{
		{"scripts/projects.json", statusOK},
		{"scripts/imessage-state.json", statusOK},
		{"wiki/_sessions_log.json", statusFail},
		{"wiki/_backlinks.json", statusOK},
		{"wiki/_index.md", statusOK},
		{"log.md", statusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findStatus(tc.name)
			if got != tc.want {
				t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestPrintChecksJSON sanity-checks the JSON schema so downstream consumers
// (monitoring probes) get stable keys.
func TestPrintChecksJSON(t *testing.T) {
	all := []check{
		{Section: "deps", Name: "claude", Status: statusOK, Detail: "/usr/bin/claude"},
		{Section: "cron", Name: "com.scribe.lint", Status: statusFail, Detail: "missing", Fix: "scribe cron install"},
		{Section: "freshness", Name: "lint", Status: statusWarn, Detail: "never run"},
	}

	// Capture stdout by temporarily replacing os.Stdout with a pipe.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	printChecksJSON(all, "/tmp/fake")
	_ = w.Close()
	os.Stdout = orig

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	var payload struct {
		KBRoot  string         `json:"kb_root"`
		Checks  []check        `json:"checks"`
		Summary map[string]int `json:"summary"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, string(data))
	}

	if payload.KBRoot != "/tmp/fake" {
		t.Errorf("kb_root: got %q", payload.KBRoot)
	}
	if len(payload.Checks) != 3 {
		t.Errorf("checks count: got %d, want 3", len(payload.Checks))
	}
	if payload.Summary["ok"] != 1 || payload.Summary["warn"] != 1 || payload.Summary["fail"] != 1 {
		t.Errorf("summary: %+v", payload.Summary)
	}
}

// ---- test helpers ----

func joinLines(lines []string) string {
	return strings.Join(lines, "\n") + "\n"
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return tm
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// mustWrite lives in link_test.go — reused here.
