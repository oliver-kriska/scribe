package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	gosync "sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
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
	MaxAbsorb    int    `help:"Override scribe.yaml absorb.max_per_run for this run (0 = use config default)." name:"max-absorb" default:"0"`
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
	if err := cfg.requireParseable(); err != nil {
		return err
	}

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
		// Apply CLI overrides to cfg so the estimate reflects the run
		// the user actually intends, not the scribe.yaml defaults.
		if s.MaxAbsorb > 0 {
			cfg.Absorb.MaxPerRun = s.MaxAbsorb
		}
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

	// First-use config discoverability: append the commented `absorb:`
	// defaults block so the user finds the knobs next time they edit
	// scribe.yaml. No-op when the key already exists or
	// SCRIBE_NO_CONFIG_BACKFILL is set. Only a real sync run rewrites
	// config — never --dry-run, and never a read-only command
	// (loadConfig is pure as of 0.2.21).
	if !s.DryRun {
		maybeBackfillAbsorbBlock(root)
		// TOFU for the team-KB config trust layer — deliberately BEFORE
		// the pull below, so the snapshot records the config the user
		// cloned/edited, and a drifted config arriving in the pull gets
		// flagged instead of silently trusted.
		ensureConfigTrust(root)
	}

	logMsg("sync", "starting")

	pulledRemote, pulledReindexed := s.pullPhase(root, cfg)

	var counters syncCounters

	// Phase 1: Discover projects from ~/.claude/projects/.
	discovered, err := s.discover(root, manifest, cfg)
	if err != nil {
		return err
	}
	counters.discovered = discovered
	logMsg("sync", "discovered %d new projects", discovered)
	if pending := manifest.pendingProjects(); len(pending) > 0 {
		logMsg("sync", "%d project(s) pending approval — run `scribe projects review` (or set sync.auto_approve: true)", len(pending))
	}

	if s.DiscoverOnly {
		logMsg("sync", "discover-only mode, stopping")
		printProjectNames(manifest)
		return nil
	}

	s.collectPhase(root, manifest)
	s.ingestPhase(root)
	s.extractPhase(root, manifest, cfg, &counters)

	// Phase 2.9: Regenerate the team digest so the committed dashboard
	// reflects this run. Team KBs only — solo KBs can run `scribe
	// digest` manually. Deterministic and cheap; failure never blocks.
	if !s.DryRun && cfg.Team {
		writeDigestFile(root, cfg)
	}

	// Phase 3: Reindex + commit. pulledRemote forces a reindex even when
	// this run produced nothing locally — in a shared KB, a teammate's
	// pulled commits would otherwise sit unindexed until the next local
	// extraction happens to fire. When the post-pull reindex already ran
	// and nothing local was produced since, skip the redundant rerun.
	produced := counters.extracted > 0 || counters.sessionsScanned > 0 || counters.absorbed > 0
	if produced || (pulledRemote && !pulledReindexed) {
		if err := s.rebuildAndReindex(root); err != nil {
			logMsg("sync", "reindex error: %v", err)
		}
	}

	if !s.DryRun && gitIsDirty(root) {
		s.commitPhase(root, counters)
	}

	// Refresh the single-file context cache if any project or session
	// produced output worth surfacing. Deterministic, cheap, silent on success.
	if !s.DryRun && produced {
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

// pullPhase (Phase 0) pulls latest from remote so teammates' committed
// pages show up before extraction/absorb. Silent no-op when the KB
// isn't a git repo, has no remote, or when the user disables it via
// sync.always_pull_before_sync. Failures (offline, auth, rebase
// conflict) log and continue — we do not want a flaky network call to
// crash a local sync run.
func (s *SyncCmd) pullPhase(root string, cfg *ScribeConfig) (pulledRemote, pulledReindexed bool) {
	if s.DryRun || !pullBeforeSyncEnabled(cfg) {
		return false, false
	}
	preP := gitSHA(root)
	ok, pulled, pErr := pullRebase(root)
	if pErr != nil {
		logMsg("sync", "pull skipped: %s (continuing)", pErr)
		return false, false
	}
	if !ok || !pulled {
		return false, false
	}
	logMsg("sync", "pulled new commits from remote")
	surfaceSubscribedArrivals(root, cfg, preP)
	// Reindex NOW, not only at the end of the run: the ingestion phases
	// (extraction research-before-create, absorb dedup, session mining)
	// query qmd, and teammates' pulled articles must be searchable when
	// those checks run — or every member re-creates pages the pull just
	// delivered.
	if err := s.rebuildAndReindex(root); err != nil {
		logMsg("sync", "post-pull reindex error: %v", err)
		return true, false
	}
	return true, true
}

// printProjectNames lists manifest projects for --discover, pending
// ones marked.
func printProjectNames(manifest *Manifest) {
	names := make([]string, 0, len(manifest.Projects))
	for name := range manifest.Projects {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !manifest.Projects[name].IsApproved() {
			name += " (pending)"
		}
		fmt.Println(name)
	}
}

// collectPhase (Phases 1.5/1.55) gathers drop files and .claude/research/
// files from tracked projects into the KB.
func (s *SyncCmd) collectPhase(root string, manifest *Manifest) {
	if totalDrops := s.collectDropFiles(root, manifest); totalDrops > 0 {
		logMsg("sync", "%d total drop files to process", totalDrops)
	}
	if s.Research {
		if totalResearch := s.collectResearchFiles(root, manifest); totalResearch > 0 {
			logMsg("sync", "%d research file(s) collected into raw/articles/", totalResearch)
		}
	}
}

// ingestPhase (Phases 1.55b/1.6/1.7) drains the file and URL inboxes
// into raw/articles/ and contextualizes the arrivals. Skipped entirely
// on --dry-run.
func (s *SyncCmd) ingestPhase(root string) {
	if s.DryRun {
		return
	}

	// Phase 1.55b: Drain file inbox (raw/inbox/<file> → raw/articles/<slug>.md
	// + originals moved to raw/inbox/.processed/). Routes through the convert
	// dispatcher (tier 1 marker → tier 0 Go-native fallback). Failed
	// conversions land in raw/inbox/.failed/<slug>/ with err.log so the user
	// can inspect without losing the source file.
	if drained, err := drainFileInbox(root); err != nil {
		logMsg("sync", "file-inbox drain error: %v", err)
	} else if drained > 0 {
		logMsg("sync", "%d file(s) ingested from inbox", drained)
	}

	// Phase 1.6: Drain ingest inbox (queued URLs → raw/articles/).
	if err := drainInbox(root, 0, false); err != nil {
		logMsg("sync", "inbox drain error: %v", err)
	}

	// Phase 1.7: Contextualize newly-ingested raw articles so qmd's embedding
	// index catches them on the upcoming reindex with a retrieval-context
	// paragraph. Idempotent via wiki/_contextualized_log.json. All knobs
	// (enable, model, max per run, timeout) live under `absorb.contextualize`
	// in scribe.yaml — zero-valued fields inherit absorbDefaults().
	cx := loadConfig(root).Absorb.Contextualize
	if cx.Enabled != nil && *cx.Enabled {
		if err := contextualizeRawArticles(root, cx.MaxPerRun, cx.Model, false, false); err != nil {
			logMsg("sync", "contextualize error: %v", err)
		}
	}
}

// extractPhase (Phases 2/2.5/2.55/2.6) runs project extraction, session
// mining (ccrider + Codex), and raw-article absorb, accumulating into
// counters. Each sub-phase's failure logs and continues — one broken
// source must not starve the others.
func (s *SyncCmd) extractPhase(root string, manifest *Manifest, cfg *ScribeConfig, counters *syncCounters) {
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

	// Phase 2.55: Codex session mining (C3). Same triage→envelope→wiki
	// path as ccrider mining, transcript sourced from ~/.codex/
	// rollouts. Part of the session phase, so gated on the same
	// s.Sessions flag; opt-in via `codex: { mine: true }`; a silent
	// no-op without codex_sessions_dir. Respects --dry-run internally.
	if s.Sessions && cfg.Codex.Mine {
		cmined, cerr := s.mineCodexSessions(root, cfg)
		if cerr != nil {
			logMsg("sync", "codex session mining error: %v", cerr)
		}
		counters.sessionsScanned += cmined
	}

	// Phase 2.6: Absorb raw articles.
	absorbed, err := s.absorbRaw(root)
	if err != nil {
		logMsg("sync", "absorb error: %v", err)
	}
	counters.absorbed = absorbed
}

// commitPhase (Phase 3 tail) stages, commits, and pushes the run's
// output, honoring the commit debounce window.
func (s *SyncCmd) commitPhase(root string, counters syncCounters) {
	cfg := loadConfig(root)
	if debounced, age, window := commitDebounced(root, cfg); debounced {
		logMsg("sync", "commit debounced (%s since last commit, window %s) — staged changes roll to next run", age.Round(time.Second), window)
		return
	}
	msg := fmt.Sprintf("sync: auto-extract %s (%d projects)", time.Now().Format("2006-01-02"), counters.extracted)
	if !gitAddWiki(root) {
		logMsg("sync", "commit skipped: a detected secret could not be held back — resolve and rerun")
		return
	}
	// If gitAddWiki staged nothing (dirty files all outside staging scope),
	// skip silently — nothing to commit.
	if !gitHasStagedChanges(root) {
		return
	}
	if err := gitCommit(root, msg); err != nil {
		logMsg("sync", "commit failed: %v", err)
		return
	}
	logMsg("sync", "committed")
	if gitRemoteURL(root) != "" {
		if err := gitPush(root); err != nil {
			logMsg("sync", "push failed: %v", err)
		} else {
			logMsg("sync", "pushed")
		}
	}
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

// commitAndPush stages wiki files, commits, and pushes. No-op if tree
// is clean. Returns committed=false on every no-op path (clean tree,
// debounce, secret hold, nothing staged) — callers that attribute work
// to "the commit that just happened" (recordBatchOutcome) must check
// it, or they stamp this batch onto the PREVIOUS commit's SHA.
func (s *SyncCmd) commitAndPush(root, message string) (bool, error) {
	if !gitIsDirty(root) {
		return false, nil
	}
	if debounced, age, window := commitDebounced(root, loadConfig(root)); debounced {
		logMsg("sync", "commit debounced (%s since last commit, window %s) — staged changes roll to next run", age.Round(time.Second), window)
		return false, nil
	}
	if !gitAddWiki(root) {
		logMsg("sync", "commit skipped: a detected secret could not be held back — resolve and rerun")
		return false, nil
	}
	// After staging, verify that the scope we stage (wiki dirs + log.md etc)
	// actually produced staged changes. Otherwise there's nothing for us to
	// commit — the dirty bit was from files outside our scope (cmd/, .claude/,
	// a parallel editor, etc). Treat this as a no-op, not an error.
	if !gitHasStagedChanges(root) {
		return false, nil
	}
	if err := gitCommit(root, message); err != nil {
		return false, err
	}
	logMsg("sync", "committed")
	if gitRemoteURL(root) != "" {
		if err := gitPush(root); err != nil {
			logMsg("sync", "push failed (offline?)")
		} else {
			logMsg("sync", "pushed")
		}
	}
	return true, nil
}

// rebuildIndexAndBacklinks runs `scribe backlinks` and `scribe index`
// in-process via the running binary. Free-function variant of
// SyncCmd.rebuildAndReindex's wiki-index portion, used by orchestrator
// callers (dream uses os/exec directly; assess + deep use this helper
// so a fresh envelope-mode run leaves _index.md and _backlinks.json
// in sync). Best-effort: any error is logged and discarded.
func rebuildIndexAndBacklinks(root string) {
	scribeExe, _ := os.Executable()
	if scribeExe == "" {
		scribeExe = "scribe"
	}
	if out, _ := runCmdErr(root, scribeExe, "backlinks"); out != "" {
		logMsg("rebuild", "%s", lastLine(out))
	}
	if out, _ := runCmdErr(root, scribeExe, "index"); out != "" {
		logMsg("rebuild", "%s", lastLine(out))
	}
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

	// Section sidecars (Phase 5A). Cheap regex pass over every article;
	// runs alongside the existing index/backlinks rebuild so any wiki
	// edit absorbed in this cycle is reflected in the section index.
	out, _ = runCmdErr(root, scribeExe, "sections", "build")
	if out != "" {
		logMsg("sync", "%s", lastLine(out))
	}

	// Contradiction ledger (Phase 6B). Cheap walk over typed
	// `contradicts:` edges; merges with any prior ledger to preserve
	// `first_observed_at` and resolution state across rebuilds.
	out, _ = runCmdErr(root, scribeExe, "contradictions", "build")
	if out != "" {
		logMsg("sync", "%s", lastLine(out))
	}

	logMsg("sync", "index/backlinks/sections/contradictions rebuilt")

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
