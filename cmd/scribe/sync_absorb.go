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
	"sync/atomic"
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
	// Same contract as the two-pass path: nothing written is a failure, so
	// the caller leaves the source unstamped and retries it next run.
	if len(res.Applied) == 0 {
		return fmt.Errorf("%w: absorb-single wrote nothing (%d action(s) refused)", errAbsorbNothingApplied, len(res.Errors))
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
	// Applied actions across every entity. Only meaningful on the json-mode
	// path — tools mode writes through the model's own tool calls, which the
	// executor never sees, so its count stays 0 and the guard below skips it.
	var appliedTotal atomic.Int64

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
				applied, err := s.runPass2JSONEntity(gctx, run, ent, pass2Prompt)
				appliedTotal.Add(int64(applied))
				if err != nil {
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
	// Zero pages written across every entity is a failed absorb, not a
	// partial one: pass 1 found entities worth writing and pass 2 produced
	// nothing usable for any of them. Returning an error here keeps the
	// caller from stamping _absorb_log.json, so the article stays in the
	// queue for the next run instead of being lost with the source marked
	// done (2026-08-10: 9/9 entities refused as empty pages, article
	// recorded as absorbed, knowledge dropped silently).
	if run.jsonMode && appliedTotal.Load() == 0 {
		return fmt.Errorf("%w: %d entities planned, 0 actions applied", errAbsorbNothingApplied, len(plan.Entities))
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

// errEnvelopeBodyless marks a pass-2 reply that parsed and satisfied the schema
// but whose every action had an empty body — see envelopeAllBodyless. Routed
// through the same corrective-retry loop as a parse failure because the outcome
// is identical: zero pages written.
var errEnvelopeBodyless = errors.New("envelope actions have no body")

// errAbsorbNothingApplied marks an absorb whose pass 2 wrote nothing at all.
// It fails the absorb so the source is not stamped into _absorb_log.json.
var errAbsorbNothingApplied = errors.New("absorb applied no actions")

// correctiveSuffixFor picks the correction to append for a retry. A bodyless
// envelope is syntactically perfect, so the generic "emit valid JSON" nudge is
// noise — it has to be told the prose itself is missing.
func correctiveSuffixFor(err error) string {
	if errors.Is(err, errEnvelopeBodyless) {
		return "\n\n## CORRECTION\n\nYour previous response was valid JSON but every action's \"content\" was empty or contained only YAML frontmatter. A page with no body is discarded. Re-emit the same envelope with each action's \"content\" holding the COMPLETE markdown page: the frontmatter block, then a blank line, then the full article prose (headings, paragraphs, lists) written from the source material. Output ONLY the JSON object.\n"
	}
	return "\n\n## CORRECTION\n\nYour previous response could not be parsed as a JSON envelope. Output ONLY one JSON object matching WikiActionEnvelope, with a non-empty \"actions\" array. No prose. No markdown fences. No explanation. The object is the entire response.\n"
}

// runPass2JSONEntity drives the json-mode pass-2 for one entity: call the
// provider, retry with a corrective prompt on a parse failure or a bodyless
// envelope, then apply the resulting envelope. Returns the number of actions
// applied. ErrRateLimit and ErrDailyBudgetExhausted (stop-the-world) propagate
// unchanged; every other failure is logged and returns (0, nil) so a partial
// absorb beats losing the whole source — the caller's zero-applied guard is
// what stops a total failure from being recorded as success.
func (s *SyncCmd) runPass2JSONEntity(gctx context.Context, run pass2Run, ent absorbEntity, prompt string) (int, error) {
	env, raw, err := runPass2JSONOnce(gctx, run.provider, prompt, run.timeout)
	if err == nil && envelopeAllBodyless(env) {
		err = errEnvelopeBodyless
		dumpEnvelopeFailure(run.root, ent.Label, "bodyless", 0, raw)
	}
	if err != nil {
		if errors.Is(err, ErrRateLimit) || errors.Is(err, ErrDailyBudgetExhausted) {
			return 0, err
		}
		// Corrective retries. The Phase 4B layer 2 e2e runs showed local
		// models occasionally wrap the envelope in prose or code fences; the
		// 2026-07-14 MiniMax M3 sync added a second failure mode — under
		// json_object the model intermittently returns the empty object "{}"
		// ("envelope has no actions"), stochastic at ~20-40%. json_schema
		// mode (schema-capable hosted providers) makes "{}" structurally
		// impossible on the first try, but ollama / a schema-less endpoint
		// reaches pass-2 through the json_object fallback where it can still
		// happen. Each empty/fenced reply is a couple of output tokens, so a
		// second corrective pass is near-free and recovered ~2/3 of the
		// stragglers in the field; two passes cover most of the rest.
		const maxCorrectiveRetries = 2
		for attempt := 1; attempt <= maxCorrectiveRetries; attempt++ {
			// The correction is chosen per failure: a bodyless envelope
			// parsed fine, so repeating "emit valid JSON" teaches the
			// model nothing — it needs to be told the prose is missing.
			correctivePrompt := prompt + correctiveSuffixFor(err)
			logMsg("sync", "pass2 entity %q: attempt %d failed (%v) — corrective retry %d/%d", ent.Label, attempt, err, attempt, maxCorrectiveRetries)
			env, raw, err = runPass2JSONOnce(gctx, run.provider, correctivePrompt, run.timeout)
			if err == nil && envelopeAllBodyless(env) {
				err = errEnvelopeBodyless
				dumpEnvelopeFailure(run.root, ent.Label, "bodyless-retry", attempt, raw)
			}
			if err == nil {
				break
			}
			if errors.Is(err, ErrRateLimit) || errors.Is(err, ErrDailyBudgetExhausted) {
				return 0, err
			}
		}
		if err != nil {
			logMsg("sync", "pass2 entity %q: all %d corrective retries failed: %v", ent.Label, maxCorrectiveRetries, err)
			return 0, nil
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
		return 0, nil
	}
	if len(res.Errors) > 0 {
		logMsg("sync", "pass2 entity %q: %d applied, %d errors: %s", ent.Label, len(res.Applied), len(res.Errors), strings.Join(res.Errors, "; "))
	} else {
		logMsg("sync", "pass2 entity %q: applied %d action(s)", ent.Label, len(res.Applied))
	}
	return len(res.Applied), nil
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
// Returns the provider's raw response alongside the parsed envelope so the
// caller can dump it on a degenerate reply (see dumpEnvelopeFailure) — a
// bodyless envelope is indistinguishable from a good one in the logs, and the
// 2026-08-10 failure could not be reproduced from a benchmark harness (40
// controlled calls, 0 reproductions). Capturing the live payload is the only
// way to see what the provider actually sent.
func runPass2JSONOnce(parent context.Context, provider llmProviderGenerator, prompt string, timeout time.Duration) (WikiActionEnvelope, string, error) {
	callCtx, cancel := context.WithTimeout(withOpLabel(parent, "absorb-pass2"), timeout)
	defer cancel()
	out, err := generateWithSchema(callCtx, provider, prompt, wikiEnvelopeSchema())
	if err != nil {
		return WikiActionEnvelope{}, "", err
	}
	jsonText, ok := extractJSON(out)
	if !ok {
		return WikiActionEnvelope{}, out, fmt.Errorf("no JSON envelope in provider output (%d bytes)", len(out))
	}
	env, perr := parseEnvelope(jsonText)
	return env, out, perr
}

// dumpEnvelopeFailure writes a degenerate pass-2 reply to
// $KB/output/absorb-failures/ for post-mortem. Best-effort and never fatal:
// losing a diagnostic must not fail an absorb. Lands under output/, which is
// gitignored in a KB, so raw provider payloads are never committed.
func dumpEnvelopeFailure(root, label, reason string, attempt int, raw string) {
	dir := filepath.Join(root, "output", "absorb-failures")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	name := fmt.Sprintf("%s-%s-a%d.json", stamp, slugify(label), attempt)
	rec := map[string]any{
		"at": time.Now().UTC().Format(time.RFC3339), "entity": label,
		"reason": reason, "attempt": attempt, "raw_len": len(raw), "raw": raw,
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err == nil {
		logMsg("sync", "captured degenerate pass2 reply → output/absorb-failures/%s", name)
	}
}

// wikiEnvelopeSchema is the json_schema sent with pass-2 requests. Its one
// load-bearing constraint is actions minItems:1 — that closes the empty-"{}"
// escape a schema-capable provider (Together, etc.) would otherwise fall into
// under bare json_object (2026-07-14 MiniMax M3 regression: ~1/6 of pass-2
// entities were dropped as "envelope has no actions"). The op vocabulary and
// extra fields are intentionally permissive: apply-time validation
// (validateActionPath, clampEnvelopeFrontmatter) stays the source of truth for
// correctness; the schema only guarantees "at least one action with an op and
// a path". Providers without json_schema support degrade to json_object then
// plain text (see doRequest), so this is a best-effort tightening, never a
// hard requirement on the provider.
func wikiEnvelopeSchema() jsonSchemaSpec {
	return jsonSchemaSpec{
		Name: "WikiActionEnvelope",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"entity": map[string]any{"type": "string"},
				"notes":  map[string]any{"type": "string"},
				"actions": map[string]any{
					"type":     "array",
					"minItems": 1,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"op":      map[string]any{"type": "string"},
							"path":    map[string]any{"type": "string"},
							"content": map[string]any{"type": "string"},
							"heading": map[string]any{"type": "string"},
						},
						// `content` is required, and that is load-bearing.
						// 2026-08-10: 253 of 253 captured body-less replies
						// omitted the content KEY ENTIRELY — the model emitted
						// {op, path} (exactly the old required set), then used
						// `notes` to describe the page it did not write. The
						// old schema permitted it, so the corrective retries
						// could not help: 107 first attempts, 83 on retry 1, 63
						// on retry 2, all identical. Requiring the key makes a
						// content-free action structurally impossible under
						// strict decoding, the same way minItems:1 killed the
						// empty-"{}" escape.
						//
						// Require the KEY, never a minLength. A tested
						// minLength forces the grammar to keep emitting when
						// the model has nothing left to say: MiniMax M3 ran
						// 62,528 chars / 326s and DeepSeek-V4-Flash 40,992,
						// both unparseable at finish=length. Ops that ignore
						// Content (update_frontmatter) can satisfy this with
						// "" at no cost.
						"required": []any{"op", "path", "content"},
					},
				},
			},
			"required": []any{"entity", "actions"},
		},
	}
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
