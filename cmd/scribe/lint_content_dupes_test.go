package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// toks builds a slice of distinct ≥4-char alnum tokens in a namespace, so
// different "topics" share no vocabulary by construction.
func toks(prefix string, lo, hi int) string {
	var s []string
	for i := lo; i <= hi; i++ {
		s = append(s, fmt.Sprintf("%s%04d", prefix, i))
	}
	return strings.Join(s, " ")
}

func writeArticle(t *testing.T, dir, name, frontmatter, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\n" + frontmatter + "---\n\n# Heading\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindExactContentDuplicates(t *testing.T) {
	root := t.TempDir()
	wiki := filepath.Join(root, "wiki")
	body := toks("alpha", 1, 60)
	// Same body, different frontmatter → still an exact content duplicate.
	writeArticle(t, wiki, "canonical.md", "title: Canonical\ntype: research\n", body)
	writeArticle(t, wiki, "copy.md", "title: Copy\ntype: research\nsources: [x]\n", body)
	// A rolling aggregation file with the same body must be EXCLUDED.
	writeArticle(t, wiki, "learnings.md", "title: L\nrolling: true\n", body)
	// An unrelated article.
	writeArticle(t, wiki, "other.md", "title: Other\ntype: research\n", toks("beta", 1, 60))

	docs := collectContentDocs(root)
	// canonical, copy, other = 3 (learnings.md excluded as aggregation).
	if len(docs) != 3 {
		t.Fatalf("collected %d docs, want 3 (aggregation excluded): %+v", len(docs), docs)
	}
	groups := findExactContentDuplicates(docs)
	if len(groups) != 1 {
		t.Fatalf("got %d exact groups, want 1: %v", len(groups), groups)
	}
	if len(groups[0]) != 2 {
		t.Errorf("exact group size = %d, want 2 (canonical+copy, not the aggregation file)", len(groups[0]))
	}
}

func TestFindNearDuplicates(t *testing.T) {
	root := t.TempDir()
	wiki := filepath.Join(root, "wiki")

	// Near-identical pair (jaccard high).
	writeArticle(t, wiki, "ni-a.md", "title: A\ntype: research\n", toks("topicni", 1, 100))
	writeArticle(t, wiki, "ni-b.md", "title: B\ntype: research\n", toks("topicni", 1, 90))

	// Fragment pair: small doc largely contained in a comparable-size larger
	// one (overlap high, jaccard mid, size ratio < 4).
	writeArticle(t, wiki, "frag-full.md", "title: F\ntype: research\n", toks("frag", 1, 120))
	writeArticle(t, wiki, "frag-stub.md", "title: S\ntype: research\n", toks("frag", 1, 50)+" "+toks("fragx", 1, 11))

	// Size-asymmetric noise: stub's tokens ⊂ huge doc, but ratio ≫ 4 and
	// jaccard tiny → must NOT be flagged.
	writeArticle(t, wiki, "noise-small.md", "title: N\ntype: research\n", toks("noise", 1, 45))
	writeArticle(t, wiki, "noise-big.md", "title: NB\ntype: research\n", toks("noise", 1, 45)+" "+toks("noisex", 1, 400))

	// Wholly distinct → never flagged.
	writeArticle(t, wiki, "distinct.md", "title: D\ntype: research\n", toks("unique", 1, 80))

	docs := collectContentDocs(root)
	pairs := findNearDuplicates(docs, dupDefaultOverlap)

	flagged := map[string]bool{}
	for _, p := range pairs {
		flagged[p.A+"|"+p.B] = true
	}
	has := func(a, b string) bool { return flagged["wiki/"+a+"|wiki/"+b] || flagged["wiki/"+b+"|wiki/"+a] }

	if !has("ni-a.md", "ni-b.md") {
		t.Error("near-identical pair (ni-a, ni-b) not flagged")
	}
	if !has("frag-full.md", "frag-stub.md") {
		t.Error("fragment pair (frag-full, frag-stub) not flagged")
	}
	if has("noise-small.md", "noise-big.md") {
		t.Error("size-asymmetric noise pair was wrongly flagged")
	}
	for _, p := range pairs {
		if strings.Contains(p.A, "distinct") || strings.Contains(p.B, "distinct") {
			t.Errorf("wholly-distinct article was flagged: %+v", p)
		}
	}
}

func TestNormalizeForDedup_StripsMachinery(t *testing.T) {
	content := []byte("---\ntitle: X\n---\n\n# Heading\n\n<!-- scribe:marker -->\n```go\ncode\n```\nReal Content Here.\n")
	body := stripFrontmatterBody(content)
	norm := normalizeForDedup(body)
	if strings.Contains(norm, "scribe:marker") {
		t.Errorf("HTML comment not stripped: %q", norm)
	}
	if !strings.Contains(norm, "real content here") {
		t.Errorf("real content lost or not lowercased: %q", norm)
	}
}
