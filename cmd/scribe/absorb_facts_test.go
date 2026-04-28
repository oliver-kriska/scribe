package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Phase 3B test scope: shouldRunFactsPass gating, merge semantics,
// per-chapter slicing, and prompt formatting. Like Phase 3A.5 the
// claude shell-out is integration-only; these tests cover the pure-
// Go decision and merge layer.

func TestShouldRunFactsPass_OffByDefault(t *testing.T) {
	cfg := AbsorbConfig{}
	if shouldRunFactsPass(cfg) {
		t.Error("default cfg should not run facts pass")
	}
}

func TestShouldRunFactsPass_RequiresChaptered(t *testing.T) {
	trueV := true
	falseV := false
	cfg := AbsorbConfig{
		AtomicFacts:  &trueV,
		ChapterAware: &falseV,
	}
	if shouldRunFactsPass(cfg) {
		t.Error("facts pass must require ChapterAware=true")
	}
}

func TestShouldRunFactsPass_HappyPath(t *testing.T) {
	trueV := true
	cfg := AbsorbConfig{
		AtomicFacts:  &trueV,
		ChapterAware: &trueV,
	}
	if !shouldRunFactsPass(cfg) {
		t.Error("expected facts pass to fire when both opt-ins are true")
	}
}

func TestShouldRunFactsPass_AtomicFactsExplicitFalse(t *testing.T) {
	trueV := true
	falseV := false
	cfg := AbsorbConfig{
		AtomicFacts:  &falseV,
		ChapterAware: &trueV,
	}
	if shouldRunFactsPass(cfg) {
		t.Error("explicit AtomicFacts=false must short-circuit")
	}
}

func TestMergeFacts_PrefixesIDsByChapter(t *testing.T) {
	tmp := t.TempDir()
	runs := []chapterRun{
		{index: 0, chunk: Chunk{Title: "Intro"}},
		{index: 1, chunk: Chunk{Title: "Method"}},
	}
	c0 := chapterFacts{
		Chapter: "Intro",
		Facts: []AtomicFact{
			{ID: "f1", Type: "definition", Claim: "X is the system.", Anchor: "X is the"},
			{ID: "f2", Type: "claim", Claim: "X improves Y by 20%.", Anchor: "improves Y"},
		},
	}
	c1 := chapterFacts{
		Chapter: "Method",
		Facts: []AtomicFact{
			{ID: "f1", Type: "decision", Claim: "We chose Z over W.", Anchor: "Z over W"},
		},
	}
	paths := []string{
		writeJSON(t, tmp, "00.json", c0),
		writeJSON(t, tmp, "01.json", c1),
	}

	merged := mergeFacts("raw.md", "Title", runs, paths)
	if merged.Version != factsSchemaVersion {
		t.Errorf("version = %d", merged.Version)
	}
	if len(merged.Facts) != 3 {
		t.Fatalf("expected 3 facts, got %d: %+v", len(merged.Facts), merged.Facts)
	}
	want := []string{"c00-f1", "c00-f2", "c01-f1"}
	for i, w := range want {
		if merged.Facts[i].ID != w {
			t.Errorf("fact[%d] ID = %q, want %q", i, merged.Facts[i].ID, w)
		}
	}
	// Chapter index entries.
	if len(merged.Chapters) != 2 {
		t.Fatalf("expected 2 chapter entries, got %d", len(merged.Chapters))
	}
	if merged.Chapters[0].IDStart != "c00-f1" || merged.Chapters[0].IDEnd != "c00-f2" || merged.Chapters[0].Count != 2 {
		t.Errorf("chapter 0 entry wrong: %+v", merged.Chapters[0])
	}
	if merged.Chapters[1].IDStart != "c01-f1" || merged.Chapters[1].IDEnd != "c01-f1" || merged.Chapters[1].Count != 1 {
		t.Errorf("chapter 1 entry wrong: %+v", merged.Chapters[1])
	}
	// Chapter title threaded through to per-fact field.
	for _, f := range merged.Facts {
		if f.ChapterTitle == "" {
			t.Errorf("fact %s missing chapter_title", f.ID)
		}
	}
}

func TestMergeFacts_TolerantOfMissingAndBrokenChunkFiles(t *testing.T) {
	tmp := t.TempDir()
	good := chapterFacts{
		Chapter: "Good",
		Facts:   []AtomicFact{{ID: "f1", Type: "claim", Claim: "real fact", Anchor: "anchor"}},
	}
	runs := []chapterRun{
		{index: 0, chunk: Chunk{Title: "Missing"}},
		{index: 1, chunk: Chunk{Title: "Good"}},
		{index: 2, chunk: Chunk{Title: "Broken"}},
	}
	paths := []string{
		filepath.Join(tmp, "missing.json"), // never written
		writeJSON(t, tmp, "good.json", good),
		writeBytesPhase3B(t, tmp, "broken.json", []byte("{ malformed")),
	}
	merged := mergeFacts("raw.md", "Title", runs, paths)
	// Only one fact survives (the good chunk).
	if len(merged.Facts) != 1 || merged.Facts[0].ID != "c01-f1" {
		t.Errorf("expected exactly the good fact; got %+v", merged.Facts)
	}
	// All three chapters indexed (two with zero count).
	if len(merged.Chapters) != 3 {
		t.Errorf("expected 3 chapter entries (incl. zero-counts); got %d", len(merged.Chapters))
	}
	zeroCount := 0
	for _, c := range merged.Chapters {
		if c.Count == 0 {
			zeroCount++
		}
	}
	if zeroCount != 2 {
		t.Errorf("expected 2 zero-count chapter entries (missing + broken); got %d", zeroCount)
	}
}

func TestMergedFacts_factsForChapter_SlicesByPrefix(t *testing.T) {
	mf := &MergedFacts{
		Facts: []AtomicFact{
			{ID: "c00-f1"},
			{ID: "c00-f2"},
			{ID: "c01-f1"},
			{ID: "c02-f1"},
			{ID: "c02-f2"},
			{ID: "c02-f3"},
		},
	}
	got := mf.factsForChapter(2)
	if len(got) != 3 {
		t.Errorf("chapter 2 should have 3 facts; got %d", len(got))
	}
	for _, f := range got {
		if !strings.HasPrefix(f.ID, "c02-") {
			t.Errorf("unexpected ID for chapter-2 slice: %s", f.ID)
		}
	}
}

func TestMergedFacts_factsForChapter_NilSafe(t *testing.T) {
	var mf *MergedFacts
	if got := mf.factsForChapter(0); got != nil {
		t.Errorf("nil receiver should return nil; got %v", got)
	}
}

func TestMergedFacts_factsForChapter_NoMatchReturnsNil(t *testing.T) {
	mf := &MergedFacts{
		Facts: []AtomicFact{{ID: "c00-f1"}},
	}
	if got := mf.factsForChapter(99); got != nil {
		t.Errorf("expected nil for chapter with no facts; got %v", got)
	}
}

func TestFormatFactsForPrompt_RendersCompactBlock(t *testing.T) {
	facts := []AtomicFact{
		{ID: "c00-f1", Type: "definition", Claim: "X is the foundation.", Anchor: "X is the"},
		{ID: "c00-f2", Type: "numeric", Claim: "73.4% accuracy.", Anchor: "73.4%"},
	}
	got := formatFactsForPrompt(facts)
	for _, want := range []string{
		"[c00-f1, definition]",
		"X is the foundation.",
		`"X is the"`,
		"[c00-f2, numeric]",
		"73.4% accuracy.",
		`"73.4%"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatted block missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatFactsForPrompt_EmptyInputIsEmptyString(t *testing.T) {
	if got := formatFactsForPrompt(nil); got != "" {
		t.Errorf("nil facts should render empty; got %q", got)
	}
	if got := formatFactsForPrompt([]AtomicFact{}); got != "" {
		t.Errorf("empty slice should render empty; got %q", got)
	}
}

func TestFormatFactsForPrompt_HandlesMissingAnchor(t *testing.T) {
	facts := []AtomicFact{{ID: "f1", Type: "claim", Claim: "X."}}
	got := formatFactsForPrompt(facts)
	if strings.Contains(got, `""`) {
		t.Errorf("empty anchor should not render quotes; got %q", got)
	}
	if !strings.Contains(got, "[f1, claim] X.") {
		t.Errorf("expected core fact line; got %q", got)
	}
}

func TestApplyAbsorbDefaults_FactsModelDefaultsHaiku(t *testing.T) {
	cfg := AbsorbConfig{}
	applyAbsorbDefaults(&cfg)
	if cfg.FactsModel != "haiku" {
		t.Errorf("FactsModel default = %q, want haiku", cfg.FactsModel)
	}
	if cfg.FactsTimeoutMin != 3 {
		t.Errorf("FactsTimeoutMin default = %d, want 3", cfg.FactsTimeoutMin)
	}
}

func TestApplyAbsorbDefaults_AtomicFactsRespectsExplicitTrue(t *testing.T) {
	trueV := true
	cfg := AbsorbConfig{AtomicFacts: &trueV}
	applyAbsorbDefaults(&cfg)
	if cfg.AtomicFacts == nil || !*cfg.AtomicFacts {
		t.Errorf("explicit AtomicFacts=true should survive defaults; got %v", cfg.AtomicFacts)
	}
}

func TestApplyAbsorbDefaults_AtomicFactsExplicitFalsePreserved(t *testing.T) {
	falseV := false
	cfg := AbsorbConfig{AtomicFacts: &falseV}
	applyAbsorbDefaults(&cfg)
	if cfg.AtomicFacts == nil || *cfg.AtomicFacts {
		t.Errorf("explicit AtomicFacts=false should survive defaults; got %v", cfg.AtomicFacts)
	}
}

// ---- helpers ----

func writeJSON(t *testing.T, dir, name string, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeBytesPhase3B is named to avoid collision with the existing
// writeBytes helper in absorb_chapter_test.go (Go test files in the
// same package share the symbol table). Same shape, different name.
func writeBytesPhase3B(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// silence unused warning if reflect ever gets removed from a refactor
var _ = reflect.DeepEqual
