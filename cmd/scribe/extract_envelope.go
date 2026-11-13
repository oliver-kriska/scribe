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
// runDeepExtractEnvelope but at project granularity.
//
// The caller is sync.go:extractProject — it dispatches here when
// cfg.Extract.Mode == "envelope" (auto-flipped under non-anthropic
// providers).
//
// Returns the staged drop files the model actually received, so the
// caller removes exactly those from staging (drops past the files budget
// stay staged for the next extraction instead of being deleted unseen).
func runExtractEnvelope(ctx context.Context, root string, cfg *ScribeConfig, _ *Manifest, pname string, entry *ProjectEntry, changed []string) ([]string, error) {
	provider := newLLMProvider(cfg.Extract.Provider, cfg.Extract.Model, cfg.Extract.OllamaURL, root)
	// Always the envelope prompt. promptForProvider picks
	// extract-anthropic.md for every non-Ollama provider, and that is the
	// tool-mode prompt for `claude -p`: it tells the model to Read and
	// Glob, and has no {{FILES_CONTENT}}. On a hosted provider the model
	// never saw the project, and every extraction wrote nothing (96 of 96
	// on the reference KB from 2026-10-19 to 2026-11-12).
	const promptName = extractEnvelopePrompt

	dropStaging := dropStagingDir(root, pname)
	filesContent, consumed := gatherExtractFiles(root, entry, changed, dropStaging, cfg.Extract.MaxFileChars, cfg.Extract.MaxTotalChars)

	prompt, err := loadPrompt(promptName, map[string]string{
		"KB_DIR":        root,
		"PROJECT":       pname,
		"P_PATH":        entry.Path,
		"DOMAIN":        entry.Domain,
		"TODAY":         time.Now().UTC().Format("2006-01-02"),
		"FILES_CONTENT": filesContent,
	})
	if err != nil {
		return nil, fmt.Errorf("load extract prompt: %w", err)
	}

	timeout := time.Duration(cfg.Extract.TimeoutMin) * time.Minute
	tagged := withOllamaNumCtx(withOpLabel(ctx, "extract"), cfg.Extract.NumCtx)
	callCtx, cancel := context.WithTimeout(tagged, timeout)
	defer cancel()

	logMsg("sync", " [%s] envelope: prompt %d chars, files %d chars, num_ctx=%d", pname, len(prompt), len(filesContent), cfg.Extract.NumCtx)
	env, err := runMetaEnvelopeWithRetry(callCtx, provider, prompt, "extract", pname)
	if err != nil {
		return nil, err
	}
	fillExtractMetaDefaults(&env, pname, entry.Domain, time.Now().UTC().Format("2006-01-02"))
	// Project extraction creates new project entities; it must not overwrite
	// an existing curated doc with a reconstruction.
	res, err := applyWikiActions(root, env, entityWriterApplyOptions())
	if err != nil {
		return nil, fmt.Errorf("extract: apply actions: %w", err)
	}
	if len(res.Errors) > 0 {
		logMsg("sync", " [%s] envelope: applied %d action(s), %d errors: %v", pname, len(res.Applied), len(res.Errors), res.Errors)
	} else {
		logMsg("sync", " [%s] envelope: applied %d action(s)", pname, len(res.Applied))
	}
	if len(consumed) > 0 && !wroteKnowledge(res) {
		// The model saw these drops and wrote nothing from them. Deleting
		// them (finishDropStaging) would lose a handoff to one empty
		// reply; retrying them every run could loop forever. Park them
		// where a person can look.
		parkUnextractedDrops(root, pname, consumed)
	}
	setRunStatIfAbsent("mode", "envelope-extract")
	setRunStatIfAbsent("project", pname)
	addRunStat("envelope_actions_applied", len(res.Applied))
	addRunStat("envelope_actions_errored", len(res.Errors))
	return consumed, nil
}

// fillExtractMetaDefaults supplies the meta fields a project extraction
// already knows, where the model left them empty. Hosted models drop the
// rolling-memory domain and the log line (the strict schema can only
// require `op`, since each op needs different fields), and the executor
// then rejects the op: 4 of 8 ops failed in one live run, losing two
// learnings from a user's drop. The domain is the project's own, which
// the prompt told the model to use; the log line falls back to the
// content, or to a plain breadcrumb.
func fillExtractMetaDefaults(env *WikiActionEnvelope, pname, domain, today string) {
	if env == nil {
		return
	}
	for i := range env.Meta {
		m := &env.Meta[i]
		switch m.Op {
		case "log_append":
			if strings.TrimSpace(m.Line) == "" {
				m.Line = strings.TrimSpace(firstLine(m.Content))
			}
			if strings.TrimSpace(m.Line) == "" {
				m.Line = fmt.Sprintf("## [%s] extract | %s", today, pname)
			}
		case "rolling_memory_append":
			if strings.TrimSpace(m.Domain) == "" {
				m.Domain = domain
			}
		}
	}
}

// extractEnvelopePrompt is the extract prompt that carries the gathered
// files ({{FILES_CONTENT}}) and asks for an envelope. The "-ollama" name
// is historical: it serves every provider in envelope mode.
const extractEnvelopePrompt = "extract-ollama.md"

// wroteKnowledge reports whether an apply wrote anything beyond the
// bookkeeping log lines: a page, or a rolling-memory paragraph.
func wroteKnowledge(res ApplyResult) bool {
	for _, a := range res.Applied {
		if a != "meta:log_append" && !strings.HasPrefix(a, "meta:sessions_log_append") {
			return true
		}
	}
	return false
}

// parkUnextractedDrops moves drops an extraction saw but wrote nothing
// from into output/drops-unextracted/<project>/ instead of letting
// finishDropStaging delete them.
func parkUnextractedDrops(root, pname string, drops []string) {
	dir := filepath.Join(root, "output", "drops-unextracted", pname)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logPhaseDegraded("sync", "drop parking", " [%s] mkdir %s: %v", pname, dir, err)
		return
	}
	moved := 0
	for _, p := range drops {
		if err := os.Rename(p, stagedDropDest(dir, p)); err != nil {
			logPhaseDegraded("sync", "drop parking", " [%s] park %s: %v", pname, p, err)
			continue
		}
		moved++
	}
	if moved > 0 {
		logMsg("sync", " [%s] extraction wrote nothing from %d drop file(s) — parked in %s for review", pname, moved, dir)
		addRunStat("extract_drops_parked", moved)
	}
}

// errEnvelopeUnparsable marks a reply that came back but is not a usable
// envelope (no JSON, or a bare "{}"): worth one corrective retry, unlike
// a transport, rate-limit or model error.
var errEnvelopeUnparsable = errors.New("reply is not a usable envelope")

// runMetaEnvelopeOnce makes one extract/deep call: strict schema where
// the provider supports it, then parse. label prefixes errors.
func runMetaEnvelopeOnce(ctx context.Context, provider llmProviderGenerator, prompt, label string) (WikiActionEnvelope, error) {
	out, err := generateWithSchema(ctx, provider, prompt, extractEnvelopeSchema())
	if err != nil {
		if errors.Is(err, ErrRateLimit) {
			return WikiActionEnvelope{}, err
		}
		return WikiActionEnvelope{}, fmt.Errorf("%s LLM: %w", label, err)
	}
	jsonText, ok := extractJSON(out)
	if !ok {
		return WikiActionEnvelope{}, fmt.Errorf("%s: %w: no JSON envelope in provider output (%d bytes)", label, errEnvelopeUnparsable, len(out))
	}
	env, err := parseEnvelopeV2(jsonText, label)
	if err != nil {
		return WikiActionEnvelope{}, fmt.Errorf("%s: %w: parse envelope: %w", label, errEnvelopeUnparsable, err)
	}
	return env, nil
}

// runMetaEnvelopeWithRetry adds one corrective retry for an unusable
// reply. A small project or directory with nothing new invites the bare
// "{}" under json_object; strict json_schema closes that on
// schema-capable providers, and the retry covers the rest. Without it
// the item stayed pending and failed every run.
func runMetaEnvelopeWithRetry(ctx context.Context, provider llmProviderGenerator, prompt, label, item string) (WikiActionEnvelope, error) {
	env, err := runMetaEnvelopeOnce(ctx, provider, prompt, label)
	if err == nil || !errors.Is(err, errEnvelopeUnparsable) {
		return env, err
	}
	logMsg("sync", " [%s] envelope: %v — retrying with corrective prompt", item, err)
	addRunStat(label+"_envelope_corrective_retries", 1)
	return runMetaEnvelopeOnce(ctx, provider, prompt+envelopeCorrectiveSuffix, label)
}

// extractEnvelopeSchema constrains extract replies on schema-capable
// providers. Empty actions are legal (nothing new in the project), so
// the guard against the "{}" escape is meta minItems:1: the prompt's
// always-present log_append op.
func extractEnvelopeSchema() jsonSchemaSpec {
	str := map[string]any{"type": "string"}
	return jsonSchemaSpec{
		Name: "WikiActionEnvelope",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"version": map[string]any{"type": "integer"},
				"entity":  str,
				"actions": map[string]any{"type": "array", "items": wikiActionItemSchema()},
				"meta": map[string]any{
					"type":     "array",
					"minItems": 1,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"op": str, "line": str, "domain": str, "target": str, "content": str,
						},
						"required": []any{"op"},
					},
				},
			},
			"required": []any{"version", "entity", "actions", "meta"},
		},
	}
}

// gatherExtractFiles assembles the FILES_CONTENT block. Reads in
// priority order — drops first (highest-value structured handoffs
// from other projects), then KB-level CLAUDE.md for schema/convention
// grounding, then the project's own CLAUDE.md / README, then the
// changed files, then a small sample of the project's .md docs.
// Stops adding files when maxTotal is exceeded; truncates each file
// at maxFile with a head/tail split (mirrors deep_orchestrator).
//
// The second result lists the staged drop files that made it into the
// block. Drops that did not fit are NOT silently dropped — the caller
// leaves them staged for the next extraction.
func gatherExtractFiles(root string, entry *ProjectEntry, changed []string, dropStaging string, maxFile, maxTotal int) (string, []string) {
	var sb strings.Builder
	used := 0

	appendFile := func(label, path string) bool {
		if used >= maxTotal {
			return false
		}
		data, err := readRegularFile(path)
		if err != nil {
			return false
		}
		text := string(data)
		if len(text) > maxFile {
			half := maxFile / 2
			text = truncateBytes(text, half) + "\n…(truncated)…\n" + tailBytes(text, half)
			if strings.HasPrefix(label, "DROP: ") {
				// A drop is the user's own handoff; losing its middle is
				// worth a line in the log, unlike a long README.
				logMsg("sync", " [%s] drop %s exceeds extract.max_file_chars (%d) — middle truncated", entry.Name, filepath.Base(path), maxFile)
			}
		}
		header := fmt.Sprintf("### %s\n\n", label)
		// Don't exceed the global cap by writing a partial file we then
		// truncate again — easier to skip and log a count of skipped.
		if used+len(header)+len(text)+2 > maxTotal {
			remaining := maxTotal - used - len(header) - 2
			if remaining < 500 {
				return false
			}
			text = truncateBytes(text, remaining) + "\n…(truncated)\n"
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
	var consumed []string
	for _, p := range stagedDrops(dropStaging) {
		rel, _ := filepath.Rel(root, p)
		if appendFile("DROP: "+rel, p) {
			consumed = append(consumed, p)
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
	// actually about. gitChangedFiles returns absolute paths from all
	// three of its branches; a relative entry is taken as repo-relative.
	// Issue #138: joining an absolute path onto entry.Path again read
	// <repo>/<repo>/file and silently dropped the whole delta.
	for _, p := range changed {
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(entry.Path, abs)
		}
		rel, err := filepath.Rel(entry.Path, abs)
		if err != nil || strings.HasPrefix(rel, "..") {
			rel = abs
		}
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
		return "(no readable files gathered)", nil
	}
	return sb.String(), consumed
}
