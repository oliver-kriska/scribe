package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// AssessCmd is a one-shot parallel deep assessment of a single project.
//
// It differs from `scribe deep` in two ways:
//
//  1. Scope: `deep` walks the project directory-by-directory across many
//     sync cycles, each run handling a small batch. `assess` takes one
//     project and produces a full structured overview in a single
//     invocation.
//  2. Execution: `assess` spawns 5 parallel `claude -p` subagents (structure,
//     features, docs, decisions, gaps), each with a narrow prompt and a
//     dedicated output file. A 6th consolidation call reads the track
//     outputs and produces the final `projects/{name}/overview.md`.
//
// Why this shape: the Codex-wiki fork takes the parallel-subagent approach
// for its one-shot project ingestion because it keeps each subagent's
// context small and lets the model avoid the "tried to do everything, did
// nothing well" failure mode. The assess command brings that approach to
// scribe so a user can run `scribe assess <project>` once and have a
// populated KB entry in 5-10 minutes.
//
// This is orthogonal to cron-driven sync. Cron handles ongoing updates as
// projects change; assess handles the first-time deep read.
type AssessCmd struct {
	Project  string `arg:"" help:"Project name to assess (must exist in projects.json)."`
	Model    string `help:"Claude model to use for all 6 calls." default:"sonnet"`
	Parallel int    `help:"Max concurrent track subagents (1-5)." default:"5"`
	Timeout  int    `help:"Per-track timeout in seconds." default:"600"`
	DryRun   bool   `help:"Print the plan without invoking claude." name:"dry-run"`
	Keep     bool   `help:"Keep per-track output files after consolidation (otherwise removed)."`
}

type assessTrack struct {
	name   string // short label: structure, features, docs, decisions, gaps
	prompt string // template filename
}

var assessTracks = []assessTrack{
	{name: "structure", prompt: "assess-structure.md"},
	{name: "features", prompt: "assess-features.md"},
	{name: "docs", prompt: "assess-docs.md"},
	{name: "decisions", prompt: "assess-decisions.md"},
	{name: "gaps", prompt: "assess-gaps.md"},
}

func (a *AssessCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}

	manifest, err := loadManifest(root)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	entry, ok := manifest.Projects[a.Project]
	if !ok {
		return fmt.Errorf("project %q not in manifest — run 'scribe sync --discover' first", a.Project)
	}
	if _, err := os.Stat(entry.Path); err != nil {
		return fmt.Errorf("project path %s not accessible: %w", entry.Path, err)
	}

	today := time.Now().Format("2006-01-02")
	outDir := filepath.Join(root, "output", "assess", fmt.Sprintf("%s-%s", a.Project, today))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", outDir, err)
	}

	parallel := min(max(a.Parallel, 1), 5)

	logMsg("assess", "project=%s path=%s domain=%s parallel=%d model=%s",
		a.Project, entry.Path, entry.Domain, parallel, a.Model)
	logMsg("assess", "track outputs -> %s", outDir)

	// Plan: each track gets a dedicated output file. Consolidation reads
	// them all by path. If assess runs again same-day, the dated outDir
	// disambiguates from the previous attempt.
	trackOuts := make(map[string]string, len(assessTracks))
	for _, t := range assessTracks {
		trackOuts[t.name] = filepath.Join(outDir, t.name+".md")
	}

	if a.DryRun {
		fmt.Println("# DRY RUN — would spawn:")
		for _, t := range assessTracks {
			fmt.Printf("  track %-10s -> %s\n", t.name, trackOuts[t.name])
		}
		fmt.Printf("  consolidate -> projects/%s/overview.md\n", strings.ToLower(a.Project))
		return nil
	}

	// Phase 1: 5 parallel track subagents.
	// errgroup with SetLimit gives bounded concurrency. If any track hits
	// a rate limit, the context cancels and remaining tracks bail early.
	tracksStart := time.Now()
	g, ctx := errgroup.WithContext(context.Background())
	g.SetLimit(parallel)

	var failed atomic.Int64
	for _, t := range assessTracks {
		outPath := trackOuts[t.name]
		g.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			logMsg("assess", "[%s] starting", t.name)
			err := a.runTrack(ctx, root, t, entry, outPath)
			if err != nil {
				if errors.Is(err, ErrRateLimit) {
					logMsg("assess", "[%s] rate limited", t.name)
					return err // cancel siblings
				}
				logMsg("assess", "[%s] failed: %v", t.name, err)
				failed.Add(1)
				return nil // don't cancel siblings on non-rate-limit errors
			}
			logMsg("assess", "[%s] done (%s)", t.name, humanFileSize(outPath))
			return nil
		})
	}
	if err := g.Wait(); err != nil && errors.Is(err, ErrRateLimit) {
		return fmt.Errorf("assess aborted: %w", err)
	}
	logMsg("assess", "tracks complete in %s (%d failed)", time.Since(tracksStart).Round(time.Second), failed.Load())

	// Require at least 3 of 5 tracks to have produced output before
	// consolidating. Fewer than that and the overview will be too sparse
	// to be useful.
	have := 0
	for _, path := range trackOuts {
		if fi, err := os.Stat(path); err == nil && fi.Size() > 100 {
			have++
		}
	}
	if have < 3 {
		return fmt.Errorf("only %d/5 tracks produced usable output — aborting consolidation", have)
	}

	// Phase 2: consolidation call.
	logMsg("assess", "consolidating (%d/5 tracks available)", have)
	consStart := time.Now()
	if err := a.runConsolidate(root, entry, trackOuts, today); err != nil {
		return fmt.Errorf("consolidate: %w", err)
	}
	logMsg("assess", "consolidation done in %s", time.Since(consStart).Round(time.Second))

	// Best-effort reindex. Non-fatal.
	runCmd(root, "qmd", "update")
	runCmd(root, "qmd", "embed")

	if !a.Keep {
		if err := os.RemoveAll(outDir); err != nil {
			logMsg("assess", "warn: failed to clean %s: %v", outDir, err)
		}
	}

	writeHotMDQuiet(root)
	logMsg("assess", "done — overview at projects/%s/overview.md", strings.ToLower(a.Project))
	return nil
}

// runTrack invokes claude -p with the track's focused prompt. Each track
// has write access to its own output file plus read-only access to the
// project.
func (a *AssessCmd) runTrack(ctx context.Context, root string, t assessTrack, entry *ProjectEntry, outPath string) error {
	prompt, err := loadPrompt(t.prompt, map[string]string{
		"PROJECT": a.Project,
		"P_PATH":  entry.Path,
		"DOMAIN":  entry.Domain,
		"OUT":     outPath,
	})
	if err != nil {
		return fmt.Errorf("load prompt: %w", err)
	}

	// Track tools: broad Read/Glob/Grep in the project, Write only for
	// the track's dedicated output file (enforced by prompt, not by
	// claude flags — the prompt names the exact path).
	tools := []string{
		"Read", "Write", "Glob", "Grep",
		"Bash(git log:*)", "Bash(git -C:*)", "Bash(git grep:*)",
		"Bash(ls:*)", "Bash(find:*)", "Bash(wc:*)",
	}
	timeout := time.Duration(a.Timeout) * time.Second
	_, err = runClaude(ctx, root, prompt, a.Model, tools, timeout)
	return err
}

// runConsolidate is the 6th call: reads all track outputs and writes the
// final project overview + any derivative articles.
func (a *AssessCmd) runConsolidate(root string, entry *ProjectEntry, trackOuts map[string]string, today string) error {
	prompt, err := loadPrompt("assess-consolidate.md", map[string]string{
		"PROJECT":       a.Project,
		"PROJECT_LOWER": strings.ToLower(a.Project),
		"P_PATH":        entry.Path,
		"DOMAIN":        entry.Domain,
		"KB_DIR":        root,
		"TODAY":         today,
		"STRUCTURE_OUT": trackOuts["structure"],
		"FEATURES_OUT":  trackOuts["features"],
		"DOCS_OUT":      trackOuts["docs"],
		"DECISIONS_OUT": trackOuts["decisions"],
		"GAPS_OUT":      trackOuts["gaps"],
	})
	if err != nil {
		return fmt.Errorf("load consolidate prompt: %w", err)
	}

	tools := []string{
		"Read", "Write", "Edit", "Glob", "Grep",
		"Bash(wc:*)", "Bash(ls:*)",
	}
	// Consolidation gets a longer timeout because it writes multiple
	// files (overview + possibly decisions + rolling entries + reindex).
	timeout := time.Duration(a.Timeout*2) * time.Second
	_, err = runClaude(context.Background(), root, prompt, a.Model, tools, timeout)
	return err
}

// humanFileSize returns a rough size string like "3.4k" for logging.
// Returns "missing" if the file does not exist.
func humanFileSize(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	n := fi.Size()
	switch {
	case n < 1024:
		return fmt.Sprintf("%db", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fk", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/(1024*1024))
	}
}
