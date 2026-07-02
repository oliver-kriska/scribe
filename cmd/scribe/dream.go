package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type DreamCmd struct {
	DryRun bool   `help:"Show what would happen." name:"dry-run"`
	Model  string `help:"Claude model to use." default:"sonnet"`
	Hot    bool   `help:"Run the daily hot-domain mini consolidation instead of the full weekly cycle." name:"hot"`
	Domain string `help:"Explicit domain override for --hot (skips auto-selection and the churn-threshold gate)." name:"domain"`
}

func (d *DreamCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}

	cfg := loadConfig(root)
	if err := cfg.requireParseable(); err != nil {
		return err
	}

	if d.Hot {
		return runHotDream(root, cfg, d.Domain, d.DryRun)
	}

	today := time.Now().Format("2006-01-02")
	preCount := countArticles(root)

	// In orchestrator mode the resolved provider/model live on
	// cfg.Dream (filled by applyDreamDefaults). The d.Model CLI flag
	// only feeds the legacy monolithic `claude -p` path. Reporting the
	// CLI default ("sonnet") regardless of mode caused the misleading
	// "starting dream cycle (… model: sonnet)" log even on a 100%-
	// Ollama config.
	effectiveModel := d.Model
	if strings.EqualFold(cfg.Dream.Mode, "orchestrator") {
		effectiveModel = fmt.Sprintf("%s/%s", cfg.Dream.Provider, cfg.Dream.Model)
	}
	logMsg("dream", "starting dream cycle (%d articles, model: %s, mode: %s)", preCount, effectiveModel, cfg.Dream.Mode)

	if d.DryRun {
		logMsg("dream", "DRY RUN — would run 4-phase dream cycle on %d articles", preCount)
		logMsg("dream", "estimated duration: 15-45 minutes")
		logMsg("dream", "model: %s", effectiveModel)
		return nil
	}

	// Hold the dream lock for the whole cycle so commit.go and a second
	// scribe invocation can see that dream is in progress.
	lockPath := lockPathFor(cfg.LockDir, "dream", root)
	lf, ok, lerr := acquireLock(lockPath)
	if lerr != nil {
		return fmt.Errorf("lock %s: %w", lockPath, lerr)
	}
	if !ok {
		logMsg("dream", "another dream cycle is running — exiting")
		return nil
	}
	defer releaseLock(lf)

	// Team KBs coordinate the weekly dream through a committed lease so
	// only one machine runs it per window — replaces the manual "run
	// dream on one machine only" rule. Solo KBs skip the round-trip.
	if cfg.Team {
		acquired, holder := acquireDreamLease(root, time.Now())
		if !acquired {
			logMsg("dream", "dream lease held by %s — skipping this cycle", holder)
			return nil
		}
		defer releaseDreamLease(root)
	}

	ctx := context.Background()

	// Phase 4D dispatch: orchestrator mode runs the LLM as a single
	// bounded envelope subtask while Go does the file walking and
	// index work itself. Monolithic mode keeps the historical
	// hour-long `claude -p` path with full tool access.
	if strings.EqualFold(cfg.Dream.Mode, "orchestrator") {
		if err := runDreamOrchestrator(ctx, root, cfg, today); err != nil {
			return fmt.Errorf("dream orchestrator: %w", err)
		}
	} else {
		// Load prompt and run claude -p
		prompt, err := loadPrompt("dream.md", map[string]string{
			"KB_DIR": root,
			"DATE":   today,
		})
		if err != nil {
			return fmt.Errorf("load dream prompt: %w", err)
		}

		tools := []string{
			"Read", "Write", "Edit", "Glob", "Grep",
			"Bash(wc:*)", "Bash(ls:*)", "Bash(find:*)",
		}

		_, err = runClaude(withOpLabel(ctx, "dream"), root, prompt, d.Model, tools, 3600*time.Second)
		if err != nil {
			if errors.Is(err, ErrRateLimit) {
				logMsg("dream", "rate limited — dream cycle interrupted, will retry next week")
				return nil
			}
			return fmt.Errorf("dream claude -p: %w", err)
		}
	}

	return commitDreamCycle(root, today, "dream", preCount)
}

// commitDreamCycle runs the shared post-LLM tail for both the full weekly
// dream and the daily hot pass (dream_hot.go): article-count safety guard,
// git status check, backlinks/index rebuild, secret-gated commit + push,
// qmd reindex, hot-cache rewrite. commitMsgPrefix distinguishes the two in
// the commit message ("dream" vs "dream-hot domain=<d>"); preCount is the
// article count captured before the LLM step so the safety guard can
// compute the delta.
//
// runStats is merged additively here — never reassigned wholesale — so a
// caller that already stamped fields into it before calling (runHotDream's
// mode/hot_domain/hot_domain_touches, or runDreamOrchestrator's
// mode="orchestrator") keeps those fields in the JSONL row. The original
// inline version of this code did `runStats = map[string]any{...}`, a bare
// reassignment that silently discarded whatever the orchestrator had
// already written — see docs/issue-24-hot-domain-consolidation-plan.md §2.5.
func commitDreamCycle(root, today, commitMsgPrefix string, preCount int) error {
	postCount := countArticles(root)
	diff := postCount - preCount
	logMsg("dream", "articles: %d -> %d (%+d)", preCount, postCount, diff)

	if runStats == nil {
		runStats = map[string]any{}
	}
	runStats["articles_before"] = preCount
	runStats["articles_after"] = postCount
	runStats["articles_delta"] = diff

	if diff < -5 {
		logMsg("dream", "WARNING: dream deleted more than 5 articles (%d), review before committing", diff)
		logMsg("dream", "run: git diff --stat")
		return fmt.Errorf("dream deleted too many articles (%d)", diff)
	}

	// Check for changes in wiki dirs
	statusArgs := append([]string{"status", "--porcelain", "--"}, wikiDirs...)
	statusArgs = append(statusArgs, "log.md")
	changes := runCmd(root, "git", statusArgs...)

	if changes != "" {
		changedCount := len(strings.Split(strings.TrimSpace(changes), "\n"))
		runStats["files_changed"] = changedCount
		commitMsg := fmt.Sprintf("%s: %s (%d files)", commitMsgPrefix, today, changedCount)

		// Rebuild index and backlinks BEFORE committing so the index is part
		// of the dream commit. Amending after push rewrites history that's
		// already on origin and the next push fails non-fast-forward.
		//
		// Skipped when SCRIBE_SKIP_REINDEX=1 — tests set this because
		// os.Executable() returns the compiled test binary under `go
		// test`, and running it with "backlinks"/"index" as argv would
		// re-enter the whole test suite instead of the production CLI.
		// Same guard, same rationale, as rebuildIndexAndBacklinks/
		// rebuildAndReindex in sync.go, which document the identical
		// hazard for their own self-invocations.
		if os.Getenv("SCRIBE_SKIP_REINDEX") != "1" {
			scribePath, _ := os.Executable()
			if scribePath == "" {
				scribePath = "scribe"
			}
			scribeBacklinks := exec.Command(scribePath, "backlinks") //nolint:noctx // local scribe self-invocation, fast
			scribeBacklinks.Dir = root
			_ = scribeBacklinks.Run()

			scribeIndex := exec.Command(scribePath, "index") //nolint:noctx // local scribe self-invocation, fast
			scribeIndex.Dir = root
			_ = scribeIndex.Run()
			logMsg("dream", "index/backlinks rebuilt")
		}

		if !gitAddWiki(root) {
			return errors.New("dream commit skipped: a detected secret could not be held back")
		}
		if err := gitCommit(root, commitMsg); err != nil {
			return fmt.Errorf("dream commit: %w", err)
		}
		logMsg("dream", "committed (%d files)", changedCount)

		if err := gitPush(root); err != nil {
			logMsg("dream", "push failed (offline?)")
		} else {
			logMsg("dream", "pushed")
		}

		// Reindex qmd — no git changes, so no push race.
		runCmd(root, "qmd", "update")
		runCmd(root, "qmd", "embed")
		logMsg("dream", "qmd reindexed")
	} else {
		logMsg("dream", "no changes made")
	}

	// Dream reshapes the KB; regenerate the hot cache so it reflects the new state.
	writeHotMDQuiet(root)

	logMsg("dream", "done")
	return nil
}

// countArticles counts .md files in wiki dirs, excluding _-prefixed and .-prefixed files.
func countArticles(root string) int {
	count := 0
	for _, dir := range wikiDirs {
		dirPath := filepath.Join(root, dir)
		if _, err := os.Stat(dirPath); os.IsNotExist(err) {
			continue
		}
		_ = filepath.Walk(dirPath, func(_ string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil //nolint:nilerr // skip unreadable or directory, continue walk
			}
			if !strings.HasSuffix(info.Name(), ".md") {
				return nil
			}
			if strings.HasPrefix(info.Name(), "_") || strings.HasPrefix(info.Name(), ".") {
				return nil
			}
			count++
			return nil
		})
	}
	return count
}
