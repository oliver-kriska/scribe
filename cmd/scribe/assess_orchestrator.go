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

// assess_orchestrator.go is the Phase 4E port of `scribe assess`. The
// legacy path spawns 5 parallel `claude -p` tracks (structure,
// features, docs, decisions, gaps) plus a 6th consolidation call —
// each with broad tool access. The orchestrator path inlines the
// project's orientation packet (top-level docs, source tree summary,
// git log) and asks for ONE EnvelopeV2 covering all five buckets in a
// single overview article.
//
// Trade-off: one call gives the model less working context per
// section but eliminates tool-use entirely, which is what makes the
// local-Ollama path work. Anthropic users can still pick the legacy
// path via `assess.mode: tools` in scribe.yaml.

// runAssessOrchestrator is the Phase 4E entry point. Called from
// AssessCmd.Run when cfg.Assess.Mode == "envelope".
func runAssessOrchestrator(ctx context.Context, root string, cfg *ScribeConfig, project string, entry *ProjectEntry, today string) error {
	orientation := assessOrientationPacket(entry)
	docs := assessReadTopDocs(entry.Path, 12000)
	tree := assessSourceTree(entry.Path, 200)
	gitLog := assessGitLog(entry.Path, 30)

	provider := newLLMProvider(cfg.Assess.Provider, cfg.Assess.Model, cfg.Assess.OllamaURL, root)
	promptName := promptForProvider("assess", providerNameFor(provider))
	prompt, err := loadPrompt(promptName, map[string]string{
		"KB_DIR":        root,
		"PROJECT":       project,
		"PROJECT_LOWER": strings.ToLower(project),
		"P_PATH":        entry.Path,
		"DOMAIN":        entry.Domain,
		"TODAY":         today,
		"ORIENTATION":   orientation,
		"DOCS":          docs,
		"TREE":          tree,
		"GITLOG":        gitLog,
	})
	if err != nil {
		return fmt.Errorf("load assess prompt: %w", err)
	}
	// Generous timeout — the model has a lot of context to digest.
	timeout := 20 * time.Minute
	tagged := withOllamaNumCtx(withOpLabel(ctx, "assess"), cfg.Assess.NumCtx)
	callCtx, cancel := context.WithTimeout(tagged, timeout)
	defer cancel()
	out, err := generateMaybeJSON(callCtx, provider, prompt)
	if err != nil {
		if errors.Is(err, ErrRateLimit) {
			return err
		}
		return fmt.Errorf("assess LLM: %w", err)
	}
	jsonText, ok := extractJSON(out)
	if !ok {
		return fmt.Errorf("assess: no JSON envelope in provider output (%d bytes)", len(out))
	}
	env, err := parseEnvelopeV2(jsonText, "assess")
	if err != nil {
		return fmt.Errorf("assess: parse envelope: %w", err)
	}
	res, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, SanitizeContent: true})
	if err != nil {
		return fmt.Errorf("assess: apply actions: %w", err)
	}
	if len(res.Errors) > 0 {
		logMsg("assess", "envelope: %d applied, %d errors: %v", len(res.Applied), len(res.Errors), res.Errors)
	} else {
		logMsg("assess", "envelope: applied %d action(s)", len(res.Applied))
	}
	if runStats == nil {
		runStats = map[string]any{}
	}
	runStats["mode"] = "envelope"
	runStats["project"] = project
	runStats["envelope_actions_applied"] = len(res.Applied)
	runStats["envelope_actions_errored"] = len(res.Errors)
	return nil
}

// assessOrientationPacket returns a short text block summarizing the
// project entry. Cheap context for the model so it doesn't have to
// re-derive metadata from the source tree.
func assessOrientationPacket(entry *ProjectEntry) string {
	return fmt.Sprintf("Path: %s\nDomain: %s\nLast SHA: %s\nLast extracted: %s", entry.Path, entry.Domain, entry.LastSHA, entry.LastExtracted)
}

// assessReadTopDocs returns concatenated content of common top-level
// docs (README.md, CLAUDE.md, AGENTS.md, ARCHITECTURE.md, ...) capped
// at maxChars. The orchestrator inlines this so the model can lead
// with the project's own self-description.
func assessReadTopDocs(projectPath string, maxChars int) string {
	candidates := []string{"README.md", "CLAUDE.md", "AGENTS.md", "ARCHITECTURE.md", "docs/README.md", "docs/architecture.md"}
	var sb strings.Builder
	for _, c := range candidates {
		p := filepath.Join(projectPath, c)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		fmt.Fprintf(&sb, "### %s\n\n%s\n\n", c, string(data))
		if sb.Len() > maxChars {
			break
		}
	}
	out := sb.String()
	if len(out) > maxChars {
		out = out[:maxChars] + "\n…(truncated)\n"
	}
	return out
}

// errAssessWalkDone is the sentinel returned from the assessSourceTree
// walk callback when we've gathered enough entries. filepath.SkipDir
// from a file callback is a no-op (it only applies to directory
// callbacks), so the cap at maxEntries*3 needs a real error to actually
// short-circuit the walk on large projects.
var errAssessWalkDone = errors.New("assess source tree: budget reached")

// assessSourceTree walks the project path and returns a flat
// `dir/file` listing. Excludes build artifacts and big binaries.
// Caps at `maxEntries`. Stops walking once 3× cap is reached so
// gigantic monorepos don't pay a full filesystem walk for a 200-entry
// sample.
func assessSourceTree(projectPath string, maxEntries int) string {
	var entries []string
	walkErr := filepath.Walk(projectPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil //nolint:nilerr
		}
		if info.IsDir() {
			if excludedDirNames[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(projectPath, path)
		entries = append(entries, rel)
		if len(entries) >= maxEntries*3 {
			return errAssessWalkDone
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errAssessWalkDone) {
		logMsg("assess", "  walk %s: %v (returning %d entries gathered so far)", projectPath, walkErr, len(entries))
	}
	sort.Strings(entries)
	if len(entries) > maxEntries {
		entries = entries[:maxEntries]
	}
	return strings.Join(entries, "\n")
}

// assessGitLog returns the last `n` commit lines using git log. Empty
// when the project is not a git repo or git is unavailable.
func assessGitLog(projectPath string, n int) string {
	if !hasGit(projectPath) {
		return "(not a git repo)"
	}
	out := runCmd(projectPath, "git", "log", "--oneline", "-n", fmt.Sprintf("%d", n))
	return out
}
