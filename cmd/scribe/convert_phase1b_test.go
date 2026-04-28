package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 1B test scope: DOCX/EPUB tier 0 conversion, watched-inbox
// drain happy + quarantine paths, marker MPS env handling, doctor
// convert-section coverage matrix shape.

// --- DOCX tier 0 ---

// minimalDOCX returns the bytes of a tiny valid DOCX with one paragraph
// reading "Hello world". Built on the fly so tests don't rely on a
// committed binary fixture.
func minimalDOCX(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:body>` + body + `</w:body>
</w:document>`
	if _, err := w.Write([]byte(xml)); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestConvertDOCXTier0_BasicParagraph(t *testing.T) {
	docx := minimalDOCX(t, `<w:p><w:r><w:t>Hello world</w:t></w:r></w:p>`)
	out, err := convertDOCXTier0(docx)
	if err != nil {
		t.Fatalf("convertDOCXTier0: %v", err)
	}
	if !strings.Contains(out, "Hello world") {
		t.Errorf("missing paragraph text; got: %q", out)
	}
}

func TestConvertDOCXTier0_HeadingStyle(t *testing.T) {
	docx := minimalDOCX(t, `
<w:p>
  <w:pPr><w:pStyle w:val="Heading1"/></w:pPr>
  <w:r><w:t>Top Level</w:t></w:r>
</w:p>
<w:p><w:r><w:t>body text</w:t></w:r></w:p>`)
	out, err := convertDOCXTier0(docx)
	if err != nil {
		t.Fatalf("convertDOCXTier0: %v", err)
	}
	if !strings.Contains(out, "# Top Level") {
		t.Errorf("expected `# Top Level`; got: %q", out)
	}
	if !strings.Contains(out, "body text") {
		t.Errorf("missing body paragraph; got: %q", out)
	}
}

func TestConvertDOCXTier0_BoldItalic(t *testing.T) {
	docx := minimalDOCX(t, `<w:p>
  <w:r><w:rPr><w:b/></w:rPr><w:t>bold</w:t></w:r>
  <w:r><w:t> and </w:t></w:r>
  <w:r><w:rPr><w:i/></w:rPr><w:t>italic</w:t></w:r>
</w:p>`)
	out, err := convertDOCXTier0(docx)
	if err != nil {
		t.Fatalf("convertDOCXTier0: %v", err)
	}
	if !strings.Contains(out, "**bold**") {
		t.Errorf("expected `**bold**`; got: %q", out)
	}
	if !strings.Contains(out, "*italic*") {
		t.Errorf("expected `*italic*`; got: %q", out)
	}
}

func TestConvertDOCXTier0_RejectsNonDOCX(t *testing.T) {
	_, err := convertDOCXTier0([]byte("not a zip"))
	if err == nil {
		t.Fatal("expected error on non-DOCX bytes")
	}
}

func TestConvertDOCXTier0_MissingDocumentXML(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("README.txt")
	w.Write([]byte("not a docx"))
	zw.Close()
	_, err := convertDOCXTier0(buf.Bytes())
	if err == nil {
		t.Fatal("expected error when word/document.xml missing")
	}
}

// --- EPUB tier 0 ---

func minimalEPUB(t *testing.T, chapters map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	must := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	must("META-INF/container.xml", `<?xml version="1.0"?>
<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
<rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`)

	manifestItems := ""
	spineItems := ""
	for id := range chapters {
		manifestItems += `<item id="` + id + `" href="` + id + `.xhtml" media-type="application/xhtml+xml"/>`
		spineItems += `<itemref idref="` + id + `"/>`
	}
	must("OEBPS/content.opf", `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf">
<manifest>`+manifestItems+`</manifest>
<spine>`+spineItems+`</spine>
</package>`)

	for id, body := range chapters {
		must("OEBPS/"+id+".xhtml", `<?xml version="1.0"?>
<html xmlns="http://www.w3.org/1999/xhtml"><body>`+body+`</body></html>`)
	}

	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestConvertEPUBTier0_TwoChapters(t *testing.T) {
	epub := minimalEPUB(t, map[string]string{
		"chap1": `<h1>Chapter One</h1><p>Opening line.</p>`,
		"chap2": `<h1>Chapter Two</h1><p>Second line.</p>`,
	})
	out, err := convertEPUBTier0(epub)
	if err != nil {
		t.Fatalf("convertEPUBTier0: %v", err)
	}
	for _, want := range []string{"Chapter One", "Opening line", "Chapter Two", "Second line"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output: %s", want, out)
		}
	}
}

func TestConvertEPUBTier0_RejectsNonZip(t *testing.T) {
	_, err := convertEPUBTier0([]byte("not a zip"))
	if err == nil {
		t.Fatal("expected error on non-zip bytes")
	}
}

func TestConvertEPUBTier0_MissingContainer(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("readme.txt")
	w.Write([]byte("nope"))
	zw.Close()
	_, err := convertEPUBTier0(buf.Bytes())
	if err == nil {
		t.Fatal("expected error when container.xml missing")
	}
}

// --- Marker env handling ---

func TestMarkerEnv_AddsMPSFallbackWhenEnabled(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/home/u"}
	env := markerEnv(base, true)
	found := false
	for _, kv := range env {
		if kv == "PYTORCH_ENABLE_MPS_FALLBACK=1" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected PYTORCH_ENABLE_MPS_FALLBACK=1 in env; got %v", env)
	}
}

func TestMarkerEnv_DoesNotAddWhenDisabled(t *testing.T) {
	base := []string{"PATH=/usr/bin"}
	env := markerEnv(base, false)
	for _, kv := range env {
		if strings.HasPrefix(kv, "PYTORCH_ENABLE_MPS_FALLBACK") {
			t.Errorf("did not expect MPS fallback when disabled; got %s", kv)
		}
	}
}

func TestMarkerEnv_OverridesExistingValue(t *testing.T) {
	base := []string{"PATH=/usr/bin", "PYTORCH_ENABLE_MPS_FALLBACK=0"}
	env := markerEnv(base, true)
	count := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "PYTORCH_ENABLE_MPS_FALLBACK=") {
			count++
			if kv != "PYTORCH_ENABLE_MPS_FALLBACK=1" {
				t.Errorf("user override should win when scribe enables fallback; got %s", kv)
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one MPS fallback entry; got %d", count)
	}
}

// --- Watched-inbox drain ---

// scaffoldKB sets up a minimal KB layout so loadConfig + buildRawArticle
// + drainFileInbox can run without mocking. Returns the root path; the
// caller passes it as a kbDir() override via SCRIBE_KB env or by
// calling drainFileInbox directly.
func scaffoldKB(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"raw/inbox", "raw/articles", "wiki", "scripts"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// scribe init writes scripts/projects.json — our kbDir walker
	// finds the KB by looking for that file. An empty JSON map is
	// enough.
	projects := filepath.Join(root, "scripts", "projects.json")
	if err := os.WriteFile(projects, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write projects.json: %v", err)
	}
	return root
}

func TestDrainFileInbox_ProcessesMD(t *testing.T) {
	root := scaffoldKB(t)
	src := filepath.Join(root, "raw", "inbox", "hello.md")
	if err := os.WriteFile(src, []byte("# Hello\n\nbody"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	n, err := drainFileInbox(root)
	if err != nil {
		t.Fatalf("drainFileInbox: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 processed; got %d", n)
	}
	// Original moved out of inbox.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("expected source moved out of inbox; stat err=%v", err)
	}
	// Article landed in raw/articles/.
	matches, _ := filepath.Glob(filepath.Join(root, "raw", "articles", "*.md"))
	if len(matches) != 1 {
		t.Errorf("expected 1 article; got %d (%v)", len(matches), matches)
	}
	// Original archived under .processed.
	processed, _ := filepath.Glob(filepath.Join(root, "raw", "inbox", ".processed", "*"))
	if len(processed) != 1 {
		t.Errorf("expected 1 archived; got %d (%v)", len(processed), processed)
	}
}

func TestDrainFileInbox_QuarantinesUnsupported(t *testing.T) {
	if markerTierAvailable() {
		t.Skip("marker installed — would actually convert .pptx instead of failing")
	}
	root := scaffoldKB(t)
	src := filepath.Join(root, "raw", "inbox", "deck.pptx")
	if err := os.WriteFile(src, []byte("fake pptx"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	n, err := drainFileInbox(root)
	if err != nil {
		t.Fatalf("drainFileInbox: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 processed for unsupported format; got %d", n)
	}
	// Source should be in .failed/<stem>/, with err.log alongside.
	failedSubs, _ := filepath.Glob(filepath.Join(root, "raw", "inbox", ".failed", "*", "deck.pptx"))
	if len(failedSubs) != 1 {
		t.Errorf("expected quarantined source; got %v", failedSubs)
	}
	logFiles, _ := filepath.Glob(filepath.Join(root, "raw", "inbox", ".failed", "*", "err.log"))
	if len(logFiles) != 1 {
		t.Errorf("expected err.log alongside quarantined source; got %v", logFiles)
	}
}

func TestDrainFileInbox_NoInboxIsNoop(t *testing.T) {
	root := t.TempDir()
	// No raw/inbox dir.
	n, err := drainFileInbox(root)
	if err != nil {
		t.Fatalf("expected no error when inbox missing; got %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 processed; got %d", n)
	}
}

func TestDrainFileInbox_SkipsZeroByteAndDotfiles(t *testing.T) {
	root := scaffoldKB(t)
	zero := filepath.Join(root, "raw", "inbox", "in-progress.md")
	if err := os.WriteFile(zero, []byte{}, 0o644); err != nil {
		t.Fatalf("write zero: %v", err)
	}
	dotfile := filepath.Join(root, "raw", "inbox", ".DS_Store")
	if err := os.WriteFile(dotfile, []byte("noise"), 0o644); err != nil {
		t.Fatalf("write dotfile: %v", err)
	}

	n, err := drainFileInbox(root)
	if err != nil {
		t.Fatalf("drainFileInbox: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 processed; got %d", n)
	}
	// Both originals should still be in place.
	for _, p := range []string{zero, dotfile} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to remain in inbox: %v", filepath.Base(p), err)
		}
	}
}

// --- Doctor convert section ---

func TestCheckConvert_ProducesMatrix(t *testing.T) {
	rows := checkConvert()
	if len(rows) == 0 {
		t.Fatal("expected at least one convert row")
	}
	// First row is always the marker probe.
	if rows[0].Name != "marker (tier 1)" {
		t.Errorf("expected marker row first; got %q", rows[0].Name)
	}
	// Every section should be "convert".
	for _, r := range rows {
		if r.Section != "convert" {
			t.Errorf("non-convert section in convert matrix: %s", r.Section)
		}
	}
	// Common formats should appear.
	wantNames := []string{".md", ".html/.htm", ".pdf", ".docx", ".epub", ".pptx", ".xlsx"}
	have := map[string]bool{}
	for _, r := range rows {
		have[r.Name] = true
	}
	for _, want := range wantNames {
		if !have[want] {
			t.Errorf("convert matrix missing %s", want)
		}
	}
}

func TestCheckConvert_PPTXRequiresMarker(t *testing.T) {
	rows := checkConvert()
	var pptx *check
	for i := range rows {
		if rows[i].Name == ".pptx" {
			pptx = &rows[i]
			break
		}
	}
	if pptx == nil {
		t.Fatal(".pptx row missing")
	}
	if markerTierAvailable() {
		if pptx.Status != statusOK {
			t.Errorf("with marker installed, .pptx should be ok; got %s", pptx.Status)
		}
	} else {
		if pptx.Status != statusFail {
			t.Errorf("without marker, .pptx should be FAIL; got %s", pptx.Status)
		}
	}
}

// --- Ingest config defaults ---

func TestIngestDefaults_HasInboxAndMarker(t *testing.T) {
	d := ingestDefaults()
	if d.InboxPath == "" {
		t.Error("expected default InboxPath")
	}
	if d.Marker.TimeoutSeconds <= 0 {
		t.Error("expected positive default marker timeout")
	}
	if d.Marker.MPSFallback == nil || !*d.Marker.MPSFallback {
		t.Error("expected MPS fallback default true")
	}
}

func TestApplyIngestDefaults_KeepsUserOverrides(t *testing.T) {
	falseV := false
	user := IngestConfig{
		InboxPath: "custom/inbox",
		Marker: IngestMarkerConfig{
			TimeoutSeconds: 999,
			MPSFallback:    &falseV,
		},
	}
	applyIngestDefaults(&user)
	if user.InboxPath != "custom/inbox" {
		t.Errorf("user inbox path lost: %s", user.InboxPath)
	}
	if user.Marker.TimeoutSeconds != 999 {
		t.Errorf("user timeout lost: %d", user.Marker.TimeoutSeconds)
	}
	if user.Marker.MPSFallback == nil || *user.Marker.MPSFallback != false {
		t.Error("user-set MPSFallback=false was overridden")
	}
}

func TestApplyIngestDefaults_FillsZeroValues(t *testing.T) {
	var user IngestConfig
	applyIngestDefaults(&user)
	if user.InboxPath == "" {
		t.Error("zero-valued InboxPath should be filled")
	}
	if user.Marker.TimeoutSeconds == 0 {
		t.Error("zero-valued timeout should be filled")
	}
	if user.Marker.MPSFallback == nil {
		t.Error("nil MPSFallback should be filled")
	}
}

// --- Errors-as-values reminder ---

func TestErrConvertUnsupported_StillWorks(t *testing.T) {
	// Phase 1A regression: make sure the Phase 1B routing changes
	// didn't break the unsupported-format error type assertion path
	// the inbox drain relies on for quarantine messaging.
	if markerTierAvailable() {
		t.Skip("marker installed — unsupported path can't fire for .pptx")
	}
	_, err := convertFile("/tmp/x.pptx", ".pptx", []byte{}, "")
	var e *ErrConvertUnsupported
	if !errors.As(err, &e) {
		t.Fatalf("expected ErrConvertUnsupported, got %T", err)
	}
}
