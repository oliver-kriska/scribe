package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DeepCmd performs batch-by-directory deep extraction for a single project.
type DeepCmd struct {
	Project  string `arg:"" help:"Project name to deep-extract."`
	BatchMax int    `help:"Max directories per run." name:"batch-max" default:"5"`
	Model    string `help:"Claude model to use." default:"sonnet"`
	DryRun   bool   `help:"Show what would happen." name:"dry-run"`
}

// excludedDirNames lists directories to skip when scanning for knowledge files.
var excludedDirNames = map[string]bool{
	"_build":       true,
	"deps":         true,
	"node_modules": true,
	".git":         true,
	".elixir_ls":   true,
	"evals":        true,
}

// Run drives deep, batch-by-directory extraction for one project. Issue #9
// decomposed the original single function — its observable semantics
// (manifest checkpointing, rate-limit stop, per-dir error tolerance, dry-run,
// already-extracted skip, batch cap) are pinned by deep_run_test.go, and the
// helpers below preserve them exactly.
func (d *DeepCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return fmt.Errorf("resolve KB dir: %w", err)
	}
	if err := loadConfig(root).requireParseable(); err != nil {
		return err
	}

	manifest, err := loadManifest(root)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}

	entry, err := manifest.resolve(d.Project)
	if err != nil {
		return fmt.Errorf("project %q not in manifest — run 'scribe sync --discover' first", d.Project)
	}
	// d.Project may be a CLI-typed Name OR a full path (manifest.resolve
	// accepts both). Every downstream use is a DISPLAY use (prompt vars,
	// log lines, commit message) and must stay the short display label.
	d.Project = entry.Name

	projectPath := entry.Path
	domain := entry.Domain

	logMsg("deep", "starting deep extraction for %s at %s", d.Project, projectPath)

	// Build set of already-extracted directories for fast lookup.
	extractedSet := parseExtractedDirs(entry.ExtractedDirs)

	// Find all knowledge directories: unique parent dirs of .md files.
	knowledgeDirs, err := findKnowledgeDirs(projectPath)
	if err != nil {
		return fmt.Errorf("scan knowledge dirs: %w", err)
	}

	logMsg("deep", "found %d knowledge directories", len(knowledgeDirs))

	batchNum, newlyExtracted, err := d.extractBatch(root, projectPath, domain, knowledgeDirs, extractedSet)
	if err != nil {
		return err
	}

	if !d.DryRun {
		d.checkpointManifest(root, manifest, entry, projectPath, extractedSet, newlyExtracted)
	}

	if !d.DryRun && batchNum > 0 {
		d.reindexAndCommit(root, batchNum)
	}

	totalDirs := len(extractedSet) + len(newlyExtracted)
	logMsg("deep", "done (%d batches extracted, %d total dirs)", batchNum, totalDirs)

	return nil
}

// parseExtractedDirs splits a comma-joined extracted_dirs string into a set.
func parseExtractedDirs(extractedDirs string) map[string]bool {
	extractedSet := make(map[string]bool)
	if extractedDirs != "" {
		for dir := range strings.SplitSeq(extractedDirs, ",") {
			extractedSet[strings.TrimSpace(dir)] = true
		}
	}
	return extractedSet
}

// extractBatch walks the knowledge dirs in order, extracting up to BatchMax
// not-yet-extracted directories. It returns the batches taken and the newly
// extracted relative dirs. A rate-limit signal stops the loop immediately
// (the rate-limited dir is not recorded, so it retries next run); a fatal
// setup error (e.g. an unloadable prompt) aborts and propagates. Any other
// per-dir error is logged inside extractDir and the dir is still marked
// extracted so one broken dir can't wedge the loop forever.
func (d *DeepCmd) extractBatch(root, projectPath, domain string, knowledgeDirs []string, extractedSet map[string]bool) (int, []string, error) {
	batchNum := 0
	var newlyExtracted []string

	for _, dir := range knowledgeDirs {
		relDir, err := filepath.Rel(projectPath, dir)
		if err != nil {
			relDir = dir
		}

		if extractedSet[relDir] {
			logMsg("deep", "  [%s] already extracted, skipping", relDir)
			continue
		}

		if batchNum >= d.BatchMax {
			logMsg("deep", "  max %d batches reached, will continue next run", d.BatchMax)
			break
		}

		// Find .md files in this directory (maxdepth 1, limit 20).
		mdFiles := findMDFilesInDir(dir, 20)
		if len(mdFiles) == 0 {
			continue
		}

		logMsg("deep", "  [%s] extracting (%d files)", relDir, len(mdFiles))

		if d.DryRun {
			for _, f := range mdFiles {
				fmt.Println(f)
			}
			batchNum++
			newlyExtracted = append(newlyExtracted, relDir)
			continue
		}

		stop, err := d.extractDir(root, projectPath, relDir, domain, mdFiles)
		if err != nil {
			return batchNum, newlyExtracted, err
		}
		if stop {
			break
		}

		newlyExtracted = append(newlyExtracted, relDir)
		batchNum++
		logMsg("deep", "  [%s] done", relDir)
	}

	return batchNum, newlyExtracted, nil
}

// extractDir runs one directory's extraction through whichever protocol the
// KB config selects: envelope mode (the provider seam) or legacy tools mode
// (the runClaude seam). It returns stop=true when the call was rate-limited,
// signaling the batch loop to halt and resume next run without recording this
// directory. A non-nil error is fatal and aborts the whole run (only the
// embedded-prompt load failure, which never happens at runtime). Other
// per-dir errors are logged and return (false, nil) so the loop records the
// dir and moves on.
func (d *DeepCmd) extractDir(root, projectPath, relDir, domain string, mdFiles []string) (bool, error) {
	ctx := context.Background()

	// Phase 4E: envelope mode pulls per-directory files into the
	// prompt and asks for one envelope; tools mode keeps the
	// legacy `claude -p` path. The cfg dispatcher matches the
	// pattern from assess + dream.
	cfg := loadConfig(root)
	if cfg != nil && strings.EqualFold(cfg.DeepIngest.Mode, "envelope") {
		rl, err := runDeepExtractEnvelope(ctx, root, cfg, d.Project, projectPath, relDir, domain, mdFiles)
		if rl {
			logMsg("deep", "  [%s] rate limited — stopping, will resume next run", relDir)
			return true, nil
		}
		if err != nil {
			logMsg("deep", "  [%s] envelope error: %v", relDir, err)
		}
		return false, nil
	}

	fileList := strings.Join(mdFiles, ",")
	prompt, err := loadPrompt("deep-extract.md", map[string]string{
		"KB_DIR":    root,
		"REL_DIR":   relDir,
		"PROJECT":   d.Project,
		"P_PATH":    projectPath,
		"DOMAIN":    domain,
		"FILE_LIST": fileList,
	})
	if err != nil {
		return false, fmt.Errorf("load prompt: %w", err)
	}

	tools := []string{
		"Read", "Write", "Edit", "Glob", "Grep",
		"Bash(git log:*)", "Bash(git -C:*)",
		"Bash(ls:*)", "Bash(find:*)", "Bash(wc:*)",
	}

	if _, err := runClaude(withOpLabel(ctx, "deep-extract"), root, prompt, d.Model, tools, 600*time.Second); err != nil {
		if errors.Is(err, ErrRateLimit) {
			logMsg("deep", "  [%s] rate limited — stopping, will resume next run", relDir)
			return true, nil
		}
		logMsg("deep", "  [%s] claude error: %v", relDir, err)
		// Continue with next directory rather than aborting the whole batch.
	}
	return false, nil
}

// checkpointManifest folds the newly-extracted dirs into the project entry,
// stamps the extraction time + SHA, saves the manifest, and records the
// revision in the shared extraction ledger so teammates skip it too.
func (d *DeepCmd) checkpointManifest(root string, manifest *Manifest, entry *ProjectEntry, projectPath string, extractedSet map[string]bool, newlyExtracted []string) {
	// Rebuild the full extracted_dirs string.
	allDirs := make([]string, 0, len(extractedSet)+len(newlyExtracted))
	for dir := range extractedSet {
		allDirs = append(allDirs, dir)
	}
	allDirs = append(allDirs, newlyExtracted...)
	sort.Strings(allDirs)

	entry.ExtractedDirs = strings.Join(allDirs, ",")

	timestamp := time.Now().UTC().Format(time.RFC3339)
	entry.LastExtracted = timestamp

	if hasGit(projectPath) {
		entry.LastSHA = gitSHA(projectPath)
	} else {
		entry.LastSHA = "no-git"
	}

	if err := manifest.save(); err != nil {
		logMsg("deep", "warning: failed to save manifest: %v", err)
	}

	// Deep extraction covers regular extraction — record it in the
	// shared ledger so teammates skip this revision too.
	if entry.LastSHA != "no-git" && entry.LastSHA != "" {
		if key := repoLedgerKey(projectPath); key != "" {
			ledger := loadLedger(root)
			ledger.record(key, entry.LastSHA, resolveContributor(root))
			if err := ledger.save(); err != nil {
				logMsg("deep", "warning: extraction ledger save failed: %v", err)
			}
		}
	}
}

// reindexAndCommit rebuilds the wiki index, re-embeds via qmd, and commits +
// pushes the batch. Envelope-mode deep writes wiki articles via
// applyWikiActions without touching _index.md / _backlinks.json (the legacy
// tools-mode path edited those inside `claude -p`), so the rebuild has to
// happen here.
func (d *DeepCmd) reindexAndCommit(root string, batchNum int) {
	rebuildIndexAndBacklinks(root)
	logMsg("deep", "reindexing qmd...")
	runCmd(root, "qmd", "update")
	runCmd(root, "qmd", "embed")

	if !gitIsDirty(root) {
		return
	}
	if !gitAddWiki(root) {
		logMsg("deep", "commit skipped: a detected secret could not be held back — resolve and rerun")
		return
	}
	commitMsg := fmt.Sprintf("deep-extract: %s %s (%d batches)",
		d.Project, time.Now().Format("2006-01-02"), batchNum)
	if err := gitCommit(root, commitMsg); err != nil {
		logMsg("deep", "warning: commit failed: %v", err)
		return
	}
	if err := gitPush(root); err != nil {
		logMsg("deep", "warning: push failed: %v", err)
		return
	}
	logMsg("deep", "committed and pushed")
}

// findKnowledgeDirs walks the project path for .md files and returns
// the sorted unique parent directories, excluding build artifacts.
func findKnowledgeDirs(projectPath string) ([]string, error) {
	dirSet := make(map[string]bool)

	err := filepath.Walk(projectPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable, continue walk
		}
		if info.IsDir() {
			if excludedDirNames[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(info.Name(), ".md") {
			dirSet[filepath.Dir(path)] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	dirs := make([]string, 0, len(dirSet))
	for dir := range dirSet {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs, nil
}

// findMDFilesInDir returns up to limit .md files in a single directory (no recursion).
func findMDFilesInDir(dir string, limit int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".md") {
			files = append(files, filepath.Join(dir, e.Name()))
			if len(files) >= limit {
				break
			}
		}
	}
	return files
}
