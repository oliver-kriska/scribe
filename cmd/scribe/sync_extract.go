// sync_extract.go — sync Phase 2: select projects with new commits (ledger
// + SHA checks) and run the LLM extraction per project.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	gosync "sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/sync/errgroup"
)

// extractScanPatterns is the glob set sync scans for changed files when
// sizing an extraction. `scribe status` reuses it so its held-by-cap
// classification counts exactly the files sync will.
var extractScanPatterns = []string{"*.md", "*.txt", "*.exs", "*.ex"}

// exceedsExtractFileCap reports whether a changed-file count trips the
// sync.max_extract_files size gate — the rule that makes sync skip a
// project and point at `scribe deep`. Shared by sync (skip + log) and
// `scribe status` (held classification) so the two surfaces can't drift.
func exceedsExtractFileCap(cfg *ScribeConfig, changedCount int) bool {
	return cfg.Sync.MaxExtractFiles > 0 && changedCount > cfg.Sync.MaxExtractFiles
}

// extract processes projects that need extraction.
// Projects run concurrently up to s.Parallel (default 3, capped at 5 to
// avoid Anthropic rate limits). A rate-limit error cancels pending work
// via the errgroup context; already-running extractions finish naturally.
func (s *SyncCmd) extract(root string, manifest *Manifest) (int, error) {
	cfg := loadConfig(root)
	toExtract := s.projectsNeedingExtraction(root, manifest)

	logMsg("sync", "%d projects need extraction (max: %d)", len(toExtract), s.Max)
	if len(toExtract) == 0 {
		logMsg("sync", "nothing to extract")
		return 0, nil
	}

	// Cap toExtract at s.Max so deferral logging is simple.
	deferred := 0
	if len(toExtract) > s.Max {
		for _, pname := range toExtract[s.Max:] {
			logMsg("sync", " [%s] deferred (max %d reached, will extract next run)", pname, s.Max)
		}
		deferred = len(toExtract) - s.Max
		toExtract = toExtract[:s.Max]
	}

	parallel := min(max(s.Parallel, 1), 5, len(toExtract))

	if parallel > 1 {
		logMsg("sync", "extracting %d projects (parallel=%d)", len(toExtract), parallel)
	}

	// DryRun bypasses goroutines entirely — the output ordering matters.
	if s.DryRun {
		for _, pname := range toExtract {
			entry := manifest.Projects[pname]
			changed := gitChangedFiles(entry.Path, entry.LastSHA, extractScanPatterns)
			logMsg("sync", " [%s] DRY RUN -- changed files (%d):", pname, len(changed))
			limit := min(len(changed), 20)
			for _, f := range changed[:limit] {
				fmt.Println(f)
			}
		}
		return len(toExtract), nil
	}

	g, ctx := errgroup.WithContext(context.Background())
	g.SetLimit(parallel)

	var (
		mu          gosync.Mutex
		extracted   int
		rateLimited bool
	)

	for _, pname := range toExtract {
		entry := manifest.Projects[pname]

		g.Go(func() error {
			// Bail early if another goroutine already hit a rate limit.
			if err := ctx.Err(); err != nil {
				return err
			}

			changed := gitChangedFiles(entry.Path, entry.LastSHA, extractScanPatterns)

			// Size gate: one `claude -p` pass reliably blows the 10-min
			// timeout above ~100 changed files. Rather than eat the
			// "signal: killed" error and defer the project to the next
			// sync (where it will just fail again), skip the project in
			// normal sync and point the user at `scribe deep <name>`,
			// which batches-by-directory and fits in the timeout.
			if exceedsExtractFileCap(cfg, len(changed)) {
				logMsg("sync", " [%s] SKIP: %d files > sync.max_extract_files (%d). Run: scribe deep %s",
					pname, len(changed), cfg.Sync.MaxExtractFiles, pname)
				return nil
			}

			logMsg("sync", " [%s] extracting (%d files to scan) from %s", pname, len(changed), entry.Path)

			if err := s.extractProject(root, manifest, pname, entry, changed); err != nil { //nolint:contextcheck // errgroup cancellation is sufficient; extractProject shells out to runCmdErr wrappers
				if errors.Is(err, ErrRateLimit) {
					logMsg("sync", " [%s] rate limited — stopping extraction, will resume next run", pname)
					mu.Lock()
					rateLimited = true
					mu.Unlock()
					// Returning the error cancels the errgroup context so
					// the remaining not-yet-started extractions are skipped.
					return err
				}
				if errors.Is(err, ErrDailyBudgetExhausted) {
					logMsg("sync", " [%s] daily anthropic budget ceiling reached — stopping extraction (%v)", pname, err)
					mu.Lock()
					rateLimited = true
					mu.Unlock()
					return err
				}
				logMsg("sync", " [%s] extraction failed: %v", pname, err)
				return nil
			}

			mu.Lock()
			extracted++
			mu.Unlock()
			logMsg("sync", " [%s] done", pname)
			return nil
		})
	}

	// We intentionally ignore the returned error — rate-limit and per-project
	// failures are already logged above. The counters are what matters.
	_ = g.Wait()

	if deferred > 0 {
		logMsg("sync", "%d projects deferred to next run", deferred)
	}
	if rateLimited {
		logMsg("sync", "rate limit hit during parallel extraction")
	}

	return extracted, nil
}

// projectsNeedingExtraction returns names of projects that need extraction.
func (s *SyncCmd) projectsNeedingExtraction(root string, manifest *Manifest) []string {
	var result []string
	ledger := loadLedger(root)
	manifestDirty := false
	unchanged := 0

	for pname, entry := range manifest.Projects {
		// If --extract specified, only consider that project.
		if s.Extract != "" && pname != s.Extract {
			continue
		}

		// Pending projects wait for `scribe projects approve` — the
		// summary hint already printed during discovery, so skip quietly
		// unless the user named this project explicitly.
		if !entry.IsApproved() {
			if s.Extract == pname {
				logMsg("sync", " [%s] pending approval — run `scribe projects approve %s` first", pname, pname)
			}
			continue
		}

		if !dirExists(entry.Path) {
			continue
		}

		// Defense in depth for manifests written before KB self-detection
		// existed: a KB that was already discovered into the manifest must
		// still never extract itself (doing so re-ingests its own wiki and
		// compounds duplicates every run). New discovery already filters
		// these out via manifest.isIgnored → isScribeKB.
		if withinScribeKB(entry.Path) {
			logMsg("sync", " [%s] is (inside) a scribe KB — skipping (KBs never harvest themselves or each other)", pname)
			continue
		}

		if s.Force {
			result = append(result, pname)
			continue
		}

		// Never extracted.
		if entry.LastSHA == "" {
			result = append(result, pname)
			continue
		}

		// Git repo: compare SHAs.
		if hasGit(entry.Path) {
			currentSHA := gitSHA(entry.Path)
			if currentSHA != "" && currentSHA != entry.LastSHA {
				// Team dedupe: the committed extraction ledger maps the
				// repo's remote URL to the last extracted SHA. When a
				// teammate already extracted this exact revision (and
				// the pages arrived via pull), redoing the work would
				// only produce duplicates — sync the local marker
				// forward instead.
				if le, ok := ledger.lookup(repoLedgerKey(entry.Path)); ok && le.SHA == currentSHA {
					who := le.By
					if who == "" {
						who = "a teammate"
					}
					logMsg("sync", " [%s] revision %.8s already extracted by %s (%s) — skipping, syncing local marker", pname, currentSHA, who, le.ExtractedAt)
					entry.LastSHA = currentSHA
					if entry.LastExtracted == "" {
						entry.LastExtracted = le.ExtractedAt
					}
					manifestDirty = true
					continue
				}
				result = append(result, pname)
				continue
			}
		} else {
			// No git: check if any .md files are newer than last extraction.
			if s.hasNewerFiles(entry) {
				result = append(result, pname)
				continue
			}
		}

		// One summary line after the loop, not one line per project: an
		// enrolled-but-idle project is steady-state, and re-listing all
		// of them (~20 identical lines per run on a populated KB) buried
		// every real event in the sync log.
		unchanged++
	}

	if unchanged > 0 {
		logMsg("sync", "%d project(s) unchanged", unchanged)
	}

	if manifestDirty && !s.DryRun {
		if err := manifest.save(); err != nil {
			logMsg("sync", "manifest save failed: %v", err)
		}
	}

	sort.Strings(result)
	return result
}

// hasNewerFiles checks if any .md files in a non-git project are newer than the last extraction.
func (s *SyncCmd) hasNewerFiles(entry *ProjectEntry) bool {
	if entry.LastExtracted == "" {
		return true
	}
	cutoff, err := time.Parse(time.RFC3339, entry.LastExtracted)
	if err != nil {
		return true
	}

	found := false
	_ = filepath.Walk(entry.Path, func(_ string, info os.FileInfo, err error) error {
		if err != nil || found {
			return filepath.SkipDir
		}
		name := info.Name()
		if info.IsDir() {
			switch name {
			case "_build", "deps", "node_modules", ".git", ".elixir_ls":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, ".md") && info.ModTime().After(cutoff) {
			found = true
			return filepath.SkipDir
		}
		return nil
	})
	return found
}

// extractProject runs the full extraction pipeline for a single project.
func (s *SyncCmd) extractProject(root string, manifest *Manifest, pname string, entry *ProjectEntry, changed []string) error {
	// Pre-scan: run scribe scan to produce a structured manifest.
	scanOutput := filepath.Join(root, "output", "scan-"+pname+".md")
	scribeExe, _ := os.Executable()
	if scribeExe == "" {
		scribeExe = "scribe"
	}
	out, err := runCmdErr("", scribeExe, "scan", entry.Path)
	if err == nil && out != "" {
		outputDir := filepath.Join(root, "output")
		if err := os.MkdirAll(outputDir, 0o755); err != nil {
			logMsg("sync", " [%s] mkdir %s: %v", pname, outputDir, err)
		} else if err := os.WriteFile(scanOutput, []byte(out), 0o644); err != nil {
			logMsg("sync", " [%s] write %s: %v", pname, scanOutput, err)
		}
	}

	// Build step 2 instruction based on scan availability.
	step2 := fmt.Sprintf("Read %s/CLAUDE.md and %s/README.md if they exist.", entry.Path, entry.Path)
	if fileExists(scanOutput) {
		step2 = fmt.Sprintf("Read the pre-scan manifest at %s.", scanOutput)
	}

	// Build changed files context.
	fileList := ""
	if len(changed) > 0 && len(changed) <= 50 {
		fileList = "Focus on these changed files since last extraction: " + strings.Join(changed, ", ")
	}

	// Detect drop files for this project.
	dropInstruction := ""
	dropStaging := filepath.Join(root, "output", "drops-"+pname)
	if dirExists(dropStaging) {
		drops, _ := filepath.Glob(filepath.Join(dropStaging, "*.md"))
		if len(drops) > 0 {
			kb := kbName(root)
			dropInstruction = fmt.Sprintf(
				"Step 1.5: Process drop files in %s/. "+
					"Each has YAML frontmatter with '%s: true', an action (create/update/append), "+
					"and optional target path. For 'create': make a new article. For 'update': merge into "+
					"existing article. For 'append': add to target or rolling file (if rolling_target is set). "+
					"Process drops BEFORE reading project docs to avoid duplication.\n",
				dropStaging, kb)
		}
	}

	// Build and run the extraction prompt.
	prompt, err := loadPrompt("extract.md", map[string]string{
		"KB_DIR":           root,
		"PROJECT":          pname,
		"P_PATH":           entry.Path,
		"DOMAIN":           entry.Domain,
		"STEP2":            step2,
		"FILELIST":         fileList,
		"DROP_INSTRUCTION": dropInstruction,
	})
	if err != nil {
		return fmt.Errorf("load extract prompt: %w", err)
	}

	ctx := context.Background()

	// Phase 4F dispatch. The envelope path skips the legacy `claude
	// -p` invocation entirely — Go gathered the same context already
	// (drops, KB CLAUDE.md, project README/CLAUDE, changed files, doc
	// dirs) inside runExtractEnvelope, so the inlined `prompt` from
	// the legacy template above is discarded for this branch. The
	// envelope prompt loads from extract-{anthropic,ollama}.md and
	// gets its own FILES_CONTENT.
	cfg := loadConfig(root)
	if strings.EqualFold(cfg.Extract.Mode, "envelope") {
		_ = prompt // legacy tool-mode prompt unused in envelope path
		if _, err := runExtractEnvelope(ctx, root, cfg, manifest, pname, entry, changed); err != nil {
			return fmt.Errorf("extract envelope: %w", err)
		}
	} else {
		tools := []string{
			"Read", "Write", "Edit", "Glob", "Grep",
			"Bash(git log:*)", "Bash(git -C:*)", "Bash(ls:*)", "Bash(find:*)", "Bash(wc:*)",
		}
		_, err = runClaude(withOpLabel(ctx, "session-extract"), root, prompt, s.Model, tools, 10*time.Minute)
		if err != nil {
			return fmt.Errorf("claude extraction: %w", err)
		}
	}

	// Post-extraction lint on changed files. --quiet keeps the
	// mid-extract relay to per-file ERROR lines plus the one-line
	// summary ("PASSED with N warnings") — a single noisy project was
	// observed re-printing 300+ WARN lines into the sync log. The full
	// grouped warning report belongs to a standalone `scribe lint` run.
	lintOut, _ := runCmdErr(root, scribeExe, "lint", "--changed", "--quiet")
	if lintOut != "" {
		for line := range strings.SplitSeq(lintOut, "\n") {
			if line != "" {
				logMsg("sync", " [%s] %s", pname, line)
			}
		}
	}

	// Update manifest with new SHA and timestamps.
	currentSHA := "no-git"
	if hasGit(entry.Path) {
		if sha := gitSHA(entry.Path); sha != "" {
			currentSHA = sha
		}
	}
	timestamp := time.Now().UTC().Format(time.RFC3339)

	// Serialize manifest mutation + save across parallel extractions.
	// Entry pointers are stable inside manifest.Projects, but concurrent
	// save() calls would race on the JSON file.
	manifestMu.Lock()
	defer manifestMu.Unlock()

	entry.LastSHA = currentSHA
	entry.LastExtracted = timestamp
	entry.LastMDScan = timestamp

	// Record in the committed extraction ledger so teammates skip this
	// revision. Load fresh under the lock — a parallel extraction of
	// another project may have written its own entry meanwhile.
	if currentSHA != "no-git" {
		if key := repoLedgerKey(entry.Path); key != "" {
			ledger := loadLedger(root)
			ledger.record(key, currentSHA, resolveContributor(root))
			if err := ledger.save(); err != nil {
				logMsg("sync", " [%s] extraction ledger save failed: %v", pname, err)
			}
		}
	}

	// Mark drops as processed and clean up staging.
	if dirExists(dropStaging) {
		drops, _ := filepath.Glob(filepath.Join(dropStaging, "*.md"))
		if len(drops) > 0 {
			entry.LastDropProcessed = timestamp
			os.RemoveAll(dropStaging)
		}
	}

	return manifest.save()
}
