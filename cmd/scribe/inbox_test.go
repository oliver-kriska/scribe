package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathWithinRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	cases := []struct {
		name   string
		target string
		want   bool
	}{
		{"root itself", root, true},
		{"direct child", filepath.Join(root, "raw", "inbox"), true},
		{"deep child", filepath.Join(root, "a", "b", "c"), true},
		{"parent escape", filepath.Join(root, "..", "escape"), false},
		{"double parent escape", filepath.Join(root, "..", "..", "etc"), false},
		{"sibling via traversal", filepath.Join(root, "raw", "..", "..", "other"), false},
		{"absolute elsewhere", "/etc/passwd", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pathWithinRoot(root, c.target); got != c.want {
				t.Errorf("pathWithinRoot(%q, %q) = %v, want %v", root, c.target, got, c.want)
			}
		})
	}
}

// TestDrainFileInboxRejectsEscapingInboxPath is the regression for the P1-B
// path-traversal hole: ingest.inbox_path lives in the (repo-controlled, NOT
// trust-locked) scribe.yaml, so a shared team config could set it to ../../..
// and make scribe drain an out-of-tree directory — reading arbitrary local
// files into the KB as articles, or scattering .processed/.failed dirs
// outside the KB. drainFileInbox must refuse an inbox that resolves outside
// root rather than touch it.
func TestDrainFileInboxRejectsEscapingInboxPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// A secret that lives OUTSIDE the KB, in the parent the malicious
	// inbox_path points at. If the guard fails, drain would walk this dir.
	outside := filepath.Dir(root)
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	yaml := "owner_name: Test\ningest:\n  inbox_path: ../\n"
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := drainFileInbox(root)
	if err == nil {
		t.Fatalf("expected drainFileInbox to refuse an escaping inbox_path, got nil error (processed %d)", n)
	}
	if n != 0 {
		t.Errorf("processed = %d, want 0 (nothing should be ingested from an out-of-tree inbox)", n)
	}
	// The guard must trip before any .processed/.failed dirs are created
	// in the escaped directory.
	if _, statErr := os.Stat(filepath.Join(outside, ".processed")); statErr == nil {
		t.Errorf(".processed dir was created outside the KB root — guard ran too late")
	}
}

// TestDrainFileInboxMissingInboxIsNoError confirms the guard doesn't break the
// common case: a KB with the default in-tree inbox that simply doesn't exist
// yet is a no-op, not an error.
func TestDrainFileInboxMissingInboxIsNoError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("owner_name: Test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := drainFileInbox(root)
	if err != nil {
		t.Fatalf("default in-tree inbox that is absent should be a no-op, got error: %v", err)
	}
	if n != 0 {
		t.Errorf("processed = %d, want 0", n)
	}
}
