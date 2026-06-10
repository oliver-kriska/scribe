package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeKBFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFirstConflictMarkerLine(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
	}{
		{"clean", "# Title\n\nbody text\n", 0},
		{"open marker", "line one\n<<<<<<< HEAD\nours\n", 2},
		{"close marker", "a\nb\n>>>>>>> origin/main\n", 3},
		{"setext heading not flagged", "Heading\n=======\n\nbody\n", 0},
		{"indented marker not flagged", "    <<<<<<< HEAD in a code block\n", 0},
		{"marker without trailing space not flagged", "<<<<<<<\n", 0},
		{"marker on last line without newline", "text\n>>>>>>> branch", 2},
		{"empty file", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstConflictMarkerLine([]byte(tt.content)); got != tt.want {
				t.Errorf("firstConflictMarkerLine = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFindConflictMarkers(t *testing.T) {
	root := t.TempDir()

	writeKBFile(t, root, "wiki/clean.md", "# Clean\n\nnothing to see\n")
	writeKBFile(t, root, "wiki/broken.md", "# Broken\n<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> origin/main\n")
	writeKBFile(t, root, "wiki/setext.md", "Title\n=======\n\nan old-style heading, not a conflict\n")
	writeKBFile(t, root, "wiki/_index.md", "- [[Clean]]\n>>>>>>> origin/main\n")
	writeKBFile(t, root, "patterns/also-broken.md", "body\n<<<<<<< HEAD\n")
	writeKBFile(t, root, "raw/articles/pulled.md", "captured\n>>>>>>> abc123 (theirs)\n")
	writeKBFile(t, root, "raw/articles/fine.md", "all good\n")

	hits := findConflictMarkers(root)

	want := map[string]int{
		"wiki/broken.md":          2,
		"wiki/_index.md":          2,
		"patterns/also-broken.md": 2,
		"raw/articles/pulled.md":  2,
	}
	if len(hits) != len(want) {
		got := make([]string, 0, len(hits))
		for _, h := range hits {
			got = append(got, h.Rel)
		}
		t.Fatalf("found %d hits %v, want %d", len(hits), got, len(want))
	}
	for _, h := range hits {
		line, ok := want[h.Rel]
		if !ok {
			t.Errorf("unexpected hit %s:%d", h.Rel, h.Line)
			continue
		}
		if h.Line != line {
			t.Errorf("%s: line = %d, want %d", h.Rel, h.Line, line)
		}
	}

	// Sorted by path for stable output.
	for i := 1; i < len(hits); i++ {
		if hits[i-1].Rel > hits[i].Rel {
			t.Errorf("hits not sorted: %q before %q", hits[i-1].Rel, hits[i].Rel)
		}
	}
}

func TestFindConflictMarkersEmptyKB(t *testing.T) {
	if hits := findConflictMarkers(t.TempDir()); len(hits) != 0 {
		t.Errorf("empty KB produced hits: %v", hits)
	}
}

func TestDoctorWarnsOnConflictMarkers(t *testing.T) {
	root := t.TempDir()
	writeKBFile(t, root, "wiki/broken.md", "# Broken\n<<<<<<< HEAD\nours\n>>>>>>> theirs\n")

	var found *check
	for _, c := range checkState(root) {
		if c.Name == "conflict-markers" {
			found = &c
			break
		}
	}
	if found == nil {
		t.Fatal("doctor did not flag conflict markers")
	}
	if found.Status != statusWarn {
		t.Errorf("status = %q, want WARN", found.Status)
	}
	if !strings.Contains(found.Detail, "wiki/broken.md:2") {
		t.Errorf("detail %q does not point at wiki/broken.md:2", found.Detail)
	}
}

func TestDoctorNoConflictMarkerCheckWhenClean(t *testing.T) {
	root := t.TempDir()
	writeKBFile(t, root, "wiki/clean.md", "# Clean\n\nbody\n")

	for _, c := range checkState(root) {
		if c.Name == "conflict-markers" {
			t.Fatalf("clean KB flagged: %+v", c)
		}
	}
}
