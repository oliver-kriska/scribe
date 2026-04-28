package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 2B test scope: marker meta.json parsing, frontmatter stats
// emission, and the inbox idempotency state file. Real marker runs
// are still hand-verified; these tests exercise the pure-Go layer.

func TestReadMarkerStats_PureDigitalText(t *testing.T) {
	tmp := t.TempDir()
	mdPath := filepath.Join(tmp, "doc.md")
	if err := os.WriteFile(mdPath, []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{
		"page_stats": []map[string]any{
			{"page_id": 0, "text_extraction_method": "pdftext"},
			{"page_id": 1, "text_extraction_method": "pdftext"},
			{"page_id": 2, "text_extraction_method": "pdftext"},
		},
	}
	writeMeta(t, tmp, "doc", meta)

	got := readMarkerStats(mdPath)
	if got == nil {
		t.Fatal("expected stats, got nil")
	}
	if got.Pages != 3 {
		t.Errorf("Pages = %d, want 3", got.Pages)
	}
	if got.OCRPages != 0 {
		t.Errorf("OCRPages = %d, want 0", got.OCRPages)
	}
	if got.ExtractionMode != "pdftext" {
		t.Errorf("ExtractionMode = %q, want pdftext", got.ExtractionMode)
	}
	if got.OCRPct != 0 {
		t.Errorf("OCRPct = %f, want 0", got.OCRPct)
	}
}

func TestReadMarkerStats_AllOCR(t *testing.T) {
	tmp := t.TempDir()
	mdPath := filepath.Join(tmp, "scan.md")
	_ = os.WriteFile(mdPath, []byte("body"), 0o644)
	meta := map[string]any{
		"page_stats": []map[string]any{
			{"text_extraction_method": "surya"},
			{"text_extraction_method": "surya"},
		},
	}
	writeMeta(t, tmp, "scan", meta)

	got := readMarkerStats(mdPath)
	if got == nil || got.ExtractionMode != "ocr" {
		t.Fatalf("expected ocr mode, got %+v", got)
	}
	if got.OCRPct != 1.0 {
		t.Errorf("OCRPct = %f, want 1.0", got.OCRPct)
	}
}

func TestReadMarkerStats_Mixed(t *testing.T) {
	tmp := t.TempDir()
	mdPath := filepath.Join(tmp, "mix.md")
	_ = os.WriteFile(mdPath, []byte("body"), 0o644)
	meta := map[string]any{
		"page_stats": []map[string]any{
			{"text_extraction_method": "pdftext"},
			{"text_extraction_method": "surya"},
			{"text_extraction_method": "pdftext"},
			{"text_extraction_method": "surya"},
		},
	}
	writeMeta(t, tmp, "mix", meta)

	got := readMarkerStats(mdPath)
	if got == nil {
		t.Fatal("expected stats, got nil")
	}
	if got.ExtractionMode != "mixed" {
		t.Errorf("ExtractionMode = %q, want mixed", got.ExtractionMode)
	}
	if got.OCRPct != 0.5 {
		t.Errorf("OCRPct = %f, want 0.5", got.OCRPct)
	}
}

func TestReadMarkerStats_MissingMetaFileReturnsNil(t *testing.T) {
	tmp := t.TempDir()
	mdPath := filepath.Join(tmp, "lonely.md")
	_ = os.WriteFile(mdPath, []byte("body"), 0o644)
	if got := readMarkerStats(mdPath); got != nil {
		t.Errorf("expected nil for missing meta.json, got %+v", got)
	}
}

func TestReadMarkerStats_MalformedJSONReturnsNil(t *testing.T) {
	tmp := t.TempDir()
	mdPath := filepath.Join(tmp, "broken.md")
	_ = os.WriteFile(mdPath, []byte("body"), 0o644)
	_ = os.WriteFile(filepath.Join(tmp, "broken_meta.json"), []byte("{not json"), 0o644)
	if got := readMarkerStats(mdPath); got != nil {
		t.Errorf("expected nil for malformed meta.json, got %+v", got)
	}
}

func writeMeta(t *testing.T, dir, stem string, meta map[string]any) {
	t.Helper()
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stem+"_meta.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildRawArticleWithStats_EmitsExtractionMode(t *testing.T) {
	tmp := t.TempDir()
	stats := &MarkerStats{
		Pages:          7,
		OCRPages:       3,
		OCRPct:         0.4285714,
		ExtractionMode: "mixed",
	}
	_, content := buildRawArticleWithStats(
		tmp, "file:///report.pdf", "Sample Report", "body text\n",
		"inbox", "general", nil, stats)

	for _, want := range []string{
		"pages: 7",
		"extraction_mode: mixed",
		"ocr_pct: 0.43",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("frontmatter missing %q in:\n%s", want, content)
		}
	}
}

func TestBuildRawArticleWithStats_NilStatsOmitsKeys(t *testing.T) {
	tmp := t.TempDir()
	_, content := buildRawArticleWithStats(
		tmp, "file:///note.html", "A Note", "body\n",
		"inbox", "general", nil, nil)

	for _, banned := range []string{"extraction_mode:", "ocr_pct:", "pages:"} {
		if strings.Contains(content, banned) {
			t.Errorf("nil stats must not emit %q; got:\n%s", banned, content)
		}
	}
}

func TestBuildRawArticleWithStats_ZeroPagesOmitsKeys(t *testing.T) {
	tmp := t.TempDir()
	stats := &MarkerStats{Pages: 0}
	_, content := buildRawArticleWithStats(
		tmp, "file:///empty.pdf", "Empty", "body\n",
		"inbox", "general", nil, stats)

	if strings.Contains(content, "pages:") {
		t.Errorf("zero-page stats must not emit pages key; got:\n%s", content)
	}
}

func TestSHA256File_StableHash(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "data.bin")
	if err := os.WriteFile(src, []byte("scriptorium"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := sha256File(src)
	if err != nil {
		t.Fatal(err)
	}
	b, err := sha256File(src)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("hash unstable: %s != %s", a, b)
	}
	if len(a) != 64 {
		t.Errorf("expected 64-char hex digest, got %d (%s)", len(a), a)
	}
}

func TestSHA256File_MissingReturnsError(t *testing.T) {
	tmp := t.TempDir()
	_, err := sha256File(filepath.Join(tmp, "nope"))
	if err == nil {
		t.Error("expected error hashing missing file")
	}
}

func TestInboxState_LookupRecordPersistRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	state := loadInboxState(tmp)
	if state == nil {
		t.Fatal("loadInboxState returned nil for empty dir")
	}
	if _, hit := state.lookup("deadbeef"); hit {
		t.Error("expected miss on empty state")
	}
	state.record("deadbeef", inboxEntry{
		Filename:    "report.pdf",
		RawPath:     "raw/articles/2026-04-28-report.md",
		ProcessedAt: "2026-04-28T10:00:00Z",
	})
	if err := state.persist(tmp); err != nil {
		t.Fatalf("persist: %v", err)
	}
	// Re-load and verify.
	reload := loadInboxState(tmp)
	got, hit := reload.lookup("deadbeef")
	if !hit {
		t.Fatal("expected hit after persist + reload")
	}
	if got.Filename != "report.pdf" {
		t.Errorf("Filename = %q", got.Filename)
	}
	if got.RawPath != "raw/articles/2026-04-28-report.md" {
		t.Errorf("RawPath = %q", got.RawPath)
	}
}

func TestInboxState_PersistWithZeroEntriesIsNoop(t *testing.T) {
	tmp := t.TempDir()
	state := loadInboxState(tmp)
	if err := state.persist(tmp); err != nil {
		t.Fatalf("persist: %v", err)
	}
	// Should not have created the state file (no entries to record).
	if _, err := os.Stat(filepath.Join(tmp, inboxStateFilename)); err == nil {
		t.Error("expected no state file for empty entries")
	}
}

func TestInboxState_CorruptStateFileFallsBackToEmpty(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, inboxStateFilename), []byte("{not valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := loadInboxState(tmp)
	if state == nil {
		t.Fatal("expected non-nil state on corrupt file")
	}
	if len(state.Entries) != 0 {
		t.Errorf("expected empty entries on corrupt file, got %d", len(state.Entries))
	}
}

func TestArchiveDuplicate_RenamesWithDupPrefix(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "again.pdf")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	processedDir := filepath.Join(tmp, "processed")
	if err := os.MkdirAll(processedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := archiveDuplicate(src, processedDir); err != nil {
		t.Fatalf("archiveDuplicate: %v", err)
	}
	if _, err := os.Stat(src); err == nil {
		t.Error("expected source removed after archive")
	}
	entries, err := os.ReadDir(processedDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 archived file, got %d", len(entries))
	}
	name := entries[0].Name()
	if !strings.HasPrefix(name, "dup-") {
		t.Errorf("archived name should start with dup-, got %q", name)
	}
	if !strings.HasSuffix(name, "-again.pdf") {
		t.Errorf("archived name should end with original filename, got %q", name)
	}
}

func TestDrainFileInbox_DuplicateIsDeduplicated(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "raw", "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	processedDir := filepath.Join(inbox, ".processed")
	if err := os.MkdirAll(processedDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-seed the state with a hash matching the file we're about
	// to drop. That simulates "this content was already ingested
	// in a previous drain" without needing to actually run marker.
	bytes := []byte("identical content\n")
	preSeed := loadInboxState(inbox)
	hashStr, err := sha256Bytes(bytes)
	if err != nil {
		t.Fatal(err)
	}
	preSeed.record(hashStr, inboxEntry{
		Filename:    "report.txt",
		RawPath:     "raw/articles/2026-04-28-report.md",
		ProcessedAt: "2026-04-28T10:00:00Z",
	})
	if err := preSeed.persist(inbox); err != nil {
		t.Fatal(err)
	}

	// Drop a fresh copy with a different filename — same content.
	dup := filepath.Join(inbox, "report-copy.txt")
	if err := os.WriteFile(dup, bytes, 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := drainFileInbox(root)
	if err != nil {
		t.Fatalf("drainFileInbox: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 newly ingested (was a duplicate), got %d", n)
	}

	// The duplicate must have been moved to .processed/ with a dup- prefix.
	entries, err := os.ReadDir(processedDir)
	if err != nil {
		t.Fatal(err)
	}
	var dupArchived bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "dup-") {
			dupArchived = true
			break
		}
	}
	if !dupArchived {
		t.Errorf("expected dup-* file in .processed/, got entries: %v", entries)
	}

	// And no new article should exist (since drain bailed before write).
	articlesDir := filepath.Join(root, "raw", "articles")
	if _, err := os.Stat(articlesDir); err == nil {
		entries, _ := os.ReadDir(articlesDir)
		if len(entries) != 0 {
			t.Errorf("expected no new articles for duplicate; got %d", len(entries))
		}
	}
}

// sha256Bytes is a small test helper that hashes an in-memory slice;
// production code only ever hashes files on disk.
func sha256Bytes(b []byte) (string, error) {
	tmp, err := os.CreateTemp("", "sha-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		return "", err
	}
	tmp.Close()
	return sha256File(tmp.Name())
}
