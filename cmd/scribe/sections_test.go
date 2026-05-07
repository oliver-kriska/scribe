package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractSections_BasicHeadings(t *testing.T) {
	body := []byte("intro paragraph\n\n# Title\n\nfirst section body\n\n## Subhead\n\nsecond section body\n\n## Another\n\nthird section body\n")
	got := extractSections(body)
	if len(got) != 3 {
		t.Fatalf("want 3 sections, got %d: %+v", len(got), got)
	}
	if got[0].ID != "title" || got[0].Level != 1 {
		t.Errorf("section 0: want id=title level=1, got id=%s level=%d", got[0].ID, got[0].Level)
	}
	if got[1].ID != "subhead" || got[1].Level != 2 {
		t.Errorf("section 1: want id=subhead level=2, got id=%s level=%d", got[1].ID, got[1].Level)
	}
	if got[2].ID != "another" || got[2].Level != 2 {
		t.Errorf("section 2: want id=another level=2, got id=%s level=%d", got[2].ID, got[2].Level)
	}
}

func TestExtractSections_NoHeadings(t *testing.T) {
	body := []byte("just a paragraph\n\nanother paragraph\n")
	got := extractSections(body)
	if got != nil {
		t.Errorf("want nil for no-heading body, got %+v", got)
	}
}

func TestExtractSections_DuplicateTitlesGetSuffix(t *testing.T) {
	body := []byte("# Setup\n\nA\n\n# Setup\n\nB\n\n# Setup\n\nC\n")
	got := extractSections(body)
	if len(got) != 3 {
		t.Fatalf("want 3 sections, got %d", len(got))
	}
	wantIDs := []string{"setup", "setup-2", "setup-3"}
	for i, w := range wantIDs {
		if got[i].ID != w {
			t.Errorf("section %d: want id=%s, got id=%s", i, w, got[i].ID)
		}
	}
}

func TestSectionAnchorSlug(t *testing.T) {
	cases := []struct {
		title, want string
	}{
		{"Methods", "methods"},
		{"Why It Works", "why-it-works"},
		{"Setup & Configuration!", "setup-configuration"},
		{"H1: Big Idea", "h1-big-idea"},
		{"Question?", "question"},
		{"   ", "section"}, // empty after strip
		{"@@@", "section"}, // all-strip becomes empty
	}
	for _, c := range cases {
		if got := sectionAnchorSlug(c.title); got != c.want {
			t.Errorf("slug(%q) = %q, want %q", c.title, got, c.want)
		}
	}
}

func TestExtractSections_LineRanges(t *testing.T) {
	body := []byte("# A\nbody A\n# B\nbody B\n")
	got := extractSections(body)
	if len(got) != 2 {
		t.Fatalf("want 2 sections, got %d", len(got))
	}
	// "# A" is line 1, "# B" is line 3.
	if got[0].Lines != [2]int{1, 2} {
		t.Errorf("section 0 lines: want [1, 2], got %v", got[0].Lines)
	}
	if got[1].Lines != [2]int{3, 4} {
		t.Errorf("section 1 lines: want [3, 4], got %v", got[1].Lines)
	}
}

func TestExtractSections_BodyRangeMatchesContent(t *testing.T) {
	body := []byte("# A\nbody A line\n## B\nbody B line\n")
	got := extractSections(body)
	if len(got) != 2 {
		t.Fatalf("want 2 sections, got %d", len(got))
	}
	// Section A spans from "# A\n" through "body A line\n" before "## B".
	a := string(body[got[0].Bytes[0]:got[0].Bytes[1]])
	if !strings.HasPrefix(a, "# A") || !strings.Contains(a, "body A line") {
		t.Errorf("section A body unexpected: %q", a)
	}
	// Section B spans the remainder.
	b := string(body[got[1].Bytes[0]:got[1].Bytes[1]])
	if !strings.HasPrefix(b, "## B") || !strings.Contains(b, "body B line") {
		t.Errorf("section B body unexpected: %q", b)
	}
}

func TestSectionsSidecarPath_StripsWikiPrefix(t *testing.T) {
	root := "/kb"
	cases := []struct {
		article string
		want    string
	}{
		{"/kb/wiki/research/foo.md", "/kb/wiki/_sections/research/foo.json"},
		{"/kb/research/foo.md", "/kb/wiki/_sections/research/foo.json"},
		{"/kb/solutions/bar.md", "/kb/wiki/_sections/solutions/bar.json"},
	}
	for _, c := range cases {
		if got := sectionsSidecarPath(root, c.article); got != c.want {
			t.Errorf("sidecarPath(%s, %s) = %s, want %s", root, c.article, got, c.want)
		}
	}
}

func TestWriteAndReadSidecar_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	articleDir := filepath.Join(dir, "research")
	if err := os.MkdirAll(articleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	articlePath := filepath.Join(articleDir, "alpha.md")
	body := []byte("# Title\nintro line\n## Methods\nmethods body\n## Results\nresults body\n")
	if err := writeSectionsSidecar(dir, articlePath, body); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readSectionsSidecar(dir, articlePath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got == nil {
		t.Fatal("read returned nil sidecar")
	}
	if len(got.Sections) != 3 {
		t.Errorf("want 3 sections, got %d", len(got.Sections))
	}
	if got.Article != "research/alpha.md" {
		t.Errorf("Article rel path: want research/alpha.md, got %s", got.Article)
	}
	if got.Version != sectionsSidecarVersion {
		t.Errorf("Version: want %d, got %d", sectionsSidecarVersion, got.Version)
	}
}

func TestWriteSidecar_RemovesStaleOnEmptyHeadings(t *testing.T) {
	dir := t.TempDir()
	articlePath := filepath.Join(dir, "research", "no-heads.md")
	if err := os.MkdirAll(filepath.Dir(articlePath), 0o755); err != nil {
		t.Fatal(err)
	}
	// First, write a sidecar from a body with headings.
	if err := writeSectionsSidecar(dir, articlePath, []byte("# H\nbody\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sectionsSidecarPath(dir, articlePath)); err != nil {
		t.Fatalf("expected sidecar to exist: %v", err)
	}
	// Then write again with no headings — the stale sidecar must be cleaned up.
	if err := writeSectionsSidecar(dir, articlePath, []byte("plain body\n")); err != nil {
		t.Fatalf("rewrite empty: %v", err)
	}
	if _, err := os.Stat(sectionsSidecarPath(dir, articlePath)); !os.IsNotExist(err) {
		t.Errorf("expected sidecar removed; stat err=%v", err)
	}
}

func TestReadSidecar_VersionMismatchIsCacheMiss(t *testing.T) {
	dir := t.TempDir()
	articlePath := filepath.Join(dir, "research", "alpha.md")
	out := sectionsSidecarPath(dir, articlePath)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	bogus := SectionsSidecar{Version: 999, Article: "x", Sections: []Section{{ID: "x"}}}
	data, _ := json.Marshal(bogus)
	if err := os.WriteFile(out, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readSectionsSidecar(dir, articlePath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil on version mismatch, got %+v", got)
	}
}

func TestArticleBody_StripsFrontmatter(t *testing.T) {
	in := []byte("---\ntitle: foo\n---\n\n# Heading\nbody\n")
	body := articleBody(in)
	if string(body) != "# Heading\nbody\n" {
		t.Errorf("got %q", string(body))
	}
}

func TestArticleBody_NoFrontmatter(t *testing.T) {
	in := []byte("# Heading\nbody\n")
	body := articleBody(in)
	if string(body) != "# Heading\nbody\n" {
		t.Errorf("got %q", string(body))
	}
}
