package main

import (
	"errors"
	"strings"
	"testing"
)

// Phase 1A test scope: dispatcher routing + tier 0 HTML conversion + helper
// behavior. Tier 0 PDF conversion needs a binary fixture and lands in a
// follow-up alongside testdata/convert/sample.pdf. Tier 1 marker tests
// require an installed marker binary; we only assert detection logic.

func TestConvertFile_PassthroughForPlainText(t *testing.T) {
	cases := []string{".md", ".markdown", ".txt", ""}
	for _, ext := range cases {
		t.Run("ext="+ext, func(t *testing.T) {
			res, err := convertFile("foo"+ext, ext, []byte("hello"), "")
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", ext, err)
			}
			if res != nil {
				t.Fatalf("expected nil result (passthrough) for %q, got %+v", ext, res)
			}
		})
	}
}

func TestConvertFile_HTMLTier0(t *testing.T) {
	html := `<html><head><title>Demo</title></head><body>
<h1>Heading</h1>
<p>Paragraph with <strong>bold</strong> text.</p>
<ul><li>item one</li><li>item two</li></ul>
</body></html>`

	// Force the tier 0 path by making sure marker isn't accidentally
	// picked up: the dispatcher checks tier 1 first, but if marker is
	// present on the test machine the result is still valid markdown.
	// We assert structural invariants rather than exact byte output.
	res, err := convertFile("/tmp/demo.html", ".html", []byte(html), "")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result for HTML")
	}
	if !strings.Contains(res.Markdown, "Heading") {
		t.Errorf("missing heading text; got: %s", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "Paragraph") {
		t.Errorf("missing paragraph text; got: %s", res.Markdown)
	}
	if res.Tier != "tier0" && res.Tier != "marker" {
		t.Errorf("unexpected tier %q (want tier0 or marker)", res.Tier)
	}
}

func TestConvertFile_UnsupportedFormat(t *testing.T) {
	if markerTierAvailable() {
		t.Skip("marker installed; cannot exercise the unsupported-format error path")
	}
	res, err := convertFile("/tmp/deck.pptx", ".pptx", []byte{0, 1, 2}, "")
	if res != nil {
		t.Fatalf("expected nil result on error, got %+v", res)
	}
	var unsupp *ErrConvertUnsupported
	if !errors.As(err, &unsupp) {
		t.Fatalf("expected ErrConvertUnsupported, got %T: %v", err, err)
	}
	if !strings.Contains(unsupp.Reason, "marker") {
		t.Errorf("error reason should hint at marker install; got: %s", unsupp.Reason)
	}
}

func TestPickTitle_PrefersHint(t *testing.T) {
	got := pickTitle("Override Wins", "# Heading\nbody", "/tmp/file_name.pdf")
	want := "Override Wins"
	if got != want {
		t.Errorf("pickTitle hint priority: got %q, want %q", got, want)
	}
}

func TestPickTitle_FallsThroughToHeading(t *testing.T) {
	got := pickTitle("", "# Real Heading\nbody", "/tmp/file_name.pdf")
	want := "Real Heading"
	if got != want {
		t.Errorf("pickTitle heading fallback: got %q, want %q", got, want)
	}
}

func TestPickTitle_FallsThroughToFilename(t *testing.T) {
	got := pickTitle("", "no heading here\nbody only", "/tmp/some_paper-2026.pdf")
	want := "some paper 2026"
	if got != want {
		t.Errorf("pickTitle filename fallback: got %q, want %q", got, want)
	}
}

func TestConvertHTMLTier0_StripsScriptStyle(t *testing.T) {
	html := `<html><body>
<script>alert("xss")</script>
<style>body { color: red; }</style>
<p>Real content.</p>
</body></html>`
	out, err := convertHTMLTier0([]byte(html))
	if err != nil {
		t.Fatalf("convertHTMLTier0: %v", err)
	}
	if strings.Contains(out, "alert") {
		t.Errorf("script tag content leaked: %s", out)
	}
	if strings.Contains(out, "color: red") {
		t.Errorf("style tag content leaked: %s", out)
	}
	if !strings.Contains(out, "Real content") {
		t.Errorf("expected paragraph text in output; got: %s", out)
	}
}

func TestConvertHTMLTier0_PreservesLists(t *testing.T) {
	html := `<ul><li>one</li><li>two</li><li>three</li></ul>`
	out, err := convertHTMLTier0([]byte(html))
	if err != nil {
		t.Fatalf("convertHTMLTier0: %v", err)
	}
	for _, want := range []string{"one", "two", "three"} {
		if !strings.Contains(out, want) {
			t.Errorf("list item %q missing from output: %s", want, out)
		}
	}
	// Should produce some kind of list marker (- or *), not raw HTML.
	if strings.Contains(out, "<li>") || strings.Contains(out, "<ul>") {
		t.Errorf("raw HTML tags leaked: %s", out)
	}
}

func TestConvertHTMLTier0_EmptyInput(t *testing.T) {
	out, err := convertHTMLTier0([]byte(""))
	if err != nil {
		t.Fatalf("convertHTMLTier0 empty: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("expected empty output for empty input, got: %q", out)
	}
}

func TestConvertPDFTier0_RejectsNonPDFBytes(t *testing.T) {
	// Plain text is not a valid PDF; ledongthuc/pdf should error.
	_, err := convertPDFTier0([]byte("not a pdf, just text"))
	if err == nil {
		t.Fatal("expected error on non-PDF bytes, got nil")
	}
}

func TestMarkerTierAvailable_BoolStable(t *testing.T) {
	// Just exercise the function — it should return a bool without
	// panicking regardless of whether marker is installed.
	a := markerTierAvailable()
	b := markerTierAvailable()
	if a != b {
		t.Errorf("marker availability changed between calls: %v then %v", a, b)
	}
}

func TestMarkerVersionLine_AlwaysReturnsString(t *testing.T) {
	got := markerVersionLine()
	if got == "" {
		t.Error("markerVersionLine should never return empty")
	}
	// When marker isn't installed the helper returns a known sentinel.
	if !markerTierAvailable() && got != "not installed" {
		t.Errorf("marker absent but version=%q (want %q)", got, "not installed")
	}
}
