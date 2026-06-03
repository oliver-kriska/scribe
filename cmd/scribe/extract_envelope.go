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

// extract_envelope.go is the Phase 4F port of project extraction. The
// legacy path (sync.go:extractProject) runs `claude -p` with full
// tool access; the envelope path inlines the gathered project context
// into a bounded prompt and applies one EnvelopeV2 back through
// applyWikiActions.
//
// Why this exists: extractProject was the last `claude -p` callsite
// fired during a normal `scribe sync` run. Even with `llm.provider:
// ollama` set in scribe.yaml, sync silently kept billing Anthropic
// for every project's extraction step. This file closes that gap.

// runExtractEnvelope is the envelope-mode entry point. Mirrors
// runDeepExtractEnvelope but at project granularity. Returns
// (rateLimited, err).
//
// The caller is sync.go:extractProject — it dispatches here when
// cfg.Extract.Mode == "envelope" (auto-flipped under non-anthropic
// providers).
func runExtractEnvelope(ctx context.Context, root string, cfg *ScribeConfig, _ *Manifest, pname string, entry *ProjectEntry, changed []string) (bool, error) {
	provider := newLLMProvider(cfg.Extract.Provider, cfg.Extract.Model, cfg.Extract.OllamaURL, root)
	promptName := promptForProvider("extract", providerNameFor(provider))

	dropStaging := filepath.Join(root, "output", "drops-"+pname)
	filesContent := gatherExtractFiles(root, entry, changed, dropStaging, cfg.Extract.MaxFileChars, cfg.Extract.MaxTotalChars)

	prompt, err := loadPrompt(promptName, map[string]string{
		"KB_DIR":        root,
		"PROJECT":       pname,
		"P_PATH":        entry.Path,
		"DOMAIN":        entry.Domain,
		"TODAY":         time.Now().UTC().Format("2006-01-02"),
		"FILES_CONTENT": filesContent,
		// Legacy extract-anthropic.md uses these placeholders. Provide
		// empty strings so it still loads when an admin has set
		// extract.mode=envelope + extract.provider=anthropic (an
		// unusual mix, but supported: the envelope path is provider-
		// agnostic).
		"STEP2":            "",
		"FILELIST":         "",
		"DROP_INSTRUCTION": "",
	})
	if err != nil {
		return false, fmt.Errorf("load extract prompt: %w", err)
	}

	timeout := time.Duration(cfg.Extract.TimeoutMin) * time.Minute
	tagged := withOllamaNumCtx(withOpLabel(ctx, "extract"), cfg.Extract.NumCtx)
	callCtx, cancel := context.WithTimeout(tagged, timeout)
	defer cancel()

	logMsg("sync", " [%s] envelope: prompt %d chars, files %d chars, num_ctx=%d", pname, len(prompt), len(filesContent), cfg.Extract.NumCtx)
	out, err := generateMaybeJSON(callCtx, provider, prompt)
	if err != nil {
		if errors.Is(err, ErrRateLimit) {
			return true, err
		}
		return false, fmt.Errorf("extract LLM: %w", err)
	}
	jsonText, ok := extractJSON(out)
	if !ok {
		return false, fmt.Errorf("extract: no JSON envelope in provider output (%d bytes)", len(out))
	}
	env, err := parseEnvelopeV2(jsonText, "extract")
	if err != nil {
		return false, fmt.Errorf("extract: parse envelope: %w", err)
	}
	// Project extraction creates new project entities; it must not overwrite
	// an existing curated doc with a reconstruction.
	res, err := applyWikiActions(root, env, entityWriterApplyOptions())
	if err != nil {
		return false, fmt.Errorf("extract: apply actions: %w", err)
	}
	logMsg("sync", " [%s] envelope: applied %d action(s), %d errors", pname, len(res.Applied), len(res.Errors))
	if runStats == nil {
		runStats = map[string]any{"mode": "envelope-extract", "project": pname}
	}
	if v, ok := runStats["envelope_actions_applied"].(int); ok {
		runStats["envelope_actions_applied"] = v + len(res.Applied)
	} else {
		runStats["envelope_actions_applied"] = len(res.Applied)
	}
	if v, ok := runStats["envelope_actions_errored"].(int); ok {
		runStats["envelope_actions_errored"] = v + len(res.Errors)
	} else {
		runStats["envelope_actions_errored"] = len(res.Errors)
	}
	return false, nil
}

// gatherExtractFiles assembles the FILES_CONTENT block. Reads in
// priority order — drops first (highest-value structured handoffs
// from other projects), then KB-level CLAUDE.md for schema/convention
// grounding, then the project's own CLAUDE.md / README, then the
// changed files, then a small sample of the project's .md docs.
// Stops adding files when maxTotal is exceeded; truncates each file
// at maxFile with a head/tail split (mirrors deep_orchestrator).
func gatherExtractFiles(root string, entry *ProjectEntry, changed []string, dropStaging string, maxFile, maxTotal int) string {
	var sb strings.Builder
	used := 0

	appendFile := func(label, path string) bool {
		if used >= maxTotal {
			return false
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		text := string(data)
		if len(text) > maxFile {
			half := maxFile / 2
			text = text[:half] + "\n…(truncated)…\n" + text[len(text)-half:]
		}
		header := fmt.Sprintf("### %s\n\n", label)
		// Don't exceed the global cap by writing a partial file we then
		// truncate again — easier to skip and log a count of skipped.
		if used+len(header)+len(text)+2 > maxTotal {
			remaining := maxTotal - used - len(header) - 2
			if remaining < 500 {
				return false
			}
			text = text[:remaining] + "\n…(truncated)\n"
		}
		sb.WriteString(header)
		sb.WriteString(text)
		sb.WriteString("\n\n")
		used += len(header) + len(text) + 2
		return true
	}

	// 1. Drop files — structured handoffs from other projects' Claude
	// Code sessions. Highest priority because they carry frontmatter
	// hints (action: create/update/append, target path) that the LLM
	// should honor verbatim.
	if dirExists(dropStaging) {
		drops, _ := filepath.Glob(filepath.Join(dropStaging, "*.md"))
		sort.Strings(drops)
		for _, p := range drops {
			rel, _ := filepath.Rel(root, p)
			appendFile("DROP: "+rel, p)
		}
	}

	// 2. KB-level CLAUDE.md — schema/frontmatter conventions. Tells
	// the LLM what `type:` values exist, what `confidence:` means,
	// rolling-memory layout, etc.
	kbClaude := filepath.Join(root, "CLAUDE.md")
	appendFile("KB CONVENTIONS (CLAUDE.md)", kbClaude)

	// 3. Project's own CLAUDE.md + README — the most concentrated
	// source of project-specific framing.
	for _, name := range []string{"CLAUDE.md", "README.md", "README"} {
		appendFile(name, filepath.Join(entry.Path, name))
	}

	// 4. Changed files since last extraction (caller passes up to 50).
	// These are the "delta to absorb" — what a re-extraction is
	// actually about.
	for _, rel := range changed {
		abs := filepath.Join(entry.Path, rel)
		appendFile("CHANGED: "+rel, abs)
	}

	// 5. Knowledge-dense .md docs as a tail — only if budget remains.
	// Mirrors the legacy extract.md's "Read docs/, decisions/, plans/"
	// instruction but does the walking in Go so the LLM never has to
	// glob filesystem.
	if used < maxTotal {
		for _, dir := range []string{"docs", "decisions", "research", "plans", "analysis", ".claude/research", ".claude/plans", ".claude/analysis", ".claude/solutions", ".claude/lessons"} {
			abs := filepath.Join(entry.Path, dir)
			if !dirExists(abs) {
				continue
			}
			mdFiles, _ := filepath.Glob(filepath.Join(abs, "*.md"))
			sort.Strings(mdFiles)
			for _, p := range mdFiles {
				rel, _ := filepath.Rel(entry.Path, p)
				if !appendFile("DOC: "+rel, p) {
					break
				}
			}
			if used >= maxTotal {
				break
			}
		}
	}

	if used == 0 {
		// Empty body block is legal but the model produces better output
		// when it knows there's *something* — explicit marker.
		return "(no readable files gathered)"
	}
	return sb.String()
}
