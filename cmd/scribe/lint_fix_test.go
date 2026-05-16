package main

import (
	"strings"
	"testing"
)

func TestAutoFixArticle_AddsMissingDefaults(t *testing.T) {
	in := `---
title: "Example"
type: pattern
created: 2026-04-20
updated: 2026-04-20
---

Body.
`
	changes, out, err := autoFixArticle("", "patterns/x.md", []byte(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(changes) == 0 || out == nil {
		t.Fatalf("expected fixes to apply; got changes=%d", len(changes))
	}
	s := string(out)
	for _, k := range []string{"tags: []", "related: []", "sources: []", "confidence: medium", "domain: general"} {
		if !strings.Contains(s, k) {
			t.Errorf("missing default %q in output:\n%s", k, s)
		}
	}
}

func TestAutoFixArticle_NormalizesSlashDate(t *testing.T) {
	in := `---
title: "X"
type: pattern
created: 2026/04/20
updated: 2026.4.5
tags: []
related: []
sources: []
confidence: medium
domain: general
---

Body.
`
	changes, out, err := autoFixArticle("", "patterns/x.md", []byte(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(changes) == 0 {
		t.Fatalf("expected date normalization to apply")
	}
	s := string(out)
	if !strings.Contains(s, "created: 2026-04-20") {
		t.Errorf("created not normalized in:\n%s", s)
	}
	if !strings.Contains(s, "updated: 2026-04-05") {
		t.Errorf("updated not normalized (dot form w/ pad) in:\n%s", s)
	}
}

func TestAutoFixArticle_StripsTrailingWhitespace(t *testing.T) {
	in := "---\ntitle: \"X\"   \ntype: pattern\t\ncreated: 2026-04-20\nupdated: 2026-04-20\ntags: []\nrelated: []\nsources: []\nconfidence: medium\ndomain: general\n---\n\nBody.\n"
	changes, out, err := autoFixArticle("", "patterns/x.md", []byte(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(changes) == 0 {
		t.Fatalf("expected trailing whitespace strip")
	}
	if strings.Contains(string(out), "pattern\t") || strings.Contains(string(out), `"X"   `) {
		t.Errorf("trailing whitespace still present:\n%s", out)
	}
}

func TestAutoFixArticle_NoopWhenClean(t *testing.T) {
	in := `---
title: "X"
type: pattern
created: 2026-04-20
updated: 2026-04-20
tags: []
related: []
sources: []
confidence: medium
domain: general
authority: contextual
---

Body.
`
	changes, out, err := autoFixArticle("", "patterns/x.md", []byte(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(changes) != 0 || out != nil {
		t.Fatalf("expected no-op on clean file; changes=%v", changes)
	}
}

func TestAutoFixArticle_SkipsNoFrontmatter(t *testing.T) {
	in := "Just a body with no frontmatter.\n"
	changes, out, err := autoFixArticle("", "patterns/x.md", []byte(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(changes) != 0 || out != nil {
		t.Fatalf("no-frontmatter file should be skipped")
	}
}

func TestAutoFixArticle_DoesNotMarkIndentedKeysAsPresent(t *testing.T) {
	// tags exists in the file as a nested list. Fix should see it as present
	// and NOT append "tags: []" at the bottom.
	in := "---\ntitle: \"X\"\ntype: pattern\ncreated: 2026-04-20\nupdated: 2026-04-20\ntags:\n  - one\n  - two\nrelated: []\nsources: []\nconfidence: medium\ndomain: general\n---\n\nBody.\n"
	changes, out, err := autoFixArticle("", "patterns/x.md", []byte(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(changes) > 0 {
		t.Logf("changes applied: %v", changes)
	}
	if out != nil && strings.Contains(string(out), "tags: []") {
		t.Fatalf("should not duplicate tags key when a nested-list form exists:\n%s", out)
	}
}

// The on-disk counterpart of the envelope seam: lint --fix must repair
// the existing-damage classes the seam now prevents for new writes —
// invalid type (clamped from the path), invalid domain (→ general) —
// and must NEVER claim a fix on frontmatter that still won't parse.

func TestAutoFixArticle_ClampsInvalidTypeFromPath(t *testing.T) {
	in := "---\ntitle: X\ntype: article\ncreated: 2026-04-20\nupdated: 2026-04-20\ntags: []\nrelated: []\nsources: []\nconfidence: medium\ndomain: general\nauthority: contextual\n---\n\nBody.\n"
	changes, out, err := autoFixArticle("", "decisions/x.md", []byte(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == nil {
		t.Fatalf("expected a fix; got none (%v)", changes)
	}
	s := string(out)
	if !strings.Contains(s, "\ntype: decision\n") {
		t.Errorf("type not clamped to canonical 'decision':\n%s", s)
	}
	if strings.Contains(s, "type: article") {
		t.Errorf("invalid type survived:\n%s", s)
	}
}

func TestAutoFixArticle_WikiInvalidTypeFallsBackToResearch(t *testing.T) {
	in := "---\ntitle: X\ntype: article\ncreated: 2026-04-20\nupdated: 2026-04-20\ntags: []\nrelated: []\nsources: []\nconfidence: medium\ndomain: general\nauthority: contextual\n---\n\nBody.\n"
	_, out, err := autoFixArticle("", "wiki/x.md", []byte(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == nil || !strings.Contains(string(out), "\ntype: research\n") {
		t.Errorf("wiki/ invalid type should fall back to 'research':\n%s", out)
	}
}

func TestAutoFixArticle_LeavesValidButDirMismatchedType(t *testing.T) {
	// type: decision is valid and NOT a lint error even in wiki/. The
	// clamp must not "correct" it — only invalid/missing types.
	in := "---\ntitle: X\ntype: decision\ncreated: 2026-04-20\nupdated: 2026-04-20\ntags: []\nrelated: []\nsources: []\nconfidence: medium\ndomain: general\nauthority: canonical\n---\n\nBody.\n"
	changes, out, err := autoFixArticle("", "wiki/x.md", []byte(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Fatalf("valid type must be left untouched (no-op expected); changes=%v\n%s", changes, out)
	}
}

func TestAutoFixArticle_ClampsInvalidDomain(t *testing.T) {
	// Empty root ⇒ validDomainsForRoot = {personal, general}; "research"
	// is therefore invalid and must clamp to general.
	in := "---\ntitle: X\ntype: pattern\ncreated: 2026-04-20\nupdated: 2026-04-20\ntags: []\nrelated: []\nsources: []\nconfidence: medium\ndomain: research\nauthority: contextual\n---\n\nBody.\n"
	_, out, err := autoFixArticle("", "patterns/x.md", []byte(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == nil || !strings.Contains(string(out), "\ndomain: general\n") {
		t.Errorf("invalid domain not clamped to general:\n%s", out)
	}
	if strings.Contains(string(out), "domain: research") {
		t.Errorf("invalid domain survived:\n%s", out)
	}
}

func TestAutoFixArticle_SkipsStillInvalidYAML(t *testing.T) {
	// Has a closing --- (passes the early check) but an unescaped colon
	// makes the YAML unparseable. Deterministic cosmetic fixes can't
	// resolve that — must error so the caller SKIPs, never a false FIX.
	in := "---\ntitle: X\ntype: pattern\nsummary: foo: bar baz\ndomain: general\n---\n\nBody.\n"
	changes, out, err := autoFixArticle("", "patterns/x.md", []byte(in))
	if err == nil {
		t.Fatalf("expected manual-repair error for unparseable YAML; got changes=%v out=%q", changes, out)
	}
	if out != nil {
		t.Errorf("must not write a still-invalid file")
	}
	if !strings.Contains(err.Error(), "manual repair") {
		t.Errorf("error should flag manual repair, got: %v", err)
	}
}
