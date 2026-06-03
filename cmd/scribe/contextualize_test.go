package main

import (
	"strings"
	"testing"
)

func TestInsertRetrievalContext(t *testing.T) {
	paragraph := "This is the context."

	t.Run("inserts after frontmatter", func(t *testing.T) {
		in := "---\ntitle: foo\n---\nbody line 1\nbody line 2\n"
		got, err := insertRetrievalContext(in, paragraph)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, retrievalContextMarker) {
			t.Error("marker missing")
		}
		if !strings.Contains(got, "body line 1") {
			t.Error("body lost")
		}
		// Marker must come after the closing --- and before body.
		mkIdx := strings.Index(got, retrievalContextMarker)
		closingIdx := strings.Index(got, "\n---\n")
		bodyIdx := strings.Index(got, "body line 1")
		if closingIdx >= mkIdx || mkIdx >= bodyIdx {
			t.Errorf("ordering wrong: closing=%d marker=%d body=%d", closingIdx, mkIdx, bodyIdx)
		}
	})

	t.Run("no-op when marker already present", func(t *testing.T) {
		in := "---\ntitle: foo\n---\n" + retrievalContextMarker + "\nbody\n"
		got, err := insertRetrievalContext(in, paragraph)
		if err != nil {
			t.Fatal(err)
		}
		if got != in {
			t.Errorf("content should be unchanged when marker present")
		}
	})

	t.Run("prepends when no frontmatter", func(t *testing.T) {
		in := "body without frontmatter\n"
		got, err := insertRetrievalContext(in, paragraph)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(strings.TrimLeft(got, "\n"), retrievalContextMarker) {
			t.Error("marker should be near the top when no frontmatter")
		}
		if !strings.Contains(got, "body without frontmatter") {
			t.Error("body lost")
		}
	})

	t.Run("errors on malformed frontmatter", func(t *testing.T) {
		in := "---\nno closing delimiter here\n"
		_, err := insertRetrievalContext(in, paragraph)
		if err == nil {
			t.Error("expected error for malformed frontmatter")
		}
	})
}

func TestContextualizeSourceMeta(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string // substrings that must all be present ("" => expect empty)
	}{
		{
			name: "web capture uses source_url + title",
			raw:  "---\ntitle: \"FOD#151: Recursive Self-Learning\"\nsource_url: \"https://www.turingpost.com/p/fod151\"\n---\nbody\n",
			want: []string{"Known source", "FOD#151", "https://www.turingpost.com/p/fod151"},
		},
		{
			name: "local file falls back to source_path",
			raw:  "---\ntitle: Notes\nsource_path: \"/Users/o/x.md\"\n---\nbody\n",
			want: []string{"Known source", "Notes", "/Users/o/x.md"},
		},
		{
			name: "source_url wins over source_path when both present",
			raw:  "---\ntitle: T\nsource_url: \"https://e.com\"\nsource_path: \"/local.md\"\n---\nb\n",
			want: []string{"https://e.com"},
		},
		{
			name: "published date surfaced as authoritative",
			raw:  "---\ntitle: T\nsource_url: \"https://e.com\"\npublished: \"March 2, 2026\"\ncaptured: \"2026-06-03\"\n---\nb\n",
			want: []string{"Published: March 2, 2026"},
		},
		{
			name: "date field used when published absent (yaml-parsed date)",
			raw:  "---\ntitle: T\nsource_path: \"/x.md\"\ndate: 2026-03-02\n---\nb\n",
			want: []string{"Published: 2026-03-02"},
		},
		{
			name: "no usable metadata yields empty (prompt falls back to inference)",
			raw:  "---\ndomain: general\n---\nbody\n",
			want: nil,
		},
		{
			name: "no frontmatter yields empty",
			raw:  "just a body, no frontmatter\n",
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := contextualizeSourceMeta([]byte(tc.raw))
			if tc.want == nil {
				if got != "" {
					t.Errorf("expected empty, got %q", got)
				}
				return
			}
			for _, sub := range tc.want {
				if !strings.Contains(got, sub) {
					t.Errorf("missing %q in %q", sub, got)
				}
			}
		})
	}
	t.Run("source_path not leaked when source_url present", func(t *testing.T) {
		got := contextualizeSourceMeta([]byte("---\ntitle: T\nsource_url: \"https://e.com\"\nsource_path: \"/local.md\"\n---\nb\n"))
		if strings.Contains(got, "/local.md") {
			t.Errorf("source_path should be omitted when source_url present: %q", got)
		}
	})
	t.Run("captured date never leaks into source meta", func(t *testing.T) {
		// The ingest `captured:` date is what a small model mistook for the
		// study date (2026-06-03 audit). It must never be surfaced.
		got := contextualizeSourceMeta([]byte("---\ntitle: T\nsource_url: \"https://e.com\"\npublished: \"March 2, 2026\"\ncaptured: \"2026-06-03\"\n---\nb\n"))
		if strings.Contains(got, "2026-06-03") {
			t.Errorf("captured date must not appear in source meta: %q", got)
		}
		if !strings.Contains(got, "Published: March 2, 2026") {
			t.Errorf("published date should be surfaced: %q", got)
		}
	})
}

func TestDegenerateContextReason(t *testing.T) {
	// body is the frontmatter-stripped article; the breadcrumb is its first
	// line, mirroring the article-05 failure where the model echoed it.
	body := "AI Search, Data & Studies\nSome real body content about the topic.\n"
	valid := "Thread by Jane Doe analyzing the architecture of distributed consensus systems, contrasting several coordination protocols and their tradeoffs, and framing when a team would reach for each approach in production. Useful for engineers comparing strategies."
	tests := []struct {
		name       string
		text       string
		degenerate bool
	}{
		{"breadcrumb echo", "AI Search, Data & Studies", true},
		{"too short fragment", "Short note about a topic and tools", true},
		{"no sentence punctuation", strings.TrimSpace(strings.Repeat("word ", 30)), true},
		{"valid paragraph", valid, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reason := degenerateContextReason(tc.text, body)
			if tc.degenerate && reason == "" {
				t.Errorf("expected degenerate, got accepted")
			}
			if !tc.degenerate && reason != "" {
				t.Errorf("expected accepted, got rejected: %s", reason)
			}
		})
	}
}
