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
