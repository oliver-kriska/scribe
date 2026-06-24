package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComparableResearchBody(t *testing.T) {
	src := "# Heading\n\nThe real research body.\n"
	pristine := "---\ntitle: \"X\"\ningested_at: \"2026-06-23T10:00:07Z\"\n---\n\n" + src
	enriched := "---\ntitle: \"X\"\ningested_at: \"2026-06-24T14:23:04Z\"\n---\n\n" +
		retrievalContextBlock("a one-line auto summary of the doc") + src

	// A bumped ingested_at and an inserted retrieval-context block must not
	// register as a content change — that's the whole point of the helper.
	if comparableResearchBody(pristine) != comparableResearchBody(enriched) {
		t.Errorf("enrichment/timestamp churn changed the comparable body:\n pristine=%q\n enriched=%q",
			comparableResearchBody(pristine), comparableResearchBody(enriched))
	}
	// A genuine body change must register as different.
	changed := "---\ntitle: \"X\"\n---\n\n# Heading\n\nDifferent body now.\n"
	if comparableResearchBody(pristine) == comparableResearchBody(changed) {
		t.Error("a changed body must not compare equal")
	}
	// No frontmatter, no enrichment: returns the trimmed content.
	if got := comparableResearchBody("  just text  "); got != "just text" {
		t.Errorf("plain content = %q, want %q", got, "just text")
	}
}

// TestCollectResearchFiles_IdempotentPreservesEnrichment is the regression
// for scriptorium's churn: a source file with a bumped mtime but identical
// bytes (git checkout / rsync / touch) was re-collected, overwriting the
// contextualize enrichment and forcing a needless re-contextualize. Collect
// must now skip an unchanged source and only overwrite a genuinely changed one.
func TestCollectResearchFiles_IdempotentPreservesEnrichment(t *testing.T) {
	kb := t.TempDir()
	if err := os.MkdirAll(filepath.Join(kb, "raw", "articles"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	researchDir := filepath.Join(src, ".claude", "research")
	if err := os.MkdirAll(researchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(researchDir, "note.md")
	if err := os.WriteFile(srcFile, []byte("# Note\n\nOriginal research content.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manifest{Projects: map[string]*ProjectEntry{
		"proj": {Path: src, Domain: "general"},
	}}
	m.path = filepath.Join(kb, "scripts", "projects.json") // save() MkdirAll's the dir
	s := &SyncCmd{}

	// First collect writes the dest.
	if got := s.collectResearchFiles(kb, m); got != 1 {
		t.Fatalf("first collect = %d, want 1", got)
	}
	dest := filepath.Join(kb, "raw", "articles", "research-proj-note.md")
	first, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("dest not written: %v", err)
	}

	// Simulate contextualize enriching the collected article in place.
	enriched, err := insertRetrievalContext(string(first), "an auto retrieval summary")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte(enriched), 0o644); err != nil {
		t.Fatal(err)
	}

	// Force a re-walk (mtime jitter: a touch/checkout that bumps the source
	// mtime without changing its bytes).
	m.Projects["proj"].LastResearchScanned = ""

	// Unchanged source → must skip, not overwrite.
	if got := s.collectResearchFiles(kb, m); got != 0 {
		t.Errorf("second collect = %d, want 0 (unchanged source must be skipped)", got)
	}
	after, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != enriched {
		t.Error("re-collect modified the enriched dest despite an unchanged source")
	}
	if !strings.Contains(string(after), retrievalContextMarker) {
		t.Error("re-collect clobbered the retrieval-context enrichment")
	}

	// Genuinely change the source → must overwrite.
	if err := os.WriteFile(srcFile, []byte("# Note\n\nRewritten research content.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.Projects["proj"].LastResearchScanned = ""
	if got := s.collectResearchFiles(kb, m); got != 1 {
		t.Errorf("third collect (changed source) = %d, want 1", got)
	}
	final, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(final), "Rewritten research content.") {
		t.Error("changed source was not collected")
	}
	if strings.Contains(string(final), retrievalContextMarker) {
		t.Error("a changed source should replace the article (enrichment gone until re-contextualize)")
	}
}
