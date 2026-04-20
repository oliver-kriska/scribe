package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateFile exercises the frontmatter validator that runs via
// the pre-commit hook and scribe lint. It takes a path, so we write
// fixtures to tmp files. The tests document the validation rules so
// future changes to them don't silently regress the gate.

const goodFrontmatter = `---
title: "Good Article"
type: solution
created: 2026-04-10
updated: 2026-04-10
domain: general
confidence: high
tags: [tag1, tag2]
related: ["[[Other Article]]"]
sources: ["source1"]
problem: "Describes problem"
applies_to: [general]
---

Body.
`

func writeTmp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateFile_Passes(t *testing.T) {
	path := writeTmp(t, "good.md", goodFrontmatter)
	if errs := validateFile("", path); len(errs) != 0 {
		t.Errorf("expected no errors, got: %v", errs)
	}
}

func TestValidateFile_SkipsNonMarkdown(t *testing.T) {
	path := writeTmp(t, "good.txt", "anything")
	if errs := validateFile("", path); errs != nil {
		t.Errorf("expected nil for non-md, got: %v", errs)
	}
}

func TestValidateFile_SkipsRaw(t *testing.T) {
	// Path must contain "/raw/" for the skip logic.
	dir := t.TempDir()
	rawDir := filepath.Join(dir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rawDir, "raw.md")
	if err := os.WriteFile(path, []byte("no frontmatter"), 0o644); err != nil {
		t.Fatal(err)
	}
	if errs := validateFile("", path); errs != nil {
		t.Errorf("expected nil for raw file, got: %v", errs)
	}
}

func TestValidateFile_EmptyFile(t *testing.T) {
	path := writeTmp(t, "empty.md", "")
	errs := validateFile("", path)
	if len(errs) != 1 || !strings.Contains(errs[0], "empty") {
		t.Errorf("expected empty-file error, got: %v", errs)
	}
}

func TestValidateFile_NoFrontmatter(t *testing.T) {
	path := writeTmp(t, "plain.md", "# Just a heading\n\nBody.\n")
	errs := validateFile("", path)
	if len(errs) != 1 || !strings.Contains(errs[0], "frontmatter") {
		t.Errorf("expected frontmatter error, got: %v", errs)
	}
}

func TestValidateFile_MissingRequiredFields(t *testing.T) {
	content := `---
title: "Partial"
type: solution
---

Body.
`
	path := writeTmp(t, "partial.md", content)
	errs := validateFile("", path)
	if len(errs) == 0 {
		t.Fatal("expected missing-field error")
	}
	joined := strings.Join(errs, " ")
	for _, field := range []string{"created", "updated", "domain", "confidence", "tags", "related", "sources"} {
		if !strings.Contains(joined, field) {
			t.Errorf("missing field %q not reported: %v", field, errs)
		}
	}
}

func TestValidateFile_InvalidType(t *testing.T) {
	content := strings.ReplaceAll(goodFrontmatter, "type: solution", "type: bogus")
	path := writeTmp(t, "bad-type.md", content)
	errs := validateFile("", path)
	found := false
	for _, e := range errs {
		if strings.Contains(e, "invalid type") && strings.Contains(e, "bogus") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected invalid type error, got: %v", errs)
	}
}

func TestValidateFile_InvalidDomain(t *testing.T) {
	content := strings.ReplaceAll(goodFrontmatter, "domain: general", "domain: klingon")
	path := writeTmp(t, "bad-domain.md", content)
	errs := validateFile("", path)
	found := false
	for _, e := range errs {
		if strings.Contains(e, "invalid domain") && strings.Contains(e, "klingon") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected invalid domain error, got: %v", errs)
	}
}

func TestValidateFile_InvalidConfidence(t *testing.T) {
	content := strings.ReplaceAll(goodFrontmatter, "confidence: high", "confidence: probable")
	path := writeTmp(t, "bad-conf.md", content)
	errs := validateFile("", path)
	found := false
	for _, e := range errs {
		if strings.Contains(e, "invalid confidence") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected invalid confidence error, got: %v", errs)
	}
}

func TestValidateFile_BadDateFormat(t *testing.T) {
	// YAML parses "2026-04-10" as time.Time automatically — to force
	// the string path we quote it, which keeps it as a string, then
	// use an invalid format.
	content := strings.ReplaceAll(goodFrontmatter, "created: 2026-04-10", `created: "April 10"`)
	path := writeTmp(t, "bad-date.md", content)
	errs := validateFile("", path)
	found := false
	for _, e := range errs {
		if strings.Contains(e, "created") && strings.Contains(e, "YYYY-MM-DD") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected date format error, got: %v", errs)
	}
}

func TestValidateFile_TypeSpecificFieldValidation(t *testing.T) {
	// Research articles must have valid status and depth values.
	content := `---
title: "Bad Research"
type: research
created: 2026-04-10
updated: 2026-04-10
domain: general
confidence: high
tags: []
related: []
sources: []
status: bogus
depth: shallow
---

Body.
`
	path := writeTmp(t, "bad-research.md", content)
	errs := validateFile("", path)
	found := false
	for _, e := range errs {
		if strings.Contains(e, "invalid status") && strings.Contains(e, "bogus") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected invalid status error for research type, got: %v", errs)
	}
}

func TestValidateFile_EmptyTitle(t *testing.T) {
	content := strings.ReplaceAll(goodFrontmatter, `title: "Good Article"`, `title: ""`)
	path := writeTmp(t, "empty-title.md", content)
	errs := validateFile("", path)
	found := false
	for _, e := range errs {
		if strings.Contains(e, "title is empty") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected empty title error, got: %v", errs)
	}
}

func TestValidateFile_TagsMustBeList(t *testing.T) {
	content := strings.ReplaceAll(goodFrontmatter, "tags: [tag1, tag2]", `tags: "tag1,tag2"`)
	path := writeTmp(t, "bad-tags.md", content)
	errs := validateFile("", path)
	found := false
	for _, e := range errs {
		if strings.Contains(e, "tags should be a list") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected tags-must-be-list error, got: %v", errs)
	}
}
