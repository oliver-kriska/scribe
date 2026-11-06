package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRepairRelatedLines(t *testing.T) {
	split := `"[[Default Target Stack (Verified Current Versions]]", "[[2026-10-09)]]"`
	joined := `"[[` + splitTitle + `]]"`
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "flow list keeps neighbors byte-for-byte",
			in:   []string{"title: X", `related: ["[[A]]", ` + split + `, "[[B]]"]`, "sources: []"},
			want: []string{"title: X", `related: ["[[A]]", ` + joined + `, "[[B]]"]`, "sources: []"},
		},
		{
			name: "block list keeps its indent",
			in: []string{
				"title: X", "related:", `    - "[[Default Target Stack (Verified Current Versions]]"`,
				`    - "[[2026-10-09)]]"`, "tags: []",
			},
			want: []string{"title: X", "related:", "    - " + joined, "tags: []"},
		},
		{
			name: "multi-line flow collapses to one line",
			in:   []string{"related: [", `  "[[A (x]]",`, `  "[[y)]]"`, "]", "tags: []"},
			want: []string{`related: ["[[A (x, y)]]"]`, "tags: []"},
		},
		{
			name: "only fragment left becomes empty list",
			in:   []string{"related:", `  - "[[2026-10-09)]]"`},
			want: []string{"related: []"},
		},
		{
			name: "CRLF preserved",
			in:   []string{"related: [" + split + "]\r", "tags: []\r"},
			want: []string{"related: [" + joined + "]\r", "tags: []\r"},
		},
		{
			name: "clean list untouched",
			in:   []string{`related: ['[[A]]', "[[B (c)]]"]`},
			want: []string{`related: ['[[A]]', "[[B (c)]]"]`},
		},
		{
			name: "scalar untouched",
			in:   []string{"related: A (b"},
			want: []string{"related: A (b"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := repairRelatedLines(slices.Clone(c.in), nil)
			if !slices.Equal(got, c.want) {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

func splitRelatedArticle(title, related string) string {
	return "---\ntitle: \"" + title + "\"\ntype: solution\ncreated: 2026-04-10\nupdated: 2026-04-10\n" +
		"domain: general\nconfidence: high\ntags: [tag1]\nrelated: " + related +
		"\nsources: [\"s\"]\nauthority: contextual\nindex_tier: standard\n---\n\nBody.\n"
}

func TestLintSplitRelated_ReportAndFix(t *testing.T) {
	root := lintTestKB(t)
	writeKBFile(t, root, "wiki/fuse.md", splitRelatedArticle("Fuse, Erlang", "[]"))
	writeKBFile(t, root, "wiki/stack.md", splitRelatedArticle(splitTitle, "[]"))
	brackets := splitRelatedArticle("Brackets",
		`["[[Fuse, Erlang]]", "[[Default Target Stack (Verified Current Versions]]", "[[2026-10-09)]]"]`)
	writeKBFile(t, root, "wiki/brackets.md", brackets)
	writeKBFile(t, root, "wiki/titles.md", splitRelatedArticle("Titles", `["[[Fuse]]", "[[Erlang]]"]`))
	clean := filepath.Join(root, "wiki", "fuse.md")
	files := []string{
		filepath.Join(root, "wiki", "brackets.md"), filepath.Join(root, "wiki", "titles.md"), clean,
	}

	out := captureLintStdout(t, func() {
		rep := newLintReport(os.Stdout, true, false)
		lintFrontmatter(rep, root, files)
	})
	for _, want := range []string{
		"wiki/brackets.md: rejoined split related entries",
		"wiki/titles.md: rejoined split related entries [[Fuse]] + [[Erlang]] → [[Fuse, Erlang]]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lint output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "wiki/fuse.md") {
		t.Errorf("clean file flagged:\n%s", out)
	}

	captureLintStdout(t, func() {
		if _, _, err := runLintFix(root, files, false); err != nil {
			t.Fatal(err)
		}
	})
	got, _ := os.ReadFile(files[0])
	want := strings.Replace(brackets, `"[[Default Target Stack (Verified Current Versions]]", "[[2026-10-09)]]"`,
		`"[[`+splitTitle+`]]"`, 1)
	if string(got) != want {
		t.Errorf("fixed file:\n%s\nwant:\n%s", got, want)
	}
	got, _ = os.ReadFile(files[1])
	if !strings.Contains(string(got), "\nrelated: [\"[[Fuse, Erlang]]\"]\n") {
		t.Errorf("title-join not applied:\n%s", got)
	}
	for _, f := range files {
		data, _ := os.ReadFile(f)
		if notes := splitRelatedFindings(data, kbTitleLookup(root)); len(notes) > 0 {
			t.Errorf("%s still reports %q after --fix", f, notes)
		}
	}
}

// The fast path must agree with the YAML parse whenever it answers.
func TestPlainFrontmatterTitle_AgreesWithParser(t *testing.T) {
	titles := []string{
		`Go Modules`,
		`"Default Target Stack (Verified Current Versions, 2026-10-09)"`,
		`Notes in CLAUDE.md`,
		`C#`,
		`"Say \"hi\""`,
		`'It''s'`,
		`Foo # comment`,
		`Foo: Bar`,
		`- dash`,
		`[flow]`,
		`null`,
		`~`,
		`2026`,
		`"unterminated`,
		``,
	}
	for _, title := range titles {
		content := "---\ntitle: " + title + "\ntype: tool\n---\nbody\n"
		got, ok := plainFrontmatterTitle([]byte(content))
		if !ok {
			continue
		}
		fm, err := parseFrontmatter([]byte(content))
		if err != nil {
			t.Errorf("title %q: fast path answered %q but YAML fails: %v", title, got, err)
			continue
		}
		if got != fm.Title {
			t.Errorf("title %q: fast path %q, YAML %q", title, got, fm.Title)
		}
	}
	for _, content := range []string{
		"---\ntitle: A\naliases: [B]\n---\n",
		"---\ntitle: A\ntitle: B\n---\n",
		"no frontmatter\n",
	} {
		if got, ok := plainFrontmatterTitle([]byte(content)); ok {
			t.Errorf("%q: fast path answered %q, want fallback", content, got)
		}
	}
	if got, ok := plainFrontmatterTitle([]byte("---\ntitle: Go Modules\n---\n")); !ok || got != "Go Modules" {
		t.Errorf("plain title = %q, %v", got, ok)
	}
}
