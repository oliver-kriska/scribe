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
	changes, out, err := autoFixArticle([]byte(in))
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
	changes, out, err := autoFixArticle([]byte(in))
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
	changes, out, err := autoFixArticle([]byte(in))
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
	changes, out, err := autoFixArticle([]byte(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(changes) != 0 || out != nil {
		t.Fatalf("expected no-op on clean file; changes=%v", changes)
	}
}

func TestAutoFixArticle_SkipsNoFrontmatter(t *testing.T) {
	in := "Just a body with no frontmatter.\n"
	changes, out, err := autoFixArticle([]byte(in))
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
	changes, out, err := autoFixArticle([]byte(in))
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
