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
	"strconv"
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
		if strictnessHoldsFile(strictness, rawFile) {
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
		"BRIEF_WORDS":    strconv.Itoa(cfg.Absorb.BriefThresholdWords),
		"BRIEF_HEADINGS": strconv.Itoa(cfg.Absorb.BriefThresholdHeadings),
		"DENSE_WORDS":    strconv.Itoa(cfg.Absorb.DenseThresholdWords),
		"DENSE_HEADINGS": strconv.Itoa(cfg.Absorb.DenseThresholdHeadings),
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
// absorbDenseTwoPass runs the entity-first two-pass absorb for dense raw
// articles. Issue #9 decomposed the original single function into the helpers
// below — the stop-the-world rate-limit/budget semantics, the per-label write
// serialization, and the parallel fan-out are all pinned by
// sync_absorb_dense_test.go and preserved exactly here.
func (s *SyncCmd) absorbDenseTwoPass(root, rawFile, rawName string) error {
	cfg := loadConfig(root)
	plansDir := filepath.Join(root, "output", "absorb-plans")
	if err := os.MkdirAll(plansDir, 0o755); err != nil {
		return fmt.Errorf("mkdir plans: %w", err)
	}
	planFile := filepath.Join(plansDir, strings.TrimSuffix(rawName, ".md")+".json")

	ctx := context.Background()

	if err := s.runPass1(ctx, root, rawFile, rawName, planFile, cfg.Absorb); err != nil {
		return err
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

	run, ctx, err := s.preparePass2(ctx, root, rawFile, rawName, planFile, planBytes, plan, cfg)
	if err != nil {
		return err
	}

	return s.runPass2(ctx, run, plan)
}

// runPass1 dispatches pass 1 (the entity-planning pass): the chaptered fan-out
// when a TOC sidecar qualifies the article, else the whole-article path. A
// non-rate-limit chaptered failure falls back to the whole-article path so the
// article still absorbs; rate-limit / budget signals propagate unchanged.
func (s *SyncCmd) runPass1(ctx context.Context, root, rawFile, rawName, planFile string, absorbCfg AbsorbConfig) error {
	// Phase 3A.5 chaptered path: when a TOC sidecar exists with at
	// least cfg.Absorb.ChapterThreshold chapters, fan pass-1 out
	// across chapters in parallel and merge the per-chapter plans.
	// Falls through to the legacy single-shot path on any disqualifier
	// (no sidecar, too few chapters, ChapterAware disabled).
	if chaptered, chunks, _ := shouldAbsorbChaptered(rawFile, absorbCfg); chaptered {
		err := s.runPass1Chaptered(ctx, root, rawFile, rawName, chunks, absorbCfg, planFile)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrRateLimit) || errors.Is(err, ErrDailyBudgetExhausted) {
			return err
		}
		// Chapter pass had a non-rate-limit failure — fall back
		// to whole-article pass-1 so the article still absorbs.
		logMsg("sync", "chaptered pass1 failed for %s (%v); falling back to whole-article pass1", rawName, err)
		return s.runPass1Whole(ctx, root, rawFile, planFile, absorbCfg)
	}
	return s.runPass1Whole(ctx, root, rawFile, planFile, absorbCfg)
}

// pass2Run carries the resolved pass-2 configuration shared by every
// per-entity goroutine in runPass2.
type pass2Run struct {
	root        string
	rawFile     string
	planFile    string
	domain      string
	model       string
	timeout     time.Duration
	tools       []string
	parallel    int
	jsonMode    bool
	provider    llmProviderGenerator
	rawBody     string // inlined raw article body (json mode only)
	planJSON    string // inlined plan JSON (json mode only)
	mergedFacts *MergedFacts
}

// preparePass2 resolves the pass-2 settings from config + the parsed plan.
// For the json-mode path it constructs the provider, preloads the raw body
// and plan JSON, and tags the returned context with num_ctx so every parallel
// goroutine inherits it. Returns the (possibly num_ctx-tagged) context.
func (s *SyncCmd) preparePass2(ctx context.Context, root, rawFile, rawName, planFile string, planBytes []byte, plan absorbPlan, cfg *ScribeConfig) (pass2Run, context.Context, error) {
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

	run := pass2Run{
		root:     root,
		rawFile:  rawFile,
		planFile: planFile,
		domain:   domain,
		model:    pass2Model,
		timeout:  time.Duration(cfg.Absorb.Pass2TimeoutMin) * time.Minute,
		tools:    []string{"Read", "Write", "Edit", "Glob", "Grep", "Bash(wc:*)"},
		// Phase 4B layer 2: pass2_mode=json runs the JSON-action-envelope
		// path through llmProviderGenerator instead of `claude -p`. The
		// inlined-everything prompt + envelope executor live in
		// prompts/absorb-pass2-json.md and wiki_actions.go respectively.
		jsonMode: strings.EqualFold(cfg.Absorb.Pass2Mode, "json"),
	}

	if run.jsonMode {
		run.provider = newLLMProvider(cfg.Absorb.Pass2Provider, pass2Model, cfg.Absorb.Contextualize.OllamaURL, root)
		// Preload the raw article body and plan JSON once; every
		// pass-2 goroutine inlines the same blobs (only the entity
		// fields differ).
		data, err := os.ReadFile(rawFile)
		if err != nil {
			return pass2Run{}, ctx, fmt.Errorf("pass2 json: read raw article: %w", err)
		}
		run.rawBody = string(data)
		run.planJSON = string(planBytes)
		logMsg("sync", "pass2 mode=json provider=%s model=%s num_ctx=%d", run.provider.Name(), pass2Model, cfg.Absorb.Pass2NumCtx)
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
	run.parallel = parallel
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
	run.mergedFacts = mergedFacts

	return run, ctx, nil
}

// runPass2 fans out one wiki-writing goroutine per planned entity, capped at
// run.parallel via errgroup SetLimit. A per-label mutex serializes the rare
// case of two entities targeting the same article. Rate-limit and
// daily-budget signals are stop-the-world: the first such signal cancels the
// group and the distinct sentinel surfaces to the caller (budget takes
// precedence over rate-limit when both fired).
func (s *SyncCmd) runPass2(ctx context.Context, run pass2Run, plan absorbPlan) error {
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
	g.SetLimit(run.parallel)

	var rateLimited bool
	var budgetExhausted bool
	var rateLimitMu gosync.Mutex

	for i, ent := range plan.Entities {
		g.Go(func() error {
			if gctx.Err() != nil {
				return nil // canceled due to rate limit
			}
			pass2Prompt, err := s.buildPass2Prompt(run, ent)
			if err != nil {
				return fmt.Errorf("load pass2 prompt: %w", err)
			}
			// Serialize writes aimed at the same wiki article label.
			lock := labelLockFor(ent.Label)
			lock.Lock()
			defer lock.Unlock()

			logMsg("sync", "pass2 [%d/%d] writing %s", i+1, len(plan.Entities), ent.Label)
			if run.jsonMode {
				if err := s.runPass2JSONEntity(gctx, run, ent, pass2Prompt); err != nil {
					rateLimitMu.Lock()
					if errors.Is(err, ErrDailyBudgetExhausted) {
						// Daily-budget ceiling is tracked separately from
						// rate-limit so the aggregator preserves the distinct
						// error for log fidelity; both stop the world.
						budgetExhausted = true
					} else {
						rateLimited = true
					}
					rateLimitMu.Unlock()
					return err
				}
				return nil
			}
			if err := s.runPass2ToolsEntity(gctx, run, ent, pass2Prompt); err != nil {
				rateLimitMu.Lock()
				rateLimited = true
				rateLimitMu.Unlock()
				return err
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

// buildPass2Prompt renders the pass-2 prompt for one entity, grounding it in
// the entity's chapter facts when available and inlining the raw body + plan
// JSON on the json-mode path.
func (s *SyncCmd) buildPass2Prompt(run pass2Run, ent absorbEntity) (string, error) {
	keyClaims := strings.Join(ent.KeyClaims, " | ")
	if keyClaims == "" {
		keyClaims = "(none flagged)"
	}
	// Phase 3B.5: pull this entity's chapter slice from the merged facts
	// (if available). nil → empty block, which the prompt template tolerates.
	factsBlock := ""
	if run.mergedFacts != nil && ent.SourceChapter != nil {
		factsBlock = formatFactsForPrompt(run.mergedFacts.factsForChapter(*ent.SourceChapter))
	}
	promptName := "absorb-pass2.md"
	vars := map[string]string{
		"KB_DIR":            run.root,
		"RAW_FILE":          run.rawFile,
		"PLAN_FILE":         run.planFile,
		"ENTITY_LABEL":      ent.Label,
		"ENTITY_TYPE":       ent.Type,
		"ENTITY_ONE_LINE":   ent.OneLine,
		"ENTITY_KEY_CLAIMS": keyClaims,
		"DOMAIN":            run.domain,
		"FACTS":             factsBlock,
	}
	if run.jsonMode {
		promptName = "absorb-pass2-json.md"
		vars["RAW_BODY"] = run.rawBody
		vars["PLAN_JSON"] = run.planJSON
		// Many local models lack date awareness and hallucinate
		// `created:` values from training-data era. Inline today's
		// date so the prompt's "use this exact value" instruction
		// has a concrete literal to substitute.
		vars["TODAY"] = time.Now().UTC().Format("2006-01-02")
	}
	return loadPrompt(promptName, vars)
}

// runPass2JSONEntity drives the json-mode pass-2 for one entity: call the
// provider, retry once with a corrective prompt on a parse failure, then apply
// the resulting envelope. Returns ErrRateLimit or ErrDailyBudgetExhausted
// (stop-the-world) unchanged; every other failure is logged and returns nil so
// a partial absorb beats losing the whole source.
func (s *SyncCmd) runPass2JSONEntity(gctx context.Context, run pass2Run, ent absorbEntity, prompt string) error {
	env, err := runPass2JSONOnce(gctx, run.provider, prompt, run.timeout)
	if err != nil {
		if errors.Is(err, ErrRateLimit) || errors.Is(err, ErrDailyBudgetExhausted) {
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
		correctivePrompt := prompt + "\n\n## CORRECTION\n\nYour previous response could not be parsed as a JSON envelope. Output ONLY one JSON object matching WikiActionEnvelope. No prose. No markdown fences. No explanation. The object is the entire response.\n"
		env, err = runPass2JSONOnce(gctx, run.provider, correctivePrompt, run.timeout)
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
	if run.mergedFacts != nil {
		validIDs = make(map[string]bool, len(run.mergedFacts.Facts))
		for _, f := range run.mergedFacts.Facts {
			validIDs[f.ID] = true
		}
	}
	res, err := applyWikiActions(run.root, env, ApplyOptions{
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

// runPass2ToolsEntity drives the legacy tools-mode pass-2 for one entity via
// the runClaude seam. Returns the rate-limit / budget sentinel unchanged
// (stop-the-world); other errors are logged and return nil so a partial
// absorb beats losing the whole source.
func (s *SyncCmd) runPass2ToolsEntity(gctx context.Context, run pass2Run, ent absorbEntity, prompt string) error {
	if _, err := runClaude(withOpLabel(gctx, "absorb-pass2"), run.root, prompt, run.model, run.tools, run.timeout); err != nil {
		if errors.Is(err, ErrRateLimit) || errors.Is(err, ErrDailyBudgetExhausted) {
			return err
		}
		logMsg("sync", "pass2 failed for entity %q: %v", ent.Label, err)
		// Continue on non-rate-limit errors — partial absorb is better
		// than losing the whole source.
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

// UnmarshalJSON accepts either the full object form or a bare string.
// Local models (ollama) sometimes emit an entities array as bare names
// ("Charlie McCollum") instead of objects. Standard decoding rejects the
// whole plan on the first such element ("cannot unmarshal string into
// ... absorbEntity"), so an otherwise-good chapter is dropped wholesale
// and its entities are lost. Coerce a bare string into a label-only
// entity instead — the label is real (it's the entity the chapter is
// about), and downstream facts injection + pass-2 can still enrich it.
// Anthropic's tool-use schema enforces the object shape for free, so this
// only ever fires on the non-anthropic absorb path.
func (e *absorbEntity) UnmarshalJSON(data []byte) error {
	if strings.HasPrefix(strings.TrimSpace(string(data)), "\"") {
		var name string
		if err := json.Unmarshal(data, &name); err != nil {
			return err
		}
		e.Label = strings.TrimSpace(name)
		return nil
	}
	// alias breaks the method set so this doesn't recurse.
	type alias absorbEntity
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*e = absorbEntity(a)
	return nil
}

// strictnessHoldsFile reports whether the absorb policy holds a file back
// instead of processing it: strictness=high with no explicit opt-in in the
// file's frontmatter. Single source of truth for the hold rule — sync's
// absorb loop and `scribe status` both call this, so the sync-time "held N
// back" summary and the status held count can't drift apart.
func strictnessHoldsFile(strictness, path string) bool {
	return strictness == "high" && !rawArticleOptsIntoAbsorb(path)
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
