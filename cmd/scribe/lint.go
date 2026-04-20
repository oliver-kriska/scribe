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

	Contradictions bool   `help:"Run LLM-based contradiction check instead of structural lints. Writes wiki/_contradictions.md."`
	Since          string `help:"For --contradictions: only check articles changed since this duration (e.g., 7d, 24h). Empty = all."`
	Max            int    `help:"For --contradictions: max articles to include in one LLM call." default:"20"`
	DryRun         bool   `help:"For --contradictions and --fix: preview without calling LLM / writing files." short:"n"`
	OutputMD       string `help:"For --contradictions: override the output markdown path." default:""`

	Fix bool `help:"Deterministically repair frontmatter: missing tags/related/sources/confidence/domain/dates; normalize YYYY/MM/DD → YYYY-MM-DD; strip trailing whitespace. Never touches title/type."`

	Resolve    bool `help:"Read wiki/_contradictions.md, weigh pairs by authority+updated+confidence, write proposals to wiki/_resolution-proposals.md. Never auto-applies — human reviews."`
	Identities bool `help:"Detect same-person mentions across wiki + raw (emails, @handles, name variants) and write clustering proposals to wiki/_identity-proposals.md."`

	ApplyIdentities bool `help:"Read wiki/_identity-proposals.md and append surface forms to matching people/*.md aliases: lists. Safe — never creates pages, never deletes. Skips low-confidence blocks unless --apply-low."`
	ApplyLow        bool `help:"With --apply-identities, also apply blocks with Confidence: low."`
}

func (l *LintCmd) Run() error {
	if l.Contradictions {
		return runContradictionsCheck(l.Since, l.Max, l.DryRun, l.OutputMD)
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

	var files []string

	if l.Changed {
		files, err = changedWikiFiles(root)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			fmt.Println("no changed wiki files to validate")
			return nil
		}
	} else if len(l.Files) > 0 {
		files = l.Files
	} else {
		err = walkArticles(root, func(path string, _ []byte) error {
			files = append(files, path)
			return nil
		})
		if err != nil {
			return err
		}
	}

	errors := 0
	warnings := 0

	// Phase 1: Frontmatter validation
	fmt.Println("Phase 1: Frontmatter validation")
	for _, path := range files {
		errs := validateFile(root, path)
		if len(errs) > 0 {
			errors++
			for _, e := range errs {
				fmt.Printf("  ERROR %s: %s\n", relPath(root, path), e)
			}
		}
	}

	// Phase 2: Size checks
	fmt.Println("Phase 2: Size checks")
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
				fmt.Printf("  WARN %s: rolling file is %d lines (should archive at 150)\n", rel, lines)
				warnings++
			}
			continue
		}

		if lines < 15 {
			fmt.Printf("  WARN %s: thin article (%d lines, minimum 15)\n", rel, lines)
			warnings++
		}
		if lines > 150 {
			fmt.Printf("  WARN %s: bloated article (%d lines, should split at 150)\n", rel, lines)
			warnings++
		}
	}

	// Phase 3: Orphans and missing pages
	if !l.Changed {
		fmt.Println("Phase 3: Orphan detection")
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
			fmt.Printf("  %d orphan articles (no inbound wikilinks)\n", orphanCount)
			warnings++
		}

		missingCount := 0
		for target := range allTargets {
			if !allTitles[target] {
				missingCount++
			}
		}
		if missingCount > 0 {
			fmt.Printf("  %d missing pages (linked but don't exist)\n", missingCount)
			warnings++
		}
	}

	// Phase 4: Index consistency
	if !l.Changed {
		fmt.Println("Phase 4: Index consistency")
		indexPath := filepath.Join(root, "wiki", "_index.md")
		if content, err := os.ReadFile(indexPath); err == nil {
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
				fmt.Printf("  WARN index has %d entries but disk has %d articles (diff: %d)\n", indexCount, diskCount, diff)
				warnings++
			} else {
				fmt.Printf("  index: %d entries, disk: %d articles\n", indexCount, diskCount)
			}
		}
	}

	runStats = map[string]any{
		"files_checked": len(files),
		"errors":        errors,
		"warnings":      warnings,
	}

	// Summary
	fmt.Println()
	if errors > 0 {
		return fmt.Errorf("FAILED: %d errors, %d warnings", errors, warnings)
	}
	if warnings > 0 {
		fmt.Printf("PASSED with %d warnings\n", warnings)
	} else {
		fmt.Println("PASSED: all checks clean")
	}
	return nil
}

// runFix collects the file set (--changed, explicit args, or all wiki
// articles) and applies frontmatter auto-repair via autoFixArticle. A
// summary line records totals for run-records.
func (l *LintCmd) runFix() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	var files []string
	switch {
	case l.Changed:
		files, err = changedWikiFiles(root)
		if err != nil {
			return err
		}
	case len(l.Files) > 0:
		files = l.Files
	default:
		err = walkArticles(root, func(path string, _ []byte) error {
			files = append(files, path)
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(files) == 0 {
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
	fmt.Printf("\n%s %d file(s), skipped %d\n", verb, fixed, skipped)
	runStats = map[string]any{
		"files_scanned": len(files),
		"files_fixed":   fixed,
		"files_skipped": skipped,
		"dry_run":       l.DryRun,
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
