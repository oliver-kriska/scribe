package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeDateValue_Quotes(t *testing.T) {
	cases := []struct {
		in, want string
		fixed    bool
	}{
		{`2026-10-14`, "2026-10-14", false},
		{`"2026-10-14"`, "2026-10-14", false}, // balanced: valid YAML, left as written
		{`'2026-10-14'`, "2026-10-14", false},
		{`2026-10-14"`, "2026-10-14", true},
		{`"2026-10-14`, "2026-10-14", true},
		{`"2026-10-14'`, "2026-10-14", true},
		{`2026/4/5`, "2026-04-05", true},
		{`soon`, "soon", false},
	}
	for _, c := range cases {
		got, fixed := normalizeDateValue(c.in)
		if got != c.want || fixed != c.fixed {
			t.Errorf("normalizeDateValue(%q) = %q, %v; want %q, %v", c.in, got, fixed, c.want, c.fixed)
		}
	}
}

func TestNormalizeConfidence(t *testing.T) {
	cases := []struct {
		in, want string
		fixed    bool
	}{
		{"high", "", false},
		{"0.9", "high", true},
		{"0.8", "high", true},
		{"0.78", "medium", true},
		{"0.5", "medium", true},
		{"0.2", "low", true},
		{"0", "low", true},
		{"1", "high", true},
		{"85%", "high", true},
		{"40 %", "low", true},
		{"High", "high", true},
		{`"medium"`, "medium", true},
		{"moderate", "medium", true},
		{"7", "", false},    // a 1–10 score? a percent? ambiguous
		{"-0.1", "", false}, // out of range
		{"NaN", "", false},
		{"certain", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, fixed := normalizeConfidence(c.in)
		if got != c.want || fixed != c.fixed {
			t.Errorf("normalizeConfidence(%q) = %q, %v; want %q, %v", c.in, got, fixed, c.want, c.fixed)
		}
	}
}

func TestAutoFixArticle_StrayQuoteDateAndNumericConfidence(t *testing.T) {
	in := "---\ntitle: \"X\"\ntype: pattern\ncreated: 2026-10-14\"\nupdated: \"2026-10-15\"\n" +
		"tags: []\nrelated: []\nsources: []\nconfidence: 0.83\ndomain: general\nauthority: contextual\n---\n\nBody.\n"
	changes, out, err := autoFixArticle("", "patterns/x.md", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"\ncreated: 2026-10-14\n", "\nupdated: \"2026-10-15\"\n", "\nconfidence: high\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if len(changes) != 2 {
		t.Errorf("changes = %v, want the date and the confidence fix", changes)
	}
	// Idempotent: a second pass has nothing left to do.
	if again, _, err := autoFixArticle("", "patterns/x.md", out); err != nil || len(again) != 0 {
		t.Errorf("second pass = %v, %v; want no changes", again, err)
	}
}

func TestApplyWikiActions_ClampNormalizesDateAndConfidence(t *testing.T) {
	root := t.TempDir()
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "create", Path: "patterns/q.md",
		Content: "---\ntitle: Q\ntype: pattern\ndomain: general\ncreated: 2026-10-14\"\nupdated: 2026-10-14\nconfidence: 0.6\n---\nbody\n",
	}}}
	if _, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, SanitizeContent: true}); err != nil {
		t.Fatal(err)
	}
	s := readBack(t, root, "patterns/q.md")
	for _, want := range []string{"\ncreated: 2026-10-14\n", "\nconfidence: medium\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	for _, e := range validateFile(root, filepath.Join(root, "patterns", "q.md")) {
		if strings.Contains(e, "confidence") || strings.Contains(e, "YYYY-MM-DD") {
			t.Errorf("clamped article still fails validation: %s", e)
		}
	}
}
