package main

import (
	"path/filepath"
	"strings"
	"sync"
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

// evalSymlinksCache memoizes EvalSymlinks results ("" = unresolvable).
// samePath runs per session × project × worktree in the mining
// pre-filter, so uncached it costs thousands of syscalls per sync.
// Process-lifetime caching is safe: scribe is a short-lived CLI and
// the compared paths are project roots that exist before comparison.
var evalSymlinksCache sync.Map

func evalSymlinksCached(p string) string {
	if v, ok := evalSymlinksCache.Load(p); ok {
		s, _ := v.(string)
		return s
	}
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		r = ""
	}
	evalSymlinksCache.Store(p, r)
	return r
}

// samePath reports whether two paths refer to the same directory,
// tolerating symlink asymmetry: git emits physical paths while session
// decodes are logical (macOS /var vs /private/var).
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ra, rb := evalSymlinksCached(a), evalSymlinksCached(b)
	return ra != "" && rb != "" && ra == rb
}

// recordWorktree folds a discovered worktree path into the main
// project's manifest entry. Returns true when the entry changed (caller
// saves). Idempotent; never records the main path itself. Comparisons are
// symlink-tolerant (canonicalizePath) so the same real worktree spelled
// two different ways (macOS /var vs /private/var) doesn't get recorded
// twice; the raw path is what's stored — collectionPaths only needs a
// valid, dirExists-checked directory, not a canonical one.
func (e *ProjectEntry) recordWorktree(path string) bool {
	if e == nil || path == "" {
		return false
	}
	canon := canonicalizePath(path)
	if canon == canonicalizePath(e.Path) {
		return false
	}
	for _, w := range e.Worktrees {
		if canonicalizePath(w) == canon {
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
