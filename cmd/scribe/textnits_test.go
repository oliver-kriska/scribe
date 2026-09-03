package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- L17 / L14 / L08 / L09 / L12: text and bookkeeping nits ----

func TestDeriveResearchTitle_FirstRuneUpper(t *testing.T) {
	if got := deriveResearchTitle("2026-04-09-élan-vital-über.md"); got != "Élan Vital Über" {
		t.Errorf("got %q", got)
	}
}

func TestCompareScalars_RefusesCollections(t *testing.T) {
	if _, ok := compareScalars([]any{"a"}, "a"); ok {
		t.Error("a list has no scalar order")
	}
	if _, ok := compareScalars("a", map[string]any{"k": 1}); ok {
		t.Error("a map has no scalar order")
	}
	if c, ok := compareScalars("a", "b"); !ok || c >= 0 {
		t.Error("scalars still compare")
	}
}

func TestChunkByTOC_SkipsUnresolvedChapter(t *testing.T) {
	body := "# One\naaaa\n# Three\ncccc\n"
	chapters := []ChapterEntry{
		{Title: "One", BodyOffset: 0, BodyLength: 11},
		{Title: "Two (phantom)"},
		{Title: "Three", BodyOffset: 11, BodyLength: 13},
	}
	got := chunkByTOC(body, chapters, chunkOptions{MaxBytes: 1024})
	if len(got) != 2 {
		t.Fatalf("want 2 chunks, got %d: %+v", len(got), got)
	}
	for _, c := range got {
		if c.Body == body {
			t.Errorf("unresolved chapter %q emitted the whole body", c.Title)
		}
	}
}

func TestBuildRawArticleWithStats_SlugCollisionSuffix(t *testing.T) {
	root := t.TempDir()
	p1, c1 := buildRawArticleWithStats(root, "https://a.test/1", "Same Title", "body one", "local", "general", nil, nil)
	if err := os.MkdirAll(filepath.Dir(p1), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p1, []byte(c1), 0o644); err != nil {
		t.Fatal(err)
	}
	p2, _ := buildRawArticleWithStats(root, "https://a.test/2", "Same Title", "body two", "local", "general", nil, nil)
	if p2 == p1 || !strings.HasSuffix(p2, "-2.md") {
		t.Errorf("second article must not overwrite the first: %s vs %s", p1, p2)
	}
}

func TestBuildContradictionLedger_KeepsResolvedWhenEdgesGone(t *testing.T) {
	dir := t.TempDir()
	writeArticleForLedger(t, dir, "decisions", "a", "Decision A", "decision", []string{"Decision B"})
	writeArticleForLedger(t, dir, "decisions", "b", "Decision B", "decision", []string{"Decision A"})
	if _, _, err := buildContradictionLedger(dir); err != nil {
		t.Fatal(err)
	}
	entries, _ := readContradictionLedger(dir)
	if err := resolveContradiction(dir, entries[0].ID, "reconciled by hand"); err != nil {
		t.Fatal(err)
	}
	writeArticleForLedger(t, dir, "decisions", "a", "Decision A", "decision", nil)
	writeArticleForLedger(t, dir, "decisions", "b", "Decision B", "decision", nil)
	if _, _, err := buildContradictionLedger(dir); err != nil {
		t.Fatal(err)
	}
	entries, _ = readContradictionLedger(dir)
	if len(entries) != 1 || entries[0].ResolutionNote != "reconciled by hand" || entries[0].ResolvedAt == "" {
		t.Errorf("resolved entry must survive edge removal as a paper trail: %+v", entries)
	}
}

// ---- L23: WARN+ to stderr ----

func TestTextHandler_WarnGoesToStderr(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origErr, origOut := os.Stderr, os.Stdout
	rOut, wOut, _ := os.Pipe()
	os.Stderr, os.Stdout = w, wOut
	h := &scribeTextHandler{level: logLevel}
	_ = h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelWarn, "careful", 0))
	_ = h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "fine", 0))
	w.Close()
	wOut.Close()
	os.Stderr, os.Stdout = origErr, origOut
	errText, _ := io.ReadAll(r)
	outText, _ := io.ReadAll(rOut)
	if !strings.Contains(string(errText), "careful") || strings.Contains(string(errText), "fine") {
		t.Errorf("stderr = %q", errText)
	}
	if !strings.Contains(string(outText), "fine") || strings.Contains(string(outText), "careful") {
		t.Errorf("stdout = %q", outText)
	}

	var low, high bytes.Buffer
	split := &levelSplitHandler{low: slog.NewJSONHandler(&low, nil), high: slog.NewJSONHandler(&high, nil)}
	logger := slog.New(split.WithAttrs([]slog.Attr{slog.String("script", "t")}))
	logger.Info("fine")
	logger.Error("bad")
	if !strings.Contains(low.String(), "fine") || strings.Contains(low.String(), "bad") || !strings.Contains(high.String(), "bad") {
		t.Errorf("json split: low=%q high=%q", low.String(), high.String())
	}
}
