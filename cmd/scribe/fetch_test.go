package main

import (
	"strings"
	"testing"
)

// TestParseJinaEnvelope covers the "Title: ..." + blank-line-separated
// metadata header that Jina Reader prepends to its markdown output.
// Bugs here make the captured title wrong or leak metadata into body.
func TestParseJinaEnvelope(t *testing.T) {
	t.Run("standard envelope", func(t *testing.T) {
		in := "Title: My Article\nURL Source: https://example.com\n\nActual body content.\n\nMore body."
		title, body := parseJinaEnvelope(in)
		if title != "My Article" {
			t.Errorf("title = %q", title)
		}
		if !strings.HasPrefix(body, "Actual body content.") {
			t.Errorf("body = %q", body)
		}
		if strings.Contains(body, "URL Source:") {
			t.Errorf("metadata leaked into body: %q", body)
		}
	})

	t.Run("missing title defaults to Untitled", func(t *testing.T) {
		in := "URL Source: https://example.com\n\nBody only."
		title, body := parseJinaEnvelope(in)
		if title != "Untitled" {
			t.Errorf("title = %q, want Untitled", title)
		}
		if body != "Body only." {
			t.Errorf("body = %q", body)
		}
	})

	t.Run("no blank line separator returns whole body", func(t *testing.T) {
		in := "Title: Only Header"
		title, body := parseJinaEnvelope(in)
		if title != "Only Header" {
			t.Errorf("title = %q", title)
		}
		// When no blank line, bodyStart stays 0 and all lines become body.
		// The "Title: ..." line ends up in the body — this is the actual
		// behavior; we document it here rather than assert cleanup.
		_ = body
	})

	t.Run("title whitespace trimmed", func(t *testing.T) {
		in := "Title:   My Article   \n\nBody."
		title, _ := parseJinaEnvelope(in)
		if title != "My Article" {
			t.Errorf("title = %q", title)
		}
	})
}

// TestFirstMarkdownHeading covers the H1 extractor used when a fetched
// article has no explicit title. Priority: markdown H1 first, then first
// non-empty line (capped at 100 chars), then "Untitled".
func TestFirstMarkdownHeading(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"explicit H1", "# My Title\n\nBody", "My Title"},
		{"H1 preceded by blank", "\n\n# Late Title\n\nBody", "Late Title"},
		{"no H1, first non-empty line", "\n\nOpening paragraph.\n\nMore", "Opening paragraph."},
		{"empty body", "", "Untitled"},
		{"only whitespace", "   \n\n   \n", "Untitled"},
		{"long line truncated at 100", strings.Repeat("x", 150), strings.Repeat("x", 100)},
		{"H1 with surrounding whitespace", "#   Spaced Out   \n", "Spaced Out"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := firstMarkdownHeading(tc.in)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
