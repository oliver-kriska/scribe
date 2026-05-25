package main

import (
	"os"
	"path/filepath"
	"testing"
)

// setupPlaceholderKB builds a KB containing the on-disk fingerprint of a
// leaked prompt placeholder: a directory literally named `{{DOMAIN}}` and a
// file named with `{{TITLE}}`, alongside legitimate content and a generated
// `_sections` tree that must be left alone.
func setupPlaceholderKB(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "projects", "{{DOMAIN}}"))
	mustWrite(t, filepath.Join(root, "projects", "{{DOMAIN}}", "scribe_analysis.md"), "leaked")
	mustMkdir(t, filepath.Join(root, "projects", "real-project"))
	mustWrite(t, filepath.Join(root, "projects", "real-project", "overview.md"), "ok")
	mustMkdir(t, filepath.Join(root, "wiki"))
	mustWrite(t, filepath.Join(root, "wiki", "{{TITLE}}.md"), "leaked file")
	mustWrite(t, filepath.Join(root, "wiki", "normal.md"), "ok")
	// Generated tree — must be skipped (regenerates on reindex).
	mustMkdir(t, filepath.Join(root, "wiki", "_sections", "projects", "{{DOMAIN}}"))
	mustWrite(t, filepath.Join(root, "wiki", "_sections", "projects", "{{DOMAIN}}", "x.json"), "{}")
	return root
}

func TestFindPlaceholderArtifacts(t *testing.T) {
	root := setupPlaceholderKB(t)
	got := findPlaceholderArtifacts(root)

	want := map[string]bool{
		"projects/{{DOMAIN}}": true, // directory, reported once
		"wiki/{{TITLE}}.md":   true, // file
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want keys %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected placeholder artifact flagged: %q", g)
		}
	}
}

func TestFixPlaceholderArtifacts(t *testing.T) {
	root := setupPlaceholderKB(t)

	removed := fixPlaceholderArtifacts(root, false)
	if removed != 2 {
		t.Errorf("removed = %d, want 2 (the {{DOMAIN}} dir + {{TITLE}}.md file)", removed)
	}
	gone := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, rel))
		return os.IsNotExist(err)
	}
	if !gone("projects/{{DOMAIN}}") {
		t.Error("placeholder directory tree should have been removed")
	}
	if !gone("wiki/{{TITLE}}.md") {
		t.Error("placeholder-named file should have been removed")
	}
	// Legitimate content untouched.
	if gone("projects/real-project/overview.md") {
		t.Error("real project was wrongly removed")
	}
	if gone("wiki/normal.md") {
		t.Error("normal article was wrongly removed")
	}
}

func TestFixPlaceholderArtifacts_DryRunWritesNothing(t *testing.T) {
	root := setupPlaceholderKB(t)
	if n := fixPlaceholderArtifacts(root, true); n != 2 {
		t.Errorf("dry-run count = %d, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(root, "projects", "{{DOMAIN}}", "scribe_analysis.md")); err != nil {
		t.Error("dry-run removed a placeholder artifact (should be untouched)")
	}
}
