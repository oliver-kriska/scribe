package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAbsorbDryRunWritesNothing: the unfetched-stub branch ran ahead of
// the --dry-run guard, so a dry run parked stubs into
// wiki/_unfetched-links.md and rewrote _absorb_log.json — while main.go
// records dry runs as read-only. A dry run must leave the KB byte-for-byte
// untouched.
func TestAbsorbDryRunWritesNothing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	rawDir := filepath.Join(root, "raw", "articles")
	wikiDir := filepath.Join(root, "wiki")
	for _, d := range []string{rawDir, wikiDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub := "---\ntitle: Unfetched\nsource_url: https://example.com/x\nfetched_via: stub\n---\n"
	if err := os.WriteFile(filepath.Join(rawDir, "2026-09-03-stub.md"), []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(wikiDir, "_absorb_log.json")
	if err := os.WriteFile(logPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	parked := filepath.Join(wikiDir, "_unfetched-links.md")

	s := &SyncCmd{DryRun: true, MaxAbsorb: 5}
	if _, err := s.absorbRaw(root); err != nil {
		t.Fatalf("absorbRaw dry run: %v", err)
	}
	if _, err := os.Stat(parked); !os.IsNotExist(err) {
		t.Error("dry run parked a stub into wiki/_unfetched-links.md")
	}
	if data, _ := os.ReadFile(logPath); string(data) != "{}\n" {
		t.Errorf("dry run rewrote _absorb_log.json: %q", data)
	}

	// The real run still parks and records the stub.
	s.DryRun = false
	if _, err := s.absorbRaw(root); err != nil {
		t.Fatalf("absorbRaw: %v", err)
	}
	if _, err := os.Stat(parked); err != nil {
		t.Error("real run must park the stub")
	}
	if data, _ := os.ReadFile(logPath); string(data) == "{}\n" {
		t.Error("real run must record the stub in _absorb_log.json")
	}
}
