package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Phase 2A test scope: pure helpers introduced for image sidecar,
// smart routing, and the install-tools machinery. Network-fetching
// paths (downloadUV, installMarkerViaUV) and the marker subprocess
// itself are exercised by hand against a real install — the test
// surface here is everything that doesn't need ~3 GB of weights.

func TestExtractImageRefs_HappyPath(t *testing.T) {
	md := `Some intro paragraph.

![first](images/figure1.png)

More prose ![inline alt](other.jpg "title text") here.

![](no-alt.gif)

Duplicate ![dup](images/figure1.png) — should not list twice.

External ![link](https://example.com/x.png) and data: ![data](data:image/png;base64,xxx).
`
	got := extractImageRefs(md)
	want := []string{
		"images/figure1.png",
		"other.jpg",
		"no-alt.gif",
		"https://example.com/x.png",
		"data:image/png;base64,xxx",
	}
	if len(got) != len(want) {
		t.Fatalf("extractImageRefs len=%d, want=%d (got %v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("ref[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestExtractImageRefs_NoImages(t *testing.T) {
	md := "Just prose. No image refs at all.\n\n[a regular link](https://x)"
	got := extractImageRefs(md)
	if len(got) != 0 {
		t.Errorf("expected zero refs, got %v", got)
	}
}

func TestSlugifyForAssets(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Some Paper Title", "some-paper-title"},
		{"file_with_underscores.pdf", "file-with-underscores-pdf"},
		{"   leading and trailing   ", "leading-and-trailing"},
		{"UPPER123case", "upper123case"},
		{"àccénts get killed", "cc-nts-get-killed"},
		{"!!!", ""},
	}
	for _, c := range cases {
		got := slugifyForAssets(c.in)
		if got != c.want {
			t.Errorf("slugifyForAssets(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPreserveMarkerImages_CopiesAndRewrites(t *testing.T) {
	tmp := t.TempDir()
	markerOut := filepath.Join(tmp, "marker_out")
	if err := os.MkdirAll(markerOut, 0o755); err != nil {
		t.Fatal(err)
	}
	// Stub the marker-emitted asset.
	imgPath := filepath.Join(markerOut, "fig1.png")
	if err := os.WriteFile(imgPath, []byte("PNG-DATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	kbRoot := filepath.Join(tmp, "kb")
	if err := os.MkdirAll(kbRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	md := "# Title\n\nIntro.\n\n![diagram](fig1.png)\n\nDone.\n"
	got := preserveMarkerImages(md, markerOut, kbRoot, "test-paper")

	// Asset must have been copied into raw/assets/<slug>/.
	dst := filepath.Join(kbRoot, "raw", "assets", "test-paper", "fig1.png")
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("expected asset copied to %s: %v", dst, err)
	}
	if string(data) != "PNG-DATA" {
		t.Errorf("asset bytes mismatch: got %q", string(data))
	}

	// Markdown link should now point at the KB-relative path.
	wantRef := "../assets/test-paper/fig1.png"
	if !strings.Contains(got, wantRef) {
		t.Errorf("rewritten markdown missing %q; got:\n%s", wantRef, got)
	}
	// Original ref should be gone.
	if strings.Contains(got, "](fig1.png)") {
		t.Errorf("old ref still present in output:\n%s", got)
	}
}

func TestPreserveMarkerImages_SkipsAbsoluteAndMissing(t *testing.T) {
	tmp := t.TempDir()
	markerOut := filepath.Join(tmp, "marker_out")
	_ = os.MkdirAll(markerOut, 0o755)
	kbRoot := filepath.Join(tmp, "kb")
	_ = os.MkdirAll(kbRoot, 0o755)

	md := "![http](https://x.test/a.png)\n![data](data:image/png;base64,xxx)\n![missing](does-not-exist.png)\n"
	got := preserveMarkerImages(md, markerOut, kbRoot, "x")
	// All three refs should remain unchanged — none are local files we copied.
	for _, want := range []string{"https://x.test/a.png", "data:image/png;base64,xxx", "does-not-exist.png"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q to remain in output; got:\n%s", want, got)
		}
	}
	// And the assets dir should be empty.
	entries, _ := os.ReadDir(filepath.Join(kbRoot, "raw", "assets", "x"))
	if len(entries) != 0 {
		t.Errorf("expected no assets copied, got %d entries", len(entries))
	}
}

func TestPreserveMarkerImages_EmptySlugIsNoop(t *testing.T) {
	md := "![](foo.png)"
	got := preserveMarkerImages(md, "/nowhere", "/also-nowhere", "")
	if got != md {
		t.Errorf("empty slug should be a no-op; got: %q", got)
	}
}

func TestCopyFile_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src.bin")
	dst := filepath.Join(tmp, "dst.bin")
	want := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0xFF}
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("copy mismatch: got %v want %v", got, want)
	}
}

func TestCopyFile_MissingSource(t *testing.T) {
	tmp := t.TempDir()
	err := copyFile(filepath.Join(tmp, "nope"), filepath.Join(tmp, "dst"))
	if err == nil {
		t.Error("expected error copying missing file")
	}
}

func TestShouldRouteSmallPDFToTier0_NonPDFReturnsFalse(t *testing.T) {
	if shouldRouteSmallPDFToTier0(".docx", []byte("anything")) {
		t.Error("non-PDF must never smart-route")
	}
	if shouldRouteSmallPDFToTier0(".html", []byte("anything")) {
		t.Error("non-PDF must never smart-route")
	}
}

func TestShouldRouteSmallPDFToTier0_NoKBContextReturnsFalse(t *testing.T) {
	// Outside a KB, kbDir errors and config is unresolvable. The
	// dispatcher must default to "no smart routing" so the existing
	// tier 1/0 fall-through stays correct.
	t.Setenv("SCRIBE_KB_ROOT", "")
	tmp := t.TempDir()
	t.Setenv("HOME", tmp) // ensure no scribe.yaml is auto-discovered
	prev, _ := os.Getwd()
	defer os.Chdir(prev)
	_ = os.Chdir(tmp)
	if shouldRouteSmallPDFToTier0(".pdf", []byte("anything")) {
		t.Error("smart routing must be off when no KB is resolvable")
	}
}

func TestPDFPageCount_OnNonPDFReturnsZero(t *testing.T) {
	got := pdfPageCount([]byte("definitely not a pdf"))
	if got != 0 {
		t.Errorf("pdfPageCount on garbage = %d, want 0", got)
	}
}

func TestScribeHome_RespectsEnvOverride(t *testing.T) {
	t.Setenv("SCRIBE_HOME", "/custom/scribe")
	got, err := scribeHome()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/custom/scribe" {
		t.Errorf("scribeHome = %q, want /custom/scribe", got)
	}
}

func TestScribeHome_FallsBackToHomeDir(t *testing.T) {
	t.Setenv("SCRIBE_HOME", "")
	got, err := scribeHome()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, ".scribe") {
		t.Errorf("scribeHome fallback %q should end in .scribe", got)
	}
}

func TestEnsureUV_PrefersEnvOverride(t *testing.T) {
	t.Setenv("SCRIBE_UV", "/usr/local/test/uv")
	got, err := ensureUV(false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/usr/local/test/uv" {
		t.Errorf("ensureUV with SCRIBE_UV = %q, want /usr/local/test/uv", got)
	}
}

func TestReadInstallState_MissingReturnsNil(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SCRIBE_HOME", tmp)
	if got := readInstallState(); got != nil {
		t.Errorf("expected nil for missing install-state.json, got %+v", got)
	}
}

func TestReadInstallState_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SCRIBE_HOME", tmp)
	if err := persistInstallState("/some/path/uv"); err != nil {
		t.Fatalf("persistInstallState: %v", err)
	}
	got := readInstallState()
	if got == nil {
		t.Fatal("expected non-nil install state after persist")
	}
	if got.UVPath != "/some/path/uv" {
		t.Errorf("UVPath = %q, want /some/path/uv", got.UVPath)
	}
	if got.UVVersion != uvVersion {
		t.Errorf("UVVersion = %q, want %q", got.UVVersion, uvVersion)
	}
	if got.InstalledAt == "" {
		t.Error("InstalledAt should be set")
	}
}

func TestUVAssetMap_CoversThisPlatform(t *testing.T) {
	// Anyone running these tests is on darwin/arm64, darwin/amd64,
	// linux/amd64, or linux/arm64 — the four platforms scribe ships
	// pinned uv binaries for. If a contributor lands on a fifth
	// (linux/ppc64, etc.), this test fails loudly so the platform map
	// gets updated alongside the rest of the change.
	target := runtime.GOOS + "/" + runtime.GOARCH
	if _, ok := uvAssetName[target]; !ok {
		t.Skipf("no pinned uv asset for %s — extend uvAssetName/uvSHA256 to add support", target)
	}
	if _, ok := uvSHA256[target]; !ok {
		t.Errorf("uvSHA256 missing entry for %s", target)
	}
}

func TestLazyBootstrapMarker_GatedByConsentEnv(t *testing.T) {
	// Without the env var set, the lazy path must refuse so the user
	// never gets a 3 GB download surprise. Result is an error referencing
	// the consent env or the explicit subcommand.
	t.Setenv("SCRIBE_AUTO_INSTALL_TOOLS", "")
	err := lazyBootstrapMarker()
	if err == nil {
		t.Fatal("expected refusal without SCRIBE_AUTO_INSTALL_TOOLS=1")
	}
	if !strings.Contains(err.Error(), "SCRIBE_AUTO_INSTALL_TOOLS") &&
		!strings.Contains(err.Error(), "install-tools") {
		t.Errorf("error should hint at consent path; got: %v", err)
	}
}
