package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestScoreHostsByRelated exercises the second-pass shared-neighbor scorer
// used when the primary tag/domain/term scorer fails to reach MinScore.
// The function is pure: (orphan, articles, minShared) -> []linkCandidate,
// so a table of in-memory articles is sufficient.
func TestScoreHostsByRelated(t *testing.T) {
	orphan := &article{
		Path:     "solutions/orphan.md",
		Title:    "Orphan",
		Outbound: []string{"A", "B", "C"},
	}
	strong := &article{Path: "solutions/strong.md", Title: "Strong", Outbound: []string{"A", "B", "C", "D"}}
	medium := &article{Path: "solutions/medium.md", Title: "Medium", Outbound: []string{"A", "B", "Z"}}
	weak := &article{Path: "solutions/weak.md", Title: "Weak", Outbound: []string{"A", "Y", "Z"}}
	unrelated := &article{Path: "solutions/unrelated.md", Title: "Unrelated", Outbound: []string{"X", "Y", "Z"}}
	rolling := &article{Path: "projects/x/learnings.md", Title: "Rolling", Outbound: []string{"A", "B", "C"}, Rolling: true}
	self := orphan

	articles := []*article{strong, medium, weak, unrelated, rolling, self}

	t.Run("minShared=2 filters out single-overlap hosts and drops rolling/self", func(t *testing.T) {
		got := scoreHostsByRelated(orphan, articles, 2)
		if len(got) != 2 {
			t.Fatalf("expected 2 candidates, got %d", len(got))
		}
		if got[0].Host != strong {
			t.Errorf("expected strongest candidate first, got %q", got[0].Host.Title)
		}
		if got[0].Score != 9 {
			t.Errorf("strong score = %v, want 9", got[0].Score)
		}
		if got[1].Host != medium {
			t.Errorf("expected medium second, got %q", got[1].Host.Title)
		}
		if got[1].Score != 6 {
			t.Errorf("medium score = %v, want 6", got[1].Score)
		}
	})

	t.Run("minShared=1 includes the weakly-related host", func(t *testing.T) {
		got := scoreHostsByRelated(orphan, articles, 1)
		if len(got) != 3 {
			t.Fatalf("expected 3 candidates with minShared=1, got %d", len(got))
		}
		last := got[len(got)-1]
		if last.Host != weak || last.Score != 3 {
			t.Errorf("expected weak last with score 3, got %q / %v", last.Host.Title, last.Score)
		}
	})

	t.Run("orphan with no outbound links returns nil", func(t *testing.T) {
		isolated := &article{Path: "solutions/isolated.md", Title: "Isolated"}
		got := scoreHostsByRelated(isolated, articles, 1)
		if got != nil {
			t.Errorf("expected nil for empty outbound, got %+v", got)
		}
	})

	t.Run("rolling files are never candidates", func(t *testing.T) {
		got := scoreHostsByRelated(orphan, []*article{rolling}, 1)
		if got != nil {
			t.Errorf("rolling file was scored as candidate: %+v", got)
		}
	})
}

// TestScoreHostsPrimary exercises the primary tag/domain/term scorer that
// produces the See Also suggestions. Bugs here silently route orphans to
// the wrong hosts, so the weighting rules (3/tag, 5/same domain, 2/term
// hit, 4/overview bonus) are worth locking in.
func TestScoreHostsPrimary(t *testing.T) {
	// Orphan about "ecto sandbox testing" tagged [ecto, testing] in domain acme.
	orphan := &article{
		Path:   "solutions/orphan.md",
		Title:  "Ecto Sandbox Testing",
		Domain: "acme",
		Tags:   []string{"ecto", "testing"},
	}

	overview := &article{
		Path: "projects/acme/overview.md", Title: "Acme",
		Domain: "acme", Tags: []string{"acme"},
		Body: "ecto sandbox testing is covered in the lease review module",
	}
	tagMatch := &article{
		Path: "solutions/another.md", Title: "Another",
		Domain: "general", Tags: []string{"ecto", "testing", "phoenix"},
		Body: "unrelated body",
	}
	domainOnly := &article{
		Path: "solutions/domain.md", Title: "Domain Only",
		Domain: "acme", Tags: []string{"unrelated"},
		Body: "prose",
	}
	general := &article{
		Path: "solutions/general.md", Title: "General",
		Domain: "general", Tags: []string{"other"},
		Body: "prose",
	}
	selfMatch := orphan
	rolling := &article{
		Path: "projects/acme/learnings.md", Title: "Learnings",
		Domain: "acme", Tags: []string{"ecto", "testing"},
		Rolling: true,
	}

	articles := []*article{overview, tagMatch, domainOnly, general, selfMatch, rolling}

	t.Run("tag overlap contributes 3 per shared tag", func(t *testing.T) {
		got := scoreHostsPrimaryScore(t, orphan, []*article{tagMatch}, 1)
		// 2 shared tags * 3 = 6 (no domain match, title terms < 4 chars so no hits)
		assertScore(t, got, "Another", 6)
	})

	t.Run("same non-general domain contributes 5", func(t *testing.T) {
		got := scoreHostsPrimaryScore(t, orphan, []*article{domainOnly}, 1)
		assertScore(t, got, "Domain Only", 5)
	})

	t.Run("general domain does not trigger same-domain bonus", func(t *testing.T) {
		got := scoreHosts(orphan, []*article{general}, 1)
		if got != nil {
			t.Errorf("general-domain host scored above threshold: %+v", got)
		}
	})

	t.Run("project overview bonus stacks with domain and title terms", func(t *testing.T) {
		got := scoreHosts(orphan, []*article{overview}, 1)
		if len(got) != 1 {
			t.Fatalf("expected 1 candidate, got %d", len(got))
		}
		// Breakdown:
		//   tag overlap: 0 (orphan has [ecto, testing], overview has [acme])
		//   same domain (acme): +5
		//   title-term hits: "ecto", "sandbox", "testing" appear in body => 3 hits * 2 = 6
		//   overview bonus: +4
		//   total: 15
		if got[0].Score != 15 {
			t.Errorf("overview composite score = %v, want 15", got[0].Score)
		}
		reasons := strings.Join(got[0].Reasons, "|")
		for _, want := range []string{"same domain", "title terms", "project overview"} {
			if !strings.Contains(reasons, want) {
				t.Errorf("reasons missing %q: %s", want, reasons)
			}
		}
	})

	t.Run("self-match is skipped", func(t *testing.T) {
		got := scoreHosts(orphan, []*article{selfMatch}, 1)
		if got != nil {
			t.Errorf("self-match returned as candidate: %+v", got)
		}
	})

	t.Run("rolling files are never scored", func(t *testing.T) {
		got := scoreHosts(orphan, []*article{rolling}, 1)
		if got != nil {
			t.Errorf("rolling file scored as candidate: %+v", got)
		}
	})

	t.Run("candidates sorted descending and all articles mixed", func(t *testing.T) {
		got := scoreHosts(orphan, articles, 1)
		if len(got) < 2 {
			t.Fatalf("expected multiple candidates, got %d", len(got))
		}
		for i := 1; i < len(got); i++ {
			if got[i-1].Score < got[i].Score {
				t.Errorf("candidates not sorted descending: %v", got)
			}
		}
	})

	t.Run("minScore filter drops low candidates", func(t *testing.T) {
		got := scoreHosts(orphan, []*article{tagMatch}, 99)
		if got != nil {
			t.Errorf("tagMatch kept despite minScore=99: %+v", got)
		}
	})
}

// scoreHostsPrimaryScore is a small helper that wraps scoreHosts and
// fails the test if the expected single result isn't there, to keep the
// table tests concise.
func scoreHostsPrimaryScore(t *testing.T, orphan *article, articles []*article, minScore int) []linkCandidate {
	t.Helper()
	return scoreHosts(orphan, articles, minScore)
}

func assertScore(t *testing.T, got []linkCandidate, title string, want float64) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(got))
	}
	if got[0].Host.Title != title {
		t.Errorf("title = %q, want %q", got[0].Host.Title, title)
	}
	if got[0].Score != want {
		t.Errorf("score = %v, want %v", got[0].Score, want)
	}
}

// TestTokenize covers the 4-char minimum, lowercase normalization,
// stopword filtering, and punctuation trimming. Bugs here silently
// inflate or deflate title-term hit counts in scoreHosts.
func TestTokenize(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Ecto Sandbox Testing", []string{"ecto", "sandbox", "testing"}},
		{"a an the", nil}, // all stopwords + < 4 chars
		{"Phoenix, LiveView.", []string{"phoenix", "liveview"}}, // punctuation trimmed
		{"BIG SHOUT", []string{"shout"}},                        // "big" is < 4 chars
		{"", nil},
		{"one-two three-four", []string{"one-two", "three-four"}}, // hyphens kept inside words
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := tokenize(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("tokenize(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestIntersectCount(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want int
	}{
		{"both empty", nil, nil, 0},
		{"no overlap", []string{"a", "b"}, []string{"c", "d"}, 0},
		{"full overlap", []string{"a", "b"}, []string{"a", "b"}, 2},
		{"case-insensitive", []string{"Ecto", "TESTING"}, []string{"ecto", "testing"}, 2},
		{"whitespace trimmed", []string{" ecto "}, []string{"ecto"}, 1},
		{"partial", []string{"a", "b", "c"}, []string{"b", "d"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := intersectCount(tc.a, tc.b); got != tc.want {
				t.Errorf("intersectCount(%v, %v) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestToStringSlice documents the YAML coercion rules. Frontmatter fields
// can be [a,b] (→ []string), ["a","b"] (→ []any of string), "a" (→ single-
// element), or missing (→ nil). Bugs here break tag/related parsing.
func TestToStringSlice(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string
	}{
		{"nil", nil, nil},
		{"already []string", []string{"a", "b"}, []string{"a", "b"}},
		{"[]any of strings (common yaml.v3 shape)", []any{"a", "b"}, []string{"a", "b"}},
		{"[]any with non-string is filtered", []any{"a", 42, "b"}, []string{"a", "b"}},
		{"single string promoted", "solo", []string{"solo"}},
		{"int returns nil", 42, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toStringSlice(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("toStringSlice(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestAppendSeeAlso exercises the three cases of See Also injection:
// no existing section (append at EOF), existing section (insert at end of
// list), and idempotency (skip if link already present). Uses tmp files.
func TestAppendSeeAlso(t *testing.T) {
	dir := t.TempDir()

	t.Run("creates section when missing", func(t *testing.T) {
		p := filepath.Join(dir, "no-section.md")
		mustWrite(t, p, "# Title\n\nBody text.\n")
		if err := appendSeeAlso(p, "Orphan One"); err != nil {
			t.Fatal(err)
		}
		got := mustRead(t, p)
		if !strings.Contains(got, "## See Also\n") {
			t.Errorf("section not created:\n%s", got)
		}
		if !strings.Contains(got, "- [[Orphan One]]") {
			t.Errorf("link not appended:\n%s", got)
		}
	})

	t.Run("appends to existing section", func(t *testing.T) {
		p := filepath.Join(dir, "has-section.md")
		mustWrite(t, p, "# Title\n\nBody.\n\n## See Also\n\n- [[Existing]]\n")
		if err := appendSeeAlso(p, "New Orphan"); err != nil {
			t.Fatal(err)
		}
		got := mustRead(t, p)
		if !strings.Contains(got, "- [[Existing]]") {
			t.Errorf("existing link dropped:\n%s", got)
		}
		if !strings.Contains(got, "- [[New Orphan]]") {
			t.Errorf("new link not added:\n%s", got)
		}
	})

	t.Run("idempotent when link already present", func(t *testing.T) {
		p := filepath.Join(dir, "already.md")
		initial := "# Title\n\nBody mentions [[Repeat]] inline.\n"
		mustWrite(t, p, initial)
		if err := appendSeeAlso(p, "Repeat"); err != nil {
			t.Fatal(err)
		}
		got := mustRead(t, p)
		if got != initial {
			t.Errorf("file changed on idempotent append:\nbefore: %q\nafter:  %q", initial, got)
		}
	})

	t.Run("inserts before trailing section when See Also is in the middle", func(t *testing.T) {
		p := filepath.Join(dir, "middle.md")
		mustWrite(t, p, "# Title\n\n## See Also\n\n- [[Old]]\n\n## Footer\n\nfooter body\n")
		if err := appendSeeAlso(p, "Middle"); err != nil {
			t.Fatal(err)
		}
		got := mustRead(t, p)
		// Both old and new links must sit above Footer.
		iMiddle := strings.Index(got, "[[Middle]]")
		iFooter := strings.Index(got, "## Footer")
		if iMiddle < 0 || iFooter < 0 || iMiddle > iFooter {
			t.Errorf("new link inserted after Footer:\n%s", got)
		}
	})
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
