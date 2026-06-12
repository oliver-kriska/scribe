package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

// Regression coverage for the 0.2.21 read-only / portability contract
// (Codex review findings, 2026-05-15):
//
//  1. doctor / status / --dry-run must not append a run record.
//  2. loadConfig must never rewrite scribe.yaml.
//  3. doctor's FDA probe must not hard-FAIL off the macOS+capture
//     happy-path (Linux, or macOS with capture unconfigured).

func parseForTest(t *testing.T, args ...string) *kong.Context {
	t.Helper()
	parser, err := kong.New(&CLI{}, kong.Name("scribe"), kong.Exit(func(int) {
		t.Fatalf("kong tried to exit during parse of %v", args)
	}))
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	ctx, err := parser.Parse(args)
	if err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return ctx
}

// commandIsReadOnly is the exact gate main() uses to decide whether to
// skip writeRunRecord — testing it directly is the right level (a full
// exec+KB round-trip would test kong, not our contract).
func TestCommandIsReadOnly(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"doctor is read-only", []string{"doctor"}, true},
		{"status is read-only", []string{"status"}, true},
		{"sync --dry-run is read-only", []string{"sync", "--dry-run"}, true},
		{"sync (real) is not read-only", []string{"sync"}, false},
		{"capture --dry-run is read-only (generic DryRun field)", []string{"capture", "--dry-run"}, true},
		{"version is not flagged read-only (writeRunRecord skips it by name)", []string{"version"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := parseForTest(t, tc.args...)
			if got := commandIsReadOnly(ctx); got != tc.want {
				t.Errorf("commandIsReadOnly(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func writeMinimalConfig(t *testing.T, dir string) string {
	t.Helper()
	cfgPath := filepath.Join(dir, "scribe.yaml")
	// No top-level `absorb:` key — this is exactly the shape that used
	// to trigger the hidden rewrite on every read.
	body := "owner_name: \"Test\"\ndomains:\n  - general\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func TestLoadConfigIsPure(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeMinimalConfig(t, dir)
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = loadConfig(dir) // must not touch the file
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("loadConfig mutated scribe.yaml\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestMaybeBackfillAbsorbBlock(t *testing.T) {
	t.Run("appends when absorb: absent", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := writeMinimalConfig(t, dir)
		before, _ := os.ReadFile(cfgPath)
		maybeBackfillAbsorbBlock(dir)
		after, _ := os.ReadFile(cfgPath)
		if len(after) <= len(before) {
			t.Fatalf("expected scribe.yaml to grow with the absorb block; before=%d after=%d", len(before), len(after))
		}
		if !hasTopLevelKey(string(after), "absorb") {
			t.Errorf("backfilled file still has no top-level absorb: key:\n%s", after)
		}
	})

	t.Run("SCRIBE_NO_CONFIG_BACKFILL leaves file untouched", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := writeMinimalConfig(t, dir)
		before, _ := os.ReadFile(cfgPath)
		t.Setenv("SCRIBE_NO_CONFIG_BACKFILL", "1")
		maybeBackfillAbsorbBlock(dir)
		after, _ := os.ReadFile(cfgPath)
		if !bytes.Equal(before, after) {
			t.Error("SCRIBE_NO_CONFIG_BACKFILL=1 must leave scribe.yaml byte-identical")
		}
	})

	t.Run("no-op when absorb: already present", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "scribe.yaml")
		body := "owner_name: \"Test\"\nabsorb:\n  strictness: high\n"
		if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		maybeBackfillAbsorbBlock(dir)
		after, _ := os.ReadFile(cfgPath)
		if string(after) != body {
			t.Errorf("existing absorb: section must not be touched\ngot:\n%s", after)
		}
	})
}

func fdaCheck(checks []check) (found bool, c check) {
	for _, ck := range checks {
		if ck.Name == "chat.db (FDA)" {
			return true, ck
		}
	}
	return false, check{}
}

func TestCheckDeps_FDACapabilityAware(t *testing.T) {
	noCapture := &ScribeConfig{} // Capture zero-value: no self-chat handles

	t.Run("non-darwin emits no chat.db FAIL", func(t *testing.T) {
		orig := runtimeGOOS
		runtimeGOOS = "linux"
		t.Cleanup(func() { runtimeGOOS = orig })

		checks := checkDeps(noCapture)
		for _, ck := range checks {
			if ck.Status == statusFail && strings.Contains(ck.Name, "FDA") {
				t.Errorf("Linux must not hard-FAIL on FDA; got %+v", ck)
			}
		}
		if found, _ := fdaCheck(checks); found {
			t.Error("Linux should not emit a chat.db (FDA) row at all")
		}
	})

	t.Run("darwin + capture unconfigured warns, never FAILs", func(t *testing.T) {
		orig := runtimeGOOS
		runtimeGOOS = "darwin"
		t.Cleanup(func() { runtimeGOOS = orig })

		checks := checkDeps(noCapture)
		found, ck := fdaCheck(checks)
		if !found {
			t.Fatal("darwin should still surface a chat.db (FDA) row")
		}
		if ck.Status == statusFail {
			t.Errorf("capture-unconfigured macOS must not hard-FAIL; got %+v", ck)
		}
		if ck.Status != statusWarn {
			t.Errorf("expected statusWarn for capture-unconfigured, got %q", ck.Status)
		}
	})
}
