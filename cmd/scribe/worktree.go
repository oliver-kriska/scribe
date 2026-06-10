package main

import (
	"path/filepath"
	"strings"
)

// worktreeMainRoot returns the main checkout's root when path is a
// LINKED git worktree (created via `git worktree add`), and "" when it
// is a main checkout, not a git repo, or unreadable.
//
// Detection: in a linked worktree `git rev-parse --git-dir` resolves to
// <main>/.git/worktrees/<name> while `--git-common-dir` resolves to
// <main>/.git — they differ. In a main checkout both are ".git". The
// main root is the parent of the common dir.
//
// Why discovery cares: every worktree of a repo shares the repo's
// knowledge; enrolling each as its own project means duplicate
// extraction and a noisy manifest (one entry per ticket branch). The
// worktree paths are still recorded on the main project's entry so
// drop files and .claude/research/ written inside a worktree — which
// can differ per branch and are exactly the high-value handoffs — keep
// being collected.
func worktreeMainRoot(path string) string {
	gitDir := runCmd(path, "git", "rev-parse", "--git-dir")
	commonDir := runCmd(path, "git", "rev-parse", "--git-common-dir")
	if gitDir == "" || commonDir == "" || gitDir == commonDir {
		return ""
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(path, commonDir)
	}
	main := filepath.Dir(filepath.Clean(commonDir))
	// Sanity: the common dir must actually be a .git directory and the
	// derived main root must not be the path itself.
	if filepath.Base(commonDir) != ".git" || main == filepath.Clean(path) {
		return ""
	}
	return main
}

// samePath reports whether two paths refer to the same directory,
// tolerating symlink asymmetry: git emits physical paths while session
// decodes are logical (macOS /var vs /private/var).
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// recordWorktree folds a discovered worktree path into the main
// project's manifest entry. Returns true when the entry changed (caller
// saves). Idempotent; never records the main path itself.
func (e *ProjectEntry) recordWorktree(path string) bool {
	if e == nil || path == "" || path == e.Path {
		return false
	}
	for _, w := range e.Worktrees {
		if w == path {
			return false
		}
	}
	e.Worktrees = append(e.Worktrees, path)
	return true
}

// collectionPaths returns every path drop-file and research collection
// should scan for this project: the main checkout plus all recorded
// worktrees. Worktree dirs that no longer exist are filtered by the
// callers' dirExists checks.
func (e *ProjectEntry) collectionPaths() []string {
	if e == nil {
		return nil
	}
	out := make([]string, 0, 1+len(e.Worktrees))
	out = append(out, e.Path)
	out = append(out, e.Worktrees...)
	return out
}

// describeWorktreeFold renders the discovery log line.
func describeWorktreeFold(path, main string) string {
	return strings.TrimSpace(path) + " is a worktree of " + main
}
