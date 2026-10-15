package main

import (
	"path/filepath"
	"slices"
	"strings"
)

// claudeToolingDir is Claude Code's per-repo config directory.
const claudeToolingDir = ".claude"

// claudeToolingSubdirs are the .claude/ subdirectories that hold Claude Code
// tooling — agent, command, rule, skill, hook, plugin and output-style
// definitions — rather than project knowledge. Extraction skips them.
//
// .claude/ as a whole is NOT excluded: its other subdirectories (research/,
// plans/, analysis/, solutions/, lessons/, …) are knowledge sources the
// extract and assess prompts read on purpose, and .claude/<kb>/ holds drop
// files. .claude/worktrees/ is not listed here: walks already skip it, along
// with every other nested checkout, through nestedCheckout (#131).
var claudeToolingSubdirs = map[string]bool{
	"agents":        true,
	"commands":      true,
	"rules":         true,
	"skills":        true,
	"hooks":         true,
	"plugins":       true,
	"output-styles": true,
}

// claudeToolingPromptList renders claudeToolingSubdirs for the extract
// prompts, so the model's own Glob/Read skips the same directories the Go
// file selection does.
func claudeToolingPromptList() string {
	dirs := make([]string, 0, len(claudeToolingSubdirs))
	for name := range claudeToolingSubdirs {
		dirs = append(dirs, claudeToolingDir+"/"+name+"/")
	}
	slices.Sort(dirs)
	return strings.Join(dirs, ", ")
}

// isClaudeToolingRelPath reports whether a repo-relative path lies under
// .claude/<tooling subdir>/.
func isClaudeToolingRelPath(rel string) bool {
	segments := strings.Split(filepath.ToSlash(rel), "/")
	for i := 0; i+1 < len(segments); i++ {
		if segments[i] == claudeToolingDir && claudeToolingSubdirs[segments[i+1]] {
			return true
		}
	}
	return false
}

// excludeClaudeTooling drops files under a .claude/ tooling subdirectory of
// repoPath. Only the part of each path relative to repoPath is checked:
// Claude Code puts worktrees under <repo>/.claude/worktrees/, so matching
// the absolute path would drop every file of a project checked out there.
func excludeClaudeTooling(repoPath string, files []string) []string {
	if len(files) == 0 {
		return files
	}
	kept := make([]string, 0, len(files))
	for _, file := range files {
		// A Rel error keeps the file: over-extracting tooling is the safer
		// failure than dropping knowledge. Unreachable in practice, since
		// every caller builds its paths from repoPath.
		if rel, err := filepath.Rel(repoPath, file); err == nil && isClaudeToolingRelPath(rel) {
			continue
		}
		kept = append(kept, file)
	}
	return kept
}

// isClaudeToolingDir reports whether a directory visited by a walk below
// walkRoot is a .claude/ tooling subdirectory. The root itself is never
// skipped, so walking a checkout that lives inside one still works.
func isClaudeToolingDir(walkRoot, dir string) bool {
	if dir == walkRoot {
		return false
	}
	return filepath.Base(filepath.Dir(dir)) == claudeToolingDir && claudeToolingSubdirs[filepath.Base(dir)]
}
