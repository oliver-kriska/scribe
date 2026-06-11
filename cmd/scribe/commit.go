package main

import (
	"fmt"
	"strings"
	"time"
)

// CommitCmd auto-commits and pushes pending KB changes.
// Replaces scripts/auto-commit.sh.
type CommitCmd struct{}

// lockNames are the processes that do their own commits.
var lockNames = []string{"sync", "dream", "capture-imessage"}

func (c *CommitCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	return commitRun(root)
}

func commitRun(root string) error {
	cfg := loadConfig(root)

	// Hold every process lock for the duration of the commit — not just
	// probe them. The old probe-and-release left a TOCTOU window where a
	// cron sync could start mid-commit and race the index (the exact
	// scenario that makes the secret gate's unstage fail).
	release, busy, err := holdLocks(cfg.LockDir, lockNames)
	if err != nil {
		return fmt.Errorf("acquire process locks: %w", err)
	}
	if busy != "" {
		logMsg("commit", "blocked by active %s process", busy)
		return nil
	}
	defer release()

	// Fast path: nothing changed (excluding output/). Raw output, not
	// runCmd: porcelain's first column may be a space (` M`), and
	// runCmd's TrimSpace would eat it on the first line.
	statusOut, _ := runCmdRaw(root, "git", "status", "--porcelain", "--", ".", ":!output/")
	if strings.TrimSpace(string(statusOut)) == "" {
		return nil
	}

	// New articles written by a sync whose commit was debounced (or that
	// died before committing) land here — stamp contributor before they
	// get staged, same as the gitAddWiki funnel.
	stampContributor(root)

	// Stage everything except output/
	runCmd(root, "git", "add", "--ignore-errors", "--", ".", ":!output/")

	// Team-mode credential gate — same funnel as gitAddWiki.
	if !holdSecretFiles(root, cfg) {
		logMsg("commit", "skipped: a detected secret could not be held back — resolve and rerun")
		return nil
	}

	// Count what is ACTUALLY staged — after the gate (which may have
	// held files back) and after git add (which may have skipped
	// paths). The message must describe the commit, not the worktree:
	// counting the pre-stage status shipped "(2 wiki)" on a one-file
	// commit whenever the gate held a secret.
	stagedOut, _ := runCmdRaw(root, "git", "diff", "--cached", "--name-only", "-z", "--no-renames")
	var wikiN, rawN, configN, totalN int
	for path := range strings.SplitSeq(string(stagedOut), "\x00") {
		if path == "" {
			continue
		}
		totalN++
		switch {
		case hasAnyPrefix(path, wikiDirs):
			wikiN++
		case strings.HasPrefix(path, "raw/"):
			rawN++
		case strings.HasPrefix(path, "scripts/") ||
			strings.HasPrefix(path, "CLAUDE.md") ||
			strings.HasPrefix(path, "log.md") ||
			strings.HasPrefix(path, ".claude/"):
			configN++
		}
	}
	if totalN == 0 {
		// Everything that changed was held back (or add skipped it) —
		// an empty git commit would error, and there is nothing to say.
		logMsg("commit", "nothing committable — pending changes were held back")
		return nil
	}

	// Build commit message
	var parts []string
	if wikiN > 0 {
		parts = append(parts, fmt.Sprintf("%d wiki", wikiN))
	}
	if rawN > 0 {
		parts = append(parts, fmt.Sprintf("%d raw", rawN))
	}
	if configN > 0 {
		parts = append(parts, fmt.Sprintf("%d config", configN))
	}
	if len(parts) == 0 {
		parts = append(parts, "changes")
	}
	msg := fmt.Sprintf("auto: %s (%s)", time.Now().Format("2006-01-02"), strings.Join(parts, ", "))

	// Commit
	if err := gitCommit(root, msg); err != nil {
		return fmt.Errorf("commit failed: %w", err)
	}

	// Push if remote exists
	if runCmd(root, "git", "remote", "get-url", "origin") != "" {
		if err := gitPush(root); err != nil {
			logMsg("commit", "push failed (offline?)")
		}
	}

	logMsg("commit", "%s", msg)
	return nil
}

// hasAnyPrefix checks if path starts with any of the given directory prefixes.
func hasAnyPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}
