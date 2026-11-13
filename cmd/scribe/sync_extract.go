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
	jobs := planExtractions(root, cfg, manifest, s.projectsNeedingExtraction(root, manifest))

	logMsg("sync", "%d projects need extraction (max: %d)", len(jobs), s.Max)
	if len(jobs) == 0 {
		logMsg("sync", "nothing to extract")
		return 0, nil
	}

	// Cap at s.Max so deferral logging is simple. The size gate already
	// ran (planExtractions), so a project sync will skip anyway can no
	// longer occupy a slot: two held projects with staged drops used to
	// take both `--max 2` slots on every run and starve all the others.
	deferred := 0
	if len(jobs) > s.Max {
		for _, j := range jobs[s.Max:] {
			logMsg("sync", " [%s] deferred (max %d reached, will extract next run)", j.entry.Name, s.Max)
		}
		deferred = len(jobs) - s.Max
		jobs = jobs[:s.Max]
	}

	parallel := min(max(s.Parallel, 1), 5, len(jobs))

	if parallel > 1 {
		logMsg("sync", "extracting %d projects (parallel=%d)", len(jobs), parallel)
	}

	// DryRun bypasses goroutines entirely — the output ordering matters.
	if s.DryRun {
		for _, j := range jobs {
			if j.dropsOnly {
				logMsg("sync", " [%s] DRY RUN -- staged drops only (%d changed files held for `scribe deep`)", j.entry.Name, j.held)
				continue
			}
			logMsg("sync", " [%s] DRY RUN -- changed files (%d):", j.entry.Name, len(j.changed))
			limit := min(len(j.changed), 20)
			for _, f := range j.changed[:limit] {
				fmt.Println(f)
			}
		}
		return len(jobs), nil
	}

	g, ctx := errgroup.WithContext(context.Background())
	g.SetLimit(parallel)

	var (
		mu          gosync.Mutex
		extracted   int
		rateLimited bool
	)

	for _, j := range jobs {
		entry := j.entry

		g.Go(func() error {
			// Bail early if another goroutine already hit a rate limit.
			if err := ctx.Err(); err != nil {
				return err
			}

			if j.dropsOnly {
				logMsg("sync", " [%s] extracting staged drops only (%d changed files held — run `scribe deep %s` for those)", entry.Name, j.held, entry.Name)
			} else {
				logMsg("sync", " [%s] extracting (%d files to scan) from %s", entry.Name, len(j.changed), entry.Path)
			}

			if err := s.extractProject(root, manifest, entry.Name, entry, j.changed, j.dropsOnly); err != nil { //nolint:contextcheck // errgroup cancellation is sufficient; extractProject shells out to runCmdErr wrappers
				if errors.Is(err, ErrRateLimit) {
					logMsg("sync", " [%s] rate limited — stopping extraction, will resume next run", entry.Name)
					mu.Lock()
					rateLimited = true
					mu.Unlock()
					// Returning the error cancels the errgroup context so
					// the remaining not-yet-started extractions are skipped.
					return err
				}
				if errors.Is(err, ErrDailyBudgetExhausted) {
					logMsg("sync", " [%s] daily anthropic budget ceiling reached — stopping extraction (%v)", entry.Name, err)
					mu.Lock()
					rateLimited = true
					mu.Unlock()
					return err
				}
				logPhaseDegraded("sync", "project extraction", " [%s] extraction failed: %v", entry.Name, err)
				return nil
			}

			mu.Lock()
			extracted++
			mu.Unlock()
			logMsg("sync", " [%s] done", entry.Name)
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

// extractJob is one planned project extraction.
type extractJob struct {
	entry   *ProjectEntry
	changed []string
	// dropsOnly marks a project over sync.max_extract_files that still
	// has staged drops: the drops are extracted, the changed files are
	// left for `scribe deep`, and last_sha does not move.
	dropsOnly bool
	held      int // changed files held back by the size gate
}

// planExtractions applies the sync.max_extract_files size gate before
// the --max cap is taken. One `claude -p` pass reliably blows the
// 10-min timeout above ~100 changed files, so such a project is skipped
// with a pointer at `scribe deep <name>`, which batches by directory.
// A held project with staged drops still gets a drops-only job:
// handoffs are small, and leaving them staged until someone runs deep
// is how they piled up unextracted for months.
func planExtractions(root string, cfg *ScribeConfig, manifest *Manifest, keys []string) []extractJob {
	var jobs []extractJob
	for _, key := range keys {
		entry := manifest.Projects[key]
		changed := gitChangedFiles(entry.Path, entry.LastSHA, extractScanPatterns)
		if !exceedsExtractFileCap(cfg, len(changed)) {
			jobs = append(jobs, extractJob{entry: entry, changed: changed})
			continue
		}
		if len(stagedDrops(dropStagingDir(root, entry.Name))) > 0 {
			jobs = append(jobs, extractJob{entry: entry, dropsOnly: true, held: len(changed)})
			continue
		}
		logMsg("sync", " [%s] SKIP: %d files > sync.max_extract_files (%d). Run: scribe deep %s",
			entry.Name, len(changed), cfg.Sync.MaxExtractFiles, entry.Name)
	}
	return jobs
}

// projectsNeedingExtraction returns manifest keys (canonical paths) of
// projects that need extraction.
func (s *SyncCmd) projectsNeedingExtraction(root string, manifest *Manifest) []string {
	var result []string
	ledger := loadLedger(root)
	cfg := loadConfig(root)
	manifestDirty := false
	unchanged := 0

	// s.Extract is resolved once, up front, to a *ProjectEntry — a CLI-typed
	// Name or a full path both work (manifest.resolve). Filtering below
	// compares by pointer identity, not by string, since the loop key is
	// now a canonical path and s.Extract may be a short display Name.
	var filterEntry *ProjectEntry
	if s.Extract != "" {
		e, err := manifest.resolve(s.Extract)
		if err != nil {
			logMsg("sync", "%v", err)
			return nil
		}
		filterEntry = e
	}

	for key, entry := range manifest.Projects {
		// If --extract specified, only consider that project.
		if filterEntry != nil && entry != filterEntry {
			continue
		}

		// Pending projects wait for `scribe projects approve` — the
		// summary hint already printed during discovery, so skip quietly
		// unless the user named this project explicitly.
		if !entry.IsApproved() {
			if filterEntry == entry {
				logMsg("sync", " [%s] pending approval — run `scribe projects approve %s` first", entry.Name, entry.Name)
			}
			continue
		}

		if !dirExists(entry.Path) {
			continue
		}

		// Approval is not the last word: the source filters are trust-
		// locked, the manifest is not. An entry approved before a path
		// landed in sources.exclude (or a manifest that arrived with a
		// clone) must still pass the same gate discovery applies.
		if !sourceAllowed(cfg, entry.Path) {
			logMsg("sync", " [%s] excluded by sources filters — skipping", entry.Name)
			continue
		}

		// Defense in depth for manifests written before KB self-detection
		// existed: a KB that was already discovered into the manifest must
		// still never extract itself (doing so re-ingests its own wiki and
		// compounds duplicates every run). New discovery already filters
		// these out via manifest.isIgnored → isScribeKB.
		if withinScribeKB(entry.Path) {
			logMsg("sync", " [%s] is (inside) a scribe KB — skipping (KBs never harvest themselves or each other)", entry.Name)
			continue
		}

		if s.Force {
			result = append(result, key)
			continue
		}

		// Staged drop files are work in their own right: a handoff
		// written into an otherwise idle project (its .claude/ dir is
		// usually gitignored, so the SHA never moves) must not wait for
		// an unrelated commit — and drops deferred past the files budget
		// need the follow-up extraction that consumes them.
		if n := len(stagedDrops(dropStagingDir(root, entry.Name))); n > 0 {
			logMsg("sync", " [%s] %d staged drop file(s) — extracting", entry.Name, n)
			result = append(result, key)
			continue
		}

		// Never extracted.
		if entry.LastSHA == "" {
			result = append(result, key)
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
					logMsg("sync", " [%s] revision %.8s already extracted by %s (%s) — skipping, syncing local marker", entry.Name, currentSHA, who, le.ExtractedAt)
					entry.LastSHA = currentSHA
					if entry.LastExtracted == "" {
						entry.LastExtracted = le.ExtractedAt
					}
					manifestDirty = true
					continue
				}
				result = append(result, key)
				continue
			}
		} else if s.hasNewerFiles(entry) {
			// No git: any .md file newer than the last extraction counts as changed.
			result = append(result, key)
			continue
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
			logPhaseFailure("sync", "manifest save", err)
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
	_ = filepath.Walk(entry.Path, func(path string, info os.FileInfo, err error) error {
		if err != nil || found {
			return filepath.SkipDir
		}
		name := info.Name()
		if info.IsDir() {
			switch name {
			case "_build", "deps", "node_modules", ".git", ".elixir_ls":
				return filepath.SkipDir
			}
			if nestedCheckout(entry.Path, path) {
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
func (s *SyncCmd) extractProject(root string, manifest *Manifest, pname string, entry *ProjectEntry, changed []string, dropsOnly bool) error {
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
	if dropsOnly {
		fileList = "This run processes only the staged drop files. Skip the project's own changed files: there are too many for one pass, and `scribe deep` extracts them."
	}

	// Detect drop files for this project.
	dropInstruction := ""
	dropStaging := dropStagingDir(root, pname)
	// Which staged drops the model gets to see. The legacy tool-mode
	// path points the model at the whole staging dir, so everything
	// counts as consumed; the envelope path reports exactly what fit in
	// its files block and the rest stays staged.
	consumedDrops := stagedDrops(dropStaging)
	if len(consumedDrops) > 0 {
		kb := kbName(root)
		dropInstruction = fmt.Sprintf(
			"Step 1.5: Process drop files in %s/. "+
				"Each has YAML frontmatter with '%s: true', an action (create/update/append), "+
				"and optional target path. For 'create': make a new article. For 'update': merge into "+
				"existing article. For 'append': add to target or rolling file (if rolling_target is set). "+
				"Process drops BEFORE reading project docs to avoid duplication.\n",
			dropStaging, kb)
	}

	// Build and run the extraction prompt.
	prompt, err := loadPrompt("extract.md", map[string]string{
		"KB_DIR":              root,
		"PROJECT":             pname,
		"P_PATH":              entry.Path,
		"DOMAIN":              entry.Domain,
		"STEP2":               step2,
		"FILELIST":            fileList,
		"DROP_INSTRUCTION":    dropInstruction,
		"CLAUDE_TOOLING_DIRS": claudeToolingPromptList(),
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
		seen, err := runExtractEnvelope(ctx, root, cfg, manifest, pname, entry, changed)
		if err != nil {
			return fmt.Errorf("extract envelope: %w", err)
		}
		consumedDrops = seen
	} else {
		tools := append([]string{
			"Read", "Write", "Edit", "Glob", "Grep",
			"Bash(git log:*)", "Bash(ls:*)", "Bash(find:*)", "Bash(wc:*)",
		}, readOnlyGitTools(entry.Path)...)
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

	// Record in the committed extraction ledger so teammates skip this
	// revision. Load fresh under the lock — a parallel extraction of
	// another project may have written its own entry meanwhile.
	if markExtracted(entry, currentSHA, timestamp, dropsOnly) && currentSHA != "no-git" {
		if key := repoLedgerKey(entry.Path); key != "" {
			ledger := loadLedger(root)
			ledger.record(key, currentSHA, resolveContributor(root))
			if err := ledger.save(); err != nil {
				logPhaseDegraded("sync", "extraction ledger save", " [%s] extraction ledger save failed: %v", pname, err)
			}
		}
	}

	// Remove the drops the model saw; anything deferred stays staged.
	finishDropStaging(pname, dropStaging, consumedDrops, entry, timestamp)

	return manifest.save()
}

// markExtracted stamps a finished extraction on the manifest entry and
// reports whether the revision counts as extracted (and so belongs in
// the team ledger). A drops-only run saw none of the changed files:
// moving last_sha would hide those commits from sync and from
// `scribe deep` alike, which is how blind runs lost months of history.
func markExtracted(entry *ProjectEntry, sha, timestamp string, dropsOnly bool) bool {
	if dropsOnly {
		return false
	}
	entry.LastSHA = sha
	entry.LastExtracted = timestamp
	entry.LastMDScan = timestamp
	return true
}
