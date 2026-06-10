package main

import (
	"fmt"
	"os/exec"
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

	cfg := loadConfig(root)

	// Skip if another scribe process holds a lock. Probe by trying to
	// acquire and immediately release — if acquisition fails the other
	// process still holds the lock.
	for _, name := range lockNames {
		path := lockPathFor(cfg.LockDir, name)
		lf, ok, err := acquireLock(path)
		if err != nil {
			return fmt.Errorf("probe %s: %w", path, err)
		}
		if !ok {
			logMsg("commit", "blocked by active %s process", name)
			return nil
		}
		releaseLock(lf)
	}

	// Check for changes (excluding output/). Raw output, not runCmd:
	// porcelain's first column may be a space (` M`), and runCmd's
	// TrimSpace would eat it on the first line, shifting the path slice.
	statusCmd := exec.Command("git", "status", "--porcelain", "--", ".", ":!output/") //nolint:noctx // quick status probe
	statusCmd.Dir = root
	statusOut, _ := statusCmd.Output()
	changes := string(statusOut)
	if strings.TrimSpace(changes) == "" {
		return nil
	}

	// Count changes by category
	var wikiN, rawN, configN int
	for line := range strings.SplitSeq(changes, "\n") {
		// git status --porcelain: 2 status columns, one space, then the
		// path. Column math, no trimming — see above.
		if len(line) < 4 {
			continue
		}
		path := line[3:]
		// Renames render as "old -> new"; count the destination.
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		path = strings.Trim(path, `"`)
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
