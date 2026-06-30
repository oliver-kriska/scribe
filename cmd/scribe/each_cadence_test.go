package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEachJobKeys(t *testing.T) {
	cases := []struct {
		args         []string
		path, specif string
	}{
		{[]string{"sync", "--max", "2"}, "sync", "sync --max"},
		{[]string{"sync", "--sessions", "--sessions-max", "3"}, "sync", "sync --sessions"},
		{[]string{"dream"}, "dream", "dream"},
		{[]string{"ingest", "drain"}, "ingest drain", "ingest drain"},
		{[]string{"capture", "--fetch"}, "capture", "capture --fetch"},
		{[]string{"commit"}, "commit", "commit"},
	}
	for _, tc := range cases {
		path, specif := eachJobKeys(tc.args)
		if path != tc.path || specif != tc.specif {
			t.Errorf("eachJobKeys(%v) = (%q,%q), want (%q,%q)", tc.args, path, specif, tc.path, tc.specif)
		}
	}
}

func TestParseCadenceDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"2h", 2 * time.Hour, true},
		{"30m", 30 * time.Minute, true},
		{"7d", 7 * 24 * time.Hour, true},
		{"1.5d", 36 * time.Hour, true},
		{"90s", 90 * time.Second, true},
		{"nonsense", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, err := parseCadenceDuration(tc.in)
		if tc.ok && (err != nil || got != tc.want) {
			t.Errorf("parseCadenceDuration(%q) = (%v,%v), want %v", tc.in, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Errorf("parseCadenceDuration(%q) = %v, want error", tc.in, got)
		}
	}
}

func TestCadenceInterval(t *testing.T) {
	cfg := &ScribeConfig{Each: EachConfig{Cadence: map[string]string{
		"sync --sessions": "6h",
		"sync":            "2h",
		"dream":           "bogus", // unparseable → ignored
	}}}

	// Specific key wins over the bare-command fallback.
	if d, ok := cadenceInterval(cfg, []string{"sync", "--sessions", "--sessions-max", "3"}); !ok || d != 6*time.Hour {
		t.Errorf("sessions: got (%v,%v), want 6h", d, ok)
	}
	// No specific entry → falls back to the bare "sync" cadence.
	if d, ok := cadenceInterval(cfg, []string{"sync", "--max", "2"}); !ok || d != 2*time.Hour {
		t.Errorf("sync --max: got (%v,%v), want 2h via base", d, ok)
	}
	// Unparseable value → treated as unconfigured (runs every tick).
	if _, ok := cadenceInterval(cfg, []string{"dream"}); ok {
		t.Error("unparseable cadence should resolve to not-configured")
	}
	// No matching key at all → not configured.
	if _, ok := cadenceInterval(cfg, []string{"lint"}); ok {
		t.Error("unconfigured command should resolve to not-configured")
	}
}

func TestCadenceSkipReason(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	writeEachConfig(t, root, "each:\n  cadence:\n    \"sync --sessions\": 6h\n")

	args := []string{"sync", "--sessions", "--sessions-max", "3"}

	// Last ok 1h ago, cadence 6h → not due, skip with a reason.
	writeEachRunRecord(t, root, "sync", []string{"--sessions", "--sessions-max", "3"}, now.Add(-1*time.Hour))
	if reason := cadenceSkipReason(root, args, now); reason == "" {
		t.Error("expected a skip reason when the job ran 1h ago under a 6h cadence")
	}

	// A run 7h ago is older than the cadence → due, run (no reason).
	root2 := t.TempDir()
	writeEachConfig(t, root2, "each:\n  cadence:\n    \"sync --sessions\": 6h\n")
	writeEachRunRecord(t, root2, "sync", []string{"--sessions"}, now.Add(-7*time.Hour))
	if reason := cadenceSkipReason(root2, args, now); reason != "" {
		t.Errorf("a 7h-old run under a 6h cadence should run; got skip %q", reason)
	}

	// No cadence configured → always runs (back-compat).
	root3 := t.TempDir()
	writeEachConfig(t, root3, "default_model: sonnet\n")
	writeEachRunRecord(t, root3, "sync", []string{"--sessions"}, now.Add(-1*time.Minute))
	if reason := cadenceSkipReason(root3, args, now); reason != "" {
		t.Errorf("no cadence configured must never skip; got %q", reason)
	}

	// Cadence configured but the job never ran ok → runs (fail open).
	root4 := t.TempDir()
	writeEachConfig(t, root4, "each:\n  cadence:\n    \"sync --sessions\": 6h\n")
	if reason := cadenceSkipReason(root4, args, now); reason != "" {
		t.Errorf("a never-run job must not be skipped; got %q", reason)
	}
}

func writeEachConfig(t *testing.T, root, yaml string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeEachRunRecord(t *testing.T, root, command string, args []string, ts time.Time) {
	t.Helper()
	runsDir := filepath.Join(root, "output", "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := map[string]any{
		"command":   command,
		"status":    "ok",
		"timestamp": ts.UTC().Format(time.RFC3339),
		"args":      args,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	dayFile := filepath.Join(runsDir, ts.UTC().Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(dayFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
}
