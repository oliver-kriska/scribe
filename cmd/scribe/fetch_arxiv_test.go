package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestArxivIDFromURL(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"https://arxiv.org/abs/2605.00424", "2605.00424"},
		{"https://arxiv.org/abs/2605.00424v1", "2605.00424"},
		{"https://arxiv.org/abs/2605.00424v3", "2605.00424"},
		{"https://arxiv.org/pdf/2605.00424", "2605.00424"},
		{"https://arxiv.org/pdf/2605.00424.pdf", "2605.00424"},
		{"https://arxiv.org/pdf/2605.00424v2", "2605.00424"},
		{"https://arxiv.org/pdf/2604.24594", "2604.24594"},
		{"https://arxiv.org/html/2605.00424v1", "2605.00424"},
		{"https://arxiv.org/abs/hep-th/0608109", "hep-th/0608109"},
		{"https://arxiv.org/abs/hep-th/0608109v2", "hep-th/0608109"},
		{"https://arxiv.org/pdf/hep-th/0608109", "hep-th/0608109"},
		{"https://arxiv.org/abs/2605.00424?context=cs.AI", "2605.00424"},
		{"https://www.arxiv.org/abs/2605.00424", "2605.00424"},
		// negatives
		{"https://example.com/abs/2605.00424", ""},
		{"https://arxiv.org/about", ""},
		{"https://arxiv.org/", ""},
	}

	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			u, err := url.Parse(c.raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got := arxivIDFromURL(u)
			if got != c.want {
				t.Errorf("arxivIDFromURL(%s) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

func TestIsArxivURL(t *testing.T) {
	cases := map[string]bool{
		"https://arxiv.org/abs/2605.00424":           true,
		"https://www.arxiv.org/pdf/2605.00424":       true,
		"https://export.arxiv.org/api/query":         true,
		"https://x.com/foo/status/123":               false,
		"https://arxiv-vanity.com/papers/2605.00424": false,
	}
	for raw, want := range cases {
		u, _ := url.Parse(raw)
		if got := isArxivURL(u); got != want {
			t.Errorf("isArxivURL(%s) = %v, want %v", raw, got, want)
		}
	}
}

func TestStripFirstH1(t *testing.T) {
	in := "# Title\n\nFirst paragraph.\n\n# Section\n\nMore text.\n"
	want := "First paragraph.\n\n# Section\n\nMore text.\n"
	if got := stripFirstH1(in); got != want {
		t.Errorf("stripFirstH1 mismatch\ngot:  %q\nwant: %q", got, want)
	}
}

func TestCollapseWhitespace(t *testing.T) {
	in := "Foo:\n  Bar  \tBaz\n\nQux"
	want := "Foo: Bar Baz Qux"
	if got := collapseWhitespace(in); got != want {
		t.Errorf("collapseWhitespace = %q, want %q", got, want)
	}
}

func TestAssembleArxivResultPreamble(t *testing.T) {
	meta := arxivMeta{
		ID:        "2605.00424",
		Title:     "A Sample Paper",
		Authors:   []string{"Alice Researcher", "Bob Coauthor"},
		Abstract:  "We propose a thing and it works.",
		Published: "2026-04-01",
	}
	res := assembleArxivResult("https://arxiv.org/abs/2605.00424", meta.ID, meta, "# A Sample Paper\n\nIntro text.\n", "arxiv-html")
	if !strings.HasPrefix(res.Body, "# A Sample Paper\n") {
		t.Errorf("preamble missing title heading: %q", res.Body[:80])
	}
	if !strings.Contains(res.Body, "Alice Researcher, Bob Coauthor") {
		t.Errorf("authors missing")
	}
	if !strings.Contains(res.Body, "## Abstract") {
		t.Errorf("abstract section missing")
	}
	if !strings.Contains(res.Body, "## Full text") {
		t.Errorf("full text section missing")
	}
	// duplicated title heading from body should be stripped
	if strings.Count(res.Body, "# A Sample Paper") != 1 {
		t.Errorf("duplicated title heading not stripped\n%s", res.Body)
	}
}
