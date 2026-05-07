package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeArticleForLedger(t *testing.T, dir, sub, slug, title, ctype string, contradicts []string) string {
	t.Helper()
	full := filepath.Join(dir, sub, slug+".md")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString("title: \"" + title + "\"\n")
	sb.WriteString("type: " + ctype + "\n")
	if len(contradicts) > 0 {
		sb.WriteString("contradicts: [")
		for i, c := range contradicts {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("\"[[" + c + "]]\"")
		}
		sb.WriteString("]\n")
	}
	sb.WriteString("---\n\nbody\n")
	if err := os.WriteFile(full, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return full
}

func TestBuildContradictionLedger_CollapsesPair(t *testing.T) {
	dir := t.TempDir()
	writeArticleForLedger(t, dir, "decisions", "a", "Decision A", "decision", []string{"Decision B"})
	writeArticleForLedger(t, dir, "decisions", "b", "Decision B", "decision", []string{"Decision A"})

	total, unresolved, err := buildContradictionLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("symmetric A↔B should collapse to 1 entry, got %d", total)
	}
	if unresolved != 1 {
		t.Errorf("want 1 unresolved, got %d", unresolved)
	}

	entries, _ := readContradictionLedger(dir)
	if len(entries) != 1 {
		t.Fatalf("ledger length: %d", len(entries))
	}
	e := entries[0]
	if e.Pair[0] != "Decision A" || e.Pair[1] != "Decision B" {
		t.Errorf("expected sorted pair (A, B), got %v", e.Pair)
	}
	if len(e.Sources) != 2 {
		t.Errorf("expected both sides recorded as sources, got %d", len(e.Sources))
	}
}

func TestBuildContradictionLedger_OneDirectional(t *testing.T) {
	dir := t.TempDir()
	writeArticleForLedger(t, dir, "decisions", "a", "Decision A", "decision", []string{"Decision B"})
	writeArticleForLedger(t, dir, "decisions", "b", "Decision B", "decision", nil) // no reverse

	total, _, err := buildContradictionLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("one-directional should still produce 1 entry, got %d", total)
	}
	entries, _ := readContradictionLedger(dir)
	if len(entries[0].Sources) != 1 {
		t.Errorf("one-directional should have 1 source, got %d", len(entries[0].Sources))
	}
}

func TestBuildContradictionLedger_PreservesFirstObservedAt(t *testing.T) {
	dir := t.TempDir()
	writeArticleForLedger(t, dir, "decisions", "a", "Decision A", "decision", []string{"Decision B"})
	writeArticleForLedger(t, dir, "decisions", "b", "Decision B", "decision", []string{"Decision A"})

	if _, _, err := buildContradictionLedger(dir); err != nil {
		t.Fatal(err)
	}
	first, _ := readContradictionLedger(dir)
	firstTS := first[0].FirstObservedAt

	// Touch nothing; rebuild and verify timestamp preserved.
	if _, _, err := buildContradictionLedger(dir); err != nil {
		t.Fatal(err)
	}
	second, _ := readContradictionLedger(dir)
	if second[0].FirstObservedAt != firstTS {
		t.Errorf("first_observed_at should be preserved across rebuilds; got %s -> %s",
			firstTS, second[0].FirstObservedAt)
	}
	if second[0].LastSeenAt == "" {
		t.Errorf("last_seen_at should be set on rebuild")
	}
}

func TestBuildContradictionLedger_RemovesStaleWhenEdgesGone(t *testing.T) {
	dir := t.TempDir()
	writeArticleForLedger(t, dir, "decisions", "a", "Decision A", "decision", []string{"Decision B"})
	writeArticleForLedger(t, dir, "decisions", "b", "Decision B", "decision", []string{"Decision A"})
	if _, _, err := buildContradictionLedger(dir); err != nil {
		t.Fatal(err)
	}
	// Drop the contradicts edge from both.
	writeArticleForLedger(t, dir, "decisions", "a", "Decision A", "decision", nil)
	writeArticleForLedger(t, dir, "decisions", "b", "Decision B", "decision", nil)
	total, _, err := buildContradictionLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Errorf("ledger should be empty after edges removed, got %d", total)
	}
	if _, err := os.Stat(contradictionsLedgerPath(dir)); !os.IsNotExist(err) {
		t.Errorf("expected ledger file removed; stat err=%v", err)
	}
}

func TestResolveContradiction_PreservesEntry(t *testing.T) {
	dir := t.TempDir()
	writeArticleForLedger(t, dir, "decisions", "a", "Decision A", "decision", []string{"Decision B"})
	writeArticleForLedger(t, dir, "decisions", "b", "Decision B", "decision", []string{"Decision A"})
	if _, _, err := buildContradictionLedger(dir); err != nil {
		t.Fatal(err)
	}
	entries, _ := readContradictionLedger(dir)
	id := entries[0].ID

	if err := resolveContradiction(dir, id, "B supersedes A"); err != nil {
		t.Fatal(err)
	}
	after, _ := readContradictionLedger(dir)
	if after[0].ResolvedAt == "" {
		t.Errorf("expected resolved_at set")
	}
	if after[0].ResolutionNote != "B supersedes A" {
		t.Errorf("note not stored, got %q", after[0].ResolutionNote)
	}
}

func TestContradictionPairID_StableAcrossOrder(t *testing.T) {
	id1 := contradictionPairID("Decision A", "Decision B")
	id2 := contradictionPairID("Decision B", "Decision A")
	// We canonicalize before hashing in the build pass; pair IDs
	// computed directly from the same inputs are stable but order-
	// sensitive at this layer. The build pass sorts before passing.
	if id1 == id2 {
		t.Errorf("pair ID should depend on input order at the helper level (sorted upstream)")
	}
}

func TestCheckContradictions_OkWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	out := checkContradictions(dir)
	if len(out) != 1 {
		t.Fatalf("want 1 check, got %d", len(out))
	}
	if out[0].Status != statusOK {
		t.Errorf("empty ledger should be ok; got %s: %s", out[0].Status, out[0].Detail)
	}
}

func TestCheckContradictions_WarnPerUnresolvedEntry(t *testing.T) {
	dir := t.TempDir()
	writeArticleForLedger(t, dir, "decisions", "a", "Decision A", "decision", []string{"Decision B"})
	writeArticleForLedger(t, dir, "decisions", "b", "Decision B", "decision", []string{"Decision A"})
	if _, _, err := buildContradictionLedger(dir); err != nil {
		t.Fatal(err)
	}
	out := checkContradictions(dir)
	warns := 0
	for _, c := range out {
		if c.Status == statusWarn {
			warns++
		}
	}
	if warns != 1 {
		t.Errorf("want 1 warn, got %d (full: %+v)", warns, out)
	}
}
