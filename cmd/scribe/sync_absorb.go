// sync_absorb.go — sync Phase 2.6: absorb raw/articles into wiki pages
// (single-pass for brief sources, entity-first two-pass for dense ones).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/sync/errgroup"
)

// absorbRaw processes unabsorbed articles from raw/articles/.
// Strictness gates auto-absorb: "high" skips raw articles without an
// explicit `absorb: true` frontmatter flag or a named domain (not "general").
// "medium" (default) processes all unabsorbed articles. "low" is identical
// to "medium" at present but reserved for future relaxations.
// Max-per-run, density thresholds, pass models, and timeouts all come from
// scribe.yaml `absorb:` (see absorbDefaults for the baseline).
func (s *SyncCmd) absorbRaw(root string) (int, error) {
	rawDir := filepath.Join(root, "raw", "articles")
	if !dirExists(rawDir) {
		return 0, nil
	}

	cfg := loadConfig(root)
	strictness := cfg.Absorb.Strictness
	maxAbsorb := cfg.Absorb.MaxPerRun
	if s.MaxAbsorb > 0 {
		maxAbsorb = s.MaxAbsorb
	}

	// Load absorb log (Phase 3C: typed, sha-aware).
	absorbLogPath := filepath.Join(root, "wiki", "_absorb_log.json")
	absorbLog, err := loadAbsorbLog(absorbLogPath)
	if err != nil {
		return 0, fmt.Errorf("load absorb log: %w", err)
	}

	// Find unabsorbed articles.
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		return 0, fmt.Errorf("read raw/articles: %w", err)
	}

	absorbed := 0
	heldByStrictness := 0

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		rawFile := filepath.Join(rawDir, entry.Name())

		// Phase 3C: hash content + decide. A sha read error falls back
		// to filename-only behavior (skip if seen, absorb if new) so a
		// transient I/O hiccup can't strand an article.
		sha, _ := sha256File(rawFile)
		refresh := false
		decision := checkAbsorbDecision(absorbLog, entry.Name(), sha)
		switch decision {
		case absorbDecisionSkipSameContent:
			continue
		case absorbDecisionSkipDupContent:
			dup := findDupName(absorbLog, entry.Name(), sha)
			logMsg("sync", "skipping %s (content duplicate of %s); not re-absorbing", entry.Name(), dup)
			// Record so future runs short-circuit on name too. The
			// shared-sha entry stays as a soft pointer to the canonical
			// absorber; we deliberately don't auto-delete the
			// duplicate raw file (no-knowledge-deletion rule).
			absorbLog[entry.Name()] = AbsorbLogEntry{SHA: sha, At: time.Now().UTC().Format(time.RFC3339)}
			if err := saveAbsorbLog(absorbLogPath, absorbLog); err != nil {
				logMsg("sync", "warn: could not persist _absorb_log.json: %v", err)
			}
			continue
		case absorbDecisionRunRefresh:
			// Logged below, after the stub and strictness gates — logging
			// here produced contradictory "re-absorbing X" → "skipping X"
			// pairs whenever a changed file then failed a gate.
			refresh = true
		case absorbDecisionRun:
			// fall through
		}

		if absorbed >= maxAbsorb {
			break
		}

		// Unfetched stubs are zero-signal — skip absorb and route the URL to
		// the parked-links list so the user can handle them manually.
		// `scribe capture --refetch` retries fetching these in a batch.
		if rawArticleIsStub(rawFile) {
			if parkStubLink(root, rawFile) {
				logMsg("sync", "parked unfetched stub %s → wiki/_unfetched-links.md", entry.Name())
			}
			absorbLog[entry.Name()] = AbsorbLogEntry{SHA: sha, At: time.Now().UTC().Format(time.RFC3339)}
			if err := saveAbsorbLog(absorbLogPath, absorbLog); err != nil {
				logMsg("sync", "warn: could not persist _absorb_log.json: %v", err)
			}
			continue
		}

		// Strictness gate: high = explicit opt-in required.
		if strictness == "high" && !rawArticleOptsIntoAbsorb(rawFile) {
			// One summary line after the loop, not one line per file:
			// a held backlog is steady-state under strictness=high and
			// re-listing it (80+ identical lines on scriptorium) buried
			// every real event in the sync log.
			heldByStrictness++
			continue
		}
		if refresh {
			logMsg("sync", "re-absorbing %s (content changed since last absorb)", entry.Name())
		}

		if s.DryRun {
			logMsg("sync", "would absorb raw/articles/%s", entry.Name())
			absorbed++
			continue
		}

		density := readRawDensity(rawFile)
		logMsg("sync", "absorbing raw/articles/%s (density=%s)", entry.Name(), density)

		var absorbErr error
		if density == "dense" {
			absorbErr = s.absorbDenseTwoPass(root, rawFile, entry.Name())
		} else {
			absorbErr = s.absorbSinglePass(root, rawFile)
		}
		if absorbErr != nil {
			if errors.Is(absorbErr, ErrRateLimit) {
				logMsg("sync", "rate limited during absorb — will resume next run")
				break
			}
			if errors.Is(absorbErr, ErrDailyBudgetExhausted) {
				logMsg("sync", "daily anthropic budget ceiling reached during absorb — stopping cleanly (%v)", absorbErr)
				break
			}
			logMsg("sync", "absorb failed for %s: %v", entry.Name(), absorbErr)
			continue
		}

		// Mark as absorbed (with sha so the next run can detect drift).
		absorbLog[entry.Name()] = AbsorbLogEntry{SHA: sha, At: time.Now().UTC().Format(time.RFC3339)}
		if err := saveAbsorbLog(absorbLogPath, absorbLog); err != nil {
			logMsg("sync", "warn: could not persist _absorb_log.json: %v", err)
		}

		absorbed++

		// Checkpoint lint every 5 absorptions.
		if absorbed%5 == 0 {
			logMsg("sync", "absorb checkpoint (%d absorbed, running lint)", absorbed)
			scribeExe, _ := os.Executable()
			if scribeExe == "" {
				scribeExe = "scribe"
			}
			_, _ = runCmdErr(root, scribeExe, "lint", "--changed", "--quiet")
		}
	}

	if heldByStrictness > 0 {
		logMsg("sync", "held %d raw article(s) back (strictness=high, no absorb opt-in — set `absorb: true` or a named domain in their frontmatter to release)", heldByStrictness)
	}
	if absorbed > 0 {
		logMsg("sync", "absorbed %d raw articles", absorbed)
	}
	return absorbed, nil
}

// readRawDensity returns the density label from a raw article's frontmatter,
// or a heuristic classification when the frontmatter field is missing (older
// raw articles written before density was added to buildRawArticle). Returns
// "standard" on any parse error so absorb falls back to single-pass.
func readRawDensity(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "standard"
	}
	if raw, err := parseFrontmatterRaw(data); err == nil {
		if d, ok := raw["density"].(string); ok && d != "" {
			return d
		}
	}
	// Fallback: strip frontmatter and classify body heuristically.
	body := stripFrontmatter(string(data))
	_, density := classifyDensity(body)
	return density
}

// stripFrontmatter returns the body portion of a markdown file, dropping the
// leading `---\n...\n---\n` block if present.
func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---") {
		return s
	}
	end := strings.Index(s[3:], "\n---")
	if end < 0 {
		return s
	}
	rest := s[end+7:] // skip `\n---`
	return strings.TrimLeft(rest, "\n")
}

// absorbSinglePass runs the single-pass absorb. Phase 4A.3 ported it
// off `claude -p` onto llmProviderGenerator + WikiActionEnvelope. The
// raw article body is inlined into the prompt; the model returns one
// envelope describing every wiki file to create/update; Go applies the
// mutations through applyWikiActions. No tools needed → works against
// Ollama out of the box.
func (s *SyncCmd) absorbSinglePass(root, rawFile string) error {
	cfg := loadConfig(root)
	rawBody, err := os.ReadFile(rawFile)
	if err != nil {
		return fmt.Errorf("absorb-single: read raw article: %w", err)
	}
	provider := cfg.Absorb.SinglePassProvider
	model := cfg.Absorb.SinglePassModel
	if model == "" {
		model = s.Model
	}
	promptName := promptForProvider("absorb", provider)
	prompt, err := loadPrompt(promptName, map[string]string{
		"KB_DIR":         root,
		"RAW_FILE":       rawFile,
		"BRIEF_WORDS":    fmt.Sprintf("%d", cfg.Absorb.BriefThresholdWords),
		"BRIEF_HEADINGS": fmt.Sprintf("%d", cfg.Absorb.BriefThresholdHeadings),
		"DENSE_WORDS":    fmt.Sprintf("%d", cfg.Absorb.DenseThresholdWords),
		"DENSE_HEADINGS": fmt.Sprintf("%d", cfg.Absorb.DenseThresholdHeadings),
		"RAW_BODY":       string(rawBody),
		"TODAY":          time.Now().UTC().Format("2006-01-02"),
	})
	if err != nil {
		return fmt.Errorf("load absorb prompt: %w", err)
	}
	ctx := context.Background()
	timeout := time.Duration(cfg.Absorb.SinglePassTimeoutMin) * time.Minute
	gen := newLLMProvider(provider, model, cfg.Absorb.Contextualize.OllamaURL, root)
	tagged := withOllamaNumCtx(withOpLabel(ctx, "absorb-single"), cfg.Absorb.SinglePassNumCtx)
	callCtx, cancel := context.WithTimeout(tagged, timeout)
	defer cancel()
	out, err := generateMaybeJSON(callCtx, gen, prompt)
	if err != nil {
		return fmt.Errorf("absorb-single: %w", err)
	}
	jsonText, ok := extractJSON(out)
	if !ok {
		return fmt.Errorf("absorb-single: no JSON envelope in provider output (%d bytes)", len(out))
	}
	env, err := parseEnvelope(jsonText)
	if err != nil {
		return fmt.Errorf("absorb-single: parse envelope: %w", err)
	}
	// Single-pass runs no facts pass, so any [cNN-fM] the model emits is
	// fabricated — ValidFactIDs nil strips them all. related: normalize
	// + strip both run inside the SanitizeContent seam now.
	res, err := applyWikiActions(root, env, ApplyOptions{
		AllowOverwrite:        true,
		RemapUnknownTopToWiki: true,
		SanitizeContent:       true,
	})
	if err != nil {
		return fmt.Errorf("absorb-single: apply actions: %w", err)
	}
	if len(res.Errors) > 0 {
		logMsg("sync", "absorb-single: %d applied, %d errors: %s", len(res.Applied), len(res.Errors), strings.Join(res.Errors, "; "))
	} else {
		logMsg("sync", "absorb-single: applied %d action(s)", len(res.Applied))
	}
	return nil
}

// absorbDenseTwoPass runs the entity-first two-pass absorb for dense raw
// articles. Pass 1 (Haiku) writes a plan JSON listing the distinct entities.
// Pass 2 (s.Model, typically Sonnet) is called once per entity, sequentially,
// writing one focused wiki page each. Pass 2 invocations do NOT touch
// _index.md or _backlinks.json — those are rebuilt by the sync-level
// rebuildAndReindex call after all absorbs complete.
//
// Sequential Pass 2 avoids concurrent writes to the same wiki page when two
// entities target the same article (rare but possible when Pass 1 proposes
// variant labels). If throughput becomes a problem, guard concurrent writes
// with a per-wiki-path lock and parallelize.
//
//nolint:gocognit // concurrent LLM pipeline with stop-the-world rate-limit semantics and 0% test coverage — decompose only once a stub-provider harness exists
func (s *SyncCmd) absorbDenseTwoPass(root, rawFile, rawName string) error {
	cfg := loadConfig(root)
	plansDir := filepath.Join(root, "output", "absorb-plans")
	if err := os.MkdirAll(plansDir, 0o755); err != nil {
		return fmt.Errorf("mkdir plans: %w", err)
	}
	planFile := filepath.Join(plansDir, strings.TrimSuffix(rawName, ".md")+".json")

	ctx := context.Background()

	// Phase 3A.5 chaptered path: when a TOC sidecar exists with at
	// least cfg.Absorb.ChapterThreshold chapters, fan pass-1 out
	// across chapters in parallel and merge the per-chapter plans.
	// Falls through to the legacy single-shot path on any disqualifier
	// (no sidecar, too few chapters, ChapterAware disabled).
	if chaptered, chunks, _ := shouldAbsorbChaptered(rawFile, cfg.Absorb); chaptered {
		if err := s.runPass1Chaptered(ctx, root, rawFile, rawName, chunks, cfg.Absorb, planFile); err != nil {
			if errors.Is(err, ErrRateLimit) || errors.Is(err, ErrDailyBudgetExhausted) {
				return err
			}
			// Chapter pass had a non-rate-limit failure — fall back
			// to whole-article pass-1 so the article still absorbs.
			logMsg("sync", "chaptered pass1 failed for %s (%v); falling back to whole-article pass1", rawName, err)
			if err := s.runPass1Whole(ctx, root, rawFile, planFile, cfg.Absorb); err != nil {
				return err
			}
		}
	} else {
		if err := s.runPass1Whole(ctx, root, rawFile, planFile, cfg.Absorb); err != nil {
			return err
		}
	}

	// Parse plan JSON.
	planBytes, err := os.ReadFile(planFile)
	if err != nil {
		return fmt.Errorf("read plan: %w", err)
	}
	var plan absorbPlan
	if err := json.Unmarshal(planBytes, &plan); err != nil {
		return fmt.Errorf("parse plan: %w", err)
	}
	if len(plan.Entities) == 0 {
		logMsg("sync", "pass1 produced 0 entities for %s — falling back to single-pass", rawName)
		return s.absorbSinglePass(root, rawFile)
	}
	logMsg("sync", "pass1 planned %d entities for %s", len(plan.Entities), rawName)

	// Pass 2: one wiki page per entity. Runs in parallel with SetLimit to
	// throttle concurrent claude -p invocations (each entity gets its own
	// process). Two entities writing to the same wiki file would race, so
	// a per-target-label mutex serializes that specific pair while letting
	// the others fan out.
	domain := plan.Domain
	if domain == "" {
		domain = "general"
	}
	pass2Model := cfg.Absorb.Pass2Model
	if pass2Model == "" {
		pass2Model = s.Model
	}
	pass2Timeout := time.Duration(cfg.Absorb.Pass2TimeoutMin) * time.Minute
	pass2Tools := []string{"Read", "Write", "Edit", "Glob", "Grep", "Bash(wc:*)"}

	// Phase 4B layer 2: pass2_mode=json runs the JSON-action-envelope
	// path through llmProviderGenerator instead of `claude -p`. The
	// inlined-everything prompt + envelope executor live in
	// prompts/absorb-pass2-json.md and wiki_actions.go respectively.
	pass2JSONMode := strings.EqualFold(cfg.Absorb.Pass2Mode, "json")
	var pass2Provider llmProviderGenerator
	var rawBodyForPrompt, planJSONForPrompt string
	if pass2JSONMode {
		pass2Provider = newLLMProvider(cfg.Absorb.Pass2Provider, pass2Model, cfg.Absorb.Contextualize.OllamaURL, root)
		// Preload the raw article body and plan JSON once; every
		// pass-2 goroutine inlines the same blobs (only the entity
		// fields differ).
		if data, err := os.ReadFile(rawFile); err == nil {
			rawBodyForPrompt = string(data)
		} else {
			return fmt.Errorf("pass2 json: read raw article: %w", err)
		}
		planJSONForPrompt = string(planBytes)
		logMsg("sync", "pass2 mode=json provider=%s model=%s num_ctx=%d", pass2Provider.Name(), pass2Model, cfg.Absorb.Pass2NumCtx)
		// Tag parent ctx so every parallel pass-2 goroutine inherits the
		// num_ctx. Anthropic providers ignore the value; Ollama reads it
		// when building the /api/generate request.
		ctx = withOllamaNumCtx(ctx, cfg.Absorb.Pass2NumCtx)
	}

	parallel := cfg.Absorb.Pass2Parallel
	if parallel <= 0 {
		parallel = 3
	}
	if parallel > len(plan.Entities) {
		parallel = len(plan.Entities)
	}
	logMsg("sync", "pass2: %d entities, parallel=%d", len(plan.Entities), parallel)

	// Phase 3B.5: load merged facts so each pass-2 prompt can be
	// grounded against the chapter's verbatim claim pool. nil =
	// no facts available (facts pass off, file absent, or schema
	// mismatch); pass-2 still works, just without verbatim
	// citations. A read error is logged but non-fatal — better to
	// run un-grounded than abort the absorb.
	mergedFacts, err := loadMergedFacts(root, rawName)
	if err != nil {
		logMsg("sync", "pass2: load facts failed for %s (%v); proceeding un-grounded", rawName, err)
	}

	// Per-target-label lock map so two entities aiming at the same wiki
	// article (rare but possible when Pass 1 proposes variants) don't race.
	var labelLocksMu gosync.Mutex
	labelLocks := map[string]*gosync.Mutex{}
	labelLockFor := func(label string) *gosync.Mutex {
		labelLocksMu.Lock()
		defer labelLocksMu.Unlock()
		if m, ok := labelLocks[label]; ok {
			return m
		}
		m := &gosync.Mutex{}
		labelLocks[label] = m
		return m
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallel)

	var rateLimited bool
	var budgetExhausted bool
	var rateLimitMu gosync.Mutex

	for i, ent := range plan.Entities {
		g.Go(func() error {
			if gctx.Err() != nil {
				return nil // canceled due to rate limit
			}
			keyClaims := strings.Join(ent.KeyClaims, " | ")
			if keyClaims == "" {
				keyClaims = "(none flagged)"
			}
			// Phase 3B.5: pull this entity's chapter slice from
			// the merged facts (if available). nil → empty block,
			// which the prompt template tolerates.
			factsBlock := ""
			if mergedFacts != nil && ent.SourceChapter != nil {
				factsBlock = formatFactsForPrompt(mergedFacts.factsForChapter(*ent.SourceChapter))
			}
			promptName := "absorb-pass2.md"
			vars := map[string]string{
				"KB_DIR":            root,
				"RAW_FILE":          rawFile,
				"PLAN_FILE":         planFile,
				"ENTITY_LABEL":      ent.Label,
				"ENTITY_TYPE":       ent.Type,
				"ENTITY_ONE_LINE":   ent.OneLine,
				"ENTITY_KEY_CLAIMS": keyClaims,
				"DOMAIN":            domain,
				"FACTS":             factsBlock,
			}
			if pass2JSONMode {
				promptName = "absorb-pass2-json.md"
				vars["RAW_BODY"] = rawBodyForPrompt
				vars["PLAN_JSON"] = planJSONForPrompt
				// Many local models lack date awareness and hallucinate
				// `created:` values from training-data era. Inline today's
				// date so the prompt's "use this exact value" instruction
				// has a concrete literal to substitute.
				vars["TODAY"] = time.Now().UTC().Format("2006-01-02")
			}
			pass2Prompt, err := loadPrompt(promptName, vars)
			if err != nil {
				return fmt.Errorf("load pass2 prompt: %w", err)
			}
			// Serialize writes aimed at the same wiki article label.
			lock := labelLockFor(ent.Label)
			lock.Lock()
			defer lock.Unlock()

			logMsg("sync", "pass2 [%d/%d] writing %s", i+1, len(plan.Entities), ent.Label)
			if pass2JSONMode {
				env, err := runPass2JSONOnce(gctx, pass2Provider, pass2Prompt, pass2Timeout)
				if err != nil {
					if errors.Is(err, ErrRateLimit) {
						rateLimitMu.Lock()
						rateLimited = true
						rateLimitMu.Unlock()
						return err
					}
					if errors.Is(err, ErrDailyBudgetExhausted) {
						// Treat the daily-budget ceiling as a stop-the-world
						// signal: same shape as rate-limit, the outer caller
						// commits progress and exits clean. Tracked
						// separately so the aggregator preserves the
						// distinct error for log fidelity.
						rateLimitMu.Lock()
						budgetExhausted = true
						rateLimitMu.Unlock()
						return err
					}
					// One-shot corrective retry. The Phase 4B layer 2 e2e
					// runs showed local models occasionally wrap the
					// envelope in prose or code fences. A second pass
					// with a sharper instruction recovers most of those
					// without burning much extra wallclock. Anthropic
					// rarely needs the retry but pays a small premium
					// for the safety net.
					logMsg("sync", "pass2 entity %q: first attempt failed (%v) — retrying with corrective prompt", ent.Label, err)
					correctivePrompt := pass2Prompt + "\n\n## CORRECTION\n\nYour previous response could not be parsed as a JSON envelope. Output ONLY one JSON object matching WikiActionEnvelope. No prose. No markdown fences. No explanation. The object is the entire response.\n"
					env, err = runPass2JSONOnce(gctx, pass2Provider, correctivePrompt, pass2Timeout)
					if err != nil {
						logMsg("sync", "pass2 entity %q: retry also failed: %v", ent.Label, err)
						return nil
					}
				}
				// Content robustness (fabricated [cNN-fM] strip +
				// related: normalize) now runs inside applyWikiActions via
				// the SanitizeContent seam — see sanitizeEnvelopeContent.
				// The valid set is whatever the facts pass produced for
				// this raw article; nil when facts is off or the file is
				// absent (every bracket strips, matching the prompt's
				// drop-the-bracket fallback).
				var validIDs map[string]bool
				if mergedFacts != nil {
					validIDs = make(map[string]bool, len(mergedFacts.Facts))
					for _, f := range mergedFacts.Facts {
						validIDs[f.ID] = true
					}
				}
				res, err := applyWikiActions(root, env, ApplyOptions{
					AllowOverwrite:        true,
					RemapUnknownTopToWiki: true,
					SanitizeContent:       true,
					ValidFactIDs:          validIDs,
				})
				if err != nil {
					logMsg("sync", "pass2 entity %q: apply actions: %v", ent.Label, err)
					return nil
				}
				if len(res.Errors) > 0 {
					logMsg("sync", "pass2 entity %q: %d applied, %d errors: %s", ent.Label, len(res.Applied), len(res.Errors), strings.Join(res.Errors, "; "))
				} else {
					logMsg("sync", "pass2 entity %q: applied %d action(s)", ent.Label, len(res.Applied))
				}
				return nil
			}
			if _, err := runClaude(withOpLabel(gctx, "absorb-pass2"), root, pass2Prompt, pass2Model, pass2Tools, pass2Timeout); err != nil {
				if errors.Is(err, ErrRateLimit) || errors.Is(err, ErrDailyBudgetExhausted) {
					rateLimitMu.Lock()
					rateLimited = true
					rateLimitMu.Unlock()
					return err
				}
				logMsg("sync", "pass2 failed for entity %q: %v", ent.Label, err)
				// Continue on non-rate-limit errors — partial absorb is better
				// than losing the whole source.
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		if budgetExhausted {
			return ErrDailyBudgetExhausted
		}
		if rateLimited {
			return ErrRateLimit
		}
		// Any other error bubbles from the one goroutine that returned non-nil.
		return err
	}
	return nil
}

// runPass2JSONOnce is one call of the pass-2 envelope path: dispatch to
// the provider (preferring GenerateJSON when supported — see
// generateMaybeJSON), extract the JSON document from whatever wrapping
// the model added, and parse it as a WikiActionEnvelope. Returns the
// envelope or an error that the caller decides whether to retry.
// Stays a free function (not a method) because it has no dependency on
// SyncCmd state — just provider + prompt + timeout.
func runPass2JSONOnce(parent context.Context, provider llmProviderGenerator, prompt string, timeout time.Duration) (WikiActionEnvelope, error) {
	callCtx, cancel := context.WithTimeout(withOpLabel(parent, "absorb-pass2"), timeout)
	defer cancel()
	out, err := generateMaybeJSON(callCtx, provider, prompt)
	if err != nil {
		return WikiActionEnvelope{}, err
	}
	jsonText, ok := extractJSON(out)
	if !ok {
		return WikiActionEnvelope{}, fmt.Errorf("no JSON envelope in provider output (%d bytes)", len(out))
	}
	return parseEnvelope(jsonText)
}

// absorbPlan mirrors the JSON schema emitted by prompts/absorb-pass1.md.
type absorbPlan struct {
	RawFile     string         `json:"raw_file"`
	SourceTitle string         `json:"source_title"`
	Domain      string         `json:"domain"`
	Entities    []absorbEntity `json:"entities"`
}

type absorbEntity struct {
	Label     string   `json:"label"`
	Type      string   `json:"type"`
	OneLine   string   `json:"one_line"`
	KeyClaims []string `json:"key_claims"`
	// SourceChapter records which chapter index in the chaptered
	// pass-1 produced this entity. Used by Phase 3B.5 to slice the
	// merged facts file when injecting verbatim claims into pass-2.
	// Pointer so we can distinguish "chapter 0" from "no chapter
	// info" — the legacy whole-article pass-1 path leaves it nil.
	SourceChapter *int `json:"source_chapter,omitempty"`
}

// rawArticleOptsIntoAbsorb returns true if a raw article's frontmatter
// signals that it should be absorbed under high strictness. Opt-in rules:
//   - `absorb: true` (explicit flag)
//   - `domain:` set to a named project domain (not empty, not "general")
//
// Parse errors are treated as "not opted in" so malformed frontmatter does
// not silently sneak past a strict gate. This is called only when
// strictness=high.
func rawArticleOptsIntoAbsorb(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	raw, err := parseFrontmatterRaw(data)
	if err != nil {
		return false
	}
	if v, ok := raw["absorb"].(bool); ok && v {
		return true
	}
	if d, ok := raw["domain"].(string); ok && d != "" && d != "general" {
		return true
	}
	return false
}
