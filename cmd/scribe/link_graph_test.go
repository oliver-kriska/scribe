package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

func resolverTestKB(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("domains: [acme]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCRIBE_KB", root)
	head := func(title, extra string) string {
		return "---\ntitle: \"" + title + "\"\ntype: solution\ndomain: general\n" + extra + "---\n\n"
	}
	writeKBFile(t, root, "wiki/a.md", head("A", "aliases: [Alpha]\n")+"A body.\n")
	writeKBFile(t, root, "wiki/b.md", head("B", "")+"[[alpha]] [[A#x]] [[Ghost]]\n")
	writeKBFile(t, root, "wiki/c.md", head("C", "related: [\"[[B]]\"]\nupdated: 2020-01-01\n")+"[[Alpha]]\n")
	writeKBFile(t, root, "wiki/d.md", head("D", "aliases: [Dee]\n")+"[[d]] [[Dee]]\n")
	writeKBFile(t, root, "wiki/_index.md", "- [[D]]\n- [[alpha]]\n- [[alfa]]\n")
	return root
}

func edgeTitles(edges []linkEdge) []string {
	titles := make([]string, 0, len(edges))
	for _, edge := range edges {
		titles = append(titles, edge.SourceTitle)
	}
	sort.Strings(titles)
	return titles
}

func buildTestGraph(t *testing.T, root string) linkGraph {
	t.Helper()
	g, err := buildLinkGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestLinkGraph_AliasAndCaseCreditCanonical(t *testing.T) {
	g := buildTestGraph(t, resolverTestKB(t))
	if got, want := edgeTitles(g.Inbound["A"]), []string{"B", "C", "wiki/_index.md"}; !slices.Equal(got, want) {
		t.Errorf("Inbound[A] sources = %v, want %v", got, want)
	}
}

func TestLinkGraph_SelfLinkViaAliasIgnored(t *testing.T) {
	g := buildTestGraph(t, resolverTestKB(t))
	want := []linkEdge{{SourcePath: "wiki/_index.md", SourceTitle: "wiki/_index.md", Generated: true}}
	if !slices.Equal(g.Inbound["D"], want) {
		t.Errorf("Inbound[D] = %v, want %v", g.Inbound["D"], want)
	}
	if got := articleByTitle(t, g, "D").Outbound; got != 0 {
		t.Errorf("D Outbound = %d, want 0", got)
	}
}

func articleByTitle(t *testing.T, g linkGraph, title string) graphArticle {
	t.Helper()
	for _, article := range g.Articles {
		if article.Title == title {
			return article
		}
	}
	t.Fatalf("%s missing from Articles", title)
	return graphArticle{}
}

func TestLinkGraph_OutboundCountsDistinctTargets(t *testing.T) {
	g := buildTestGraph(t, resolverTestKB(t))
	if got := articleByTitle(t, g, "B").Outbound; got != 3 {
		t.Errorf("B Outbound = %d, want 3", got)
	}
}

func TestLinkGraph_BrokenAfterExactLinkToSameArticle(t *testing.T) {
	root := resolverTestKB(t)
	writeKBFile(t, root, "wiki/f.md", "---\ntitle: \"F\"\ntype: solution\ndomain: general\n---\n\n[[A]] then [[alpha]]\n")
	g := buildTestGraph(t, root)
	want := brokenLink{Raw: "alpha", Canonical: "A", SourcePath: "wiki/f.md"}
	if !slices.Contains(g.Broken, want) {
		t.Errorf("Broken = %v, missing %v", g.Broken, want)
	}
}

func TestLinkGraph_MissingKeepsRaw(t *testing.T) {
	g := buildTestGraph(t, resolverTestKB(t))
	keys := make([]string, 0, len(g.Missing))
	for key := range g.Missing {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if want := []string{"Ghost", "alfa"}; !slices.Equal(keys, want) {
		t.Errorf("Missing keys = %v, want %v", keys, want)
	}
}

func TestLinkGraph_BrokenSkipsGenerated(t *testing.T) {
	g := buildTestGraph(t, resolverTestKB(t))
	want := []brokenLink{{Raw: "alpha", Canonical: "A", SourcePath: "wiki/b.md"}}
	if !slices.Equal(g.Broken, want) {
		t.Errorf("Broken = %v, want %v", g.Broken, want)
	}
}

func TestLinkGraph_UpdatedUnquotedDate(t *testing.T) {
	g := buildTestGraph(t, resolverTestKB(t))
	for _, article := range g.Articles {
		if article.Title == "C" {
			if article.Updated != "2020-01-01" {
				t.Errorf("C Updated = %q", article.Updated)
			}
			return
		}
	}
	t.Fatal("C missing from Articles")
}

func TestLinkGraph_UnparsedTitleStillResolves(t *testing.T) {
	root := resolverTestKB(t)
	writeKBFile(t, root, "wiki/broken.md", "---\ntitle: \"Broken Yaml\"\ntags: [unclosed\n---\n\nbody\n")
	writeKBFile(t, root, "wiki/e.md", "---\ntitle: \"E\"\ntype: solution\ndomain: general\n---\n\n[[Broken Yaml]]\n")
	g := buildTestGraph(t, root)
	if _, ok := g.Missing["Broken Yaml"]; ok {
		t.Error("Broken Yaml reported missing")
	}
	if g.Unparsed != 1 {
		t.Errorf("Unparsed = %d, want 1", g.Unparsed)
	}
}

func TestLinkGraph_CountsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read chmod 000 directories")
	}
	root := resolverTestKB(t)
	locked := filepath.Join(root, "tools", "locked")
	writeKBFile(t, root, "tools/locked/x.md", "---\ntitle: \"X\"\n---\n")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	g, err := buildLinkGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	if g.Unreadable != 1 {
		t.Errorf("Unreadable = %d, want 1", g.Unreadable)
	}
}

func TestLinkGraph_CodeSpanLinkIsNotBroken(t *testing.T) {
	root := resolverTestKB(t)
	head := "---\ntitle: \"%s\"\ntype: solution\ndomain: general\n---\n\n"
	writeKBFile(t, root, "wiki/code.md", fmt.Sprintf(head, "Title with `code` inside")+"body\n")
	writeKBFile(t, root, "wiki/g.md", fmt.Sprintf(head, "G")+"[[Title with `code` inside]]\n")
	g := buildTestGraph(t, root)
	if got, want := edgeTitles(g.Inbound["Title with `code` inside"]), []string{"G"}; !slices.Equal(got, want) {
		t.Errorf("Inbound sources = %v, want %v", got, want)
	}
	for _, broken := range g.Broken {
		if broken.SourcePath == "wiki/g.md" {
			t.Errorf("code-span link reported broken: %v", broken)
		}
	}
}

func TestLinkGraph_BrokenRawKeepsAnchor(t *testing.T) {
	root := resolverTestKB(t)
	writeKBFile(t, root, "wiki/h.md", "---\ntitle: \"H\"\ntype: solution\ndomain: general\n---\n\n[[alpha#x]]\n")
	g := buildTestGraph(t, root)
	want := brokenLink{Raw: "alpha#x", Canonical: "A", SourcePath: "wiki/h.md"}
	if !slices.Contains(g.Broken, want) {
		t.Errorf("Broken = %v, missing %v", g.Broken, want)
	}
}

func TestBacklinks_KeysAreCanonical(t *testing.T) {
	resolverTestKB(t)
	var err error
	out := captureLintStdout(t, func() { err = (&BacklinksCmd{JSON: true}).Run() })
	if err != nil {
		t.Fatal(err)
	}
	var got map[string][]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(got))
	for key := range got {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if want := []string{"A", "B", "D", "Ghost", "alfa"}; !slices.Equal(keys, want) {
		t.Errorf("keys = %v, want %v", keys, want)
	}
	for key, want := range map[string][]string{
		"A": {"B", "C", "wiki/_index.md"},
		"B": {"C"},
		"D": {"wiki/_index.md"},
	} {
		if !slices.Equal(got[key], want) {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
}
