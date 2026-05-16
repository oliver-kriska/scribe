package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// deep_orchestrator.go is the Phase 4E port of `scribe deep`. The
// legacy path runs one `claude -p` per directory with broad tool
// access; the orchestrator path inlines per-directory file contents
// into a bounded prompt and asks for one EnvelopeV2 per directory.

// runDeepExtractEnvelope is the per-directory envelope subtask. The
// caller (DeepCmd.Run) iterates over directories and calls this for
// each. Returns (rateLimited, err).
func runDeepExtractEnvelope(ctx context.Context, root string, cfg *ScribeConfig, project, projectPath, relDir, domain string, mdFiles []string) (bool, error) {
	provider := newLLMProvider(cfg.DeepIngest.Provider, cfg.DeepIngest.Model, cfg.DeepIngest.OllamaURL, root)
	promptName := promptForProvider("deep-extract", providerNameFor(provider))
	filesContent := deepReadFilesForPrompt(mdFiles, 24000)
	prompt, err := loadPrompt(promptName, map[string]string{
		"KB_DIR":        root,
		"PROJECT":       project,
		"P_PATH":        projectPath,
		"REL_DIR":       relDir,
		"DOMAIN":        domain,
		"TODAY":         time.Now().UTC().Format("2006-01-02"),
		"FILES_CONTENT": filesContent,
	})
	if err != nil {
		return false, fmt.Errorf("load deep-extract prompt: %w", err)
	}
	timeout := 10 * time.Minute
	tagged := withOllamaNumCtx(withOpLabel(ctx, "deep-extract"), cfg.DeepIngest.NumCtx)
	callCtx, cancel := context.WithTimeout(tagged, timeout)
	defer cancel()
	out, err := generateMaybeJSON(callCtx, provider, prompt)
	if err != nil {
		if errors.Is(err, ErrRateLimit) {
			return true, err
		}
		return false, fmt.Errorf("deep LLM: %w", err)
	}
	jsonText, ok := extractJSON(out)
	if !ok {
		return false, fmt.Errorf("deep: no JSON envelope in provider output (%d bytes)", len(out))
	}
	env, err := parseEnvelopeV2(jsonText, "deep")
	if err != nil {
		return false, fmt.Errorf("deep: parse envelope: %w", err)
	}
	res, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, SanitizeContent: true})
	if err != nil {
		return false, fmt.Errorf("deep: apply actions: %w", err)
	}
	logMsg("deep", "  [%s] envelope: applied %d action(s), %d errors", relDir, len(res.Applied), len(res.Errors))
	// Per-directory metrics accumulate into runStats as the caller
	// iterates. We track totals not per-dir so the JSONL row stays
	// flat.
	if runStats == nil {
		runStats = map[string]any{"mode": "envelope", "project": project}
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

// deepReadFilesForPrompt concatenates a set of .md files into one
// flat block, capped at maxChars. Each file gets a header showing its
// relative path so the model can attribute claims.
//
// Per-file truncation lets a directory with one huge file still
// surface its neighbors. The head/tail trim mirrors the transcript
// renderer: 800 chars head + 800 chars tail keeps the framing claims
// without burning the whole budget on one giant essay.
func deepReadFilesForPrompt(paths []string, maxChars int) string {
	perFileCap := maxChars / len(paths)
	if perFileCap < 1200 {
		perFileCap = 1200
	}
	var sb strings.Builder
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		text := string(data)
		if len(text) > perFileCap {
			half := perFileCap / 2
			text = text[:half] + "\n…(truncated)…\n" + text[len(text)-half:]
		}
		fmt.Fprintf(&sb, "### %s\n\n%s\n\n", filepath.Base(p), text)
		if sb.Len() >= maxChars {
			break
		}
	}
	out := sb.String()
	if len(out) > maxChars {
		out = out[:maxChars] + "\n…(truncated)\n"
	}
	return out
}
