package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestExtractURLsFromBytes is the most important capture test. The iMessage
// attributedBody blob is NSKeyedArchiver-encoded and URLs live deep inside,
// surrounded by framing bytes. The scanner walks byte-by-byte looking for
// http:// or https:// followed by URL-safe bytes. Bugs here silently drop
// captures or inject garbage into the KB.
func TestExtractURLsFromBytes(t *testing.T) {
	t.Run("single plain URL", func(t *testing.T) {
		got := extractURLsFromBytes([]byte("https://example.com/path"))
		if !reflect.DeepEqual(got, []string{"https://example.com/path"}) {
			t.Errorf("got %v", got)
		}
	})

	t.Run("URL embedded in binary noise", func(t *testing.T) {
		// Simulates the shape of an attributedBody blob: NULL bytes and
		// framing around the real URL.
		blob := []byte("\x00\x00streamed\x01\x02https://example.com/article?id=42\x00ending")
		got := extractURLsFromBytes(blob)
		if len(got) != 1 || got[0] != "https://example.com/article?id=42" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("multiple URLs", func(t *testing.T) {
		blob := []byte("first https://a.example.org/x second http://b.example.org/y end")
		got := extractURLsFromBytes(blob)
		want := []string{"https://a.example.org/x", "http://b.example.org/y"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v want %v", got, want)
		}
	})

	t.Run("URL stops at stop bytes", func(t *testing.T) {
		cases := []struct {
			stop byte
			name string
		}{
			{' ', "space"},
			{'<', "lt"},
			{'>', "gt"},
			{'"', "quote"},
			{'\'', "apostrophe"},
			{')', "rparen"},
			{']', "rbracket"},
			{0x00, "null"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				blob := append([]byte("https://example.com/path"), tc.stop, 'x')
				got := extractURLsFromBytes(blob)
				if len(got) != 1 || got[0] != "https://example.com/path" {
					t.Errorf("stop byte %q: got %v", tc.stop, got)
				}
			})
		}
	})

	t.Run("URL with only scheme is rejected", func(t *testing.T) {
		// "http://" alone (no host) and "https://" alone must not be captured.
		got := extractURLsFromBytes([]byte("before http:// after"))
		if got != nil {
			t.Errorf("bare scheme captured: %v", got)
		}
	})

	t.Run("empty blob", func(t *testing.T) {
		if got := extractURLsFromBytes(nil); got != nil {
			t.Errorf("got %v", got)
		}
	})
}

func TestShouldSkipURL(t *testing.T) {
	skip := []string{"instagram.com/reel", "audiolibrix.com"}
	cases := []struct {
		url  string
		skip bool
	}{
		{"https://instagram.com/reel/CxYz/", true},
		{"https://audiolibrix.com/book/foo", true},
		{"https://instagram.com/p/CxYz/", false}, // /p/ is a post, not a reel
		{"https://example.com/", false},
	}
	for _, tc := range cases {
		if got := shouldSkipURL(tc.url, skip); got != tc.skip {
			t.Errorf("shouldSkipURL(%q) = %v, want %v", tc.url, got, tc.skip)
		}
	}
	// Empty skip list — nothing should match.
	if shouldSkipURL("https://instagram.com/reel/abc", nil) {
		t.Error("empty skip list should not match")
	}
}

func TestSlugFromCapturedURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://example.com/article", "example-com-article"},
		{"https://www.example.com/article", "example-com-article"},
		{"http://example.com/", "example-com"},
		{"https://example.com/a/very/long/nested/path/with/many/segments/indeed/and/more/content", "example-com-a-very-long-nested-path-with-many-segments-indee"},
		{"https://example.com/?q=foo&r=bar", "example-com-q-foo-r-bar"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := slugFromCapturedURL(tc.in)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if len(got) > 60 {
				t.Errorf("slug length %d > 60", len(got))
			}
		})
	}
}

func TestSlugifyText(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"Hello World", 60, "hello-world"},
		{"Quick Note: something important!", 60, "quick-note-something-important"},
		{"A" + strings.Repeat("b", 100), 20, "a" + strings.Repeat("b", 19)},
		{"  leading/trailing  ", 60, "leading-trailing"},
		{"", 60, ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := slugifyText(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAppleNanosToTime locks in the Apple CFAbsoluteTime epoch conversion.
// Apple epoch is 2001-01-01 00:00:00 UTC, which is Unix 978307200.
func TestAppleNanosToTime(t *testing.T) {
	// Apple nanos = 0 → 2001-01-01 00:00:00 UTC
	t0 := appleNanosToTime(0)
	want0 := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if !t0.Equal(want0) {
		t.Errorf("got %v, want %v", t0, want0)
	}

	// One full day after epoch: 86400 * 1e9 nanos
	t1 := appleNanosToTime(86400 * 1_000_000_000)
	want1 := time.Date(2001, 1, 2, 0, 0, 0, 0, time.UTC)
	if !t1.Equal(want1) {
		t.Errorf("got %v, want %v", t1, want1)
	}
}

// TestBuildCaptureArticle covers the frontmatter shape — missing URL (for
// free-form notes), tags, and trailing newline normalization. The article
// body is injected verbatim so we just check the framing around it.
func TestBuildCaptureArticle(t *testing.T) {
	t.Run("with URL and tags", func(t *testing.T) {
		got := buildCaptureArticle("https://example.com", "Hello", "body text", "trafilatura", "2026-04-10", []string{"tag1", "tag2"})
		checks := []string{
			`title: "Hello"`,
			`source_url: "https://example.com"`,
			`captured: 2026-04-10`,
			`fetched_via: trafilatura`,
			`tags: [tag1, tag2]`,
			"body text",
		}
		for _, c := range checks {
			if !strings.Contains(got, c) {
				t.Errorf("missing %q in:\n%s", c, got)
			}
		}
		if !strings.HasSuffix(got, "\n") {
			t.Errorf("missing trailing newline")
		}
	})

	t.Run("note without URL omits source_url", func(t *testing.T) {
		got := buildCaptureArticle("", "Quick thought", "a note", "imessage", "2026-04-10", nil)
		if strings.Contains(got, "source_url:") {
			t.Errorf("note should not have source_url field:\n%s", got)
		}
		if !strings.Contains(got, "tags: []") {
			t.Errorf("empty tags should render as []:\n%s", got)
		}
	})

	t.Run("title with quotes is escaped", func(t *testing.T) {
		got := buildCaptureArticle("https://example.com", `Title with "quotes"`, "body", "trafilatura", "2026-04-10", nil)
		if !strings.Contains(got, `title: "Title with \"quotes\""`) {
			t.Errorf("quote escape missing:\n%s", got)
		}
	})
}
