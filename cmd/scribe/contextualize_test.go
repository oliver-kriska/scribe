package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestFileHasMarker_BoundedScan(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	m := retrievalContextMarker

	if !fileHasMarker(write("top.md", "---\nt: x\n---\n"+m+"\n> ctx\n\nbody"), m) {
		t.Error("marker near the top should be found")
	}
	if fileHasMarker(write("none.md", "---\nt: x\n---\n\nno marker here"), m) {
		t.Error("absent marker should not be found")
	}
	// Marker pushed past the scan window must read as absent — proves the
	// bound. (By construction the real marker is always near the top, so this
	// only ever costs a needless re-contextualize, never corruption.)
	filler := strings.Repeat("x", markerScanBytes+1024)
	if fileHasMarker(write("deep.md", "---\nt: x\n---\n"+filler+"\n"+m), m) {
		t.Error("marker beyond markerScanBytes must not be found (bounded scan)")
	}
	if fileHasMarker(filepath.Join(dir, "missing.md"), m) {
		t.Error("missing file should return false")
	}
}

// TestContextualizeRawArticles_MarkerIsSourceOfTruth is the regression for the
// clobbered-enrichment recall loss: an article recorded "done" in the filename
// log but whose marker was later stripped (re-collect clobber, or stub→real
// upgrade) must be re-contextualized, while one that still carries the marker
// is left alone.
func TestContextualizeRawArticles_MarkerIsSourceOfTruth(t *testing.T) {
	root := t.TempDir()
	rawDir := filepath.Join(root, "raw", "articles")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := retrievalContextMarker

	// a.md: logged AND still carries the marker → must be skipped.
	enriched := "---\ntitle: A\n---\n\n" + m + "\n> **Retrieval context (auto):** prior summary.\n\nAlpha body.\n"
	if err := os.WriteFile(filepath.Join(rawDir, "a.md"), []byte(enriched), 0o644); err != nil {
		t.Fatal(err)
	}
	// b.md: logged but its marker was stripped → must be reprocessed.
	stripped := "---\ntitle: B\n---\n\nBravo body that lost its enrichment.\n"
	if err := os.WriteFile(filepath.Join(rawDir, "b.md"), []byte(stripped), 0o644); err != nil {
		t.Fatal(err)
	}
	// Both recorded "done" in the (now non-authoritative) filename log.
	logBytes, err := json.Marshal(map[string]string{
		"a.md": "2026-06-01T00:00:00Z",
		"b.md": "2026-06-01T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "_contextualized_log.json"), logBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	stub := &stubLLM{DefaultReply: "This article records the connection pool saturation incident, walking through the observed timeline, the database metrics inspected during triage, and the remediation the engineering team applied to restore stable throughput across services."}
	installStubLLM(t, stub)

	if err := contextualizeRawArticles(root, 0, "gemma3:4b", false, false); err != nil {
		t.Fatalf("contextualizeRawArticles: %v", err)
	}

	if n := len(stub.Calls()); n != 1 {
		t.Fatalf("LLM called %d times, want 1 (only the stripped, marker-less file)", n)
	}
	if !fileHasMarker(filepath.Join(rawDir, "b.md"), m) {
		t.Error("stripped-but-logged file b.md was not re-enriched — stale log suppressed it")
	}
	a, _ := os.ReadFile(filepath.Join(rawDir, "a.md"))
	if strings.Count(string(a), m) != 1 {
		t.Errorf("marker-bearing a.md should be untouched; marker count = %d", strings.Count(string(a), m))
	}
}
