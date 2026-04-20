package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// gitSHA returns the current HEAD SHA for a git repo.
func gitSHA(repoPath string) string {
	return runCmd(repoPath, "git", "rev-parse", "HEAD")
}

// gitBranch returns the current branch name.
func gitBranch(repoPath string) string {
	return runCmd(repoPath, "git", "branch", "--show-current")
}

// gitRemoteURL returns the origin remote URL, or empty string.
func gitRemoteURL(repoPath string) string {
	return runCmd(repoPath, "git", "remote", "get-url", "origin")
}

// gitChangedFiles returns files changed between two SHAs (or all files if oldSHA is empty).
func gitChangedFiles(repoPath, oldSHA string, patterns []string) []string {
	if oldSHA == "" {
		// Never synced: list all matching files
		return findFiles(repoPath, patterns)
	}

	args := make([]string, 0, 7+len(patterns))
	args = append(args, "-C", repoPath, "diff", "--name-only", oldSHA, "HEAD", "--")
	args = append(args, patterns...)
	out := runCmd("", "git", args...)
	if out == "" {
		return nil
	}
	var files []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, filepath.Join(repoPath, line))
		}
	}
	return files
}

// gitIsDirty returns true if the working tree has changes.
func gitIsDirty(repoPath string) bool {
	out := runCmd(repoPath, "git", "status", "--porcelain")
	return out != ""
}

// pullRebase runs `git pull --rebase --autostash` in repoPath. Returns
// (ok, pulled_anything, err). Non-fatal by design for sync's preflight:
// if the KB isn't a git repo, has no remote, or the pull fails (offline,
// auth, conflict), we log and let the caller continue. "pulled_anything"
// is a cheap HEAD-changed check so callers can decide whether a reindex
// is warranted.
func pullRebase(repoPath string) (ok bool, pulled bool, err error) {
	if !hasGit(repoPath) {
		return false, false, nil
	}
	if gitRemoteURL(repoPath) == "" {
		return false, false, nil
	}
	beforeSHA := gitSHA(repoPath)
	cmd := exec.Command("git", "pull", "--rebase", "--autostash") //nolint:noctx // git subprocess, network-bound
	cmd.Dir = repoPath
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = runErr.Error()
		}
		return false, false, fmt.Errorf("%s", firstLine(msg))
	}
	afterSHA := gitSHA(repoPath)
	return true, beforeSHA != "" && afterSHA != "" && beforeSHA != afterSHA, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// commitDebounced returns true when the configured commit debounce window
// has not yet elapsed since the last commit on HEAD. Callers should skip
// committing (and pushing) in that case; the staged/unstaged changes stay
// in the working tree for the next run to pick up and roll into a single
// larger commit. Zero or negative CommitDebounceMinutes disables the
// check entirely (existing behavior).
func commitDebounced(repoPath string, cfg *ScribeConfig) (bool, time.Duration, time.Duration) {
	minutes := 0
	if cfg != nil {
		minutes = cfg.Sync.CommitDebounceMinutes
	}
	if minutes <= 0 {
		return false, 0, 0
	}
	window := time.Duration(minutes) * time.Minute
	age := gitLastCommitAge(repoPath)
	return age < window, age, window
}

// gitLastCommitAge returns the time elapsed since the last commit on HEAD.
// Returns a very large duration if there is no HEAD yet or git can't be
// queried — so callers comparing with a debounce window treat "unknown"
// as "old enough to commit now".
func gitLastCommitAge(repoPath string) time.Duration {
	out := runCmd(repoPath, "git", "log", "-1", "--format=%ct")
	if out == "" {
		return 365 * 24 * time.Hour
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 365 * 24 * time.Hour
	}
	return time.Since(time.Unix(secs, 0))
}

// gitHasStagedChanges returns true if there are staged changes ready to commit.
// Uses `git diff --cached --quiet`: exit 0 = no changes, exit 1 = changes.
func gitHasStagedChanges(repoPath string) bool {
	cmd := exec.Command("git", "diff", "--cached", "--quiet") //nolint:noctx // quick status probe
	cmd.Dir = repoPath
	err := cmd.Run()
	// Exit 1 means there ARE staged changes; any other error means git itself failed.
	exitErr := &exec.ExitError{}
	return errors.As(err, &exitErr)
}

// gitAddWiki stages wiki content directories.
func gitAddWiki(root string) {
	args := make([]string, 0, 1+len(wikiDirs)+2)
	args = append(args, "add")
	args = append(args, wikiDirs...)
	args = append(args, "scripts/projects.json", "log.md")
	cmd := exec.Command("git", args...) //nolint:noctx // git add subprocess
	cmd.Dir = root
	_ = cmd.Run()
}

// gitCommit creates a commit with the given message. Captures stderr so callers
// see the real reason (pre-commit hook failure, signing issue, nothing staged).
func gitCommit(root, message string) error {
	cmd := exec.Command("git", "commit", "-m", message, "--no-gpg-sign") //nolint:noctx // git commit subprocess
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("git commit: %w", err)
		}
		// Collapse multi-line stderr to first non-empty line to keep sync log tidy.
		first := msg
		if idx := strings.Index(msg, "\n"); idx > 0 {
			first = strings.TrimSpace(msg[:idx])
		}
		return fmt.Errorf("git commit: %s", first)
	}
	return nil
}

// gitDiffShortstat returns (files_changed, lines_added, lines_deleted) for a
// commit range — e.g. gitDiffShortstat(root, "HEAD~1", "HEAD"). Returns zeros
// on any git error (never fails the caller — this is used for instrumentation).
func gitDiffShortstat(repoPath, from, to string) (files, added, deleted int) {
	cmd := exec.Command("git", "diff", "--shortstat", from, to) //nolint:noctx // git diff subprocess
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, 0
	}
	// Example: " 3 files changed, 45 insertions(+), 2 deletions(-)"
	line := strings.TrimSpace(string(out))
	if line == "" {
		return 0, 0, 0
	}
	for part := range strings.SplitSeq(line, ",") {
		part = strings.TrimSpace(part)
		var n int
		if _, err := fmt.Sscanf(part, "%d ", &n); err != nil {
			continue
		}
		switch {
		case strings.Contains(part, "file"):
			files = n
		case strings.Contains(part, "insertion"):
			added = n
		case strings.Contains(part, "deletion"):
			deleted = n
		}
	}
	return files, added, deleted
}

// gitPush pushes to origin. On a non-fast-forward failure (remote advanced
// while we were committing locally) we try a `pull --rebase` and push again
// once — that handles the common case where an earlier scribe run pushed
// while another was still extracting. A force-push is never attempted: KB
// history is append-only and losing remote commits would be worse than a
// failed push we can reconcile manually.
func gitPush(root string) error {
	err := runGitPush(root)
	if err == nil {
		return nil
	}
	// Detect non-fast-forward without parsing git's localized stderr by
	// re-running the push capturing output and checking for known tokens.
	out, _ := exec.Command("git", "-C", root, "push").CombinedOutput() //nolint:noctx // git push subprocess
	if !isNonFastForward(string(out)) {
		return err
	}
	logMsg("git", "push rejected (non-fast-forward); attempting `git pull --rebase`")
	pullCmd := exec.Command("git", "-C", root, "pull", "--rebase", "--autostash") //nolint:noctx // git pull subprocess
	pullCmd.Stdout = os.Stdout
	pullCmd.Stderr = os.Stderr
	if rerr := pullCmd.Run(); rerr != nil {
		logMsg("git", "pull --rebase failed: %v — resolve manually then push", rerr)
		return err
	}
	return runGitPush(root)
}

func runGitPush(root string) error {
	cmd := exec.Command("git", "push") //nolint:noctx // git push subprocess
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// isNonFastForward recognizes the two strings git emits for a rejected push
// that needs a rebase/merge first. Keeping the detection string-level means
// we don't need to parse exit codes (exec.ExitError doesn't distinguish
// non-fast-forward from, say, auth failure).
func isNonFastForward(stderr string) bool {
	return strings.Contains(stderr, "non-fast-forward") ||
		strings.Contains(stderr, "rejected") && strings.Contains(stderr, "fetch first") ||
		strings.Contains(stderr, "Updates were rejected because the tip of your current branch is behind")
}

// findFiles finds files matching patterns in a directory, excluding build artifacts.
func findFiles(root string, patterns []string) []string {
	excludeDirs := map[string]bool{
		"_build": true, "deps": true, "node_modules": true,
		".git": true, ".elixir_ls": true, "temp": true,
	}

	var files []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable, continue walk
		}
		if info.IsDir() {
			if excludeDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		for _, pattern := range patterns {
			if matched, _ := filepath.Match(pattern, info.Name()); matched {
				files = append(files, path)
				break
			}
		}
		return nil
	})
	return files
}

// hasGit returns true if path contains a .git directory.
func hasGit(path string) bool {
	return dirExists(filepath.Join(path, ".git"))
}
