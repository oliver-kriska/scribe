package main

import (
	"strings"
	"testing"
)

// TestNormalizeAliasesBlock_RepairsCorruption reproduces the people/*.md
// frontmatter the un-quoted identity-apply writer produced — a bare @handle
// (invalid YAML) plus over-indented duplicate entries — and asserts the
// normalizer quotes, re-indents, and dedups it.
func TestNormalizeAliasesBlock_RepairsCorruption(t *testing.T) {
	lines := []string{
		"aliases:",
		"  - Omar Sanseviero",
		"  - @omarsar0",
		"    - '@omarsar0'",
		"    - Omar Sanseviero",
		"authority: opinion",
	}
	got, changed := normalizeAliasesBlock(lines)
	if !changed {
		t.Fatal("expected the malformed block to be changed")
	}
	want := []string{
		"aliases:",
		"  - Omar Sanseviero",
		"  - '@omarsar0'",
		"authority: opinion",
	}
	if len(got) != len(want) {
		t.Fatalf("line count = %d, want %d:\n%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestNormalizeAliasesBlock_Idempotent: a clean block is left untouched, so
// `lint --fix` doesn't churn well-formed files every run.
func TestNormalizeAliasesBlock_Idempotent(t *testing.T) {
	lines := []string{
		"aliases:",
		"  - Omar Sanseviero",
		"  - '@omarsar0'",
		"authority: opinion",
	}
	if _, changed := normalizeAliasesBlock(lines); changed {
		t.Error("clean aliases block should not be reported as changed")
	}
	// Inline form must be left alone.
	inline := []string{"aliases: [a, b]", "authority: opinion"}
	if _, changed := normalizeAliasesBlock(inline); changed {
		t.Error("inline aliases form should not be touched")
	}
	// Already-valid DOUBLE-quoted @handles must be preserved verbatim, not
	// rewritten to single quotes — otherwise every people/*.md churns on
	// each run (the over-eager-normalizer regression).
	doubleQuoted := []string{
		"aliases:",
		`  - "@karpathy"`,
		"  - Andrej Karpathy",
		"domain: general",
	}
	if _, changed := normalizeAliasesBlock(doubleQuoted); changed {
		t.Error(`valid "@handle" double-quoted block must not be rewritten`)
	}
}

// TestAutoFixArticle_RepairsInvalidAliasYAML: end-to-end, a file lint rejects
// for invalid alias YAML becomes a real FIX (not a SKIP).
func TestAutoFixArticle_RepairsInvalidAliasYAML(t *testing.T) {
	in := `---
title: "Omar Sanseviero"
type: person
aliases:
  - Omar Sanseviero
  - @omarsar0
    - '@omarsar0'
created: 2026-04-13
updated: 2026-04-13
tags: []
related: []
sources: []
confidence: medium
domain: general
---

Body.
`
	changes, out, err := autoFixArticle("", "people/omar-sanseviero.md", []byte(in))
	if err != nil {
		t.Fatalf("expected repair, got SKIP error: %v", err)
	}
	if out == nil || len(changes) == 0 {
		t.Fatal("expected the file to be fixed")
	}
	// The result must now be valid frontmatter.
	if _, perr := parseFrontmatter(out); perr != nil {
		t.Errorf("output still invalid YAML: %v\n%s", perr, out)
	}
	if !strings.Contains(string(out), "  - '@omarsar0'") {
		t.Errorf("expected quoted @handle in output:\n%s", out)
	}
}

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

func TestAutoFixArticle_TrailingSpaceFenceIsFixedNotSkipped(t *testing.T) {
	// Regression: a closing fence with a trailing space ("--- ") is
	// valid per `scribe lint` (parseFrontmatter prefix-matches "\n---")
	// but used to make lint --fix bail with "no closing ---". The
	// validator and fixer must agree; this must be a real FIX that
	// normalizes the fence to bare "---", with the body preserved.
	in := "---\ntitle: \"X\"\ntype: decision\ncreated: 2026-05-16\nupdated: 2026-05-16\ndomain: general\nconfidence: high\ntags: []\nrelated: []\nsources: []\n--- \n\nBody text.\n"
	changes, out, err := autoFixArticle("", "decisions/x.md", []byte(in))
	if err != nil {
		t.Fatalf("trailing-space fence must be fixed, not skipped: %v", err)
	}
	if out == nil {
		t.Fatalf("expected a rewritten file")
	}
	got := string(out)
	if strings.Contains(got, "--- \n") {
		t.Errorf("trailing-space fence not normalized:\n%s", got)
	}
	if !strings.Contains(got, "\n---\n\nBody text.\n") {
		t.Errorf("body not preserved after bare fence:\n%s", got)
	}
	var sawFence bool
	for _, c := range changes {
		if strings.Contains(c, "normalized closing frontmatter fence") {
			sawFence = true
		}
	}
	if !sawFence {
		t.Errorf("expected a fence-normalization change, got: %v", changes)
	}
}

func TestAutoFixArticle_CRLFClosingFencePreserved(t *testing.T) {
	// CRLF closing fence support predates this change — must not regress.
	in := "---\ntitle: \"X\"\ntype: pattern\ncreated: 2026-04-20\nupdated: 2026-04-20\ndomain: general\nconfidence: medium\ntags: []\nrelated: []\nsources: []\n---\r\n\r\nBody.\n"
	_, out, err := autoFixArticle("", "patterns/x.md", []byte(in))
	if err != nil {
		t.Fatalf("CRLF closing fence must not error: %v", err)
	}
	if out == nil || !strings.Contains(string(out), "Body.") {
		t.Errorf("CRLF fence body not preserved:\n%s", out)
	}
}

func TestAutoFixArticle_GenuinelyNoClosingFenceStillErrors(t *testing.T) {
	// An opening fence with keys but no closing fence anywhere is real
	// manual-repair-class corruption — must still SKIP, not silently
	// invent a fence.
	in := "---\ntitle: X\ntype: pattern\ndomain: general\n"
	_, out, err := autoFixArticle("", "patterns/x.md", []byte(in))
	if err == nil {
		t.Fatalf("genuinely missing closing fence must error; got out=%q", out)
	}
	if !strings.Contains(err.Error(), "no closing ---") {
		t.Errorf("error should name the missing fence, got: %v", err)
	}
	if out != nil {
		t.Errorf("must not write a file with no closing fence")
	}
}
