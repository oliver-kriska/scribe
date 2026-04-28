package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// Phase 3A.5 test scope: dispatcher decision (shouldAbsorbChaptered),
// merge semantics (mergeChapterPlans, mergeStringSet), and the
// helper readers (readArticleTitle, slugifyForChunk). The actual
// runPass1Chaptered loop is integration-only — it shells out to
// claude — so we exercise it by hand against a real article rather
// than mocking runClaude in unit tests.

func TestShouldAbsorbChaptered_RequiresAllSignals(t *testing.T) {
	tmp := t.TempDir()
	rawPath := filepath.Join(tmp, "doc.md")
	body := "---\ntitle: x\n---\n# Chapter A\nbody A\n\n# Chapter B\nbody B\n\n# Chapter C\nbody C\n\n# Chapter D\nbody D\n"
	if err := os.WriteFile(rawPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	trueV := true
	falseV := false

	t.Run("ChapterAware off short-circuits", func(t *testing.T) {
		cfg := AbsorbConfig{ChapterAware: &falseV, ChapterThreshold: 3}
		ok, _, _ := shouldAbsorbChaptered(rawPath, cfg)
		if ok {
			t.Error("expected false when ChapterAware is false")
		}
	})

	t.Run("nil ChapterAware short-circuits (defensive)", func(t *testing.T) {
		cfg := AbsorbConfig{ChapterThreshold: 3}
		ok, _, _ := shouldAbsorbChaptered(rawPath, cfg)
		if ok {
			t.Error("expected false when ChapterAware is nil")
		}
	})

	t.Run("no sidecar short-circuits", func(t *testing.T) {
		cfg := AbsorbConfig{ChapterAware: &trueV, ChapterThreshold: 3}
		ok, _, _ := shouldAbsorbChaptered(rawPath, cfg)
		if ok {
			t.Error("expected false without sidecar")
		}
	})

	t.Run("sidecar with enough chapters triggers", func(t *testing.T) {
		stats := &MarkerStats{
			Pages: 30,
			Chapters: []ChapterEntry{
				{Title: "Chapter A"},
				{Title: "Chapter B"},
				{Title: "Chapter C"},
				{Title: "Chapter D"},
			},
		}
		if err := writeTOCSidecar(rawPath, "src.pdf", stats); err != nil {
			t.Fatal(err)
		}
		cfg := AbsorbConfig{ChapterAware: &trueV, ChapterThreshold: 3}
		ok, chunks, strategy := shouldAbsorbChaptered(rawPath, cfg)
		if !ok {
			t.Fatal("expected chaptered path to fire")
		}
		if strategy != "toc" {
			t.Errorf("strategy = %q, want toc", strategy)
		}
		if len(chunks) < 4 {
			t.Errorf("expected ≥4 chunks, got %d", len(chunks))
		}
	})

	t.Run("threshold above chapter count short-circuits", func(t *testing.T) {
		// Sidecar already on disk from previous subtest with 4 chapters.
		cfg := AbsorbConfig{ChapterAware: &trueV, ChapterThreshold: 10}
		ok, _, _ := shouldAbsorbChaptered(rawPath, cfg)
		if ok {
			t.Error("expected false when threshold > chapter count")
		}
	})
}

func TestMergeChapterPlans_DedupesByLabel(t *testing.T) {
	tmp := t.TempDir()
	rawFile := filepath.Join(tmp, "doc.md")

	plan1 := chapterPlan{
		RawFile:     rawFile,
		SourceTitle: "The Paper",
		Chapter:     "Chapter A",
		Domain:      "research",
		Entities: []absorbEntity{
			{Label: "Pattern X", Type: "pattern", OneLine: "first hook", KeyClaims: []string{"claim 1"}},
			{Label: "Tool Y", Type: "tool", OneLine: "tool hook", KeyClaims: []string{"v2.0"}},
		},
	}
	plan2 := chapterPlan{
		RawFile: rawFile,
		Chapter: "Chapter B",
		Entities: []absorbEntity{
			// Same label as plan1 — should merge.
			{Label: "Pattern X", Type: "pattern", OneLine: "second hook (ignored)", KeyClaims: []string{"claim 2", "claim 1"}},
			{Label: "Decision Z", Type: "decision", OneLine: "decision hook", KeyClaims: []string{"chose B over C"}},
		},
	}

	runs := []chapterRun{
		{index: 0, planJSON: writePlan(t, tmp, "00.json", plan1)},
		{index: 1, planJSON: writePlan(t, tmp, "01.json", plan2)},
	}

	merged, err := mergeChapterPlans(rawFile, runs)
	if err != nil {
		t.Fatal(err)
	}
	if merged.SourceTitle != "The Paper" {
		t.Errorf("source_title = %q", merged.SourceTitle)
	}
	if merged.Domain != "research" {
		t.Errorf("domain = %q, want research", merged.Domain)
	}
	if len(merged.Entities) != 3 {
		t.Fatalf("expected 3 entities (Pattern X merged, Tool Y, Decision Z); got %d: %+v", len(merged.Entities), labels(merged.Entities))
	}
	// Pattern X should be first (preserved order from plan1).
	if merged.Entities[0].Label != "Pattern X" {
		t.Errorf("first label = %q, want Pattern X", merged.Entities[0].Label)
	}
	// First-occurrence one_line wins (plan2's "second hook (ignored)" must not).
	if merged.Entities[0].OneLine != "first hook" {
		t.Errorf("expected plan1's one_line to win on conflict; got %q", merged.Entities[0].OneLine)
	}
	// Pattern X key_claims should be union, deduped.
	wantClaims := []string{"claim 1", "claim 2"}
	got := append([]string(nil), merged.Entities[0].KeyClaims...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, wantClaims) {
		t.Errorf("key_claims = %v, want %v", got, wantClaims)
	}
}

func TestMergeChapterPlans_SkipsMissingAndUnparseable(t *testing.T) {
	tmp := t.TempDir()
	good := chapterPlan{
		Chapter:     "C1",
		SourceTitle: "Title",
		Domain:      "general",
		Entities:    []absorbEntity{{Label: "Real", Type: "pattern"}},
	}
	runs := []chapterRun{
		{index: 0, planJSON: filepath.Join(tmp, "missing.json")}, // never written
		{index: 1, planJSON: writePlan(t, tmp, "ok.json", good)},
		{index: 2, planJSON: writeBytes(t, tmp, "broken.json", []byte("{not json"))},
	}
	merged, err := mergeChapterPlans("raw", runs)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Entities) != 1 || merged.Entities[0].Label != "Real" {
		t.Errorf("expected one entity from the good chunk only; got %+v", merged.Entities)
	}
}

func TestMergeChapterPlans_DefaultsDomainToGeneral(t *testing.T) {
	tmp := t.TempDir()
	plan := chapterPlan{Entities: []absorbEntity{{Label: "X", Type: "pattern"}}}
	runs := []chapterRun{{index: 0, planJSON: writePlan(t, tmp, "00.json", plan)}}
	merged, _ := mergeChapterPlans("raw", runs)
	if merged.Domain != "general" {
		t.Errorf("domain = %q, want general", merged.Domain)
	}
}

func TestMergeChapterPlans_StampsSourceChapter(t *testing.T) {
	tmp := t.TempDir()
	plan0 := chapterPlan{
		Chapter:  "Intro",
		Entities: []absorbEntity{{Label: "Pattern A", Type: "pattern"}},
	}
	plan2 := chapterPlan{
		Chapter:  "Method",
		Entities: []absorbEntity{{Label: "Tool B", Type: "tool"}},
	}
	// Same label re-appearing later; first-seen chapter must win.
	plan5 := chapterPlan{
		Chapter:  "Result",
		Entities: []absorbEntity{{Label: "Pattern A", Type: "pattern", KeyClaims: []string{"extra"}}},
	}
	runs := []chapterRun{
		{index: 0, planJSON: writePlan(t, tmp, "00.json", plan0)},
		{index: 2, planJSON: writePlan(t, tmp, "02.json", plan2)},
		{index: 5, planJSON: writePlan(t, tmp, "05.json", plan5)},
	}
	merged, err := mergeChapterPlans("raw.md", runs)
	if err != nil {
		t.Fatal(err)
	}
	bySrc := map[string]int{}
	for _, e := range merged.Entities {
		if e.SourceChapter == nil {
			t.Errorf("entity %q missing source_chapter", e.Label)
			continue
		}
		bySrc[e.Label] = *e.SourceChapter
	}
	if bySrc["Pattern A"] != 0 {
		t.Errorf("Pattern A source_chapter = %d, want 0 (first-seen wins)", bySrc["Pattern A"])
	}
	if bySrc["Tool B"] != 2 {
		t.Errorf("Tool B source_chapter = %d, want 2", bySrc["Tool B"])
	}
}

func TestMergedFacts_LoadMergedFacts_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	factsDir := filepath.Join(tmp, "output", "facts")
	if err := os.MkdirAll(factsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mf := MergedFacts{
		Version:     factsSchemaVersion,
		RawFile:     "raw/articles/doc.md",
		SourceTitle: "Doc",
		Facts: []AtomicFact{
			{ID: "c00-f1", Type: "claim", Claim: "x", Anchor: "y"},
		},
	}
	data, _ := json.Marshal(mf)
	if err := os.WriteFile(filepath.Join(factsDir, "doc.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadMergedFacts(tmp, "doc.md")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.SourceTitle != "Doc" || len(got.Facts) != 1 {
		t.Fatalf("round-trip lost data: %+v", got)
	}
}

func TestMergedFacts_LoadMergedFacts_MissingReturnsNil(t *testing.T) {
	tmp := t.TempDir()
	got, err := loadMergedFacts(tmp, "absent.md")
	if err != nil {
		t.Fatalf("missing file should not error; got %v", err)
	}
	if got != nil {
		t.Errorf("missing file should return nil; got %+v", got)
	}
}

func TestMergedFacts_LoadMergedFacts_VersionMismatchReturnsNil(t *testing.T) {
	tmp := t.TempDir()
	factsDir := filepath.Join(tmp, "output", "facts")
	if err := os.MkdirAll(factsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Future schema version on disk — loader must shrug and return nil.
	body := []byte(`{"version": 999, "facts": []}`)
	if err := os.WriteFile(filepath.Join(factsDir, "future.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadMergedFacts(tmp, "future.md")
	if err != nil {
		t.Fatalf("schema mismatch should not error; got %v", err)
	}
	if got != nil {
		t.Errorf("schema mismatch should return nil; got %+v", got)
	}
}

func TestMergeStringSet_PreservesOrderAndDedupes(t *testing.T) {
	a := []string{"first", "second", "third"}
	b := []string{"second", "fourth", "first", "fifth", " "}
	got := mergeStringSet(a, b)
	want := []string{"first", "second", "third", "fourth", "fifth"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeStringSet = %v, want %v", got, want)
	}
}

func TestMergeStringSet_BothEmpty(t *testing.T) {
	got := mergeStringSet(nil, nil)
	if len(got) != 0 {
		t.Errorf("expected empty result, got %v", got)
	}
}

func TestReadArticleTitle_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "x.md")
	body := "---\ntitle: \"My Paper Title\"\nsource_url: \"file:///a.pdf\"\n---\nbody\n"
	_ = os.WriteFile(p, []byte(body), 0o644)
	got := readArticleTitle(p)
	if got != "My Paper Title" {
		t.Errorf("title = %q, want My Paper Title", got)
	}
}

func TestReadArticleTitle_UnquotedTitle(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "x.md")
	body := "---\ntitle: Plain Title\n---\nbody\n"
	_ = os.WriteFile(p, []byte(body), 0o644)
	if got := readArticleTitle(p); got != "Plain Title" {
		t.Errorf("title = %q", got)
	}
}

func TestReadArticleTitle_NoFrontmatter(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "x.md")
	_ = os.WriteFile(p, []byte("just body, no fm\n"), 0o644)
	if got := readArticleTitle(p); got != "" {
		t.Errorf("title = %q, want empty", got)
	}
}

func TestReadArticleTitle_MissingFile(t *testing.T) {
	if got := readArticleTitle("/nope/does/not/exist.md"); got != "" {
		t.Errorf("expected empty for missing file; got %q", got)
	}
}

func TestSlugifyForChunk(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Chapter One", "chapter-one"},
		{"3.1 Event-Level Binding", "3-1-event-level-binding"},
		{"!!!", "chunk"}, // empty after slug → fallback
		{"a very very very very very very very very very long heading", "a-very-very-very-very-very-very-very-ver"},
	}
	for _, c := range cases {
		got := slugifyForChunk(c.in)
		if got != c.want {
			t.Errorf("slugifyForChunk(%q) = %q, want %q", c.in, got, c.want)
		}
		if len(got) > 40 {
			t.Errorf("slug exceeds 40 chars: %q", got)
		}
	}
}

func TestAbsorbDefaults_ChapterAwareTrueDefault(t *testing.T) {
	d := absorbDefaults()
	if d.ChapterAware == nil || !*d.ChapterAware {
		t.Errorf("ChapterAware default should be &true; got %v", d.ChapterAware)
	}
	if d.ChapterThreshold != 3 {
		t.Errorf("ChapterThreshold default = %d, want 3", d.ChapterThreshold)
	}
}

func TestApplyAbsorbDefaults_PreservesUserChapterValues(t *testing.T) {
	falseV := false
	cfg := AbsorbConfig{
		ChapterAware:     &falseV, // user explicitly opted out
		ChapterThreshold: 7,
	}
	applyAbsorbDefaults(&cfg)
	if cfg.ChapterAware == nil || *cfg.ChapterAware {
		t.Errorf("user-set ChapterAware=false should survive defaults; got %v", cfg.ChapterAware)
	}
	if cfg.ChapterThreshold != 7 {
		t.Errorf("user-set ChapterThreshold should survive; got %d", cfg.ChapterThreshold)
	}
}

func TestApplyAbsorbDefaults_FillsZeroValues(t *testing.T) {
	cfg := AbsorbConfig{}
	applyAbsorbDefaults(&cfg)
	if cfg.ChapterAware == nil || !*cfg.ChapterAware {
		t.Errorf("zero ChapterAware should default to true; got %v", cfg.ChapterAware)
	}
	if cfg.ChapterThreshold != 3 {
		t.Errorf("zero threshold should default to 3; got %d", cfg.ChapterThreshold)
	}
}

// ---- helpers ----

func writePlan(t *testing.T, dir, name string, plan chapterPlan) string {
	t.Helper()
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeBytes(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func labels(ents []absorbEntity) []string {
	out := make([]string, len(ents))
	for i, e := range ents {
		out[i] = e.Label
	}
	return out
}
