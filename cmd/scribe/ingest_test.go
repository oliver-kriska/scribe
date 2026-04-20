package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseQueueEntry covers the `key: value` format used by the ingest queue.
// Bugs here silently drop queued URLs or drop fields, so the common shapes
// are worth locking in.
func TestParseQueueEntry(t *testing.T) {
	t.Run("basic fields", func(t *testing.T) {
		in := "url: https://example.com\ntitle: Hello\ndomain: general\n"
		got := parseQueueEntry(in)
		want := map[string]string{
			"url":    "https://example.com",
			"title":  "Hello",
			"domain": "general",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v want %v", got, want)
		}
	})

	t.Run("comments and blank lines skipped", func(t *testing.T) {
		in := "# comment\n\nurl: https://example.com\n# trailing comment\n"
		got := parseQueueEntry(in)
		if got["url"] != "https://example.com" {
			t.Errorf("url missing: %v", got)
		}
		if len(got) != 1 {
			t.Errorf("expected 1 field, got %d: %v", len(got), got)
		}
	})

	t.Run("value with colon keeps everything after first colon", func(t *testing.T) {
		// Matches the real format: URLs contain colons, which must not be split.
		in := "url: https://example.com:8080/path\n"
		got := parseQueueEntry(in)
		if got["url"] != "https://example.com:8080/path" {
			t.Errorf("url lost colon: %q", got["url"])
		}
	})

	t.Run("leading/trailing whitespace stripped from key and value", func(t *testing.T) {
		in := "   key   :   value   \n"
		got := parseQueueEntry(in)
		if got["key"] != "value" {
			t.Errorf("got %q, want %q", got["key"], "value")
		}
	})

	t.Run("line without colon is ignored", func(t *testing.T) {
		in := "url: https://example.com\njust a line\n"
		got := parseQueueEntry(in)
		if len(got) != 1 {
			t.Errorf("expected 1 field, got %d: %v", len(got), got)
		}
	})
}

// TestSlugify covers the ingest slug format — used for raw article filenames.
// The key behaviors: lowercase, scheme+www stripping, non-alnum collapse,
// trim surrounding dashes.
func TestSlugify(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Hello World", "hello-world"},
		{"https://example.com/path", "example-com-path"},
		{"https://www.example.com/path", "example-com-path"},
		{"http://example.com/", "example-com"},
		{"Title with Unicode: Café", "title-with-unicode-caf"}, // non-ASCII is stripped (only a-z0-9 kept)
		{"  padded  ", "padded"},
		{"UPPER", "upper"},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := slugify(tc.in)
			if got != tc.want {
				t.Errorf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestBuildRawArticle checks the frontmatter shape of ingested articles.
// We can't assert on the full path (time.Now influences the date), but we
// can assert the frontmatter + body contract.
func TestBuildRawArticle(t *testing.T) {
	path, content := buildRawArticle("/tmp/kb", "https://example.com/a", "Article Title", "body prose", "trafilatura", "general", []string{"tag1"})

	if !strings.HasPrefix(path, "/tmp/kb/raw/articles/") {
		t.Errorf("path not under raw/articles: %s", path)
	}
	if !strings.Contains(path, "article-title.md") {
		t.Errorf("slug not in path: %s", path)
	}

	checks := []string{
		`title: "Article Title"`,
		`source_url: "https://example.com/a"`,
		`fetched_via: trafilatura`,
		`type: article`,
		`domain: general`,
		`tags: [tag1]`,
		"body prose",
	}
	for _, c := range checks {
		if !strings.Contains(content, c) {
			t.Errorf("missing %q in content:\n%s", c, content)
		}
	}
	if !strings.HasSuffix(content, "\n") {
		t.Errorf("content missing trailing newline")
	}
}
