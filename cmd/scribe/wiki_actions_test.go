package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 4B test scope: action envelope parsing, path validation, and
// every op (create / append / replace_section / update_frontmatter)
// against a real tmp KB. The executor's contract is that catastrophic
// errors return an error, while per-action failures land in
// result.Errors so the surrounding pass-2 invocation can log + carry
// on. Tests assert that contract on both shapes.

func TestParseEnvelope_Valid(t *testing.T) {
	in := `{"entity":"Foo","actions":[{"op":"create","path":"wiki/foo.md","content":"hello"}]}`
	env, err := parseEnvelope(in)
	if err != nil {
		t.Fatalf("expected ok: %v", err)
	}
	if env.Entity != "Foo" {
		t.Errorf("entity = %q", env.Entity)
	}
	if len(env.Actions) != 1 || env.Actions[0].Op != "create" {
		t.Errorf("unexpected actions: %+v", env.Actions)
	}
}

func TestParseEnvelope_RejectsEmptyActions(t *testing.T) {
	if _, err := parseEnvelope(`{"actions":[]}`); err == nil {
		t.Error("expected error on empty actions")
	}
}

func TestParseEnvelope_RejectsMissingOp(t *testing.T) {
	in := `{"actions":[{"path":"wiki/foo.md"}]}`
	if _, err := parseEnvelope(in); err == nil {
		t.Error("expected error when op missing")
	}
}

func TestParseEnvelope_RejectsMissingPath(t *testing.T) {
	in := `{"actions":[{"op":"create","content":"x"}]}`
	if _, err := parseEnvelope(in); err == nil {
		t.Error("expected error when path missing")
	}
}

func TestValidateActionPath_RejectsAbsolute(t *testing.T) {
	root := t.TempDir()
	if _, err := validateActionPath(root, "/etc/passwd"); err == nil {
		t.Error("absolute path should be refused")
	}
}

func TestValidateActionPath_RejectsTraversal(t *testing.T) {
	root := t.TempDir()
	if _, err := validateActionPath(root, "../escape.md"); err == nil {
		t.Error("../traversal should be refused")
	}
	if _, err := validateActionPath(root, "wiki/../../escape.md"); err == nil {
		t.Error("nested traversal should be refused")
	}
}

func TestValidateActionPath_RejectsUnknownTopDir(t *testing.T) {
	root := t.TempDir()
	if _, err := validateActionPath(root, "raw/articles/foo.md"); err == nil {
		t.Error("raw/ is not in wikiDirs and should be refused")
	}
	if _, err := validateActionPath(root, "scripts/junk.md"); err == nil {
		t.Error("scripts/ should be refused")
	}
}

func TestValidateActionPath_AcceptsAllWikiDirs(t *testing.T) {
	root := t.TempDir()
	for _, d := range wikiDirs {
		rel := filepath.Join(d, "x.md")
		if _, err := validateActionPath(root, rel); err != nil {
			t.Errorf("wiki dir %q rejected: %v", d, err)
		}
	}
}

func TestApplyWikiActions_CreateNew(t *testing.T) {
	root := t.TempDir()
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "create", Path: "patterns/new-pattern.md", Content: "---\ntitle: \"New\"\n---\n\nbody\n",
	}}}
	res, err := applyWikiActions(root, env, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	if len(res.Applied) != 1 {
		t.Fatalf("expected 1 applied, got %v", res.Applied)
	}
	got, err := os.ReadFile(filepath.Join(root, "patterns", "new-pattern.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "title: \"New\"") {
		t.Errorf("file content not written as expected: %s", string(got))
	}
}

func TestApplyWikiActions_CreateRefusesOverwrite(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "wiki", "exists.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "create", Path: "wiki/exists.md", Content: "new",
	}}}
	res, _ := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: false})
	if len(res.Applied) != 0 {
		t.Errorf("create against existing file should be refused; applied=%v", res.Applied)
	}
	if len(res.Errors) == 0 {
		t.Error("expected error on overwrite-refused")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "original" {
		t.Errorf("original content was overwritten: %q", string(got))
	}
}

func TestApplyWikiActions_CreateAllowOverwrite(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "wiki", "x.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "create", Path: "wiki/x.md", Content: "new",
	}}}
	res, _ := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true})
	if len(res.Errors) > 0 {
		t.Fatalf("unexpected errors with AllowOverwrite=true: %v", res.Errors)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "new" {
		t.Errorf("expected overwrite to succeed; content=%q", string(got))
	}
}

func TestApplyWikiActions_DryRunSkipsWrites(t *testing.T) {
	root := t.TempDir()
	env := WikiActionEnvelope{Actions: []WikiAction{
		{Op: "create", Path: "wiki/a.md", Content: "x"},
		{Op: "append", Path: "wiki/b.md", Content: "y"},
	}}
	res, _ := applyWikiActions(root, env, ApplyOptions{DryRun: true})
	if len(res.Applied) != 0 {
		t.Errorf("dry run wrote files: %v", res.Applied)
	}
	if len(res.Skipped) != 2 {
		t.Errorf("expected 2 skipped, got %v", res.Skipped)
	}
}

func TestApplyWikiActions_AppendExisting(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "wiki", "log.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "append", Path: "wiki/log.md", Content: "second\n",
	}}}
	res, _ := applyWikiActions(root, env, ApplyOptions{})
	if len(res.Errors) > 0 {
		t.Fatalf("append failed: %v", res.Errors)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "first\nsecond\n" {
		t.Errorf("append content wrong: %q", string(got))
	}
}

func TestApplyWikiActions_AppendMissingFails(t *testing.T) {
	root := t.TempDir()
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "append", Path: "wiki/missing.md", Content: "x",
	}}}
	res, _ := applyWikiActions(root, env, ApplyOptions{})
	if len(res.Applied) != 0 {
		t.Errorf("append against missing file should fail; applied=%v", res.Applied)
	}
	if len(res.Errors) == 0 {
		t.Error("expected error on missing append target")
	}
}

func TestApplyWikiActions_ReplaceSection(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "wiki", "page.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "---\ntitle: \"P\"\n---\n\n# Page\n\n## Intro\n\nold intro\n\n## Details\n\ndeets\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "replace_section", Path: "wiki/page.md", Heading: "Intro", Content: "fresh intro paragraph",
	}}}
	res, _ := applyWikiActions(root, env, ApplyOptions{})
	if len(res.Errors) > 0 {
		t.Fatalf("replace failed: %v", res.Errors)
	}
	got, _ := os.ReadFile(target)
	s := string(got)
	if !strings.Contains(s, "fresh intro paragraph") {
		t.Errorf("new content missing: %s", s)
	}
	if strings.Contains(s, "old intro") {
		t.Errorf("old content not removed: %s", s)
	}
	// Details section must survive untouched.
	if !strings.Contains(s, "## Details\n\ndeets\n") {
		t.Errorf("Details section corrupted: %s", s)
	}
}

func TestApplyWikiActions_ReplaceSectionUnknownHeading(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "wiki", "p.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("# X\n\n## Real\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "replace_section", Path: "wiki/p.md", Heading: "Imaginary", Content: "x",
	}}}
	res, _ := applyWikiActions(root, env, ApplyOptions{})
	if len(res.Applied) != 0 {
		t.Errorf("expected section-not-found to fail; applied=%v", res.Applied)
	}
	if len(res.Errors) == 0 {
		t.Error("expected error for unknown heading")
	}
}

func TestApplyWikiActions_UpdateFrontmatter(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "wiki", "fm.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "---\ntitle: \"Old\"\nconfidence: low\ntags: [a, b]\n---\n\nbody preserved\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "update_frontmatter", Path: "wiki/fm.md",
		Frontmatter: map[string]any{"confidence": "high", "updated": "2026-04-28"},
	}}}
	res, _ := applyWikiActions(root, env, ApplyOptions{})
	if len(res.Errors) > 0 {
		t.Fatalf("frontmatter update failed: %v", res.Errors)
	}
	got, _ := os.ReadFile(target)
	s := string(got)
	if !strings.Contains(s, "confidence: high") {
		t.Errorf("confidence not updated: %s", s)
	}
	if !strings.Contains(s, "updated: \"2026-04-28\"") && !strings.Contains(s, "updated: 2026-04-28") {
		t.Errorf("updated key not added: %s", s)
	}
	if !strings.Contains(s, "title: Old") && !strings.Contains(s, "title: \"Old\"") {
		t.Errorf("title preserved? %s", s)
	}
	if !strings.Contains(s, "body preserved") {
		t.Errorf("body should be untouched: %s", s)
	}
}

func TestApplyWikiActions_UnknownOpReportsError(t *testing.T) {
	root := t.TempDir()
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "rm_rf", Path: "wiki/anything.md",
	}}}
	res, _ := applyWikiActions(root, env, ApplyOptions{})
	if len(res.Errors) == 0 {
		t.Error("expected error on unknown op")
	}
	if len(res.Applied) != 0 {
		t.Errorf("unknown op should not apply; got %v", res.Applied)
	}
}

func TestApplyWikiActions_PartialFailureContinues(t *testing.T) {
	// First action is bad (path traversal); second is good. Executor
	// must continue past the first and apply the second.
	root := t.TempDir()
	env := WikiActionEnvelope{Actions: []WikiAction{
		{Op: "create", Path: "../escape.md", Content: "x"},
		{Op: "create", Path: "wiki/ok.md", Content: "y"},
	}}
	res, _ := applyWikiActions(root, env, ApplyOptions{})
	if len(res.Errors) == 0 {
		t.Error("expected an error from the traversal action")
	}
	if len(res.Applied) != 1 || res.Applied[0] != "wiki/ok.md" {
		t.Errorf("good action should still apply; got %v", res.Applied)
	}
}

func TestApplyWikiActions_AtomicWriteSemantics(t *testing.T) {
	// Quick sanity: writeFileAtomic should not leave a .tmp file
	// behind on success. (Failure paths are harder to simulate
	// without a write-failure-injecting filesystem; covered by code
	// review of the rename branch.)
	root := t.TempDir()
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "create", Path: "wiki/atomic.md", Content: "ok",
	}}}
	if _, err := applyWikiActions(root, env, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "wiki"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover tmp file: %s", e.Name())
		}
	}
}

func TestApplyWikiActions_EmptyRootError(t *testing.T) {
	env := WikiActionEnvelope{Actions: []WikiAction{{Op: "create", Path: "wiki/x.md", Content: "x"}}}
	if _, err := applyWikiActions("", env, ApplyOptions{}); err == nil {
		t.Error("expected catastrophic error on empty root")
	}
}
