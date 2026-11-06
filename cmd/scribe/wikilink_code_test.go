package main

import (
	"slices"
	"testing"
)

func TestExtractWikilinks_CodeSpans(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"code inside link text is kept", "See [[The `foo` flag]] now.", []string{"The `foo` flag"}},
		{"double-tick code inside link", "See [[A ``b`` c]].", []string{"A ``b`` c"}},
		{"code inside link with label", "[[The `foo` flag|flag]]", []string{"The `foo` flag"}},
		{"link inside code is ignored", "inline `[[Hidden]]` then [[Visible]]", []string{"Visible"}},
		{"double span hides link", "``double `tick` span [[hidden]]`` then [[Visible]]", []string{"Visible"}},
		{"code between links is dropped", "[[A]] `x` [[B]]", []string{"A", "B"}},
		{"fenced block ignored", "```\n[[InFence `x`]]\n```\n[[Out `y`]]\n", []string{"Out `y`"}},
		{"stray opener before a link", "`[[` [[Foo `x`]]", []string{"Foo `x`"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractWikilinks([]byte(c.in)); !slices.Equal(got, c.want) {
				t.Errorf("extractWikilinks(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// A link whose title contains inline code used to reach promote's
// missing-link check as "The  flag" and was reported missing.
func TestMissingWikilinksIn_TitleWithCode(t *testing.T) {
	target := t.TempDir()
	writeKBFile(t, target, "wiki/flag.md", "---\ntitle: \"The `foo` flag\"\n---\n\nbody\n")
	if missing := missingWikilinksIn(target, []byte("See [[The `foo` flag]].\n")); len(missing) != 0 {
		t.Errorf("missing = %v, want none", missing)
	}
}

// The link graph credits a code-span title exactly: inbound edge, no broken
// entry, and the orphan linker sees the verbatim outbound target.
func TestLinkGraph_TitleWithCodeIsExact(t *testing.T) {
	root := t.TempDir()
	head := func(title string) string {
		return "---\ntitle: \"" + title + "\"\ntype: solution\ndomain: general\n---\n\n"
	}
	writeKBFile(t, root, "wiki/flag.md", head("The `foo` flag")+"body\n")
	writeKBFile(t, root, "wiki/user.md", head("User")+"Uses [[The `foo` flag]].\n")
	g := buildTestGraph(t, root)
	if got := edgeTitles(g.Inbound["The `foo` flag"]); !slices.Equal(got, []string{"User"}) {
		t.Errorf("Inbound = %v, want [User]", got)
	}
	if len(g.Broken) != 0 || len(g.Missing) != 0 {
		t.Errorf("Broken = %v, Missing = %v, want none", g.Broken, g.Missing)
	}
	articles, err := loadArticles(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range articles {
		if a.Title == "User" && !slices.Contains(a.Outbound, "The `foo` flag") {
			t.Errorf("linker outbound = %q, want the verbatim title", a.Outbound)
		}
	}
}
