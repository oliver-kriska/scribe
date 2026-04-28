package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 3A test scope: TOC sidecar IO, byte-offset computation,
// and the 3-tier chunker (TOC / headings / fixed-token). Pure-Go
// — no marker subprocess, no claude calls.

// ---- TOC sidecar ----

func TestStripFrontmatter_RemovesYAMLBlock(t *testing.T) {
	in := "---\ntitle: foo\n---\nbody text\nmore\n"
	got := stripFrontmatter(in)
	want := "body text\nmore\n"
	if got != want {
		t.Errorf("stripFrontmatter:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestStripFrontmatter_NoFrontmatter(t *testing.T) {
	in := "no fm here\njust body\n"
	got := stripFrontmatter(in)
	if got != in {
		t.Errorf("expected unchanged; got %q", got)
	}
}

func TestComputeBodyOffsets_AssignsAndChainsLengths(t *testing.T) {
	body := "intro paragraph\n\n# Chapter One\nbody one\n\n# Chapter Two\nbody two\n\n# Chapter Three\nbody three\n"
	chapters := []ChapterEntry{
		{Title: "Chapter One"},
		{Title: "Chapter Two"},
		{Title: "Chapter Three"},
	}
	computeBodyOffsets(body, chapters)

	if chapters[0].BodyOffset == 0 {
		t.Error("chapter 1 should have non-zero offset")
	}
	if chapters[1].BodyOffset <= chapters[0].BodyOffset {
		t.Errorf("chapter 2 offset %d not after chapter 1 %d", chapters[1].BodyOffset, chapters[0].BodyOffset)
	}
	if chapters[2].BodyOffset <= chapters[1].BodyOffset {
		t.Errorf("chapter 3 offset %d not after chapter 2 %d", chapters[2].BodyOffset, chapters[1].BodyOffset)
	}
	// Body lengths should chain: c1.length = c2.offset - c1.offset
	if got := chapters[0].BodyLength; got != chapters[1].BodyOffset-chapters[0].BodyOffset {
		t.Errorf("chapter 1 length = %d, want %d", got, chapters[1].BodyOffset-chapters[0].BodyOffset)
	}
	// Last chapter eats the rest.
	if got := chapters[2].BodyLength; got != len(body)-chapters[2].BodyOffset {
		t.Errorf("last chapter length = %d, want %d", got, len(body)-chapters[2].BodyOffset)
	}
}

func TestComputeBodyOffsets_SkipsMissingTitles(t *testing.T) {
	body := "# Real Chapter\nactual body\n"
	chapters := []ChapterEntry{
		{Title: "Real Chapter"},
		{Title: "Phantom Chapter"}, // not in body
		{Title: ""},                // empty title
	}
	computeBodyOffsets(body, chapters)
	if chapters[0].BodyOffset == 0 {
		t.Error("real chapter should resolve")
	}
	if chapters[1].BodyOffset != 0 {
		t.Error("phantom chapter should not resolve")
	}
	if chapters[2].BodyOffset != 0 {
		t.Error("empty-title chapter should not resolve")
	}
}

func TestWriteTOCSidecar_EmptyChaptersIsNoop(t *testing.T) {
	tmp := t.TempDir()
	rawPath := filepath.Join(tmp, "article.md")
	if err := os.WriteFile(rawPath, []byte("# X\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stats := &MarkerStats{Pages: 5} // no chapters
	if err := writeTOCSidecar(rawPath, "src.pdf", stats); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tocSidecarPath(rawPath)); err == nil {
		t.Error("expected no sidecar for stats with empty chapters")
	}
}

func TestWriteTOCSidecar_NilStatsIsNoop(t *testing.T) {
	tmp := t.TempDir()
	rawPath := filepath.Join(tmp, "article.md")
	_ = os.WriteFile(rawPath, []byte("body"), 0o644)
	if err := writeTOCSidecar(rawPath, "src.pdf", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tocSidecarPath(rawPath)); err == nil {
		t.Error("expected no sidecar for nil stats")
	}
}

func TestWriteAndReadTOCSidecar_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	rawPath := filepath.Join(tmp, "article.md")
	body := "---\ntitle: x\n---\n# Chapter A\ntext A\n\n# Chapter B\ntext B\n"
	if err := os.WriteFile(rawPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	stats := &MarkerStats{
		Pages: 12,
		Chapters: []ChapterEntry{
			{Title: "Chapter A", HeadingLvl: 1, PageID: 0},
			{Title: "Chapter B", HeadingLvl: 1, PageID: 5},
		},
	}
	if err := writeTOCSidecar(rawPath, "source.pdf", stats); err != nil {
		t.Fatal(err)
	}
	got, err := readTOCSidecar(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected sidecar to be readable")
	}
	if got.Version != tocSidecarVersion {
		t.Errorf("version = %d, want %d", got.Version, tocSidecarVersion)
	}
	if got.SourcePDF != "source.pdf" {
		t.Errorf("source_pdf = %q", got.SourcePDF)
	}
	if got.Pages != 12 {
		t.Errorf("pages = %d, want 12", got.Pages)
	}
	if len(got.Chapters) != 2 {
		t.Fatalf("expected 2 chapters, got %d", len(got.Chapters))
	}
	if got.Chapters[0].Title != "Chapter A" {
		t.Errorf("chapter 0 title = %q", got.Chapters[0].Title)
	}
	if got.Chapters[0].BodyOffset == 0 {
		t.Error("chapter A should have a non-zero body offset")
	}
}

func TestReadTOCSidecar_VersionMismatchReturnsNil(t *testing.T) {
	tmp := t.TempDir()
	rawPath := filepath.Join(tmp, "article.md")
	_ = os.WriteFile(rawPath, []byte("body"), 0o644)
	bad := TOCSidecar{Version: 99, Chapters: []ChapterEntry{{Title: "x"}}}
	data, _ := json.Marshal(bad)
	if err := os.WriteFile(tocSidecarPath(rawPath), data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readTOCSidecar(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("future-version sidecar should return nil; got %+v", got)
	}
}

func TestReadTOCSidecar_MissingReturnsNilNil(t *testing.T) {
	tmp := t.TempDir()
	rawPath := filepath.Join(tmp, "article.md")
	got, err := readTOCSidecar(rawPath)
	if err != nil {
		t.Errorf("missing sidecar should be (nil, nil); got err=%v", err)
	}
	if got != nil {
		t.Errorf("missing sidecar should be nil; got %+v", got)
	}
}

func TestArticleHasTOCSidecar(t *testing.T) {
	tmp := t.TempDir()
	rawPath := filepath.Join(tmp, "article.md")
	if articleHasTOCSidecar(rawPath) {
		t.Error("expected false for missing sidecar")
	}
	_ = os.WriteFile(tocSidecarPath(rawPath), []byte("{}"), 0o644)
	if !articleHasTOCSidecar(rawPath) {
		t.Error("expected true after sidecar created")
	}
}

func TestRemoveTOCSidecar(t *testing.T) {
	tmp := t.TempDir()
	rawPath := filepath.Join(tmp, "article.md")
	_ = os.WriteFile(tocSidecarPath(rawPath), []byte("{}"), 0o644)
	removeTOCSidecar(rawPath)
	if _, err := os.Stat(tocSidecarPath(rawPath)); err == nil {
		t.Error("expected sidecar removed")
	}
	// Should not error on already-missing sidecar.
	removeTOCSidecar(rawPath)
}

// ---- Chunker ----

func TestChunkByTokens_SmallInputSingleChunk(t *testing.T) {
	body := "tiny body text"
	got := chunkByTokens(body, chunkOptions{MaxBytes: 100})
	if len(got) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(got))
	}
	if got[0].Body != body {
		t.Errorf("body = %q", got[0].Body)
	}
}

func TestChunkByTokens_SplitsParagraphs(t *testing.T) {
	body := strings.Repeat("paragraph one is fairly long.\n\n", 20)
	got := chunkByTokens(body, chunkOptions{MaxBytes: 100})
	if len(got) < 2 {
		t.Errorf("expected multiple chunks, got %d", len(got))
	}
	for _, c := range got {
		if len(c.Body) > 100 && !strings.HasPrefix(c.Title, "(body)") {
			t.Errorf("chunk exceeds max bytes: %d", len(c.Body))
		}
	}
}

func TestChunkByTokens_EmptyInput(t *testing.T) {
	got := chunkByTokens("", chunkOptions{MaxBytes: 100})
	if len(got) != 1 {
		t.Errorf("expected single empty chunk, got %d", len(got))
	}
	if got[0].Title != "(body)" {
		t.Errorf("empty body should produce (body) title; got %q", got[0].Title)
	}
}

func TestChunkByTokens_MonolithicParagraphForceSplit(t *testing.T) {
	body := strings.Repeat("x", 500) // single 500-byte paragraph, no breaks
	got := chunkByTokens(body, chunkOptions{MaxBytes: 100})
	if len(got) < 5 {
		t.Errorf("expected ≥5 chunks for 500/100, got %d", len(got))
	}
	for _, c := range got {
		if len(c.Body) > 100 {
			t.Errorf("chunk over max: len=%d", len(c.Body))
		}
	}
}

func TestChunkByHeadings_SplitsOnH1AndH2(t *testing.T) {
	body := "intro before any heading\n\n# Section One\nbody one\n\n## Subsection\nsubbody\n\n# Section Two\nbody two\n"
	got := chunkByHeadings(body, chunkOptions{MaxBytes: 1024})
	if len(got) < 4 {
		t.Fatalf("expected 4 chunks (intro + 3 headings), got %d: %+v", len(got), titleList(got))
	}
	if got[0].Title != "(intro)" {
		t.Errorf("first chunk should be intro; got %q", got[0].Title)
	}
	titles := titleList(got)
	wantTitles := []string{"(intro)", "Section One", "Subsection", "Section Two"}
	for i, want := range wantTitles {
		if i >= len(titles) || titles[i] != want {
			t.Errorf("chunk[%d] title = %q, want %q", i, titles[i], want)
		}
	}
}

func TestChunkByHeadings_NoHeadingsReturnsNil(t *testing.T) {
	body := "just paragraphs\n\nno markdown headings here\n"
	got := chunkByHeadings(body, chunkOptions{MaxBytes: 1024})
	if got != nil {
		t.Errorf("expected nil for no-heading input, got %v", got)
	}
}

func TestChunkByHeadings_LevelDetection(t *testing.T) {
	body := "# H1\nx\n\n## H2\ny\n\n### H3\nz\n"
	got := chunkByHeadings(body, chunkOptions{MaxBytes: 1024})
	if len(got) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(got))
	}
	for i, want := range []int{1, 2, 3} {
		if got[i].Level != want {
			t.Errorf("chunk[%d] level = %d, want %d", i, got[i].Level, want)
		}
	}
}

func TestChunkByHeadings_OverflowGetsTokenSplit(t *testing.T) {
	huge := strings.Repeat("a paragraph of text.\n\n", 200)
	body := "# Big Section\n" + huge
	got := chunkByHeadings(body, chunkOptions{MaxBytes: 200})
	if len(got) < 2 {
		t.Errorf("expected overflow split into multiple parts, got %d", len(got))
	}
	for _, c := range got {
		if !strings.Contains(c.Title, "Big Section") {
			t.Errorf("split chunks should retain title 'Big Section'; got %q", c.Title)
		}
		if !strings.Contains(c.Title, "part ") {
			t.Errorf("expected (part N/M) suffix on split title; got %q", c.Title)
		}
	}
}

func TestChunkByTOC_HappyPath(t *testing.T) {
	body := "# Chapter One\nbody one stuff\n\n# Chapter Two\nbody two stuff\n\n# Chapter Three\nbody three stuff\n"
	chapters := []ChapterEntry{
		{Title: "Chapter One", PageID: 0},
		{Title: "Chapter Two", PageID: 5},
		{Title: "Chapter Three", PageID: 10},
	}
	computeBodyOffsets(body, chapters)
	got := chunkByTOC(body, chapters, chunkOptions{MaxBytes: 1024})
	if len(got) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(got))
	}
	if got[0].Title != "Chapter One" {
		t.Errorf("first chunk title = %q", got[0].Title)
	}
	if got[1].Title != "Chapter Two" {
		t.Errorf("second chunk title = %q", got[1].Title)
	}
	if !strings.Contains(got[0].Body, "body one") {
		t.Errorf("chunk 1 body missing content: %q", got[0].Body)
	}
	if got[0].SourcePages == nil || got[0].SourcePages[0] != 0 {
		t.Errorf("chunk 1 source pages = %v", got[0].SourcePages)
	}
}

func TestChunkByTOC_PartialResolutionFallsBack(t *testing.T) {
	body := "# Chapter One\nbody\n"
	chapters := []ChapterEntry{
		{Title: "Chapter One"},
		{Title: "Phantom Chapter Two"},
		{Title: "Phantom Chapter Three"},
		{Title: "Phantom Chapter Four"},
	}
	computeBodyOffsets(body, chapters)
	got := chunkByTOC(body, chapters, chunkOptions{MaxBytes: 1024})
	if got != nil {
		t.Errorf("expected nil when fewer than half resolve; got %d chunks", len(got))
	}
}

func TestChunkArticle_DispatchesToTOCWhenAvailable(t *testing.T) {
	tmp := t.TempDir()
	rawPath := filepath.Join(tmp, "article.md")
	body := "---\ntitle: x\n---\n# Chapter A\nbody A\n\n# Chapter B\nbody B\n"
	if err := os.WriteFile(rawPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	stats := &MarkerStats{
		Pages: 30,
		Chapters: []ChapterEntry{
			{Title: "Chapter A", PageID: 0},
			{Title: "Chapter B", PageID: 15},
		},
	}
	if err := writeTOCSidecar(rawPath, "src.pdf", stats); err != nil {
		t.Fatal(err)
	}
	strategy, chunks, err := ChunkArticle(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	if strategy != "toc" {
		t.Errorf("strategy = %q, want toc", strategy)
	}
	if len(chunks) != 2 {
		t.Errorf("expected 2 chunks, got %d", len(chunks))
	}
}

func TestChunkArticle_FallsBackToHeadingsWithoutSidecar(t *testing.T) {
	tmp := t.TempDir()
	rawPath := filepath.Join(tmp, "article.md")
	body := "---\ntitle: x\n---\nintro\n\n# Section One\ntext\n\n# Section Two\ntext\n"
	if err := os.WriteFile(rawPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	strategy, chunks, err := ChunkArticle(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	if strategy != "headings" {
		t.Errorf("strategy = %q, want headings", strategy)
	}
	if len(chunks) < 2 {
		t.Errorf("expected ≥2 chunks, got %d", len(chunks))
	}
}

func TestChunkArticle_SingleChunkForShortFlatDoc(t *testing.T) {
	tmp := t.TempDir()
	rawPath := filepath.Join(tmp, "article.md")
	body := "---\ntitle: x\n---\njust some prose, no headings, very short\n"
	if err := os.WriteFile(rawPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	strategy, chunks, err := ChunkArticle(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	if strategy != "single" {
		t.Errorf("strategy = %q, want single", strategy)
	}
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(chunks))
	}
}

func TestChunkArticle_FallsBackToTokensOnLongFlatDoc(t *testing.T) {
	tmp := t.TempDir()
	rawPath := filepath.Join(tmp, "article.md")
	huge := "---\ntitle: x\n---\n" + strings.Repeat("paragraph of text content here.\n\n", 2000)
	if err := os.WriteFile(rawPath, []byte(huge), 0o644); err != nil {
		t.Fatal(err)
	}
	strategy, chunks, err := ChunkArticle(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	if strategy != "tokens" {
		t.Errorf("strategy = %q, want tokens", strategy)
	}
	if len(chunks) < 2 {
		t.Errorf("expected ≥2 chunks for long flat doc, got %d", len(chunks))
	}
}

func TestFmtPart(t *testing.T) {
	cases := []struct {
		title      string
		idx, total int
		want       string
	}{
		{"Chapter 1", 1, 1, "Chapter 1"}, // singleton — no suffix
		{"Chapter 1", 2, 3, "Chapter 1 (part 2/3)"},
		{"", 1, 2, "(part) (part 1/2)"},
	}
	for _, c := range cases {
		got := fmtPart(c.title, c.idx, c.total)
		if got != c.want {
			t.Errorf("fmtPart(%q, %d, %d) = %q, want %q", c.title, c.idx, c.total, got, c.want)
		}
	}
}

func TestItoa(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{42, "42"},
		{-7, "-7"},
		{1000, "1000"},
	}
	for _, c := range cases {
		if got := itoa(c.n); got != c.want {
			t.Errorf("itoa(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// titleList collects chunk titles for tabular assertions.
func titleList(chunks []Chunk) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.Title
	}
	return out
}
