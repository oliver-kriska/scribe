package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInsertAfterFrontmatter is critical — it's the core of scribe write's
// rolling-file append mode. A bug here corrupts learnings.md and
// decisions-log.md by inserting entries in the wrong place or mangling
// the frontmatter delimiters.
func TestInsertAfterFrontmatter(t *testing.T) {
	entry := "## 2026-04-10 | new entry\n\nBody text.\n\n---\n"

	t.Run("inserts between frontmatter and first entry", func(t *testing.T) {
		content := "---\ntitle: Rolling\nrolling: true\n---\n\n## 2026-04-09 | older entry\n\nBody.\n"
		got, err := insertAfterFrontmatter(content, entry)
		if err != nil {
			t.Fatal(err)
		}
		// The new entry must sit above the older entry (newest-first order).
		newIdx := strings.Index(got, "new entry")
		oldIdx := strings.Index(got, "older entry")
		if newIdx < 0 || oldIdx < 0 || newIdx > oldIdx {
			t.Errorf("new entry not above older entry:\n%s", got)
		}
		// Frontmatter must remain intact at the top.
		if !strings.HasPrefix(got, "---\ntitle: Rolling\nrolling: true\n---\n") {
			t.Errorf("frontmatter damaged:\n%s", got)
		}
	})

	t.Run("inserts into empty rolling file", func(t *testing.T) {
		content := "---\ntitle: Rolling\n---\n"
		got, err := insertAfterFrontmatter(content, entry)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "new entry") {
			t.Errorf("entry missing:\n%s", got)
		}
	})

	t.Run("rejects content without frontmatter", func(t *testing.T) {
		_, err := insertAfterFrontmatter("no frontmatter here", entry)
		if err == nil {
			t.Error("expected error for missing frontmatter")
		}
	})

	t.Run("rejects content with unclosed frontmatter", func(t *testing.T) {
		_, err := insertAfterFrontmatter("---\ntitle: Broken\n", entry)
		if err == nil {
			t.Error("expected error for unclosed frontmatter")
		}
	})

	t.Run("blank lines between frontmatter and body are collapsed", func(t *testing.T) {
		content := "---\ntitle: Rolling\n---\n\n\n\n## existing\n"
		got, err := insertAfterFrontmatter(content, entry)
		if err != nil {
			t.Fatal(err)
		}
		// No triple-blank-line gap after the frontmatter.
		if strings.Contains(got, "---\n\n\n\n") {
			t.Errorf("excess blank lines left after insert:\n%s", got)
		}
	})
}

// TestJoinTags covers the comma-splitting behavior used when tags come in
// as a Kong slice of strings that may contain commas themselves.
func TestJoinTags(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"already split", []string{"a", "b", "c"}, "a, b, c"},
		{"needs splitting", []string{"a,b,c"}, "a, b, c"},
		{"mixed", []string{"a,b", "c"}, "a, b, c"},
		{"whitespace around commas trimmed", []string{" a , b "}, "a, b"},
		{"empty pieces dropped", []string{"a,,b"}, "a, b"},
		{"all empty", []string{"", ""}, ""},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinTags(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestJoinWikilinks covers the "[[Title]]" formatter used in the related:
// frontmatter field. Empty entries are dropped; everything else is wrapped.
func TestJoinWikilinks(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"single", []string{"Article One"}, `"[[Article One]]"`},
		{"multiple", []string{"A", "B"}, `"[[A]]", "[[B]]"`},
		{"whitespace trimmed", []string{"  A  "}, `"[[A]]"`},
		{"empty dropped", []string{"A", "", "B"}, `"[[A]]", "[[B]]"`},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinWikilinks(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestJoinQuoted covers the generic quoted-list formatter used for sources.
func TestJoinQuoted(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"single", []string{"source1"}, `"source1"`},
		{"multiple", []string{"a", "b"}, `"a", "b"`},
		{"quotes inside", []string{`a "b" c`}, `"a \"b\" c"`},
		{"empty dropped", []string{"a", "", "b"}, `"a", "b"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinQuoted(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuildFrontmatter covers the YAML emitter. Every article the write
// command creates passes through this function, so a regression corrupts
// all new articles until noticed.
func TestBuildFrontmatter(t *testing.T) {
	t.Run("all fields populated", func(t *testing.T) {
		w := &WriteCmd{
			Title:   "Chose X over Y",
			Type:    "decision",
			Domain:  "acme",
			Tags:    []string{"tokens", "rate-limit"},
			Related: []string{"Acme Architecture"},
			Sources: []string{"plans/budget.md"},
			Status:  "decided",
		}
		got := w.buildFrontmatter()

		// Required frontmatter delimiters.
		if !strings.HasPrefix(got, "---\n") || !strings.HasSuffix(got, "---\n") {
			t.Errorf("missing frontmatter delimiters:\n%s", got)
		}
		// Spot-check every field.
		wants := []string{
			`title: "Chose X over Y"`,
			"type: decision",
			"domain: acme",
			"status: decided",
			"confidence: medium",
			"tags: [tokens, rate-limit]",
			`related: ["[[Acme Architecture]]"]`,
			`sources: ["plans/budget.md"]`,
			"created:",
			"updated:",
		}
		for _, want := range wants {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in:\n%s", want, got)
			}
		}
	})

	t.Run("status omitted when empty", func(t *testing.T) {
		w := &WriteCmd{Title: "T", Type: "decision", Domain: "general"}
		got := w.buildFrontmatter()
		if strings.Contains(got, "status:") {
			t.Errorf("status line leaked into output without Status field:\n%s", got)
		}
	})

	t.Run("empty tags / related / sources render as empty lists", func(t *testing.T) {
		w := &WriteCmd{Title: "T", Type: "decision", Domain: "general"}
		got := w.buildFrontmatter()
		for _, want := range []string{"tags: []", "related: []", "sources: []"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in:\n%s", want, got)
			}
		}
	})
}

// testKBRoot creates a tempdir with the minimal structure that kbDir and
// the write command expect: CLAUDE.md marker + projects/acme/ + wiki/.
// Callers must also set SCRIBE_KB to the returned path.
func testKBRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("# test"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Tests exercise "acme" as the domain; seed scribe.yaml so the domain
	// validator accepts it alongside the universal "personal"/"general" set.
	scribeYAML := "domains: [acme]\n"
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(scribeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"wiki", "decisions", "projects/acme"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("SCRIBE_KB", root)
	// Prevent WriteCmd.reindex from re-executing the test binary.
	t.Setenv("SCRIBE_SKIP_REINDEX", "1")
	return root
}

// TestWriteRunCreate covers the create path end-to-end against a tempdir
// KB: a new article lands at the expected canonical path with correct
// frontmatter and body.
func TestWriteRunCreate(t *testing.T) {
	root := testKBRoot(t)

	w := &WriteCmd{
		Title:  "Chose pgvector over qdrant",
		Type:   "decision",
		Domain: "acme",
		Tags:   []string{"vector,db"},
		Body:   "Oliver chose pgvector because the stack is already on postgres.",
	}
	if err := w.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	expected := filepath.Join(root, "decisions", "chose-pgvector-over-qdrant.md")
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("article not at expected path: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		`title: "Chose pgvector over qdrant"`,
		"domain: acme",
		"tags: [vector, db]",
		"Oliver chose pgvector because the stack is already on postgres.",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("missing %q in article:\n%s", want, content)
		}
	}
}

// TestWriteRunCreateRejectsDuplicate locks in Rule 12 (append-only):
// trying to create over an existing article must fail with a clear error.
func TestWriteRunCreateRejectsDuplicate(t *testing.T) {
	root := testKBRoot(t)

	existing := filepath.Join(root, "decisions", "already-here.md")
	if err := os.WriteFile(existing, []byte("existing content"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := &WriteCmd{
		Title:  "already here",
		Type:   "decision",
		Domain: "general",
		Body:   "some body",
	}
	err := w.Run()
	if err == nil {
		t.Fatal("expected error for duplicate, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error message should mention Rule 12: %v", err)
	}

	// Original file must be untouched.
	data, _ := os.ReadFile(existing)
	if string(data) != "existing content" {
		t.Errorf("existing file was overwritten: %q", data)
	}
}

// TestWriteRunRolling covers the append-to-rolling-memory path: new entry
// must land at the top (newest-first), after the frontmatter, without
// damaging existing entries.
func TestWriteRunRolling(t *testing.T) {
	root := testKBRoot(t)

	rollingPath := filepath.Join(root, "projects", "acme", "learnings.md")
	initial := `---
title: Acme Learnings
rolling: true
---

## 2026-04-05 | Earlier lesson

Earlier body.

---
`
	if err := os.WriteFile(rollingPath, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	w := &WriteCmd{
		Title:   "New insight today",
		Rolling: "learnings",
		Project: "acme",
		Body:    "Today we realized the thing.",
	}
	if err := w.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(rollingPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := string(data)

	// Frontmatter intact.
	if !strings.HasPrefix(updated, "---\ntitle: Acme Learnings\nrolling: true\n---\n") {
		t.Errorf("frontmatter damaged:\n%s", updated)
	}
	// New entry appears before the old one.
	newIdx := strings.Index(updated, "New insight today")
	oldIdx := strings.Index(updated, "Earlier lesson")
	if newIdx < 0 || oldIdx < 0 {
		t.Fatalf("entries missing:\n%s", updated)
	}
	if newIdx >= oldIdx {
		t.Errorf("new entry not above old entry:\n%s", updated)
	}
	// Body text preserved.
	if !strings.Contains(updated, "Today we realized the thing.") {
		t.Errorf("new body missing:\n%s", updated)
	}
}

// TestWriteRunRollingRejectsMissingFile covers the "don't auto-create
// rolling files" contract: the file must exist with rolling: true
// frontmatter before the CLI will append.
func TestWriteRunRollingRejectsMissingFile(t *testing.T) {
	testKBRoot(t)

	w := &WriteCmd{
		Title:   "orphan",
		Rolling: "decisions",
		Project: "acme", // file projects/acme/decisions-log.md does not exist
		Body:    "body",
	}
	err := w.Run()
	if err == nil {
		t.Fatal("expected error for missing rolling file")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error message should mention missing file: %v", err)
	}
}

// TestWriteRunRejectsInvalidDomain is a cheap guardrail: typos in the
// domain field would otherwise produce silently-misindexed articles.
func TestWriteRunRejectsInvalidDomain(t *testing.T) {
	testKBRoot(t)

	w := &WriteCmd{
		Title:  "T",
		Type:   "decision",
		Domain: "not-a-real-domain",
		Body:   "body",
	}
	err := w.Run()
	if err == nil || !strings.Contains(err.Error(), "invalid --domain") {
		t.Errorf("expected invalid domain error, got: %v", err)
	}
}
