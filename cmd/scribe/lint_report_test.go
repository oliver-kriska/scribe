package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLintReportGroupedFlush: default mode groups per-file warnings by
// class — count descending, counts right-aligned, remediation hints from
// lintHints column-aligned. This is the issue-15 output shape:
//
//	412× index_tier missing       (run: scribe tier write --missing-only)
//	 23× thin article
func TestLintReportGroupedFlush(t *testing.T) {
	var buf bytes.Buffer
	rep := newLintReport(&buf, false, false)

	for range 12 {
		rep.warnf(lintClassIndexTierMissing, "wiki/a.md: index_tier missing")
	}
	for range 3 {
		rep.warnf(lintClassThinArticle, "wiki/b.md: thin article (4 lines, minimum 15)")
	}
	rep.warnf(lintClassFilenameAsTitle, "wiki/c_md.md: looks like a filename-as-title duplicate of wiki/c.md")

	if buf.Len() != 0 {
		t.Fatalf("default mode must not print warnings per-file, got:\n%s", buf.String())
	}
	if rep.warnings != 16 {
		t.Fatalf("warnings = %d, want 16", rep.warnings)
	}

	rep.flush()
	want := "\n" +
		"12× index_tier missing          (run: scribe tier write --missing-only)\n" +
		" 3× thin article\n" +
		" 1× filename-as-title duplicate (run: scribe lint --fix)\n"
	if got := buf.String(); got != want {
		t.Errorf("grouped flush mismatch:\ngot:\n%q\nwant:\n%q", got, want)
	}
}

// TestLintReportFlushTieBreak: classes with equal counts sort
// alphabetically so the summary is deterministic run-to-run.
func TestLintReportFlushTieBreak(t *testing.T) {
	var buf bytes.Buffer
	rep := newLintReport(&buf, false, false)
	rep.warnf(lintClassThinArticle, "x")
	rep.warnf(lintClassBloatedArticle, "y")

	rep.flush()
	want := "\n" +
		"1× bloated article\n" +
		"1× thin article\n"
	if got := buf.String(); got != want {
		t.Errorf("tie-break flush mismatch:\ngot:\n%q\nwant:\n%q", got, want)
	}
}

// TestLintReportModes: per-mode visibility of each line kind. Errors are
// always per-file; per-file warnings print only in verbose; aggregate
// warnings and info lines print except in quiet; counts accrue in every
// mode.
func TestLintReportModes(t *testing.T) {
	tests := []struct {
		name    string
		verbose bool
		quiet   bool

		wantWarnLine      bool // per-file WARN visible immediately
		wantAggregateLine bool
		wantInfoLine      bool
		wantFlushOutput   bool
	}{
		{name: "default", wantAggregateLine: true, wantInfoLine: true, wantFlushOutput: true},
		{name: "verbose", verbose: true, wantWarnLine: true, wantAggregateLine: true, wantInfoLine: true},
		{name: "quiet", quiet: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			rep := newLintReport(&buf, tt.verbose, tt.quiet)

			rep.errorf("wiki/e.md: broken frontmatter")
			rep.warnf(lintClassThinArticle, "wiki/t.md: thin article (3 lines, minimum 15)")
			rep.warnAggregatef("%d orphan articles (no inbound wikilinks)", 7)
			rep.infof("index: 10 entries, disk: 10 articles")
			preFlush := buf.String()
			rep.flush()
			postFlush := buf.String()[len(preFlush):]

			if !strings.Contains(preFlush, "ERROR wiki/e.md: broken frontmatter") {
				t.Errorf("ERROR line must print in %s mode, got:\n%s", tt.name, preFlush)
			}
			if got := strings.Contains(preFlush, "WARN wiki/t.md"); got != tt.wantWarnLine {
				t.Errorf("per-file WARN visible = %v, want %v in %s mode", got, tt.wantWarnLine, tt.name)
			}
			if got := strings.Contains(preFlush, "7 orphan articles"); got != tt.wantAggregateLine {
				t.Errorf("aggregate warning visible = %v, want %v in %s mode", got, tt.wantAggregateLine, tt.name)
			}
			if got := strings.Contains(preFlush, "index: 10 entries"); got != tt.wantInfoLine {
				t.Errorf("info line visible = %v, want %v in %s mode", got, tt.wantInfoLine, tt.name)
			}
			if got := strings.Contains(postFlush, "1× thin article"); got != tt.wantFlushOutput {
				t.Errorf("flush output present = %v, want %v in %s mode", got, tt.wantFlushOutput, tt.name)
			}

			if rep.errors != 1 {
				t.Errorf("errors = %d, want 1 in %s mode", rep.errors, tt.name)
			}
			if rep.warnings != 2 {
				t.Errorf("warnings = %d, want 2 in %s mode (per-file + aggregate)", rep.warnings, tt.name)
			}
		})
	}
}

// TestLintReportVerboseHint: verbose per-file lines carry the remediation
// hint from lintHints — call sites no longer embed it — and classes
// without a hint render bare.
func TestLintReportVerboseHint(t *testing.T) {
	var buf bytes.Buffer
	rep := newLintReport(&buf, true, false)

	rep.warnf(lintClassIndexTierMissing, "wiki/a.md: index_tier missing")
	rep.warnf(lintClassThinArticle, "wiki/b.md: thin article (4 lines, minimum 15)")

	out := buf.String()
	wantHinted := "  WARN wiki/a.md: index_tier missing (run `scribe tier write --missing-only`)\n"
	if !strings.Contains(out, wantHinted) {
		t.Errorf("verbose line missing data-driven hint:\ngot:\n%s\nwant substring:\n%s", out, wantHinted)
	}
	if strings.Contains(out, "thin article (4 lines, minimum 15) (run") {
		t.Errorf("hintless class must not grow a hint:\n%s", out)
	}
}

// TestLintSizesGroupsByClass drives the real Phase 2 walker over fixture
// articles and asserts each size/tier warning lands in its class.
func TestLintSizesGroupsByClass(t *testing.T) {
	root := t.TempDir()
	wiki := filepath.Join(root, "wiki")
	if err := os.MkdirAll(wiki, 0o755); err != nil {
		t.Fatal(err)
	}

	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(wiki, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	body := func(lines int) string {
		return strings.Repeat("line\n", lines)
	}

	files := []string{
		// thin + index_tier missing (2 warnings, 2 classes)
		write("thin.md", "---\ntitle: Thin\n---\n"+body(2)),
		// clean: tiered, between 15 and 150 lines
		write("ok.md", "---\ntitle: OK\nindex_tier: core\n---\n"+body(40)),
		// bloated only (tier present)
		write("bloated.md", "---\ntitle: Big\nindex_tier: core\n---\n"+body(160)),
		// rolling overgrown (rolling files skip thin/bloated/tier checks)
		write("rolling.md", "---\ntitle: Roll\nrolling: true\n---\n"+body(210)),
		// rolling archive: exempt from the size warning entirely
		write("roll-archive-2025.md", "---\ntitle: Arch\nrolling: true\n---\n"+body(210)),
	}

	var buf bytes.Buffer
	rep := newLintReport(&buf, false, false)
	lintSizes(rep, root, files)

	wantCounts := map[string]int{
		lintClassThinArticle:      1,
		lintClassIndexTierMissing: 1,
		lintClassBloatedArticle:   1,
		lintClassRollingOvergrown: 1,
	}
	for class, want := range wantCounts {
		if got := rep.classCounts[class]; got != want {
			t.Errorf("class %q count = %d, want %d", class, got, want)
		}
	}
	if rep.warnings != 4 {
		t.Errorf("warnings = %d, want 4", rep.warnings)
	}
	if rep.errors != 0 {
		t.Errorf("errors = %d, want 0", rep.errors)
	}
	if buf.Len() != 0 {
		t.Errorf("default mode printed per-file output:\n%s", buf.String())
	}
}

// TestRemediationFooter_AlwaysSuggestsFixOnErrors: the `scribe lint --fix`
// command must appear whenever there are frontmatter errors — even an
// all-manual batch (missing title, invalid confidence). Gating it on a
// mechanical error being present is what made the hint look absent after the
// auto-fixable errors were already cleared. The human-only residue is listed
// under "Needs a human".
func TestRemediationFooter_AlwaysSuggestsFixOnErrors(t *testing.T) {
	var buf bytes.Buffer
	r := newLintReport(&buf, false, false)
	r.noteErrorKind("missing required fields: title")
	r.noteErrorKind("invalid confidence: 'confirmed' (expected: high, low, medium)")
	r.noteErrorKind("invalid YAML frontmatter: no frontmatter delimiter")
	r.remediationFooter()
	out := buf.String()
	if !strings.Contains(out, "To fix, run:") || !strings.Contains(out, "scribe lint --fix") {
		t.Errorf("--fix command must show even for an all-manual batch:\n%s", out)
	}
	for _, want := range []string{"missing title", "invalid confidence", "no frontmatter"} {
		if !strings.Contains(out, want) {
			t.Errorf("manual residue %q missing:\n%s", want, out)
		}
	}
}

// TestRemediationFooter_WarningsOnly: a passing run (no errors) with a
// warning class that carries a fix command still prints the "To fix, run:"
// footer naming that command — this is the case the user hit, where lint
// PASSES but there's still an index_tier command to run. `scribe lint --fix`
// must NOT appear (there are no frontmatter errors to fix). A no-command
// class (bloated) routes to the "Needs review" section, not the command list.
func TestRemediationFooter_WarningsOnly(t *testing.T) {
	var buf bytes.Buffer
	r := newLintReport(&buf, false, false)
	r.warnf(lintClassIndexTierMissing, "wiki/a.md: index_tier missing")
	r.warnf(lintClassBloatedArticle, "wiki/b.md: bloated") // no command → review section
	r.remediationFooter()
	out := buf.String()
	if !strings.Contains(out, "To fix, run:") || !strings.Contains(out, "scribe tier write --missing-only") {
		t.Errorf("warning fix command must show on a passing run:\n%s", out)
	}
	if strings.Contains(out, "scribe lint --fix") {
		t.Errorf("--fix must not appear with zero frontmatter errors:\n%s", out)
	}
	if !strings.Contains(out, "Needs review") || !strings.Contains(out, "bloated article") {
		t.Errorf("no-command warning must land in the review section:\n%s", out)
	}
}

// TestRemediationFooter_ReviewSection: the judgment-requiring, no-command
// warning classes (bloated/thin/rolling/self-named-dir) each render a review
// line with their remediation guidance, and the footer points at `scribe lint
// -v` so the file list is reachable. This is the "55 warnings, no guidance"
// gap the user hit — the footer must never leave a warning unexplained.
func TestRemediationFooter_ReviewSection(t *testing.T) {
	var buf bytes.Buffer
	r := newLintReport(&buf, false, false)
	r.warnf(lintClassBloatedArticle, "a.md: bloated")
	r.warnf(lintClassThinArticle, "b.md: thin")
	r.remediationFooter()
	out := buf.String()
	if !strings.Contains(out, "Needs review (no automatic fix):") {
		t.Fatalf("expected the review section:\n%s", out)
	}
	for _, want := range []string{
		"split at 150 lines", "expand, or merge",
		"scribe lint -v", "scribe-kb-tidy skill",
		"scribe-cli", "scribe skill install",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("review section missing %q:\n%s", want, out)
		}
	}
	// A no-command class must not be mislabeled as command-fixable.
	if strings.Contains(out, "To fix, run:") {
		t.Errorf("pure review warnings must not print a command section:\n%s", out)
	}
}

// TestRemediationFooter_SilentWhenCleanOrQuiet: nothing actionable ⇒ no
// footer (all-clean lint); quiet mode ⇒ no footer (sync's mid-extract lint
// must stay terse) even with errors and warnings recorded.
func TestRemediationFooter_SilentWhenCleanOrQuiet(t *testing.T) {
	var buf bytes.Buffer
	newLintReport(&buf, false, false).remediationFooter() // nothing recorded
	if buf.Len() != 0 {
		t.Errorf("clean run must emit no footer, got: %q", buf.String())
	}
	buf.Reset()
	rq := newLintReport(&buf, false, true)
	rq.noteErrorKind("missing required fields: title")
	rq.warnf(lintClassIndexTierMissing, "wiki/a.md: index_tier missing")
	rq.remediationFooter()
	if buf.Len() != 0 {
		t.Errorf("quiet mode must emit no footer, got: %q", buf.String())
	}
}
