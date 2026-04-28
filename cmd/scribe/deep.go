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

func (d *DeepCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return fmt.Errorf("resolve KB dir: %w", err)
	}

	manifest, err := loadManifest(root)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}

	entry, ok := manifest.Projects[d.Project]
	if !ok {
		return fmt.Errorf("project %q not in manifest — run 'scribe sync --discover' first", d.Project)
	}

	projectPath := entry.Path
	domain := entry.Domain
	extractedDirs := entry.ExtractedDirs

	logMsg("deep", "starting deep extraction for %s at %s", d.Project, projectPath)

	// Build set of already-extracted directories for fast lookup.
	extractedSet := make(map[string]bool)
	if extractedDirs != "" {
		for dir := range strings.SplitSeq(extractedDirs, ",") {
			extractedSet[strings.TrimSpace(dir)] = true
		}
	}

	// Find all knowledge directories: unique parent dirs of .md files.
	knowledgeDirs, err := findKnowledgeDirs(projectPath)
	if err != nil {
		return fmt.Errorf("scan knowledge dirs: %w", err)
	}

	logMsg("deep", "found %d knowledge directories", len(knowledgeDirs))

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
			return fmt.Errorf("load prompt: %w", err)
		}

		tools := []string{
			"Read", "Write", "Edit", "Glob", "Grep",
			"Bash(git log:*)", "Bash(git -C:*)",
			"Bash(ls:*)", "Bash(find:*)", "Bash(wc:*)",
		}

		ctx := context.Background()
		_, err = runClaude(withOpLabel(ctx, "deep-extract"), root, prompt, d.Model, tools, 600*time.Second)
		if err != nil {
			if errors.Is(err, ErrRateLimit) {
				logMsg("deep", "  [%s] rate limited — stopping, will resume next run", relDir)
				break
			}
			logMsg("deep", "  [%s] claude error: %v", relDir, err)
			// Continue with next directory rather than aborting the whole batch.
		}

		newlyExtracted = append(newlyExtracted, relDir)
		batchNum++
		logMsg("deep", "  [%s] done", relDir)
	}

	// Update manifest with extracted dirs.
	if !d.DryRun {
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
	}

	// Reindex and commit.
	if !d.DryRun && batchNum > 0 {
		logMsg("deep", "reindexing qmd...")
		runCmd(root, "qmd", "update")
		runCmd(root, "qmd", "embed")

		if gitIsDirty(root) {
			gitAddWiki(root)
			commitMsg := fmt.Sprintf("deep-extract: %s %s (%d batches)",
				d.Project, time.Now().Format("2006-01-02"), batchNum)
			if err := gitCommit(root, commitMsg); err != nil {
				logMsg("deep", "warning: commit failed: %v", err)
			} else {
				if err := gitPush(root); err != nil {
					logMsg("deep", "warning: push failed: %v", err)
				} else {
					logMsg("deep", "committed and pushed")
				}
			}
		}
	}

	totalDirs := len(extractedSet) + len(newlyExtracted)
	logMsg("deep", "done (%d batches extracted, %d total dirs)", batchNum, totalDirs)

	return nil
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
