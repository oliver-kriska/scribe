package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const splitTitle = "Default Target Stack (Verified Current Versions, 2026-10-09)"

func TestBracketsBalanced(t *testing.T) {
	for in, want := range map[string]bool{
		"":                          true,
		"Plain":                     true,
		"A (b) [c] {d}":             true,
		"A ([b])":                   true,
		"A (b":                      false,
		"2026-10-09)":               false,
		"A (b]":                     false,
		"A )(":                      false,
		splitTitle:                  true,
		"Fuse (Erlang Circuit Brk)": true,
	} {
		if got := bracketsBalanced(in); got != want {
			t.Errorf("bracketsBalanced(%q) = %v, want %v", in, got, want)
		}
	}
}

// numberedEntries returns n distinct balanced entries [[n1]] … [[nN]].
func numberedEntries(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("[[n%d]]", i+1)
	}
	return out
}

// titlesFrom wraps a name predicate as a knownTitles that completes
// nothing; nil stays nil.
func titlesFrom(has func(string) bool) *knownTitles {
	if has == nil {
		return nil
	}
	return &knownTitles{has: has}
}

func TestRepairSplitRelated(t *testing.T) {
	known := func(name string) bool {
		return slices.Contains([]string{"Fuse, Erlang", "Core", "Other"}, name)
	}
	cases := []struct {
		name  string
		in    []string
		known func(string) bool
		want  []string
		notes int
	}{
		{
			name: "real split pair rejoins by brackets",
			in: []string{
				"[[Fuse (Erlang Circuit Breaker)]]", "[[Default Target Stack (Verified Current Versions]]",
				"[[2026-10-09)]]", "[[Core Principle: Why Literal Translation Fails]]",
			},
			want: []string{
				"[[Fuse (Erlang Circuit Breaker)]]", "[[" + splitTitle + "]]",
				"[[Core Principle: Why Literal Translation Fails]]",
			},
			notes: 1,
		},
		{
			name:  "three pieces rejoin",
			in:    []string{"[[A (x]]", "[[y]]", "[[z)]]"},
			want:  []string{"[[A (x, y, z)]]"},
			notes: 1,
		},
		{
			name:  "bare entries rejoin bare",
			in:    []string{"A (x", "y)"},
			want:  []string{"A (x, y)"},
			notes: 1,
		},
		{
			name:  "leftover unbalanced fragment dropped",
			in:    []string{"[[Good]]", "[[2026-10-09)]]"},
			want:  []string{"[[Good]]"},
			notes: 1,
		},
		{
			name:  "known title rejoins without brackets",
			in:    []string{"[[Fuse]]", "[[Erlang]]", "[[Core]]"},
			known: known,
			want:  []string{"[[Fuse, Erlang]]", "[[Core]]"},
			notes: 1,
		},
		{
			name:  "no join when a piece is itself a title",
			in:    []string{"[[Core]]", "[[Other]]"},
			known: func(n string) bool { return n == "Core" || n == "Other" || n == "Core, Other" },
			want:  []string{"[[Core]]", "[[Other]]"},
		},
		{
			name: "clean list untouched",
			in:   []string{"[[A]]", "[[B (c)]]"},
			want: []string{"[[A]]", "[[B (c)]]"},
		},
		{
			// The real case: a dozen short ids inside one parenthesis.
			name: "bracket join spans a long id list",
			in: []string{
				"[[Keep]]", "[[Threads (9bc7]]", "[[620a]]", "[[6221]]", "[[6245]]", "[[6254]]",
				"[[5270]]", "[[529e]]", "[[52a7]]", "[[b5b]]", "[[8ccf]]", "[[8d71]]", "[[8e0f)]]",
			},
			want:  []string{"[[Keep]]", "[[Threads (9bc7, 620a, 6221, 6245, 6254, 5270, 529e, 52a7, b5b, 8ccf, 8d71, 8e0f)]]"},
			notes: 1,
		},
		{
			name:  "title join window is bounded",
			in:    []string{"[[A]]", "[[B]]", "[[C]]", "[[D]]", "[[E]]"},
			known: func(n string) bool { return n == "A, B, C, D, E" },
			want:  []string{"[[A]]", "[[B]]", "[[C]]", "[[D]]", "[[E]]"},
		},
		{
			name:  "bracket join window is bounded",
			in:    append(append([]string{"[[A (1]]"}, numberedEntries(maxBracketJoinParts-1)...), "[[z)]]"),
			want:  numberedEntries(maxBracketJoinParts - 1),
			notes: 2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, notes := repairSplitRelated(c.in, titlesFrom(c.known))
			if !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
			if len(notes) != c.notes {
				t.Errorf("notes = %q, want %d", notes, c.notes)
			}
		})
	}
}

// The normalizer split every [[…]] body on commas, which is how the real
// KB got "[[Default Target Stack (Verified Current Versions]]" + "[[2026-10-09)]]".
func TestNormalizeRelatedFrontmatter_CommaInsideWikilink(t *testing.T) {
	cases := map[string]string{
		"intact link stays one link": "---\ntitle: X\nrelated: [\"[[Fuse (Erlang Circuit Breaker)]]\", \"[[" + splitTitle + "]]\"]\n---\nbody\n",
		"split halves are rejoined":  "---\ntitle: X\nrelated: [\"[[Fuse (Erlang Circuit Breaker)]]\", \"[[Default Target Stack (Verified Current Versions]]\", \"[[2026-10-09)]]\"]\n---\nbody\n",
		"bare list halves rejoined":  "---\ntitle: X\nrelated: [Fuse (Erlang Circuit Breaker), " + splitTitle + "]\n---\nbody\n",
	}
	want := []any{"[[Fuse (Erlang Circuit Breaker)]]", "[[" + splitTitle + "]]"}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got := relatedField(t, normalizeRelatedFrontmatter(in))
			if !slices.Equal(got.([]any), want) {
				t.Errorf("related = %q, want %q", got, want)
			}
		})
	}
}

func TestApplyWikiActions_UpdateFrontmatterRejoinsSplitRelated(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "wiki", "fm.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("---\ntitle: \"Old\"\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	related := []any{"[[Default Target Stack (Verified Current Versions]]", "[[2026-10-09)]]", "[[Other]]"}
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "update_frontmatter", Path: "wiki/fm.md", Frontmatter: map[string]any{"related": related},
	}}}
	if res, err := applyWikiActions(root, env, ApplyOptions{}); err != nil || len(res.Errors) > 0 {
		t.Fatalf("err=%v res=%v", err, res.Errors)
	}
	data, _ := os.ReadFile(target)
	fm, err := parseFrontmatter(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"[[" + splitTitle + "]]", "[[Other]]"}
	if got := toStringSlice(fm.Related); !slices.Equal(got, want) {
		t.Errorf("related = %q, want %q\n%s", got, want, data)
	}
	if len(env.Actions[0].Frontmatter["related"].([]any)) != 3 {
		t.Error("the caller's frontmatter map must not be mutated")
	}
}

func TestDreamLinkReport_WantedSkipsUnbalancedTargets(t *testing.T) {
	three := []linkEdge{{SourcePath: "wiki/1.md"}, {SourcePath: "wiki/2.md"}, {SourcePath: "wiki/3.md"}}
	g := linkGraph{Missing: map[string][]linkEdge{
		"2026-10-09)": three,
		"Default Target Stack (Verified Current Versions": three,
		"Real (Page)": three,
	}}
	got := dreamLinkReport(g, linkReportToday, 60)
	if strings.Contains(got, "2026-10-09)") || strings.Contains(got, "Current Versions]]") {
		t.Errorf("split artifact listed as wanted:\n%s", got)
	}
	if !strings.Contains(got, "- [[Real (Page)]] — 3 articles") {
		t.Errorf("balanced wanted page missing:\n%s", got)
	}
}

// A fragment whose other half is gone is restored from the one title it
// starts, and a repair never leaves the same link twice.
func TestRepairSplitRelated_CompletesOrphanFragment(t *testing.T) {
	set := newTitleSet(map[string]bool{
		"Reflexion (Shinn et al., NeurIPS 2023)":    true,
		"Twin (a, x)":                               true,
		"Twin (a, y)":                               true,
		"Self-Refine (Madaan et al., NeurIPS 2023)": true,
	})
	known := &knownTitles{has: func(n string) bool { return set.names[n] }, complete: set.complete}
	got, notes := repairSplitRelated([]string{
		"[[Self-Refine (Madaan et al.]]", "[[NeurIPS 2023)]]",
		"[[Reflexion (Shinn et al.]]", "[[Twin (a]]",
		"[[Self-Refine (Madaan et al., NeurIPS 2023)]]",
	}, known)
	want := []string{"[[Self-Refine (Madaan et al., NeurIPS 2023)]]", "[[Reflexion (Shinn et al., NeurIPS 2023)]]"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	// rejoin, complete, drop ambiguous "Twin (a", drop duplicate
	if len(notes) != 4 {
		t.Errorf("notes = %q, want 4", notes)
	}
}
