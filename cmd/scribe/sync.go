package main

import (
	"context"
	"database/sql"
	"encoding/json"
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

// manifestMu serializes writes to projects.json during parallel extraction.
// The manifest is shared across goroutines because each extractProject call
// mutates its project entry and calls manifest.save() at the end.
var manifestMu gosync.Mutex

type SyncCmd struct {
	Force        bool   `help:"Re-extract all regardless of SHA." short:"f"`
	DryRun       bool   `help:"Show what would happen without doing it." name:"dry-run"`
	ReindexOnly  bool   `help:"Only reindex qmd." name:"reindex"`
	DiscoverOnly bool   `help:"Only discover new projects." name:"discover"`
	Extract      string `help:"Extract one specific project." name:"extract"`
	Changed      string `help:"Show changed files for a project." name:"changed"`
	Max          int    `help:"Max projects to extract per run." default:"3"`
	Model        string `help:"Claude model to use." default:"sonnet"`
	Sessions     bool   `help:"Mine Claude Code sessions."`
	SessionsMax  int    `help:"Max sessions per run." name:"sessions-max" default:"3"`
	SessionSort  string `help:"Session sort: score (highest first, default) or date (newest first)." name:"session-sort" default:"score" enum:"date,score"`
	SkipLarge    bool   `help:"Skip large sessions (>300 messages)." name:"skip-large"`
	Parallel     int    `help:"Max concurrent project extractions (1-5)." default:"3"`
	Research     bool   `help:"Collect .claude/research/ files from tracked projects." default:"true" negatable:""`
	Estimate     bool   `help:"With --dry-run, print a token-count estimate for the queued work. Use to sanity-check scope before a real sync."`
}

// Counters for the sync run summary.
type syncCounters struct {
	discovered      int
	extracted       int
	sessionsScanned int
	absorbed        int
}

func (s *SyncCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}

	cfg := loadConfig(root)

	manifest, err := loadManifest(root)
	if err != nil {
		return err
	}

	// --changed: print changed files and exit.
	if s.Changed != "" {
		return s.showChanged(manifest)
	}

	// --reindex: run qmd update/embed and exit.
	if s.ReindexOnly {
		return s.reindex(root)
	}

	// --dry-run --estimate: print token estimate and exit without running.
	if s.DryRun && s.Estimate {
		ests := estimateSync(root, cfg)
		printEstimate(ests)
		return nil
	}

	// Advisory lock held for the whole sync. commit.go and a second concurrent
	// sync read this to decide whether to back off.
	if !s.DryRun {
		lockPath := lockPathFor(cfg.LockDir, "sync")
		lf, ok, lerr := acquireLock(lockPath)
		if lerr != nil {
			return fmt.Errorf("lock %s: %w", lockPath, lerr)
		}
		if !ok {
			logMsg("sync", "another scribe sync is running — exiting")
			return nil
		}
		defer releaseLock(lf)
	}

	logMsg("sync", "starting")

	// Phase 0: pull latest from remote so teammates' committed pages show
	// up before extraction/absorb. Silent no-op when KB isn't a git repo,
	// has no remote, or when the user disables it via sync.always_pull_before_sync.
	// Failures (offline, auth, rebase conflict) log and continue — we do
	// not want a flaky network call to crash a local sync run.
	if !s.DryRun && pullBeforeSyncEnabled(cfg) {
		if ok, pulled, pErr := pullRebase(root); pErr != nil {
			logMsg("sync", "pull skipped: %s (continuing)", pErr)
		} else if ok && pulled {
			logMsg("sync", "pulled new commits from remote")
		}
	}

	var counters syncCounters

	// Phase 1: Discover projects from ~/.claude/projects/.
	discovered, err := s.discover(root, manifest, cfg)
	if err != nil {
		return err
	}
	counters.discovered = discovered
	logMsg("sync", "discovered %d new projects", discovered)

	if s.DiscoverOnly {
		logMsg("sync", "discover-only mode, stopping")
		names := make([]string, 0, len(manifest.Projects))
		for name := range manifest.Projects {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Println(name)
		}
		return nil
	}

	// Phase 1.5: Collect drop files.
	totalDrops := s.collectDropFiles(root, manifest)
	if totalDrops > 0 {
		logMsg("sync", "%d total drop files to process", totalDrops)
	}

	// Phase 1.55: Collect research files from .claude/research/ in tracked projects.
	if s.Research {
		totalResearch := s.collectResearchFiles(root, manifest)
		if totalResearch > 0 {
			logMsg("sync", "%d research file(s) collected into raw/articles/", totalResearch)
		}
	}

	// Phase 1.55b: Drain file inbox (raw/inbox/<file> → raw/articles/<slug>.md
	// + originals moved to raw/inbox/.processed/). Routes through the convert
	// dispatcher (tier 1 marker → tier 0 Go-native fallback). Failed
	// conversions land in raw/inbox/.failed/<slug>/ with err.log so the user
	// can inspect without losing the source file.
	if !s.DryRun {
		if drained, err := drainFileInbox(root); err != nil {
			logMsg("sync", "file-inbox drain error: %v", err)
		} else if drained > 0 {
			logMsg("sync", "%d file(s) ingested from inbox", drained)
		}
	}

	// Phase 1.6: Drain ingest inbox (queued URLs → raw/articles/).
	if !s.DryRun {
		if err := drainInbox(root, 0, false); err != nil {
			logMsg("sync", "inbox drain error: %v", err)
		}
	}

	// Phase 1.7: Contextualize newly-ingested raw articles so qmd's embedding
	// index catches them on the upcoming reindex with a retrieval-context
	// paragraph. Idempotent via wiki/_contextualized_log.json. All knobs
	// (enable, model, max per run, timeout) live under `absorb.contextualize`
	// in scribe.yaml — zero-valued fields inherit absorbDefaults().
	if !s.DryRun {
		cfg := loadConfig(root)
		cx := cfg.Absorb.Contextualize
		if cx.Enabled != nil && *cx.Enabled {
			if err := contextualizeRawArticles(root, cx.MaxPerRun, cx.Model, false, false); err != nil {
				logMsg("sync", "contextualize error: %v", err)
			}
		}
	}

	// Phase 2: Extract changed projects.
	extracted, err := s.extract(root, manifest)
	if err != nil {
		logMsg("sync", "extraction error: %v", err)
	}
	counters.extracted = extracted

	// Phase 2.5: Session mining.
	if s.Sessions {
		mined, err := s.mineSessions(root)
		if err != nil {
			logMsg("sync", "session mining error: %v", err)
		}
		counters.sessionsScanned = mined
	}

	// Phase 2.6: Absorb raw articles.
	absorbed, err := s.absorbRaw(root)
	if err != nil {
		logMsg("sync", "absorb error: %v", err)
	}
	counters.absorbed = absorbed

	// Phase 3: Reindex + commit.
	if counters.extracted > 0 || counters.sessionsScanned > 0 || counters.absorbed > 0 {
		if err := s.rebuildAndReindex(root); err != nil {
			logMsg("sync", "reindex error: %v", err)
		}
	}

	if !s.DryRun && gitIsDirty(root) {
		cfg := loadConfig(root)
		if debounced, age, window := commitDebounced(root, cfg); debounced {
			logMsg("sync", "commit debounced (%s since last commit, window %s) — staged changes roll to next run", age.Round(time.Second), window)
		} else {
			msg := fmt.Sprintf("sync: auto-extract %s (%d projects)", time.Now().Format("2006-01-02"), counters.extracted)
			gitAddWiki(root)
			// If gitAddWiki staged nothing (dirty files all outside staging scope),
			// skip silently — nothing to commit.
			if gitHasStagedChanges(root) {
				if err := gitCommit(root, msg); err != nil {
					logMsg("sync", "commit failed: %v", err)
				} else {
					logMsg("sync", "committed")
					if gitRemoteURL(root) != "" {
						if err := gitPush(root); err != nil {
							logMsg("sync", "push failed: %v", err)
						} else {
							logMsg("sync", "pushed")
						}
					}
				}
			}
		}
	}

	// Refresh the single-file context cache if any project or session
	// produced output worth surfacing. Deterministic, cheap, silent on success.
	if !s.DryRun && (counters.extracted > 0 || counters.sessionsScanned > 0 || counters.absorbed > 0) {
		writeHotMDQuiet(root)
	}

	runStats = map[string]any{
		"discovered": counters.discovered,
		"extracted":  counters.extracted,
		"sessions":   counters.sessionsScanned,
		"absorbed":   counters.absorbed,
	}

	logMsg("sync", "done (discovered: %d, extracted: %d, sessions: %d, absorbed: %d)",
		counters.discovered, counters.extracted, counters.sessionsScanned, counters.absorbed)
	return nil
}

// showChanged prints changed files for a project and exits.
func (s *SyncCmd) showChanged(manifest *Manifest) error {
	entry, ok := manifest.Projects[s.Changed]
	if !ok {
		return fmt.Errorf("project %q not in manifest — run --discover first", s.Changed)
	}

	fmt.Printf("Changed files in %s since %s:\n", s.Changed, coalesce(entry.LastExtracted, "never"))

	patterns := []string{"*.md", "*.txt", "*.exs", "*.ex"}
	files := gitChangedFiles(entry.Path, entry.LastSHA, patterns)
	for _, f := range files {
		fmt.Println(f)
	}
	return nil
}

// reindex runs qmd update + embed and exits.
func (s *SyncCmd) reindex(root string) error {
	logMsg("sync", "reindex-only mode")
	out := runCmd(root, "qmd", "update")
	if out != "" {
		fmt.Println(out)
	}
	out = runCmd(root, "qmd", "embed")
	if out != "" {
		fmt.Println(out)
	}
	logMsg("sync", "reindex complete")
	return nil
}

// discover scans ~/.claude/projects/ for new projects and adds them to the manifest.
func (s *SyncCmd) discover(root string, manifest *Manifest, cfg *ScribeConfig) (int, error) {
	claudeDir := cfg.ClaudeProjectsDir
	if !dirExists(claudeDir) {
		return 0, nil
	}

	entries, err := os.ReadDir(claudeDir)
	if err != nil {
		return 0, fmt.Errorf("read claude projects dir: %w", err)
	}

	discovered := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		decoded := decodeClaudePath(entry.Name())
		if decoded == "" || !dirExists(decoded) {
			continue
		}

		if manifest.isIgnored(decoded) {
			continue
		}

		if !hasSignificantContent(decoded) {
			continue
		}

		pname := projectName(decoded)
		if _, exists := manifest.Projects[pname]; exists {
			continue
		}

		domain := manifest.resolveDomain(decoded)
		logMsg("sync", " DISCOVERED: %s -> %s (domain: %s)", pname, decoded, domain)
		discovered++

		if s.DryRun {
			continue
		}

		manifest.Projects[pname] = &ProjectEntry{
			Path:   decoded,
			Domain: domain,
		}
		if err := manifest.save(); err != nil {
			logMsg("sync", "manifest save failed: %v", err)
		}

		// Create .repo.yaml in the project's wiki directory.
		s.ensureRepoYAML(root, decoded, pname, domain)
	}

	return discovered, nil
}

// ensureRepoYAML creates a .repo.yaml in the KB project directory if it doesn't exist.
func (s *SyncCmd) ensureRepoYAML(root, projectPath, pname, domain string) {
	// Determine the wiki directory for this project.
	wikiDir := filepath.Join(root, "projects", strings.ToLower(filepath.Base(projectPath)))
	if domain != "general" {
		candidate := filepath.Join(root, "projects", strings.ToLower(domain))
		if dirExists(candidate) {
			wikiDir = candidate
		}
	}

	if err := os.MkdirAll(wikiDir, 0o755); err != nil {
		logMsg("sync", "   mkdir %s: %v", wikiDir, err)
		return
	}

	repoYAML := filepath.Join(wikiDir, ".repo.yaml")
	if fileExists(repoYAML) {
		return
	}

	remote := gitRemoteURL(projectPath)
	branch := gitBranch(projectPath)
	if branch == "" {
		branch = "main"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "name: %s\n", pname)
	fmt.Fprintf(&sb, "path: %s\n", projectPath)
	fmt.Fprintf(&sb, "domain: %s\n", domain)
	if remote != "" {
		fmt.Fprintf(&sb, "remote: %s\n", remote)
	}
	fmt.Fprintf(&sb, "branch: %s\n", branch)

	if err := os.WriteFile(repoYAML, []byte(sb.String()), 0o644); err != nil {
		logMsg("sync", "   write %s: %v", repoYAML, err)
		return
	}
	logMsg("sync", "   created %s", repoYAML)
}

// collectDropFiles gathers unprocessed drop files from each project's
// .claude/<kb-name>/ dir (e.g. .claude/scribe/, or a renamed KB's own
// folder). Returns total count of collected drops.
func (s *SyncCmd) collectDropFiles(root string, manifest *Manifest) int {
	totalDrops := 0
	kb := kbName(root)

	for pname, entry := range manifest.Projects {
		dropDir := filepath.Join(entry.Path, ".claude", kb)
		if !dirExists(dropDir) {
			continue
		}

		drops, err := filepath.Glob(filepath.Join(dropDir, "*.md"))
		if err != nil || len(drops) == 0 {
			continue
		}

		// Filter to unprocessed drops (newer than last_drop_processed).
		var unprocessed []string
		if entry.LastDropProcessed != "" {
			cutoff, err := time.Parse(time.RFC3339, entry.LastDropProcessed)
			if err == nil {
				for _, d := range drops {
					info, err := os.Stat(d)
					if err == nil && info.ModTime().After(cutoff) {
						unprocessed = append(unprocessed, d)
					}
				}
			}
		} else {
			unprocessed = drops
		}

		if len(unprocessed) == 0 {
			continue
		}

		staging := filepath.Join(root, "output", "drops-"+pname)
		if err := os.MkdirAll(staging, 0o755); err != nil {
			logMsg("sync", " [%s] mkdir %s: %v", pname, staging, err)
			continue
		}

		for _, d := range unprocessed {
			data, err := os.ReadFile(d)
			if err != nil {
				continue
			}
			dest := filepath.Join(staging, filepath.Base(d))
			if err := os.WriteFile(dest, data, 0o644); err != nil {
				logMsg("sync", " [%s] write %s: %v", pname, dest, err)
			}
		}

		logMsg("sync", " [%s] %d drop file(s) collected", pname, len(unprocessed))
		totalDrops += len(unprocessed)
	}

	return totalDrops
}

// collectResearchFiles gathers unprocessed .claude/research/**/*.md files from tracked projects
// and copies them into raw/articles/ with proper frontmatter for the absorb pipeline.
func (s *SyncCmd) collectResearchFiles(root string, manifest *Manifest) int {
	total := 0

	for pname, entry := range manifest.Projects {
		researchDir := filepath.Join(entry.Path, ".claude", "research")
		if !dirExists(researchDir) {
			continue
		}

		// Walk the entire research directory tree for .md files.
		var files []string
		_ = filepath.WalkDir(researchDir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // skip unreadable or directory, continue walk
			}
			if strings.HasSuffix(path, ".md") {
				files = append(files, path)
			}
			return nil
		})
		if len(files) == 0 {
			continue
		}

		// Filter to files newer than last scan.
		var unscanned []string
		if entry.LastResearchScanned != "" {
			cutoff, err := time.Parse(time.RFC3339, entry.LastResearchScanned)
			if err == nil {
				for _, f := range files {
					info, err := os.Stat(f)
					if err == nil && info.ModTime().After(cutoff) {
						unscanned = append(unscanned, f)
					}
				}
			}
		} else {
			unscanned = files
		}

		if len(unscanned) == 0 {
			continue
		}

		destDir := filepath.Join(root, "raw", "articles")
		domain := entry.Domain
		if domain == "" {
			domain = "general"
		}

		collected := 0
		for _, f := range unscanned {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}

			// Build a flat dest filename from the relative path within research/.
			rel, _ := filepath.Rel(researchDir, f)
			flatName := strings.ReplaceAll(rel, string(filepath.Separator), "-")
			content := string(data)

			// Add frontmatter if missing.
			if !strings.HasPrefix(strings.TrimSpace(content), "---") {
				title := strings.TrimSuffix(flatName, ".md")
				// Strip date prefix if present (e.g. "2026-04-09-topic" → "topic").
				parts := strings.SplitN(title, "-", 4)
				if len(parts) >= 4 && len(parts[0]) == 4 && len(parts[1]) == 2 && len(parts[2]) == 2 {
					title = parts[3]
				}
				title = strings.ReplaceAll(title, "-", " ")
				words := strings.Fields(title)
				for i, w := range words {
					if len(w) > 0 {
						words[i] = strings.ToUpper(w[:1]) + w[1:]
					}
				}
				title = strings.Join(words, " ")

				fm := fmt.Sprintf("---\ntitle: \"%s\"\nsource_path: \"%s\"\ningested_at: \"%s\"\nformat: markdown\ndomain: %s\nproject: %s\n---\n\n",
					title, f, time.Now().UTC().Format(time.RFC3339), domain, pname)
				content = fm + content
			}

			destName := fmt.Sprintf("research-%s-%s", pname, flatName)
			dest := filepath.Join(destDir, destName)

			if s.DryRun {
				logMsg("sync", " [%s] would collect research: %s", pname, rel)
				collected++
				continue
			}

			if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
				logMsg("sync", " [%s] failed to write %s: %v", pname, destName, err)
				continue
			}

			logMsg("sync", " [%s] collected research: %s → %s", pname, rel, destName)
			collected++
		}

		total += collected

		// Update timestamp in manifest for this project.
		if !s.DryRun && collected > 0 {
			manifestMu.Lock()
			entry.LastResearchScanned = time.Now().UTC().Format(time.RFC3339)
			if err := manifest.save(); err != nil {
				logMsg("sync", "warn: manifest save failed: %v", err)
			}
			manifestMu.Unlock()
		}
	}

	return total
}

// extract processes projects that need extraction.
// Projects run concurrently up to s.Parallel (default 3, capped at 5 to
// avoid Anthropic rate limits). A rate-limit error cancels pending work
// via the errgroup context; already-running extractions finish naturally.
func (s *SyncCmd) extract(root string, manifest *Manifest) (int, error) {
	cfg := loadConfig(root)
	toExtract := s.projectsNeedingExtraction(manifest)

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
			patterns := []string{"*.md", "*.txt", "*.exs", "*.ex"}
			changed := gitChangedFiles(entry.Path, entry.LastSHA, patterns)
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

			patterns := []string{"*.md", "*.txt", "*.exs", "*.ex"}
			changed := gitChangedFiles(entry.Path, entry.LastSHA, patterns)

			// Size gate: one `claude -p` pass reliably blows the 10-min
			// timeout above ~100 changed files. Rather than eat the
			// "signal: killed" error and defer the project to the next
			// sync (where it will just fail again), skip the project in
			// normal sync and point the user at `scribe deep <name>`,
			// which batches-by-directory and fits in the timeout.
			if cfg.Sync.MaxExtractFiles > 0 && len(changed) > cfg.Sync.MaxExtractFiles {
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
func (s *SyncCmd) projectsNeedingExtraction(manifest *Manifest) []string {
	var result []string

	for pname, entry := range manifest.Projects {
		// If --extract specified, only consider that project.
		if s.Extract != "" && pname != s.Extract {
			continue
		}

		if !dirExists(entry.Path) {
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

		logMsg("sync", " [%s] unchanged, skipping", pname)
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

	tools := []string{
		"Read", "Write", "Edit", "Glob", "Grep",
		"Bash(git log:*)", "Bash(git -C:*)", "Bash(ls:*)", "Bash(find:*)", "Bash(wc:*)",
	}
	ctx := context.Background()
	_, err = runClaude(ctx, root, prompt, s.Model, tools, 10*time.Minute)
	if err != nil {
		return fmt.Errorf("claude extraction: %w", err)
	}

	// Post-extraction lint on changed files.
	lintOut, _ := runCmdErr(root, scribeExe, "lint", "--changed")
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

// sessionFilterStats captures the per-session numbers the pre-filter looks at.
type sessionFilterStats struct {
	UserMsgs   int
	TotalChars int
	Found      bool
}

// filterVerdict returns "" (keep) or a reason string (skip).
// Keep in sync with preFilterSessions decision logic — this is the single
// source of truth for the pre-filter rule set.
func (st sessionFilterStats) filterVerdict() string {
	if !st.Found {
		return "" // keep on lookup error
	}
	// Skip truly empty sessions (<500 chars or no user turn at all).
	if st.UserMsgs < 1 || st.TotalChars < 500 {
		return "empty"
	}
	// Drop short back-and-forth with no depth: need at least 3 user
	// messages OR a rich one-shot assistant response (>=2000 chars).
	if st.UserMsgs < 3 && st.TotalChars < 2000 {
		return "thin"
	}
	return ""
}

// querySessionStats hits the ccrider DB for a single session.
func querySessionStats(db *sql.DB, sid string) sessionFilterStats {
	var st sessionFilterStats
	//nolint:noctx // CLI top-level
	err := db.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN type = 'user' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(LENGTH(text_content)), 0)
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE s.session_id = ?`, sid).Scan(&st.UserMsgs, &st.TotalChars)
	st.Found = (err == nil)
	return st
}

// preFilterSessions removes sessions that are too mechanical to be worth extracting.
// Queries ccrider DB directly for message stats. Returns filtered list + skipped IDs.
func preFilterSessions(dbPath string, sessionIDs []string) (kept, skipped []string) {
	if len(sessionIDs) == 0 {
		return sessionIDs, nil
	}

	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		// If we can't open DB, keep all sessions (no filtering).
		return sessionIDs, nil
	}
	defer db.Close()

	for _, sid := range sessionIDs {
		stats := querySessionStats(db, sid)
		if !stats.Found {
			kept = append(kept, sid) // On error, keep the session.
			continue
		}
		if stats.filterVerdict() != "" {
			skipped = append(skipped, sid)
			continue
		}
		kept = append(kept, sid)
	}
	return kept, skipped
}

// relatedSession is a nearby session surfaced to the extractor so it can
// reference or deduplicate against sibling work from the same project.
type relatedSession struct {
	SessionID string
	Date      string
	Summary   string
}

// queryRelatedSessions returns up to `limit` sessions from the same project
// whose updated_at is within `daysWindow` of the target session. The target
// session itself is excluded. Best effort — returns nil on any DB error.
func queryRelatedSessions(dbPath, sessionID string, daysWindow, limit int) []relatedSession {
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		return nil
	}
	defer db.Close()

	// Fetch the target session's project + updated_at first.
	var projectPath string
	var updatedAt string
	//nolint:noctx // CLI top-level
	err = db.QueryRow(
		`SELECT project_path, updated_at FROM sessions WHERE session_id = ?`,
		sessionID,
	).Scan(&projectPath, &updatedAt)
	if err != nil || projectPath == "" {
		return nil
	}

	//nolint:noctx // CLI top-level; sync orchestrates its own cancellation via errgroup
	rows, err := db.Query(`
		SELECT session_id, date(updated_at),
		       COALESCE(NULLIF(llm_summary, ''), COALESCE(summary, ''))
		FROM sessions
		WHERE project_path = ?
		  AND session_id != ?
		  AND ABS(JULIANDAY(updated_at) - JULIANDAY(?)) < ?
		ORDER BY updated_at DESC
		LIMIT ?`,
		projectPath, sessionID, updatedAt, daysWindow, limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []relatedSession
	for rows.Next() {
		var rs relatedSession
		if err := rows.Scan(&rs.SessionID, &rs.Date, &rs.Summary); err == nil {
			if len(rs.Summary) > 140 {
				rs.Summary = rs.Summary[:140] + "…"
			}
			out = append(out, rs)
		}
	}
	return out
}

// formatRelatedSessions turns a slice of related sessions into the bullet
// list injected into the prompt as the {{RELATED_SESSIONS}} variable.
func formatRelatedSessions(related []relatedSession) string {
	if len(related) == 0 {
		return "(none — this is the first or only recent session for this project)"
	}
	var sb strings.Builder
	for _, r := range related {
		summary := strings.ReplaceAll(r.Summary, "\n", " ")
		if summary == "" {
			summary = "(no summary)"
		}
		fmt.Fprintf(&sb, "- %s (%s): %s\n", r.SessionID, r.Date, summary)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// triageSessionIDs calls scribe triage and returns session IDs.
func (s *SyncCmd) triageSessionIDs(top int, extraArgs ...string) []string {
	scribeExe, _ := os.Executable()
	if scribeExe == "" {
		scribeExe = "scribe"
	}
	args := make([]string, 0, 6+len(extraArgs))
	args = append(args, "triage", "--ids", "--top", fmt.Sprintf("%d", top), "--sort", s.SessionSort)
	args = append(args, extraArgs...)
	idsOutput, err := runCmdErr("", scribeExe, args...)
	if err != nil {
		return nil
	}
	var ids []string
	for line := range strings.SplitSeq(idsOutput, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			ids = append(ids, line)
		}
	}
	return ids
}

// sessionResult captures the outcome of a single session extraction.
type sessionResult struct {
	sessionID   string
	success     bool
	rateLimited bool
	err         error
}

// recordBatchOutcome appends one entry to wiki/_extraction_outcomes.json
// summarizing what the last checkpoint commit contained. This is approximate
// (all sessions in a parallel checkpoint share credit) but gives Phase 4
// threshold-tuning a data feed without racing on per-goroutine writes.
// Called AFTER a successful commitAndPush — reads HEAD~1..HEAD shortstat.
func recordBatchOutcome(root, label string, sessionIDs []string) {
	if len(sessionIDs) == 0 {
		return
	}
	files, added, deleted := gitDiffShortstat(root, "HEAD~1", "HEAD")
	sha := gitSHA(root)
	outcomesPath := filepath.Join(root, "wiki", "_extraction_outcomes.json")
	if !fileExists(outcomesPath) {
		if err := os.WriteFile(outcomesPath, []byte(`{"entries": []}`+"\n"), 0o644); err != nil {
			logMsg("sync", "init %s: %v", outcomesPath, err)
			return
		}
	}
	if err := updateJSONFile(outcomesPath, func(data map[string]any) {
		entries, _ := data["entries"].([]any)
		entry := map[string]any{
			"timestamp":     time.Now().UTC().Format(time.RFC3339),
			"label":         label,
			"commit_sha":    sha,
			"session_ids":   sessionIDs,
			"files_changed": files,
			"lines_added":   added,
			"lines_deleted": deleted,
		}
		data["entries"] = append(entries, entry)
	}); err != nil {
		logMsg("sync", "warn: could not update outcomes.json: %v", err)
	}
}

// mineSessionBatches processes session IDs with bounded parallelism.
// Returns total mined and whether a rate limit was hit.
// For parallel > 1, each session gets its own claude -p call.
// For parallel == 1 (large sessions), runs serially.
func (s *SyncCmd) mineSessionBatches(root string, sessionIDs []string, parallel int, timeout time.Duration, promptName string, label string) (int, bool) {
	tools := []string{
		"Read", "Write", "Edit", "Glob", "Grep",
		"mcp__ccrider__get_session_messages", "mcp__ccrider__generate_session_anchor",
	}

	cfg := loadConfig(root)
	checkpointInterval := cfg.Sync.CheckpointInterval
	if checkpointInterval <= 0 {
		checkpointInterval = 5
	}

	dbPath := cfg.CcriderDB

	if parallel <= 1 {
		// Serial mode for large sessions (avoids concurrent wiki writes).
		return s.mineSessionsSerial(root, sessionIDs, timeout, promptName, label, tools, checkpointInterval)
	}

	// Parallel mode: bounded concurrency via semaphore channel.
	sem := make(chan struct{}, parallel)
	results := make(chan sessionResult, len(sessionIDs))
	var wg gosync.WaitGroup

	logMsg("sync", "%s: processing %d sessions (parallel=%d)", label, len(sessionIDs), parallel)

	for i, sid := range sessionIDs {
		wg.Add(1)
		go func(idx int, sessionID string) {
			defer wg.Done()
			sem <- struct{}{}        // acquire
			defer func() { <-sem }() // release

			logMsg("sync", "%s [%d/%d] extracting %s", label, idx+1, len(sessionIDs), sessionID)

			related := queryRelatedSessions(dbPath, sessionID, 7, 10)
			vars := map[string]string{
				"KB_DIR":           root,
				"SESSION_ID_LIST":  sessionID,
				"RELATED_SESSIONS": formatRelatedSessions(related),
			}
			prompt, err := loadPrompt(promptName, vars)
			if err != nil {
				results <- sessionResult{sessionID, false, false, err}
				return
			}

			ctx := context.Background()
			_, err = runClaude(ctx, root, prompt, s.Model, tools, timeout)
			if err != nil {
				rl := errors.Is(err, ErrRateLimit)
				results <- sessionResult{sessionID, false, rl, err}
				return
			}
			results <- sessionResult{sessionID, true, false, nil}
		}(i, sid)
	}

	// Close results channel when all goroutines complete.
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results and checkpoint periodically.
	totalMined := 0
	rateLimited := false
	sinceCheckpoint := 0
	var batchIDs []string // session IDs rolled into the next checkpoint commit

	for r := range results {
		if r.rateLimited {
			logMsg("sync", "%s: rate limited on %s — will resume next run", label, r.sessionID)
			rateLimited = true
			// Don't break — let in-flight goroutines finish, just stop launching new ones would be ideal
			// but with the channel approach, already-launched goroutines will complete.
			continue
		}
		if r.err != nil {
			logMsg("sync", "%s: %s failed: %v", label, r.sessionID, r.err)
			continue
		}

		totalMined++
		sinceCheckpoint++
		batchIDs = append(batchIDs, r.sessionID)
		logMsg("sync", "%s: %s complete (%d/%d mined)", label, r.sessionID, totalMined, len(sessionIDs))

		// Checkpoint after every N successful extractions.
		if sinceCheckpoint >= checkpointInterval && totalMined < len(sessionIDs) {
			sinceCheckpoint = 0
			if err := s.rebuildAndReindex(root); err != nil {
				logMsg("sync", "checkpoint reindex error: %v", err)
			}
			if err := s.commitAndPush(root, fmt.Sprintf("sync: %s checkpoint (%d sessions)", label, totalMined)); err != nil {
				logMsg("sync", "checkpoint commit error: %v", err)
			} else {
				recordBatchOutcome(root, label, batchIDs)
				batchIDs = nil
			}
		}
	}

	return totalMined, rateLimited
}

// mineSessionsSerial processes sessions one at a time (for large sessions).
func (s *SyncCmd) mineSessionsSerial(root string, sessionIDs []string, timeout time.Duration, promptName string, label string, tools []string, checkpointInterval int) (int, bool) {
	totalMined := 0
	cfg := loadConfig(root)
	var batchIDs []string

	for i, sid := range sessionIDs {
		logMsg("sync", "%s [%d/%d] extracting %s", label, i+1, len(sessionIDs), sid)

		related := queryRelatedSessions(cfg.CcriderDB, sid, 7, 10)
		vars := map[string]string{
			"KB_DIR":           root,
			"SESSION_ID":       sid,
			"SESSION_ID_LIST":  sid,
			"MESSAGE_COUNT":    "large",
			"RELATED_SESSIONS": formatRelatedSessions(related),
		}
		prompt, err := loadPrompt(promptName, vars)
		if err != nil {
			logMsg("sync", "load prompt error: %v", err)
			continue
		}

		ctx := context.Background()
		_, err = runClaude(ctx, root, prompt, s.Model, tools, timeout)
		if err != nil {
			if errors.Is(err, ErrRateLimit) {
				logMsg("sync", "%s: rate limited — will resume next run (%d mined)", label, totalMined)
				return totalMined, true
			}
			logMsg("sync", "%s: %s failed: %v", label, sid, err)
			continue
		}

		totalMined++
		batchIDs = append(batchIDs, sid)
		logMsg("sync", "%s [%d/%d] complete (%d mined)", label, i+1, len(sessionIDs), totalMined)

		// Checkpoint.
		if totalMined%checkpointInterval == 0 && i < len(sessionIDs)-1 {
			if err := s.rebuildAndReindex(root); err != nil {
				logMsg("sync", "checkpoint reindex error: %v", err)
			}
			if err := s.commitAndPush(root, fmt.Sprintf("sync: %s checkpoint (%d sessions)", label, totalMined)); err != nil {
				logMsg("sync", "checkpoint commit error: %v", err)
			} else {
				recordBatchOutcome(root, label, batchIDs)
				batchIDs = nil
			}
		}
	}
	return totalMined, false
}

// mineSessions runs session mining: triage via FTS5 then extract via LLM.
// Two passes: normal sessions (<=300 msgs, batches of 3) then large sessions (>300 msgs, one at a time).
func (s *SyncCmd) mineSessions(root string) (int, error) {
	logMsg("sync", "session mining (triage + extract, max %d)...", s.SessionsMax)

	// Ensure ccrider DB is fresh before triaging.
	if out := runCmd("", "ccrider", "sync"); out != "" {
		logMsg("sync", "ccrider sync: %s", lastLine(out))
	}

	if s.DryRun {
		// Peek at the hook queue without clearing it. Lets `sync --dry-run`
		// show the near-real-time capture pipeline is working without
		// actually consuming the queue (that belongs to the real sync run).
		if peeked := peekPendingSessions(); len(peeked) > 0 {
			logMsg("sync", "DRY RUN -- hook queue: %d pending session(s): %s",
				len(peeked), strings.Join(peeked, ", "))
		}
		logMsg("sync", "DRY RUN -- triage results:")
		scribeExe, _ := os.Executable()
		if scribeExe == "" {
			scribeExe = "scribe"
		}
		out, _ := runCmdErr("", scribeExe, "triage", "--top", fmt.Sprintf("%d", s.SessionsMax), "--sort", s.SessionSort)
		if out != "" {
			fmt.Println(out)
		}
		return 0, nil
	}

	// Ensure _sessions_log.json exists.
	sessionsLog := filepath.Join(root, "wiki", "_sessions_log.json")
	if !fileExists(sessionsLog) {
		if err := os.WriteFile(sessionsLog, []byte(`{"processed": {}, "last_scan": null}`+"\n"), 0o644); err != nil {
			return 0, fmt.Errorf("init sessions log: %w", err)
		}
	}

	totalMined := 0

	// Drain the hook queue first. The SessionEnd hook (see hook.go) drops
	// high-value session IDs here as they happen, so draining before the
	// normal triage gives those sessions priority over whatever the FTS5
	// scorer would surface next. Already-processed pending IDs are filtered
	// out below; the file itself is cleared on read so IDs are not reused.
	pendingIDs, err := readAndClearPendingSessions()
	if err != nil {
		logMsg("sync", "pending queue read error (continuing): %v", err)
	}
	if len(pendingIDs) > 0 {
		// Drop anything already extracted before. A hook might enqueue a
		// session that a previous sync already absorbed — don't waste a
		// slot on it.
		processedSet := make(map[string]bool)
		for _, id := range loadProcessedSessionIDs(filepath.Join(root, "wiki", "_sessions_log.json")) {
			processedSet[id] = true
		}
		filtered := pendingIDs[:0]
		for _, id := range pendingIDs {
			if !processedSet[id] {
				filtered = append(filtered, id)
			}
		}
		pendingIDs = filtered
		if len(pendingIDs) > 0 {
			logMsg("sync", "hook queue: %d pending session(s) to prioritize", len(pendingIDs))
		}
	}

	// Pass 1: Normal sessions (<=300 messages) — batches of 3, 10min timeout.
	normalIDs := s.triageSessionIDs(s.SessionsMax, "--message-limit", "300")

	// Merge pending IDs ahead of triage picks, then trim to the slot budget.
	// Pending items go first so high-value sessions jump the queue; trim
	// keeps the parallel extractor from blowing past SessionsMax.
	if len(pendingIDs) > 0 {
		seen := make(map[string]bool, len(pendingIDs)+len(normalIDs))
		merged := make([]string, 0, len(pendingIDs)+len(normalIDs))
		for _, id := range pendingIDs {
			if !seen[id] {
				seen[id] = true
				merged = append(merged, id)
			}
		}
		for _, id := range normalIDs {
			if !seen[id] {
				seen[id] = true
				merged = append(merged, id)
			}
		}
		if len(merged) > s.SessionsMax {
			merged = merged[:s.SessionsMax]
		}
		normalIDs = merged
	}

	// Pre-filter: skip mechanical sessions with too few user messages or content.
	cfg := loadConfig(root)
	if len(normalIDs) > 0 {
		kept, skipped := preFilterSessions(cfg.CcriderDB, normalIDs)
		if len(skipped) > 0 {
			logMsg("sync", "pre-filter: skipped %d mechanical sessions (<%d user msgs or <500 chars)", len(skipped), 3)
			// Mark skipped sessions so they're not re-triaged.
			sessionsLog := filepath.Join(root, "wiki", "_sessions_log.json")
			if err := updateJSONFile(sessionsLog, func(data map[string]any) {
				processed, _ := data["processed"].(map[string]any)
				if processed == nil {
					processed = make(map[string]any)
					data["processed"] = processed
				}
				for _, sid := range skipped {
					processed[sid] = map[string]any{
						"extracted": time.Now().UTC().Format(time.RFC3339),
						"skipped":   true,
						"reason":    "mechanical (low user messages or content)",
					}
				}
			}); err != nil {
				logMsg("sync", "warn: could not update _sessions_log.json: %v", err)
			}
		}
		normalIDs = kept
	}

	if len(normalIDs) > 0 {
		logMsg("sync", "triage found %d normal sessions (<=300 msgs)", len(normalIDs))
		mined, rateLimited := s.mineSessionBatches(root, normalIDs, cfg.Sync.ParallelExtractions, 10*time.Minute, "session-extract.md", "session")
		totalMined += mined
		if rateLimited {
			logMsg("sync", "rate limited during normal session mining — skipping large sessions")
			s.updateScanTimestamp(sessionsLog)
			logMsg("sync", "session mining complete (%d sessions mined)", totalMined)
			return totalMined, nil
		}
	} else {
		logMsg("sync", "no normal sessions to mine")
	}

	// Pass 2: Large sessions (>300 messages) — one at a time, 20min timeout.
	if !s.SkipLarge {
		largeMax := max(1, s.SessionsMax/3) // Process fewer large sessions per run.
		largeIDs := s.triageSessionIDs(largeMax, "--min-messages", "301")
		if len(largeIDs) > 0 {
			logMsg("sync", "triage found %d large sessions (>300 msgs)", len(largeIDs))
			mined, _ := s.mineSessionBatches(root, largeIDs, 1, 20*time.Minute, "session-extract-large.md", "large-session")
			totalMined += mined
		}
	}

	s.updateScanTimestamp(sessionsLog)
	logMsg("sync", "session mining complete (%d sessions mined)", totalMined)
	return totalMined, nil
}

// updateScanTimestamp updates the last_scan field in _sessions_log.json.
func (s *SyncCmd) updateScanTimestamp(sessionsLog string) {
	if err := updateJSONFile(sessionsLog, func(data map[string]any) {
		data["last_scan"] = time.Now().UTC().Format(time.RFC3339)
	}); err != nil {
		logMsg("sync", "warn: could not update last_scan in _sessions_log.json: %v", err)
	}
}

// absorbRaw processes unabsorbed articles from raw/articles/.
// Strictness gates auto-absorb: "high" skips raw articles without an
// explicit `absorb: true` frontmatter flag or a named domain (not "general").
// "medium" (default) processes all unabsorbed articles. "low" is identical
// to "medium" at present but reserved for future relaxations.
// Max-per-run, density thresholds, pass models, and timeouts all come from
// scribe.yaml `absorb:` (see absorbDefaults for the baseline).
func (s *SyncCmd) absorbRaw(root string) (int, error) {
	rawDir := filepath.Join(root, "raw", "articles")
	if !dirExists(rawDir) {
		return 0, nil
	}

	cfg := loadConfig(root)
	strictness := cfg.Absorb.Strictness
	maxAbsorb := cfg.Absorb.MaxPerRun

	// Load absorb log.
	absorbLogPath := filepath.Join(root, "wiki", "_absorb_log.json")
	absorbLog := loadJSONMap(absorbLogPath)

	// Find unabsorbed articles.
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		return 0, fmt.Errorf("read raw/articles: %w", err)
	}

	absorbed := 0

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		// Already absorbed?
		if _, ok := absorbLog[entry.Name()]; ok {
			continue
		}

		if absorbed >= maxAbsorb {
			break
		}

		rawFile := filepath.Join(rawDir, entry.Name())

		// Unfetched stubs are zero-signal — skip absorb and route the URL to
		// the parked-links list so the user can handle them manually.
		// `scribe capture --refetch` retries fetching these in a batch.
		if rawArticleIsStub(rawFile) {
			if parkStubLink(root, rawFile) {
				logMsg("sync", "parked unfetched stub %s → wiki/_unfetched-links.md", entry.Name())
			}
			absorbLog[entry.Name()] = time.Now().UTC().Format(time.RFC3339)
			if err := saveJSONMap(absorbLogPath, absorbLog); err != nil {
				logMsg("sync", "warn: could not persist _absorb_log.json: %v", err)
			}
			continue
		}

		// Strictness gate: high = explicit opt-in required.
		if strictness == "high" && !rawArticleOptsIntoAbsorb(rawFile) {
			logMsg("sync", "skipping %s (strictness=high, no absorb opt-in)", entry.Name())
			continue
		}

		if s.DryRun {
			logMsg("sync", "would absorb raw/articles/%s", entry.Name())
			absorbed++
			continue
		}

		density := readRawDensity(rawFile)
		logMsg("sync", "absorbing raw/articles/%s (density=%s)", entry.Name(), density)

		var absorbErr error
		if density == "dense" {
			absorbErr = s.absorbDenseTwoPass(root, rawFile, entry.Name())
		} else {
			absorbErr = s.absorbSinglePass(root, rawFile)
		}
		if absorbErr != nil {
			if errors.Is(absorbErr, ErrRateLimit) {
				logMsg("sync", "rate limited during absorb — will resume next run")
				break
			}
			logMsg("sync", "absorb failed for %s: %v", entry.Name(), absorbErr)
			continue
		}

		// Mark as absorbed.
		absorbLog[entry.Name()] = time.Now().UTC().Format(time.RFC3339)
		if err := saveJSONMap(absorbLogPath, absorbLog); err != nil {
			logMsg("sync", "warn: could not persist _absorb_log.json: %v", err)
		}

		absorbed++

		// Checkpoint lint every 5 absorptions.
		if absorbed%5 == 0 {
			logMsg("sync", "absorb checkpoint (%d absorbed, running lint)", absorbed)
			scribeExe, _ := os.Executable()
			if scribeExe == "" {
				scribeExe = "scribe"
			}
			_, _ = runCmdErr(root, scribeExe, "lint", "--changed")
		}
	}

	if absorbed > 0 {
		logMsg("sync", "absorbed %d raw articles", absorbed)
	}
	return absorbed, nil
}

// readRawDensity returns the density label from a raw article's frontmatter,
// or a heuristic classification when the frontmatter field is missing (older
// raw articles written before density was added to buildRawArticle). Returns
// "standard" on any parse error so absorb falls back to single-pass.
func readRawDensity(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "standard"
	}
	if raw, err := parseFrontmatterRaw(data); err == nil {
		if d, ok := raw["density"].(string); ok && d != "" {
			return d
		}
	}
	// Fallback: strip frontmatter and classify body heuristically.
	body := stripFrontmatter(string(data))
	_, density := classifyDensity(body)
	return density
}

// stripFrontmatter returns the body portion of a markdown file, dropping the
// leading `---\n...\n---\n` block if present.
func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---") {
		return s
	}
	end := strings.Index(s[3:], "\n---")
	if end < 0 {
		return s
	}
	rest := s[end+7:] // skip `\n---`
	return strings.TrimLeft(rest, "\n")
}

// absorbSinglePass runs the original single-pass absorb.md prompt. Timeout
// comes from AbsorbConfig.SinglePassTimeoutMin.
func (s *SyncCmd) absorbSinglePass(root, rawFile string) error {
	cfg := loadConfig(root)
	prompt, err := loadPrompt("absorb.md", map[string]string{
		"KB_DIR":         root,
		"RAW_FILE":       rawFile,
		"BRIEF_WORDS":    fmt.Sprintf("%d", cfg.Absorb.BriefThresholdWords),
		"BRIEF_HEADINGS": fmt.Sprintf("%d", cfg.Absorb.BriefThresholdHeadings),
		"DENSE_WORDS":    fmt.Sprintf("%d", cfg.Absorb.DenseThresholdWords),
		"DENSE_HEADINGS": fmt.Sprintf("%d", cfg.Absorb.DenseThresholdHeadings),
	})
	if err != nil {
		return fmt.Errorf("load absorb prompt: %w", err)
	}
	tools := []string{"Read", "Write", "Edit", "Glob", "Grep", "Bash(wc:*)"}
	ctx := context.Background()
	timeout := time.Duration(cfg.Absorb.SinglePassTimeoutMin) * time.Minute
	_, err = runClaude(ctx, root, prompt, s.Model, tools, timeout)
	return err
}

// absorbDenseTwoPass runs the entity-first two-pass absorb for dense raw
// articles. Pass 1 (Haiku) writes a plan JSON listing the distinct entities.
// Pass 2 (s.Model, typically Sonnet) is called once per entity, sequentially,
// writing one focused wiki page each. Pass 2 invocations do NOT touch
// _index.md or _backlinks.json — those are rebuilt by the sync-level
// rebuildAndReindex call after all absorbs complete.
//
// Sequential Pass 2 avoids concurrent writes to the same wiki page when two
// entities target the same article (rare but possible when Pass 1 proposes
// variant labels). If throughput becomes a problem, guard concurrent writes
// with a per-wiki-path lock and parallelize.
func (s *SyncCmd) absorbDenseTwoPass(root, rawFile, rawName string) error {
	cfg := loadConfig(root)
	plansDir := filepath.Join(root, "output", "absorb-plans")
	if err := os.MkdirAll(plansDir, 0o755); err != nil {
		return fmt.Errorf("mkdir plans: %w", err)
	}
	planFile := filepath.Join(plansDir, strings.TrimSuffix(rawName, ".md")+".json")

	ctx := context.Background()

	// Phase 3A.5 chaptered path: when a TOC sidecar exists with at
	// least cfg.Absorb.ChapterThreshold chapters, fan pass-1 out
	// across chapters in parallel and merge the per-chapter plans.
	// Falls through to the legacy single-shot path on any disqualifier
	// (no sidecar, too few chapters, ChapterAware disabled).
	if chaptered, chunks, _ := shouldAbsorbChaptered(rawFile, cfg.Absorb); chaptered {
		if err := s.runPass1Chaptered(ctx, root, rawFile, rawName, chunks, cfg.Absorb, planFile); err != nil {
			if errors.Is(err, ErrRateLimit) {
				return err
			}
			// Chapter pass had a non-rate-limit failure — fall back
			// to whole-article pass-1 so the article still absorbs.
			logMsg("sync", "chaptered pass1 failed for %s (%v); falling back to whole-article pass1", rawName, err)
			if err := s.runPass1Whole(ctx, root, rawFile, planFile, cfg.Absorb); err != nil {
				return err
			}
		}
	} else {
		if err := s.runPass1Whole(ctx, root, rawFile, planFile, cfg.Absorb); err != nil {
			return err
		}
	}

	// Parse plan JSON.
	planBytes, err := os.ReadFile(planFile)
	if err != nil {
		return fmt.Errorf("read plan: %w", err)
	}
	var plan absorbPlan
	if err := json.Unmarshal(planBytes, &plan); err != nil {
		return fmt.Errorf("parse plan: %w", err)
	}
	if len(plan.Entities) == 0 {
		logMsg("sync", "pass1 produced 0 entities for %s — falling back to single-pass", rawName)
		return s.absorbSinglePass(root, rawFile)
	}
	logMsg("sync", "pass1 planned %d entities for %s", len(plan.Entities), rawName)

	// Pass 2: one wiki page per entity. Runs in parallel with SetLimit to
	// throttle concurrent claude -p invocations (each entity gets its own
	// process). Two entities writing to the same wiki file would race, so
	// a per-target-label mutex serializes that specific pair while letting
	// the others fan out.
	domain := plan.Domain
	if domain == "" {
		domain = "general"
	}
	pass2Model := cfg.Absorb.Pass2Model
	if pass2Model == "" {
		pass2Model = s.Model
	}
	pass2Timeout := time.Duration(cfg.Absorb.Pass2TimeoutMin) * time.Minute
	pass2Tools := []string{"Read", "Write", "Edit", "Glob", "Grep", "Bash(wc:*)"}

	parallel := cfg.Absorb.Pass2Parallel
	if parallel <= 0 {
		parallel = 3
	}
	if parallel > len(plan.Entities) {
		parallel = len(plan.Entities)
	}
	logMsg("sync", "pass2: %d entities, parallel=%d", len(plan.Entities), parallel)

	// Per-target-label lock map so two entities aiming at the same wiki
	// article (rare but possible when Pass 1 proposes variants) don't race.
	var labelLocksMu gosync.Mutex
	labelLocks := map[string]*gosync.Mutex{}
	labelLockFor := func(label string) *gosync.Mutex {
		labelLocksMu.Lock()
		defer labelLocksMu.Unlock()
		if m, ok := labelLocks[label]; ok {
			return m
		}
		m := &gosync.Mutex{}
		labelLocks[label] = m
		return m
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallel)

	var rateLimited bool
	var rateLimitMu gosync.Mutex

	for i, ent := range plan.Entities {
		g.Go(func() error {
			if gctx.Err() != nil {
				return nil // canceled due to rate limit
			}
			keyClaims := strings.Join(ent.KeyClaims, " | ")
			if keyClaims == "" {
				keyClaims = "(none flagged)"
			}
			pass2Prompt, err := loadPrompt("absorb-pass2.md", map[string]string{
				"KB_DIR":            root,
				"RAW_FILE":          rawFile,
				"PLAN_FILE":         planFile,
				"ENTITY_LABEL":      ent.Label,
				"ENTITY_TYPE":       ent.Type,
				"ENTITY_ONE_LINE":   ent.OneLine,
				"ENTITY_KEY_CLAIMS": keyClaims,
				"DOMAIN":            domain,
			})
			if err != nil {
				return fmt.Errorf("load pass2 prompt: %w", err)
			}
			// Serialize writes aimed at the same wiki article label.
			lock := labelLockFor(ent.Label)
			lock.Lock()
			defer lock.Unlock()

			logMsg("sync", "pass2 [%d/%d] writing %s", i+1, len(plan.Entities), ent.Label)
			if _, err := runClaude(gctx, root, pass2Prompt, pass2Model, pass2Tools, pass2Timeout); err != nil {
				if errors.Is(err, ErrRateLimit) {
					rateLimitMu.Lock()
					rateLimited = true
					rateLimitMu.Unlock()
					return err
				}
				logMsg("sync", "pass2 failed for entity %q: %v", ent.Label, err)
				// Continue on non-rate-limit errors — partial absorb is better
				// than losing the whole source.
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		if rateLimited {
			return ErrRateLimit
		}
		// Any other error bubbles from the one goroutine that returned non-nil.
		return err
	}
	return nil
}

// absorbPlan mirrors the JSON schema emitted by prompts/absorb-pass1.md.
type absorbPlan struct {
	RawFile     string         `json:"raw_file"`
	SourceTitle string         `json:"source_title"`
	Domain      string         `json:"domain"`
	Entities    []absorbEntity `json:"entities"`
}

type absorbEntity struct {
	Label     string   `json:"label"`
	Type      string   `json:"type"`
	OneLine   string   `json:"one_line"`
	KeyClaims []string `json:"key_claims"`
}

// rawArticleOptsIntoAbsorb returns true if a raw article's frontmatter
// signals that it should be absorbed under high strictness. Opt-in rules:
//   - `absorb: true` (explicit flag)
//   - `domain:` set to a named project domain (not empty, not "general")
//
// Parse errors are treated as "not opted in" so malformed frontmatter does
// not silently sneak past a strict gate. This is called only when
// strictness=high.
func rawArticleOptsIntoAbsorb(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	raw, err := parseFrontmatterRaw(data)
	if err != nil {
		return false
	}
	if v, ok := raw["absorb"].(bool); ok && v {
		return true
	}
	if d, ok := raw["domain"].(string); ok && d != "" && d != "general" {
		return true
	}
	return false
}

// commitAndPush stages wiki files, commits, and pushes. No-op if tree is clean.
func (s *SyncCmd) commitAndPush(root, message string) error {
	if !gitIsDirty(root) {
		return nil
	}
	if debounced, age, window := commitDebounced(root, loadConfig(root)); debounced {
		logMsg("sync", "commit debounced (%s since last commit, window %s) — staged changes roll to next run", age.Round(time.Second), window)
		return nil
	}
	gitAddWiki(root)
	// After staging, verify that the scope we stage (wiki dirs + log.md etc)
	// actually produced staged changes. Otherwise there's nothing for us to
	// commit — the dirty bit was from files outside our scope (cmd/, .claude/,
	// a parallel editor, etc). Treat this as a no-op, not an error.
	if !gitHasStagedChanges(root) {
		return nil
	}
	if err := gitCommit(root, message); err != nil {
		return err
	}
	logMsg("sync", "committed")
	if gitRemoteURL(root) != "" {
		if err := gitPush(root); err != nil {
			logMsg("sync", "push failed (offline?)")
		} else {
			logMsg("sync", "pushed")
		}
	}
	return nil
}

// rebuildAndReindex runs backlinks, index, and qmd reindex.
func (s *SyncCmd) rebuildAndReindex(root string) error {
	scribeExe, _ := os.Executable()
	if scribeExe == "" {
		scribeExe = "scribe"
	}

	out, _ := runCmdErr(root, scribeExe, "backlinks")
	if out != "" {
		logMsg("sync", "%s", lastLine(out))
	}

	out, _ = runCmdErr(root, scribeExe, "index")
	if out != "" {
		logMsg("sync", "%s", lastLine(out))
	}

	logMsg("sync", "index/backlinks rebuilt")

	logMsg("sync", "reindexing qmd...")
	out = runCmd(root, "qmd", "update")
	if out != "" {
		logMsg("sync", "%s", lastLine(out))
	}
	out = runCmd(root, "qmd", "embed")
	if out != "" {
		logMsg("sync", "%s", lastLine(out))
	}
	logMsg("sync", "qmd reindex complete")

	return nil
}

// hasSignificantContent checks if a project directory has enough content to be worth tracking.
func hasSignificantContent(path string) bool {
	for _, name := range []string{"CLAUDE.md", "README.md", "AGENTS.md"} {
		if fileExists(filepath.Join(path, name)) {
			return true
		}
	}

	// Check for any .md files within 2 levels of depth.
	count := 0
	_ = filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable, continue walk
		}
		if info.IsDir() {
			rel, _ := filepath.Rel(path, p)
			if strings.Count(rel, string(filepath.Separator)) >= 2 {
				return filepath.SkipDir
			}
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(info.Name(), ".md") {
			count++
			if count >= 1 {
				return filepath.SkipAll
			}
		}
		return nil
	})
	return count > 0
}

// --- JSON helpers ---

// loadJSONMap reads a JSON file into a string-keyed map.
// Returns an empty map if the file doesn't exist or is invalid.
func loadJSONMap(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return make(map[string]any)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return make(map[string]any)
	}
	return m
}

// saveJSONMap writes a map to a JSON file atomically.
func saveJSONMap(path string, m map[string]any) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// updateJSONFile reads a JSON object file, applies a mutation, and writes it back.
func updateJSONFile(path string, fn func(data map[string]any)) error {
	m := loadJSONMap(path)
	fn(m)
	return saveJSONMap(path, m)
}

// --- String helpers ---

// coalesce returns the first non-empty string, or the fallback.
func coalesce(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// lastLine returns the last non-empty line from a multiline string.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}
