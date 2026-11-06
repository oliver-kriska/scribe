package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lintDecisionArticle renders a decision that passes validateFile, with
// extra frontmatter lines appended.
func lintDecisionArticle(title string, extra ...string) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString("title: \"" + title + "\"\n")
	sb.WriteString("type: decision\ncreated: 2026-04-10\nupdated: 2026-04-10\ndomain: general\n")
	sb.WriteString("confidence: high\ntags: [t]\nrelated: []\nsources: [s]\nindex_tier: standard\nauthority: contextual\n")
	for _, e := range extra {
		sb.WriteString(e + "\n")
	}
	sb.WriteString("---\n\n")
	sb.WriteString(strings.Repeat("Body line.\n", 20))
	return sb.String()
}

// lintFrontmatterClasses runs Phase 1 over rels and returns the warning
// counts per class plus the verbose output.
func lintFrontmatterClasses(t *testing.T, root string, rels ...string) (map[string]int, string) {
	t.Helper()
	paths := make([]string, len(rels))
	for i, rel := range rels {
		paths[i] = filepath.Join(root, rel)
		if errs := validateFile(root, paths[i]); len(errs) > 0 {
			t.Fatalf("%s: fixture does not validate: %v", rel, errs)
		}
	}
	var counts map[string]int
	out := captureLintStdout(t, func() {
		rep := newLintReport(os.Stdout, true, false)
		lintFrontmatter(rep, root, paths)
		counts = rep.classCounts
	})
	return counts, out
}

func TestLintWarnsOnNonCanonicalSupersededSpelling(t *testing.T) {
	root := lintTestKB(t)
	writeKBFile(t, root, "solutions/cap.md", lintValidArticle("Cap", 20, "authority: contextual", "status: Superseded"))
	writeKBFile(t, root, "solutions/canonical.md", lintValidArticle("Canonical", 20, "authority: contextual", "status: superseded"))
	writeKBFile(t, root, "solutions/live.md", lintValidArticle("Live", 20, "authority: contextual"))

	counts, out := lintFrontmatterClasses(t, root, "solutions/cap.md", "solutions/canonical.md", "solutions/live.md")
	if counts[lintClassSupersededSpelling] != 1 {
		t.Fatalf("spelling warnings = %d, want 1 (only the capitalized one)\n%s", counts[lintClassSupersededSpelling], out)
	}
	if !strings.Contains(out, "cap.md") || !strings.Contains(out, "`status: superseded`") {
		t.Errorf("warning should name the file and the canonical spelling:\n%s", out)
	}
	if lintReviewGuidance[lintClassSupersededSpelling] == "" {
		t.Error("spelling class has no review guidance, so the footer would not mention it")
	}
}

func TestLintWarnsOnSupersededByWithoutSupersededStatus(t *testing.T) {
	root := lintTestKB(t)
	writeKBFile(t, root, "wiki/live.md", lintDecisionArticle("Live Decision", "status: decided", `superseded_by: ["[[Newer Decision]]"]`))
	writeKBFile(t, root, "wiki/nested.md", lintDecisionArticle("Nested Decision", "status: decided", "superseded_by: [[Newer Decision]]"))
	writeKBFile(t, root, "wiki/marked.md", lintDecisionArticle("Marked Decision", "status: superseded", `superseded_by: ["[[Newer Decision]]"]`))
	writeKBFile(t, root, "wiki/empty.md", lintDecisionArticle("Empty Decision", "status: decided", "superseded_by: []"))

	counts, out := lintFrontmatterClasses(t, root, "wiki/live.md", "wiki/nested.md", "wiki/marked.md", "wiki/empty.md")
	if counts[lintClassSupersededByLive] != 2 {
		t.Fatalf("superseded_by warnings = %d, want 2 (live.md and nested.md)\n%s", counts[lintClassSupersededByLive], out)
	}
	for _, want := range []string{"live.md", "nested.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning should name %s:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"marked.md", "empty.md"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("%s should not be flagged:\n%s", unwanted, out)
		}
	}
	// Lint only reports: the file must be unchanged.
	data, err := os.ReadFile(filepath.Join(root, "wiki", "live.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "status: decided") {
		t.Errorf("lint rewrote the article:\n%s", data)
	}
}
