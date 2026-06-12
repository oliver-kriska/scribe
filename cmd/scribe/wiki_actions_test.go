package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestValidateActionPath_RejectsDoubledMdExtension guards the visible
// symptom of the KB-self-ingestion bug: a filename fed back in as a title
// (KB README → article "readme.md" → page "readme.md.md"). No legitimate
// page ends in ".md.md", so the gate turns it into a recorded error for any
// source instead of a silent duplicate.
func TestValidateActionPath_RejectsDoubledMdExtension(t *testing.T) {
	root := t.TempDir()
	rejected := []string{
		"wiki/readme.md.md",
		"wiki/README.MD.MD", // case-insensitive
		"projects/foo/readme.md.md",
	}
	for _, p := range rejected {
		if _, err := validateActionPath(root, p); err == nil {
			t.Errorf("expected %q to be rejected (doubled .md extension)", p)
		}
	}
	accepted := []string{
		"wiki/readme.md",
		"wiki/2026-05-25.notes.md", // a dotted stem is fine; only ".md.md" is malformed
		"wiki/readme_md.md",        // distinct (underscore) basename — not the doubled-ext shape
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

// TestApplyWikiActions_CreateRefusesEmptyBody is the regression for the
// frontmatter-only / blank pages that accumulated in the KB: a `create`
// whose content has no body after the frontmatter must be refused (recorded
// as an error) and must NOT touch disk — even with AllowOverwrite (pass-2).
func TestApplyWikiActions_CreateRefusesEmptyBody(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"frontmatter_only", "---\ntitle: \"Stub\"\ntags: []\n---\n"},
		{"frontmatter_only_no_trailing_nl", "---\ntitle: \"Stub\"\n---"},
		{"completely_empty", ""},
		{"whitespace_only", "   \n\n\t\n"},
		{"frontmatter_then_whitespace", "---\ntitle: \"Stub\"\n---\n   \n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			env := WikiActionEnvelope{Actions: []WikiAction{{
				Op: "create", Path: "wiki/stub.md", Content: tc.content,
			}}}
			// AllowOverwrite:true mirrors the pass-2 entity writer — the
			// guard must fire before the write regardless.
			res, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Applied) != 0 {
				t.Errorf("empty page should not be applied; applied=%v", res.Applied)
			}
			if len(res.Errors) == 0 {
				t.Error("expected an error recorded for the empty page")
			}
			if _, statErr := os.Stat(filepath.Join(root, "wiki", "stub.md")); statErr == nil {
				t.Error("empty page was written to disk; guard failed")
			}
		})
	}
}

// TestApplyWikiActions_CreateAcceptsRealBody guards the opposite direction:
// a page with an actual body (with or without frontmatter) still writes.
func TestApplyWikiActions_CreateAcceptsRealBody(t *testing.T) {
	for _, content := range []string{
		"---\ntitle: \"Real\"\n---\n\nThis page has a body.\n",
		"no frontmatter but a real body",
	} {
		root := t.TempDir()
		env := WikiActionEnvelope{Actions: []WikiAction{{
			Op: "create", Path: "wiki/real.md", Content: content,
		}}}
		res, err := applyWikiActions(root, env, ApplyOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Errors) != 0 || len(res.Applied) != 1 {
			t.Errorf("real-body page should write cleanly; applied=%v errors=%v", res.Applied, res.Errors)
		}
	}
}

func TestBodyAfterFrontmatter(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"---\ntitle: x\n---\n\nbody here\n", "body here"},
		{"---\ntitle: x\n---\n", ""},
		{"---\ntitle: x\n---", ""},
		{"---\ntitle: x\n---\n   \n", ""},
		{"", ""},
		{"   \n\t\n", ""},
		{"no frontmatter body", "no frontmatter body"},
		{"---\nunterminated frontmatter\nstill going", "---\nunterminated frontmatter\nstill going"},
	}
	for _, tc := range cases {
		if got := bodyAfterFrontmatter(tc.in); got != tc.want {
			t.Errorf("bodyAfterFrontmatter(%q) = %q, want %q", tc.in, got, tc.want)
		}
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

// ---- Unified content sanitization seam (SanitizeContent) ----
//
// Before this seam, fact-ID strip + related: normalize were hand-wired
// into only the two absorb call sites; dream/assess/extract/deep/
// session-mine wrote local-model-corrupted content straight to disk.
// These assert the seam now runs for ANY caller that opts in, with the
// same behavior the absorb paths had, and is a strict no-op when off.

func TestApplyWikiActions_SanitizeContentSeam(t *testing.T) {
	// A local model's output: malformed related: frontmatter + fabricated
	// fact-ID brackets in the body.
	const corrupt = "---\ntitle: X\nrelated: [][AuthoredUp][LangChain]\n---\nclaim one [c01-f99] and claim two [c02-f01].\n"

	t.Run("opt-in strips all fact-IDs (nil set) and normalizes related", func(t *testing.T) {
		root := t.TempDir()
		env := WikiActionEnvelope{Actions: []WikiAction{{
			Op: "create", Path: "wiki/x.md", Content: corrupt,
		}}}
		// ValidFactIDs nil ⇒ strip every [cNN-fM] — the situation for the
		// six non-absorb callers (no facts pass produced any valid IDs).
		res, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, SanitizeContent: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Errors) != 0 {
			t.Fatalf("unexpected errors: %v", res.Errors)
		}
		s := readBack(t, root, "wiki/x.md")
		if strings.Contains(s, "[c01-f99]") || strings.Contains(s, "[c02-f01]") {
			t.Errorf("fabricated fact-ID brackets survived the seam:\n%s", s)
		}
		assertRelated(t, s, []string{"[[AuthoredUp]]", "[[LangChain]]"})
	})

	t.Run("ValidFactIDs keeps grounded IDs, strips the rest", func(t *testing.T) {
		root := t.TempDir()
		env := WikiActionEnvelope{Actions: []WikiAction{{
			Op: "create", Path: "wiki/x.md",
			Content: "---\ntitle: X\n---\nreal [c01-f01] fake [c09-f09].\n",
		}}}
		res, err := applyWikiActions(root, env, ApplyOptions{
			AllowOverwrite:  true,
			SanitizeContent: true,
			ValidFactIDs:    map[string]bool{"c01-f01": true},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Errors) != 0 {
			t.Fatalf("unexpected errors: %v", res.Errors)
		}
		s := readBack(t, root, "wiki/x.md")
		if !strings.Contains(s, "[c01-f01]") {
			t.Errorf("grounded fact-ID was wrongly stripped:\n%s", s)
		}
		if strings.Contains(s, "[c09-f09]") {
			t.Errorf("fabricated fact-ID survived:\n%s", s)
		}
	})

	t.Run("opt-out leaves content byte-identical (backward compatible)", func(t *testing.T) {
		root := t.TempDir()
		env := WikiActionEnvelope{Actions: []WikiAction{{
			Op: "create", Path: "wiki/x.md", Content: corrupt,
		}}}
		if _, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true}); err != nil {
			t.Fatal(err)
		}
		if got := readBack(t, root, "wiki/x.md"); got != corrupt {
			t.Errorf("opt-out must write content verbatim; got:\n%s", got)
		}
	})

	t.Run("already-correct related survives the seam intact", func(t *testing.T) {
		root := t.TempDir()
		const good = "---\ntitle: X\nrelated: [\n  \"[[Alpha]]\",\n  \"[[Beta]]\"\n]\n---\nclean body\n"
		env := WikiActionEnvelope{Actions: []WikiAction{{
			Op: "create", Path: "wiki/x.md", Content: good,
		}}}
		if _, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, SanitizeContent: true}); err != nil {
			t.Fatal(err)
		}
		s := readBack(t, root, "wiki/x.md")
		assertRelated(t, s, []string{"[[Alpha]]", "[[Beta]]"})
		if !strings.HasSuffix(s, "\nclean body\n") {
			t.Errorf("body must be preserved verbatim:\n%s", s)
		}
	})
}

func readBack(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read back %s: %v", rel, err)
	}
	return string(b)
}

func assertRelated(t *testing.T, content string, want []string) {
	t.Helper()
	rel := relatedField(t, content)
	list, ok := rel.([]any)
	if !ok {
		t.Fatalf("related is not a normalized YAML list: %#v", rel)
	}
	if len(list) != len(want) {
		t.Fatalf("want %d related, got %d (%#v)", len(want), len(list), list)
	}
	for i, w := range want {
		if got, _ := list[i].(string); got != w {
			t.Errorf("related[%d] = %q, want %q", i, got, w)
		}
	}
}

// TestApplyWikiActions_ClampFrontmatter covers the frontmatter half of
// the pre-apply seam (clampEnvelopeFrontmatter). It is the executor's
// answer to the local-model lint dump: invalid type/domain and
// unparseable frontmatter must never reach disk, regardless of which of
// the 8 envelope consumers produced the envelope. Tempdir KBs have no
// scribe.yaml, so validDomainsForRoot resolves to the universal set
// {personal, general} — "research" is therefore an invalid domain here.
func TestApplyWikiActions_ClampFrontmatter(t *testing.T) {
	t.Run("invalid type clamps to the path's canonical type", func(t *testing.T) {
		root := t.TempDir()
		env := WikiActionEnvelope{Actions: []WikiAction{{
			Op: "create", Path: "decisions/x.md",
			Content: "---\ntitle: X\ntype: article\ndomain: general\n---\nbody\n",
		}}}
		res, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, SanitizeContent: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Errors) != 0 {
			t.Fatalf("unexpected errors: %v", res.Errors)
		}
		s := readBack(t, root, "decisions/x.md")
		if !strings.Contains(s, "\ntype: decision\n") {
			t.Errorf("type not clamped to canonical 'decision':\n%s", s)
		}
		if strings.Contains(s, "type: article") {
			t.Errorf("invalid type survived:\n%s", s)
		}
		if !strings.HasSuffix(s, "\nbody\n") {
			t.Errorf("body must be preserved verbatim:\n%s", s)
		}
	})

	t.Run("invalid domain clamps to general", func(t *testing.T) {
		root := t.TempDir()
		env := WikiActionEnvelope{Actions: []WikiAction{{
			Op: "create", Path: "research/y.md",
			Content: "---\ntitle: Y\ntype: research\ndomain: research\n---\nbody\n",
		}}}
		if _, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, SanitizeContent: true}); err != nil {
			t.Fatal(err)
		}
		s := readBack(t, root, "research/y.md")
		if !strings.Contains(s, "\ndomain: general\n") {
			t.Errorf("domain not clamped to 'general':\n%s", s)
		}
	})

	t.Run("missing type is inserted from the path", func(t *testing.T) {
		root := t.TempDir()
		env := WikiActionEnvelope{Actions: []WikiAction{{
			Op: "create", Path: "patterns/z.md",
			Content: "---\ntitle: Z\ndomain: general\n---\nbody\n",
		}}}
		if _, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, SanitizeContent: true}); err != nil {
			t.Fatal(err)
		}
		s := readBack(t, root, "patterns/z.md")
		if !strings.Contains(s, "\ntype: pattern\n") {
			t.Errorf("missing type not inserted from path:\n%s", s)
		}
	})

	t.Run("unparseable frontmatter drops the action (no garbage on disk)", func(t *testing.T) {
		root := t.TempDir()
		env := WikiActionEnvelope{Actions: []WikiAction{{
			// No closing delimiter — the exact "no closing frontmatter
			// delimiter" lint class.
			Op: "create", Path: "decisions/broken.md",
			Content: "---\ntitle: Broken\ntype: decision\nbody with no close\n",
		}}}
		res, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, SanitizeContent: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Applied) != 0 {
			t.Errorf("unparseable action must not be applied, got: %v", res.Applied)
		}
		if _, statErr := os.Stat(filepath.Join(root, "decisions", "broken.md")); statErr == nil {
			t.Error("broken frontmatter was written to disk; clamp must drop it")
		}
	})

	t.Run("invalid type in wiki/ falls back to research, not dropped", func(t *testing.T) {
		root := t.TempDir()
		env := WikiActionEnvelope{Actions: []WikiAction{{
			Op: "create", Path: "wiki/general-note.md",
			Content: "---\ntitle: M\ntype: article\ndomain: general\n---\nbody\n",
		}}}
		res, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, SanitizeContent: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Applied) != 1 {
			t.Fatalf("wiki/ content must be salvaged, not dropped: %v / %v", res.Applied, res.Errors)
		}
		s := readBack(t, root, "wiki/general-note.md")
		if !strings.Contains(s, "\ntype: research\n") {
			t.Errorf("wiki/ invalid type should fall back to 'research':\n%s", s)
		}
	})

	t.Run("valid frontmatter passes through untouched", func(t *testing.T) {
		root := t.TempDir()
		const good = "---\ntitle: Good\ntype: solution\ndomain: general\ncreated: 2026-01-05\nupdated: 2026-02-01\nconfidence: high\n---\nclean body\n"
		env := WikiActionEnvelope{Actions: []WikiAction{{
			Op: "create", Path: "solutions/good.md", Content: good,
		}}}
		if _, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, SanitizeContent: true}); err != nil {
			t.Fatal(err)
		}
		if got := readBack(t, root, "solutions/good.md"); got != good {
			t.Errorf("valid frontmatter must be byte-identical; got:\n%s", got)
		}
	})

	t.Run("opt-out writes verbatim (backward compatible)", func(t *testing.T) {
		root := t.TempDir()
		const bad = "---\ntitle: X\ntype: article\ndomain: research\n---\nbody\n"
		env := WikiActionEnvelope{Actions: []WikiAction{{
			Op: "create", Path: "decisions/x.md", Content: bad,
		}}}
		if _, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true}); err != nil {
			t.Fatal(err)
		}
		if got := readBack(t, root, "decisions/x.md"); got != bad {
			t.Errorf("opt-out must write verbatim; got:\n%s", got)
		}
	})

	t.Run("missing created/updated/confidence are stamped with lint --fix defaults", func(t *testing.T) {
		root := t.TempDir()
		env := WikiActionEnvelope{Actions: []WikiAction{{
			Op: "create", Path: "solutions/stamped.md",
			Content: "---\ntitle: S\ntype: solution\ndomain: general\n---\nbody\n",
		}}}
		if _, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, SanitizeContent: true}); err != nil {
			t.Fatal(err)
		}
		s := readBack(t, root, "solutions/stamped.md")
		today := time.Now().UTC().Format("2006-01-02")
		for _, want := range []string{
			"\ncreated: " + today + "\n",
			"\nupdated: " + today + "\n",
			"\nconfidence: medium\n",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("missing stamp %q in:\n%s", strings.TrimSpace(want), s)
			}
		}
		// The write must satisfy the validator's scalar requirements —
		// the whole point is that envelope-born articles pass lint.
		fm, err := parseFrontmatter([]byte(s))
		if err != nil {
			t.Fatalf("stamped frontmatter unparseable: %v", err)
		}
		if fm.Confidence != "medium" {
			t.Errorf("Confidence = %q, want medium", fm.Confidence)
		}
	})

	t.Run("existing dates and confidence are never overwritten", func(t *testing.T) {
		root := t.TempDir()
		const authored = "---\ntitle: A\ntype: solution\ndomain: general\ncreated: 2025-11-30\nupdated: 2025-12-15\nconfidence: low\n---\nbody\n"
		env := WikiActionEnvelope{Actions: []WikiAction{{
			Op: "create", Path: "solutions/authored.md", Content: authored,
		}}}
		if _, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, SanitizeContent: true}); err != nil {
			t.Fatal(err)
		}
		if got := readBack(t, root, "solutions/authored.md"); got != authored {
			t.Errorf("model-provided scalars must survive; got:\n%s", got)
		}
	})

	t.Run("invalid confidence is left for lint, not rewritten", func(t *testing.T) {
		root := t.TempDir()
		env := WikiActionEnvelope{Actions: []WikiAction{{
			Op: "create", Path: "solutions/odd.md",
			Content: "---\ntitle: O\ntype: solution\ndomain: general\ncreated: 2026-01-01\nupdated: 2026-01-01\nconfidence: speculative\n---\nbody\n",
		}}}
		if _, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, SanitizeContent: true}); err != nil {
			t.Fatal(err)
		}
		s := readBack(t, root, "solutions/odd.md")
		if !strings.Contains(s, "confidence: speculative") {
			t.Errorf("stated confidence must not be silently rewritten:\n%s", s)
		}
		if strings.Contains(s, "confidence: medium") {
			t.Errorf("invalid confidence wrongly clamped to medium:\n%s", s)
		}
	})
}

// TestLoadPrompt_StripsUnsubstitutedPlaceholders asserts the centralized
// guard in loadPrompt: any {{VAR}} the caller didn't supply (the
// session-extract {{DOMAIN}} class) is stripped, never shipped to the
// model verbatim. Uses a real embedded prompt with NO vars supplied so
// every token is residual.
func TestLoadPrompt_StripsUnsubstitutedPlaceholders(t *testing.T) {
	out, err := loadPrompt("session-extract-ollama.md", map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "{{") || strings.Contains(out, "}}") {
		t.Errorf("residual placeholder survived loadPrompt:\n%s", out)
	}
}

// TestApplyWikiActions_SanitizeContentEnablesRemap asserts the
// 2026-05-16 fix: a SanitizeContent caller inherits the unknown-top-dir
// remap automatically (no explicit RemapUnknownTopToWiki), so a local
// model that invents debugging/ / todo/ / github-issues/ no longer
// loses the mined entity — it is re-homed under wiki/.
func TestApplyWikiActions_SanitizeContentEnablesRemap(t *testing.T) {
	root := t.TempDir()
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "create", Path: "debugging/plugin-activation-caveat.md",
		Content: "---\ntitle: Caveat\ntype: research\ndomain: general\n---\nbody\n",
	}}}
	res, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, SanitizeContent: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("invented top dir must be remapped, not rejected: %v", res.Errors)
	}
	if _, statErr := os.Stat(filepath.Join(root, "wiki", "debugging", "plugin-activation-caveat.md")); statErr != nil {
		t.Errorf("entity not re-homed under wiki/: %v", statErr)
	}
}

// --- Dream blind-write guards (B1/B2/B3) ---------------------------------
//
// Dream runs blind: the model sees article metadata, never bodies. These
// tests pin the executor guards that stop a hallucinated envelope from
// destroying curated content. See the 2026-06-03 ingestion-quality audit.

// TestApplyWikiActions_DreamOptionsRefuseClobber: under dream's exact
// options (AllowOverwrite off, ProtectProvenance on) a `create` against an
// existing curated doc is refused — the 88→40 master-doc gutting vector.
func TestApplyWikiActions_DreamOptionsRefuseClobber(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "research", "master.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := "---\ntitle: Master\ntype: research\ndomain: general\nsources:\n  - /raw/a.md\n  - /raw/b.md\n---\n\n| study | finding |\n|---|---|\n| A | x |\n"
	if err := os.WriteFile(target, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op:      "create",
		Path:    "research/master.md",
		Content: "---\ntitle: Master\ntype: research\ndomain: general\n---\n\nthin hallucinated body\n",
	}}}
	res, _ := applyWikiActions(root, env, ApplyOptions{SanitizeContent: true, ProtectProvenance: true})
	if len(res.Applied) != 0 {
		t.Errorf("blind create over existing curated doc must be refused; applied=%v", res.Applied)
	}
	got, _ := os.ReadFile(target)
	if string(got) != orig {
		t.Errorf("curated doc was clobbered:\n%s", string(got))
	}
}

// TestApplyWikiActions_ProtectProvenanceDropsIdentityKeys: update_frontmatter
// under ProtectProvenance keeps allowlisted keys (updated) and drops
// provenance/identity keys (sources, title) the blind model invented.
func TestApplyWikiActions_ProtectProvenanceDropsIdentityKeys(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "research", "doc.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := "---\ntitle: Real Title\nsources:\n  - /raw/a.md\nupdated: 2026-01-01\n---\n\nbody\n"
	if err := os.WriteFile(target, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op:   "update_frontmatter",
		Path: "research/doc.md",
		Frontmatter: map[string]any{
			"updated": "2026-06-03",
			"sources": "session:abc",
			"title":   "Hallucinated",
		},
	}}}
	res, _ := applyWikiActions(root, env, ApplyOptions{ProtectProvenance: true})
	if len(res.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	s := mustRead(t, target)
	// yaml.Marshal may quote the scalar (updated: "2026-06-03"); assert on
	// the value + key, not an exact unquoted rendering.
	if !strings.Contains(s, "updated:") || !strings.Contains(s, "2026-06-03") || strings.Contains(s, "2026-01-01") {
		t.Errorf("allowlisted key `updated` not applied:\n%s", s)
	}
	if strings.Contains(s, "session:abc") {
		t.Errorf("provenance key `sources` was overwritten:\n%s", s)
	}
	if !strings.Contains(s, "/raw/a.md") {
		t.Errorf("original sources lost:\n%s", s)
	}
	if strings.Contains(s, "Hallucinated") || !strings.Contains(s, "Real Title") {
		t.Errorf("identity key `title` was overwritten:\n%s", s)
	}
}

// TestApplyWikiActions_ProtectProvenanceAllDroppedIsNoOp: when every key the
// model sent is non-allowlisted, the action is a skip (not an error) and the
// file is untouched byte-for-byte.
func TestApplyWikiActions_ProtectProvenanceAllDroppedIsNoOp(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "research", "doc.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := "---\ntitle: T\nsources:\n  - /raw/a.md\n---\n\nbody\n"
	if err := os.WriteFile(target, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op:          "update_frontmatter",
		Path:        "research/doc.md",
		Frontmatter: map[string]any{"sources": "session:x", "created": "2026-06-03"},
	}}}
	res, _ := applyWikiActions(root, env, ApplyOptions{ProtectProvenance: true})
	if len(res.Errors) != 0 {
		t.Fatalf("full-drop should be a no-op, not an error: %v", res.Errors)
	}
	if len(res.Skipped) != 1 {
		t.Errorf("expected 1 skipped, got applied=%v skipped=%v", res.Applied, res.Skipped)
	}
	if mustRead(t, target) != orig {
		t.Errorf("file changed on full-drop no-op")
	}
}

// TestApplyWikiActions_ProtectProvenanceOffKeepsKeys: without the flag the
// pre-existing behavior is unchanged — every key is merged.
func TestApplyWikiActions_ProtectProvenanceOffKeepsKeys(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "research", "doc.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("---\ntitle: T\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op:          "update_frontmatter",
		Path:        "research/doc.md",
		Frontmatter: map[string]any{"sources": "session:x"},
	}}}
	res, _ := applyWikiActions(root, env, ApplyOptions{})
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	if !strings.Contains(mustRead(t, target), "session:x") {
		t.Errorf("without ProtectProvenance, sources should merge as before")
	}
}

// TestApplyWikiActions_DecayMarkerIdempotent: a second decay append onto a
// file that already carries the marker is skipped, so two dream passes leave
// exactly one marker.
func TestApplyWikiActions_DecayMarkerIdempotent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "wiki", "stale.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("---\ntitle: S\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := "\n<!-- decay-candidate 2026-06-03 -->\n"
	mk := func() WikiActionEnvelope {
		return WikiActionEnvelope{Actions: []WikiAction{{Op: "append", Path: "wiki/stale.md", Content: marker}}}
	}
	if res, _ := applyWikiActions(root, mk(), ApplyOptions{}); len(res.Applied) != 1 {
		t.Fatalf("first decay append should write; got %v", res)
	}
	res2, _ := applyWikiActions(root, mk(), ApplyOptions{})
	if len(res2.Skipped) != 1 {
		t.Errorf("second decay append should skip; got applied=%v skipped=%v", res2.Applied, res2.Skipped)
	}
	if n := strings.Count(mustRead(t, target), decayCandidateMarker); n != 1 {
		t.Errorf("decay marker should appear exactly once, got %d", n)
	}
}

// TestEntityWriterApplyOptions pins the shared policy for mining/assess/
// extract/dream consumers: never overwrite an existing curated doc, always
// protect provenance, always sanitize. The 2026-06-03 master-doc gutting
// was a copy-pasted AllowOverwrite:true on session-mine; this guards the
// regression at the policy seam (TestApplyWikiActions_DreamOptionsRefuseClobber
// proves the same options refuse a clobber at the executor).
func TestEntityWriterApplyOptions(t *testing.T) {
	opts := entityWriterApplyOptions()
	if opts.AllowOverwrite {
		t.Error("entity-writer consumers must not overwrite existing curated docs")
	}
	if !opts.ProtectProvenance {
		t.Error("entity-writer consumers must protect provenance frontmatter")
	}
	if !opts.SanitizeContent {
		t.Error("entity-writer consumers must sanitize envelope content")
	}
}

// writeDecayTarget creates a wiki article with the given `updated:` date and
// returns its absolute path. Empty updated omits the field entirely.
func writeDecayTarget(t *testing.T, root, name, updated string) string {
	t.Helper()
	target := filepath.Join(root, "wiki", name)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	fm := "---\ntitle: S\n"
	if updated != "" {
		fm += "updated: " + updated + "\n"
	}
	fm += "---\n\nbody\n"
	if err := os.WriteFile(target, []byte(fm), 0o644); err != nil {
		t.Fatal(err)
	}
	return target
}

// TestTargetIsFreshForDecay pins the date math that decides whether a decay
// marker is legitimate. Fresh (updated within decayStaleDays) ⇒ true;
// genuinely stale, missing date, malformed date, and missing file all ⇒
// false (fail-open: only block when freshness is provable).
func TestTargetIsFreshForDecay(t *testing.T) {
	root := t.TempDir()
	today := time.Now().Format("2006-01-02")
	stale := time.Now().AddDate(0, 0, -decayStaleDays-30).Format("2006-01-02")
	// One day inside the window is unambiguously fresh; one day past it is
	// unambiguously stale. The exact cutoff day is intentionally untested —
	// it depends on time.Now()'s time-of-day, and the guard inherits that
	// fuzz from dreamStaleCandidates on purpose (see targetIsFreshForDecay).
	justInside := time.Now().AddDate(0, 0, -decayStaleDays+1).Format("2006-01-02")

	cases := []struct {
		name    string
		updated string
		want    bool
	}{
		{"updated-today", today, true},
		{"updated-90d-ago", stale, false},
		{"just-inside-window", justInside, true},
		{"no-updated-field", "", false},
		{"malformed-date", "not-a-date", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			abs := writeDecayTarget(t, root, tc.name+".md", tc.updated)
			if got := targetIsFreshForDecay(abs); got != tc.want {
				t.Errorf("targetIsFreshForDecay(updated=%q) = %v, want %v", tc.updated, got, tc.want)
			}
		})
	}
	t.Run("missing-file", func(t *testing.T) {
		if targetIsFreshForDecay(filepath.Join(root, "wiki", "ghost.md")) {
			t.Error("missing file must be treated as not-fresh (fail-open), got true")
		}
	})
}

// TestApplyWikiActions_DecayRefusesFreshDoc proves the executor staleness
// guard: a decay marker append onto a doc updated within decayStaleDays is
// skipped (not written), regardless of what the model proposed. This is the
// hard guarantee behind the 2026-06-03 incident where 114 markers landed on
// docs updated <60 days prior.
func TestApplyWikiActions_DecayRefusesFreshDoc(t *testing.T) {
	root := t.TempDir()
	today := time.Now().Format("2006-01-02")
	target := writeDecayTarget(t, root, "fresh.md", today)
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "append", Path: "wiki/fresh.md", Content: "\n<!-- decay-candidate " + today + " -->\n",
	}}}
	res, err := applyWikiActions(root, env, entityWriterApplyOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || len(res.Applied) != 0 {
		t.Fatalf("fresh doc decay marker should be skipped; got applied=%v skipped=%v", res.Applied, res.Skipped)
	}
	if strings.Contains(mustRead(t, target), decayCandidateMarker) {
		t.Error("fresh doc must not receive a decay marker on disk")
	}
}

// TestApplyWikiActions_DecayAllowsStaleDoc is the complement: a genuinely
// stale doc (updated well beyond decayStaleDays) still accepts the marker, so
// the guard rejects only the self-contradictory case, not all decay marking.
func TestApplyWikiActions_DecayAllowsStaleDoc(t *testing.T) {
	root := t.TempDir()
	today := time.Now().Format("2006-01-02")
	stale := time.Now().AddDate(0, 0, -decayStaleDays-30).Format("2006-01-02")
	target := writeDecayTarget(t, root, "old.md", stale)
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "append", Path: "wiki/old.md", Content: "\n<!-- decay-candidate " + today + " -->\n",
	}}}
	res, err := applyWikiActions(root, env, entityWriterApplyOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 1 {
		t.Fatalf("stale doc decay marker should be written; got applied=%v skipped=%v errors=%v", res.Applied, res.Skipped, res.Errors)
	}
	if !strings.Contains(mustRead(t, target), decayCandidateMarker) {
		t.Error("stale doc should carry the decay marker on disk")
	}
}

// TestApplyWikiActions_DecayGuardFiresInDryRun proves the guard sits BEFORE
// the dry-run gate, so `dream --dry-run` reports a refusal (Skipped) on a
// fresh doc rather than a phantom "would append" — and writes nothing.
func TestApplyWikiActions_DecayGuardFiresInDryRun(t *testing.T) {
	root := t.TempDir()
	today := time.Now().Format("2006-01-02")
	target := writeDecayTarget(t, root, "fresh.md", today)
	before := mustRead(t, target)
	env := WikiActionEnvelope{Actions: []WikiAction{{
		Op: "append", Path: "wiki/fresh.md", Content: "\n<!-- decay-candidate " + today + " -->\n",
	}}}
	res, err := applyWikiActions(root, env, ApplyOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("dry-run fresh-doc decay should be skipped by the guard; got %+v", res)
	}
	if mustRead(t, target) != before {
		t.Error("dry-run must not modify the file")
	}
}
