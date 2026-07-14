package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type LintCmd struct {
	Files   []string `arg:"" optional:"" help:"Files to lint. If empty, lints all wiki articles."`
	Changed bool     `help:"Lint only uncommitted git changes." short:"c"`
	Verbose bool     `help:"Print every warning per-file instead of the grouped-by-class summary." short:"v" xor:"output"`
	Quiet   bool     `help:"Errors and final summary only — suppress phase headers and warnings. Used by sync mid-extract." short:"q" xor:"output"`

	Contradictions bool    `help:"Run LLM-based contradiction check instead of structural lints. Writes wiki/_contradictions.md."`
	Duplicates     bool    `help:"Structural (no-LLM) content-duplicate scan: exact-body hashes + token overlap. Writes wiki/_duplicates.md. Report-only — never deletes."`
	Threshold      float64 `help:"For --duplicates: overlap-coefficient threshold for near-dup pairs (0 = default 0.60)." default:"0"`
	Since          string  `help:"For --contradictions: only check articles changed since this duration (e.g., 7d, 24h). Empty = all."`
	Max            int     `help:"For --contradictions: max articles to include in one LLM call." default:"20"`
	DryRun         bool    `help:"For --contradictions and --fix: preview without calling LLM / writing files." short:"n"`
	OutputMD       string  `help:"For --contradictions: override the output markdown path." default:""`

	Fix bool `help:"Deterministically repair frontmatter: missing tags/related/sources/confidence/domain/dates; normalize YYYY/MM/DD → YYYY-MM-DD; strip trailing whitespace. Never touches title/type. On a full run (no files / not --changed) also removes self-ingestion duplicate pages (foo.md.md, foo_md.md), collapses byte-identical duplicate pages, and removes paths with an unsubstituted {{VAR}} template placeholder — all git-recoverable."`

	Resolve    bool `help:"Read wiki/_contradictions.md, weigh pairs by authority+updated+confidence, write proposals to wiki/_resolution-proposals.md. Never auto-applies — human reviews."`
	Identities bool `help:"Detect same-person mentions across wiki + raw (emails, @handles, name variants) and write clustering proposals to wiki/_identity-proposals.md."`

	ApplyIdentities bool `help:"Read wiki/_identity-proposals.md and append surface forms to matching people/*.md aliases: lists. Safe — never creates pages, never deletes. Skips low-confidence blocks unless --apply-low."`
	ApplyLow        bool `help:"With --apply-identities, also apply blocks with Confidence: low."`
}

func (l *LintCmd) Run() error {
	if l.Contradictions {
		return runContradictionsCheck(l.Since, l.Max, l.DryRun, l.OutputMD)
	}
	if l.Duplicates {
		return runDuplicatesCheck(l.Threshold, l.OutputMD, l.DryRun)
	}
	if l.Resolve {
		return runResolveContradictions(l.OutputMD, l.DryRun)
	}
	if l.Identities {
		return runIdentitiesCheck(l.OutputMD, l.DryRun)
	}
	if l.ApplyIdentities {
		return runApplyIdentities(l.OutputMD, l.ApplyLow, l.DryRun)
	}
	if l.Fix {
		return l.runFix()
	}

	root, err := kbDir()
	if err != nil {
		return err
	}

	files, err := l.targetFiles(root)
	if err != nil {
		return err
	}
	if l.Changed && len(files) == 0 {
		fmt.Println("no changed wiki files to validate")
		return nil
	}

	rep := newLintReport(os.Stdout, l.Verbose, l.Quiet)
	phase := func(name string) {
		if !l.Quiet {
			fmt.Println(name)
		}
	}

	phase("Phase 1: Frontmatter validation")
	lintFrontmatter(rep, root, files)
	phase("Phase 2: Size checks")
	lintSizes(rep, root, files)

	// Phases 3-6 are cross-KB structural checks — they only make sense
	// on a full scan, not a --changed subset.
	if !l.Changed {
		phase("Phase 3: Orphan detection")
		lintOrphans(rep, root)
		phase("Phase 4: Index consistency")
		lintIndexConsistency(rep, root)
		phase("Phase 5: Self-ingestion artifacts")
		lintSelfIngestion(rep, root)
		phase("Phase 6: Conflict markers")
		lintConflictMarkers(rep, root)
	}

	runStats = map[string]any{
		"files_checked": len(files),
		"errors":        rep.errors,
		"warnings":      rep.warnings,
	}

	// Grouped warning summary (default mode only), the "To fix, run:"
	// footer, then the verdict. The footer prints on a warnings-only PASS
	// too, so `scribe lint` always ends by naming the command(s) to run.
	rep.flush()
	if !l.Quiet {
		fmt.Println()
	}
	rep.remediationFooter()
	if rep.errors > 0 {
		return fmt.Errorf("FAILED: %d errors, %d warnings", rep.errors, rep.warnings)
	}
	if rep.warnings > 0 {
		fmt.Printf("PASSED with %d warnings\n", rep.warnings)
	} else {
		fmt.Println("PASSED: all checks clean")
	}
	return nil
}

// targetFiles resolves the lint scope: --changed → uncommitted wiki
// files, explicit args → as given, otherwise every article on disk.
func (l *LintCmd) targetFiles(root string) ([]string, error) {
	switch {
	case l.Changed:
		return changedWikiFiles(root)
	case len(l.Files) > 0:
		return l.Files, nil
	default:
		var files []string
		err := walkArticles(root, func(path string, _ []byte) error {
			files = append(files, path)
			return nil
		})
		return files, err
	}
}

// lintFrontmatter (Phase 1) validates each file's frontmatter. Counts
// files with at least one error (a file with three problems counts
// once, matching the historical summary) while still printing every
// finding per-file.
func lintFrontmatter(rep *lintReport, root string, files []string) {
	for _, path := range files {
		errs := validateFile(root, path)
		if len(errs) > 0 {
			rep.errors++
			for _, e := range errs {
				rep.errorLinef("%s: %s", relPath(root, path), e)
				rep.noteErrorKind(e)
			}
		}
	}
}

// lintSizes (Phase 2) warns on thin/bloated articles, overgrown rolling
// files, and missing index_tier frontmatter.
func lintSizes(rep *lintReport, root string, files []string) {
	for _, path := range files {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		rel := relPath(root, path)
		lines := countLines(content)

		// Check if rolling file. Archive variants (*-archive-YYYY.md) rotate
		// by year, not by size — once entries land in an archive they stay
		// there until the year ends. Skip the size warning for those; only
		// warn on the live rolling files that accumulate until someone
		// archives their oldest entries.
		fm, _ := parseFrontmatter(content)
		if fm != nil && fm.Rolling {
			base := filepath.Base(rel)
			isArchive := strings.Contains(base, "-archive-")
			if !isArchive && lines > 200 {
				rep.warnf(lintClassRollingOvergrown, "%s: rolling file is %d lines (should archive at 150)", rel, lines)
			}
			continue
		}

		if lines < 15 {
			rep.warnf(lintClassThinArticle, "%s: thin article (%d lines, minimum 15)", rel, lines)
		}
		if lines > 150 {
			rep.warnf(lintClassBloatedArticle, "%s: bloated article (%d lines, should split at 150)", rel, lines)
		}

		// Phase 5B: index_tier presence. Warn (not error) when the
		// computed tier hasn't been written into frontmatter yet, so
		// the field can roll out without a flag-day migration. Lint
		// remains green; a follow-up `scribe tier write --missing-only`
		// (hinted via lintHints) resolves the warning. Skip for rolling
		// files (they don't participate in retrieval ranking the same way).
		if fm != nil && !fm.Rolling && strings.TrimSpace(fm.IndexTier) == "" {
			rep.warnf(lintClassIndexTierMissing, "%s: index_tier missing", rel)
		}
	}
}

// lintOrphans (Phase 3) counts articles with no inbound wikilinks and
// wikilink targets that don't exist. Both findings are KB-wide
// aggregates — already one line per class — so they print inline via
// warnAggregatef instead of joining the grouped per-file summary.
func lintOrphans(rep *lintReport, root string) {
	allTitles := make(map[string]bool)
	allTargets := make(map[string]bool)
	inboundCount := make(map[string]int)

	// Collect titles (and aliases) from articles. Aliases from the
	// optional `aliases:` frontmatter field share the same namespace
	// as canonical titles so a wikilink to a known alias does not
	// trigger a missing-page warning.
	_ = walkArticles(root, func(_ string, content []byte) error {
		if fm, err := parseFrontmatter(content); err == nil && fm.Title != "" {
			allTitles[fm.Title] = true
			for _, alias := range toStringSlice(fm.Aliases) {
				if alias != "" {
					allTitles[alias] = true
				}
			}
			return nil
		}
		if title := extractTitleFast(content); title != "" {
			allTitles[title] = true
		}
		return nil
	})

	// Scan ALL markdown (including _-prefixed) for wikilinks
	_ = walkAllMarkdown(root, func(_ string, content []byte) error {
		sourceTitle := extractTitleFast(content)
		for _, link := range extractWikilinks(content) {
			allTargets[link] = true
			if link != sourceTitle {
				inboundCount[link]++
			}
		}
		return nil
	})

	orphanCount := 0
	for title := range allTitles {
		if inboundCount[title] == 0 && !allTargets[title] {
			orphanCount++
		}
	}
	if orphanCount > 0 {
		rep.warnAggregatef("%d orphan articles (no inbound wikilinks)", orphanCount)
	}

	missingCount := 0
	for target := range allTargets {
		if !allTitles[target] {
			missingCount++
		}
	}
	if missingCount > 0 {
		rep.warnAggregatef("%d missing pages (linked but don't exist)", missingCount)
	}
}

// lintIndexConsistency (Phase 4) compares wiki/_index.md entry count to
// the article count on disk; small drift is normal between reindexes.
func lintIndexConsistency(rep *lintReport, root string) {
	indexPath := filepath.Join(root, "wiki", "_index.md")
	content, err := os.ReadFile(indexPath)
	if err != nil {
		return
	}
	// Count lines that *start* with "- [[", not every "- [[" occurrence.
	// Some one-line summaries contain a second "[[X]]" after a dash
	// (e.g. "- [[Foo]] -- [[Bar]] implements..."), which would be
	// double-counted by strings.Count and drifts the index vs disk
	// check from what the eye sees when grepping for bullet entries.
	indexCount := 0
	for line := range strings.SplitSeq(string(content), "\n") {
		if strings.HasPrefix(line, "- [[") {
			indexCount++
		}
	}
	diskCount := 0
	_ = walkArticles(root, func(_ string, _ []byte) error {
		diskCount++
		return nil
	})
	diff := diskCount - indexCount
	if diff > 3 || diff < -3 {
		rep.warnAggregatef("WARN index has %d entries but disk has %d articles (diff: %d)", indexCount, diskCount, diff)
	} else {
		rep.infof("index: %d entries, disk: %d articles", indexCount, diskCount)
	}
}

// lintSelfIngestion (Phase 5) flags the artifacts of the
// KB-extracts-itself bug — a filename fed back in as a title. Doubled
// ".md.md" extensions are always malformed (ERROR); slugified
// "<x>_md.md" files that shadow an existing "<x>.md" are likely
// duplicates (WARN). Both are remediated by `scribe lint --fix`.
func lintSelfIngestion(rep *lintReport, root string) {
	for _, d := range findSelfIngestionDuplicates(root) {
		if d.Shape == "doubled-ext" {
			rep.errorf("%s: doubled .md extension — malformed page (run `scribe lint --fix`)", d.Rel)
		} else {
			rep.warnf(lintClassFilenameAsTitle, "%s: looks like a filename-as-title duplicate of %s", d.Rel, d.CanonicalRel)
		}
	}
	// Directories named after the KB itself — the KB filed pages under
	// a project folder for itself when its curation sessions were mined.
	// Contents may hold unique fragments, so this is a review pointer,
	// not an auto-fix.
	for _, dir := range findSelfNamedDirs(root) {
		rep.warnf(lintClassSelfNamedDir, "%s/: directory named after the KB itself — likely self-ingested pages; review and merge into canonical articles", dir)
	}
}

// lintConflictMarkers (Phase 6) hard-errors on unresolved merge
// markers. Team KBs auto-pull, so a botched merge can leave
// "<<<<<<< HEAD" blocks inside articles where they poison search and
// LLM context until a human resolves them.
func lintConflictMarkers(rep *lintReport, root string) {
	for _, h := range findConflictMarkers(root) {
		rep.errorf("%s:%d: unresolved git conflict marker — resolve the merge and recommit", h.Rel, h.Line)
	}
}

// runFix collects the file set (--changed, explicit args, or all wiki
// articles) and applies frontmatter auto-repair via autoFixArticle. A
// summary line records totals for run-records.
func (l *LintCmd) runFix() error {
	root, err := kbDir()
	if err != nil {
		return err
	}

	// KB-wide structural cleanup runs only on a full `--fix` (no explicit
	// files, not --changed): remove/rename the filename-as-title duplicate
	// artifacts the self-ingestion bug left behind, collapse byte-identical
	// duplicate pages, and remove paths carrying an unsubstituted {{VAR}}
	// template placeholder. Done before the per-file frontmatter pass so
	// removed files aren't then re-read, and so a renamed `foo.md.md → foo.md`
	// gets frontmatter-repaired in the same run. All three are git-recoverable.
	dupRemoved, dupRenamed, exactRemoved, phRemoved := 0, 0, 0, 0
	if !l.Changed && len(l.Files) == 0 {
		dupRemoved, dupRenamed = fixSelfIngestionDuplicates(root, l.DryRun)
		exactRemoved = fixByteIdenticalDuplicates(root, l.DryRun)
		phRemoved = fixPlaceholderArtifacts(root, l.DryRun)
	}

	files, err := l.targetFiles(root)
	if err != nil {
		return err
	}
	if len(files) == 0 && dupRemoved == 0 && dupRenamed == 0 && exactRemoved == 0 && phRemoved == 0 {
		fmt.Println("no files in scope")
		return nil
	}
	fixed, skipped, err := runLintFix(root, files, l.DryRun)
	if err != nil {
		return err
	}
	verb := "fixed"
	if l.DryRun {
		verb = "would fix"
	}
	fmt.Printf("\n%s %d file(s), skipped %d", verb, fixed, skipped)
	if dupRemoved > 0 || dupRenamed > 0 || exactRemoved > 0 || phRemoved > 0 {
		fmt.Printf("; duplicates: removed %d, renamed %d, byte-identical %d; placeholder-paths %d",
			dupRemoved, dupRenamed, exactRemoved, phRemoved)
	}
	fmt.Println()
	runStats = map[string]any{
		"files_scanned":       len(files),
		"files_fixed":         fixed,
		"files_skipped":       skipped,
		"dup_removed":         dupRemoved,
		"dup_renamed":         dupRenamed,
		"dup_exact_removed":   exactRemoved,
		"placeholder_removed": phRemoved,
		"dry_run":             l.DryRun,
	}
	return nil
}

// changedWikiFiles returns wiki .md files with uncommitted changes.
func changedWikiFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "diff", "--name-only", "HEAD", "--") //nolint:noctx // git ls subprocess
	cmd.Args = append(cmd.Args, wikiDirs...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		// May fail if no HEAD yet
		out = nil
	}

	// Also untracked files
	cmd2 := exec.Command("git", "ls-files", "--others", "--exclude-standard", "--") //nolint:noctx // git ls subprocess
	cmd2.Args = append(cmd2.Args, wikiDirs...)
	cmd2.Dir = root
	out2, _ := cmd2.Output()

	combined := string(out) + string(out2)
	seen := make(map[string]bool)
	var files []string
	for line := range strings.SplitSeq(strings.TrimSpace(combined), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasSuffix(line, ".md") {
			continue
		}
		base := filepath.Base(line)
		if strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".") || skipFiles[base] {
			continue
		}
		abs := filepath.Join(root, line)
		if !seen[abs] {
			seen[abs] = true
			files = append(files, abs)
		}
	}
	return files, nil
}
