package main

import (
	"strings"
	"testing"
)

func TestNormalizeForAbsorb_Markdown(t *testing.T) {
	raw := "# Hello World\n\nSome body text.\n"
	title, body := normalizeForAbsorb(".md", raw, "")
	if title != "Hello World" {
		t.Fatalf("title = %q, want %q", title, "Hello World")
	}
	if body != raw {
		t.Fatalf("body should pass through unchanged")
	}
}

func TestNormalizeForAbsorb_HTML(t *testing.T) {
	raw := `<html><head><title>My Page</title><style>x{}</style></head>
<body><script>alert(1)</script><p>Hello <b>world</b>!</p></body></html>`
	title, body := normalizeForAbsorb(".html", raw, "")
	if title != "My Page" {
		t.Fatalf("title = %q, want %q", title, "My Page")
	}
	if strings.Contains(body, "<script") || strings.Contains(body, "<style") || strings.Contains(body, "<p>") {
		t.Fatalf("body still contains HTML tags: %q", body)
	}
	if !strings.Contains(body, "Hello") || !strings.Contains(body, "world") {
		t.Fatalf("body lost content: %q", body)
	}
}

func TestNormalizeForAbsorb_TextFirstLineIsTitle(t *testing.T) {
	raw := "\n\nSome Topic\n\nFollow-up paragraph here.\n"
	title, body := normalizeForAbsorb(".txt", raw, "")
	if title != "Some Topic" {
		t.Fatalf("title = %q, want %q", title, "Some Topic")
	}
	if body != raw {
		t.Fatalf("txt body should pass through unchanged")
	}
}

func TestNormalizeForAbsorb_OverrideTitle(t *testing.T) {
	raw := "# Original\n"
	title, _ := normalizeForAbsorb(".md", raw, "Override")
	if title != "Override" {
		t.Fatalf("title = %q, want %q", title, "Override")
	}
}

func TestStripHTML_EntityDecodeAndCollapseWhitespace(t *testing.T) {
	in := `<p>a &amp; b</p>    <p>c</p>`
	out := stripHTML(in)
	if !strings.Contains(out, "a & b") {
		t.Fatalf("entities not decoded: %q", out)
	}
	if strings.Contains(out, "    ") {
		t.Fatalf("whitespace not collapsed: %q", out)
	}
}
