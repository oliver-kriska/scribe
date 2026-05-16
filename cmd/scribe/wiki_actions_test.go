package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// Layer 1 (0.2.18): the executor must reject any action whose basename
// starts with "_". These are scribe-generated artifacts (_index.md,
// _backlinks.json, _absorb_log.json, …); a model `append` onto an
// existing one silently corrupts it and aborts the absorb phase. The
// guard is basename-based so a _-file in any wiki subdir is caught.
func TestValidateActionPath_RejectsUnderscorePrefixedArtifacts(t *testing.T) {
	root := t.TempDir()
	rejected := []string{
		"wiki/_index.md",
		"wiki/_backlinks.json",
		"wiki/_absorb_log.json",
		"wiki/_hot.md",
		"wiki/_staleness.jsonl",
		"projects/_scratch.md",  // _-file in a subdir, not just wiki/
		"decisions/_draft.md",   // basename check, not top-level only
		"sessions/2026/_tmp.md", // nested subdir
	}
	for _, p := range rejected {
		if _, err := validateActionPath(root, p); err == nil {
			t.Errorf("expected %q to be rejected (underscore-prefixed artifact)", p)
		}
	}
	accepted := []string{
		"wiki/real-article.md",
		"patterns/some_pattern.md", // underscore mid-name is fine
		"projects/my-proj/overview.md",
		"decisions/a_b_c.md", // underscores allowed when not the prefix
	}
	for _, p := range accepted {
		if _, err := validateActionPath(root, p); err != nil {
			t.Errorf("expected %q to be accepted, got: %v", p, err)
		}
	}
}

// Layer 2 (0.2.18): an `append` whose target is missing is promoted to
// `create` rather than erroring — the model's intent (this content
// belongs at this path) is still satisfiable. Layer 1 runs first, so a
// _-prefixed missing target is still rejected, never promoted.
func TestApplyWikiActions_AppendMissingPromotesToCreate(t *testing.T) {
	root := t.TempDir()
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "append", Path: "patterns/brand-new.md", Content: "---\ntitle: \"X\"\n---\n\nbody\n",
	}}}
	res, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("append-to-missing should promote to create, got errors: %v", res.Errors)
	}
	if len(res.Applied) != 1 {
		t.Fatalf("expected 1 applied, got %v", res.Applied)
	}
	got, err := os.ReadFile(filepath.Join(root, "patterns", "brand-new.md"))
	if err != nil {
		t.Fatalf("promoted create did not write the file: %v", err)
	}
	if !strings.Contains(string(got), "title: \"X\"") {
		t.Errorf("promoted content mismatch: %q", string(got))
	}
}

func TestApplyWikiActions_AppendExistingStillAppends(t *testing.T) {
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
		t.Fatalf("append to existing file errored: %v", res.Errors)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "first\nsecond\n" {
		t.Errorf("append did not concatenate; got %q", string(got))
	}
}

func TestApplyWikiActions_AppendMissingUnderscoreStillRejected(t *testing.T) {
	root := t.TempDir()
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "append", Path: "wiki/_absorb_log.json", Content: `{"bogus":1}`,
	}}}
	res, _ := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true})
	if len(res.Applied) != 0 {
		t.Errorf("underscore-prefixed append must NOT be promoted to create; applied=%v", res.Applied)
	}
	if len(res.Errors) == 0 {
		t.Error("expected a path-validation error for _-prefixed append target")
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "_absorb_log.json")); err == nil {
		t.Error("Layer 1 must prevent the file from being created via append→create")
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

// Behavior change (0.2.18): append-to-missing is no longer a hard
// error — it's promoted to create (see Layer 2). The model's intent
// is satisfiable, so honoring it beats discarding the generation. The
// _-prefixed-still-rejected and existing-still-appends cases are
// covered by the dedicated Layer 2 tests above.
func TestApplyWikiActions_AppendMissingPromotesNotFails(t *testing.T) {
	root := t.TempDir()
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "append", Path: "wiki/missing.md", Content: "x",
	}}}
	res, _ := applyWikiActions(root, env, ApplyOptions{})
	if len(res.Errors) != 0 {
		t.Errorf("append-to-missing should promote to create, not error; errors=%v", res.Errors)
	}
	if len(res.Applied) != 1 {
		t.Errorf("expected the promoted create to land; applied=%v", res.Applied)
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "missing.md")); err != nil {
		t.Errorf("promoted create did not write the file: %v", err)
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

// ---- Bug 1: related: frontmatter normalizer ----

func relatedField(t *testing.T, content string) any {
	t.Helper()
	body := strings.TrimPrefix(content, "---\n")
	end := strings.Index(body, "\n---")
	if end < 0 {
		t.Fatalf("no closing frontmatter delimiter in:\n%s", content)
	}
	var fm map[string]any
	if err := yaml.Unmarshal([]byte(body[:end]), &fm); err != nil {
		t.Fatalf("normalized frontmatter is not valid YAML: %v\n%s", err, content)
	}
	return fm["related"]
}

func TestNormalizeRelatedFrontmatter(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string // expected related entries (inner names), nil = empty list
	}{
		{
			name: "invalid yaml junk",
			in:   "---\ntitle: X\nrelated: [][AuthoredUp][LangChain]\nsources: [a]\n---\nbody\n",
			want: []string{"[[AuthoredUp]]", "[[LangChain]]"},
		},
		{
			name: "bare bracket-stripped list",
			in:   "---\ntitle: X\nrelated: [Terminal Bench 2.0, LangSmith, Harness Engineering]\n---\nbody\n",
			want: []string{"[[Terminal Bench 2.0]]", "[[LangSmith]]", "[[Harness Engineering]]"},
		},
		{
			name: "escaped garbage",
			in:   "---\ntitle: X\nrelated: [\"\\[Harness Engineering\\]\", \"\\ \\[Tools\\]\"]\n---\nbody\n",
			want: []string{"[[Harness Engineering]]", "[[Tools]]"},
		},
		{
			name: "already correct multiline block collapses cleanly",
			in:   "---\ntitle: X\nrelated: [\n  \"[[LangSmith Traces]]\",\n  \"[[Coding Agent]]\"\n]\nsources: [s]\n---\nbody\n",
			want: []string{"[[LangSmith Traces]]", "[[Coding Agent]]"},
		},
		{
			name: "empty list preserved",
			in:   "---\ntitle: X\nrelated: []\n---\nbody\n",
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := normalizeRelatedFrontmatter(c.in)
			if !strings.HasSuffix(out, "\nbody\n") {
				t.Errorf("body must be preserved verbatim, got:\n%s", out)
			}
			rel := relatedField(t, out)
			if c.want == nil {
				if rel != nil {
					if l, ok := rel.([]any); !ok || len(l) != 0 {
						t.Errorf("expected empty related, got %#v", rel)
					}
				}
				return
			}
			list, ok := rel.([]any)
			if !ok {
				t.Fatalf("related is not a YAML list: %#v", rel)
			}
			if len(list) != len(c.want) {
				t.Fatalf("want %d related, got %d (%#v)", len(c.want), len(list), list)
			}
			for i, w := range c.want {
				if got := list[i].(string); got != w {
					t.Errorf("related[%d] = %q, want %q", i, got, w)
				}
			}
		})
	}
}

func TestNormalizeRelatedFrontmatter_PassthroughWhenNoFrontmatterOrKey(t *testing.T) {
	noFM := "no frontmatter here, just [[a body link]]\n"
	if got := normalizeRelatedFrontmatter(noFM); got != noFM {
		t.Errorf("content without frontmatter must be unchanged")
	}
	noKey := "---\ntitle: X\ntags: [a, b]\n---\n[[body link]] stays\n"
	if got := normalizeRelatedFrontmatter(noKey); got != noKey {
		t.Errorf("frontmatter without related: must be unchanged, got:\n%s", got)
	}
}

// ---- Bug 2: out-of-bounds path remap (opt-in) ----

func TestValidateActionPath_UnknownTopSentinel(t *testing.T) {
	root := t.TempDir()
	_, err := validateActionPath(root, "middleware/foo.md")
	if !errors.Is(err, errUnknownTopDir) {
		t.Errorf("unknown top dir should wrap errUnknownTopDir, got %v", err)
	}
	if _, err := validateActionPath(root, "/etc/passwd"); errors.Is(err, errUnknownTopDir) {
		t.Error("absolute path must NOT be classified as unknown-top")
	}
	if _, err := validateActionPath(root, "../escape.md"); errors.Is(err, errUnknownTopDir) {
		t.Error("traversal must NOT be classified as unknown-top")
	}
}

func TestApplyWikiActions_RemapUnknownTopOptIn(t *testing.T) {
	root := t.TempDir()
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "create", Path: "middleware/loop-detection.md", Content: "---\ntitle: LDM\n---\nbody\n",
	}}}

	// Opted in: page is re-homed under wiki/, not dropped.
	res, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, RemapUnknownTopToWiki: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("remap should avoid errors, got %v", res.Errors)
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "middleware", "loop-detection.md")); err != nil {
		t.Errorf("remapped file not written under wiki/: %v", err)
	}

	// Default (no opt-in): unknown top is still a hard error, file dropped.
	root2 := t.TempDir()
	res2, _ := applyWikiActions(root2, env, ApplyOptions{AllowOverwrite: true})
	if len(res2.Errors) == 0 {
		t.Error("without opt-in, unknown top dir must remain an error")
	}
	if _, err := os.Stat(filepath.Join(root2, "wiki", "middleware", "loop-detection.md")); err == nil {
		t.Error("file must NOT be written when remap is off")
	}
}

func TestApplyWikiActions_RemapNeverResurrectsUnsafePaths(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"../escape.md", "wiki/_index.md", "/etc/passwd"} {
		env := WikiActionEnvelope{Actions: []WikiAction{{Op: "create", Path: p, Content: "x"}}}
		res, _ := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, RemapUnknownTopToWiki: true})
		if len(res.Errors) == 0 {
			t.Errorf("unsafe path %q must stay rejected even with remap opted in", p)
		}
	}
}
