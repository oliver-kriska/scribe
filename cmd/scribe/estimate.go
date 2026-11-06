package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// tokenEstimate is a rough approximation expressed in tokens. Prices drift
// too fast to be useful; token counts are stable and what engineers reason
// about. ~4 chars ≈ 1 token for English prose is the industry heuristic.
type tokenEstimate struct {
	InputTokens  int
	OutputTokens int
	Model        string // "haiku", "sonnet", "ollama:<model>", or "local"
	Note         string // free-form "3 sources × contextualize" etc.
}

// humanTokens formats a token count like "1.2K" or "34K" for readability.
func humanTokens(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	if n < 1_000_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
}

// estimateSync walks what a non-dry-run sync would do given the current KB
// state + config, and returns per-phase token estimates. Values come from
// actually reading the queue contents (inbox sizes, unabsorbed raw files,
// uncontextualized raw files) — not fabricated.
//
// Output tokens are rough per-class medians, not per-source fits:
//
//	contextualize:  input ≈ clipped raw body (≤20KB) → ~5K tok; output ≈ 100 tok paragraph.
//	absorb-single:  input ≈ full raw body + wiki context ≈ 2x body size; output ≈ 1–5K tok wiki page.
//	absorb-pass1:   input ≈ full raw body; output ≈ 500 tok JSON plan.
//	absorb-pass2:   per entity: input ≈ raw + plan ≈ 6K tok; output ≈ 1.5K tok wiki page.
//	For dense sources we assume 6 entities — median for our sample.
func estimateSync(root string, cfg *ScribeConfig) []tokenEstimate {
	var out []tokenEstimate

	// --- contextualize queue ---
	ctxQ := unprocessedForContextualize(root)
	if cfg.Absorb.Contextualize.Enabled != nil && *cfg.Absorb.Contextualize.Enabled && len(ctxQ) > 0 {
		limit := cfg.Absorb.Contextualize.MaxPerRun
		if limit > 0 && len(ctxQ) > limit {
			ctxQ = ctxQ[:limit]
		}
		var inTok int
		for _, p := range ctxQ {
			info, err := os.Stat(p)
			if err != nil {
				continue
			}
			// Clip to the contextualize truncation limit.
			size := min(int(info.Size()), maxRawArticleBytesForContextualize)
			inTok += (size + 3) / 4
		}
		outTok := 100 * len(ctxQ) // ~100-token paragraph per doc
		model := cfg.Absorb.Contextualize.Model
		provider := cfg.Absorb.Contextualize.Provider
		if strings.EqualFold(provider, "ollama") {
			model = "ollama:" + model + " (local, free)"
		}
		out = append(out, tokenEstimate{
			InputTokens:  inTok,
			OutputTokens: outTok,
			Model:        model,
			Note:         fmt.Sprintf("%d source(s) × contextualize", len(ctxQ)),
		})
	}

	// --- absorb queue ---
	absorbQ := unprocessedForAbsorb(root)
	maxAbsorb := cfg.Absorb.MaxPerRun
	if maxAbsorb > 0 && len(absorbQ) > maxAbsorb {
		absorbQ = absorbQ[:maxAbsorb]
	}
	if len(absorbQ) > 0 {
		var singleIn, singleOut, denseIn, denseOut int
		singleN, denseN := 0, 0
		for _, p := range absorbQ {
			density := readRawDensity(p)
			info, _ := os.Stat(p)
			body := 0
			if info != nil {
				body = int(info.Size())
			}
			bodyTok := (body + 3) / 4
			if density == "dense" {
				denseN++
				// Pass 1 + 6 × Pass 2 (per-entity median).
				denseIn += bodyTok                // pass 1 reads full raw
				denseOut += 500                   // plan JSON
				denseIn += 6 * (bodyTok/2 + 1000) // pass 2: each reads ~half + plan + wiki context
				denseOut += 6 * 1500              // six 1.5K-tok wiki pages
			} else {
				singleN++
				// Single-pass reads raw + wiki dedupe; writes one page.
				singleIn += bodyTok + 3000
				singleOut += 2500
			}
		}
		if singleN > 0 {
			out = append(out, tokenEstimate{
				InputTokens:  singleIn,
				OutputTokens: singleOut,
				Model:        "sonnet (single-pass)",
				Note:         fmt.Sprintf("%d brief/standard source(s)", singleN),
			})
		}
		if denseN > 0 {
			p1Model := cfg.Absorb.Pass1Model
			p2Model := cfg.Absorb.Pass2Model
			if p2Model == "" {
				p2Model = "sonnet (inherits default_model)"
			}
			out = append(out, tokenEstimate{
				InputTokens:  denseIn,
				OutputTokens: denseOut,
				Model:        fmt.Sprintf("pass1=%s / pass2=%s", p1Model, p2Model),
				Note:         fmt.Sprintf("%d dense source(s) × (plan + ~6 entity pages)", denseN),
			})
		}
	}

	return out
}

// estimateLedgerDays is how far back estimateWorkPhases looks in the cost
// ledger for per-call medians. Long enough to cover a weekly cadence,
// short enough that a provider or prompt change ages out.
const estimateLedgerDays = 30

// Per-call fallbacks for a KB with no ledger history for an op yet.
// Envelope values are the medians of a populated KB's hosted/local runs;
// tools values reflect that the agent reads files on its own, so output
// (which the daily ceiling meters) dominates.
var (
	defaultExtractEnvelopeCall = tokenEstimate{InputTokens: 9000, OutputTokens: 1300}
	defaultExtractToolsCall    = tokenEstimate{InputTokens: 20000, OutputTokens: 15000}
	defaultSessionEnvCall      = tokenEstimate{InputTokens: 7000, OutputTokens: 2000}
	defaultSessionToolsCall    = tokenEstimate{InputTokens: 20000, OutputTokens: 9000}
)

// estimateWorkPhases prices the LLM phases estimateSync can't see from the
// raw/articles queues: project extraction (every sync) and session mining
// (sync --sessions). Call counts come from the selection the real run makes,
// evaluated read-only; per-call tokens are medians from this KB's own cost
// ledger, falling back to the defaults above when there's no history.
//
// Requires s.DryRun: projectsNeedingExtraction only skips its manifest save
// under DryRun.
func (s *SyncCmd) estimateWorkPhases(root string, cfg *ScribeConfig, manifest *Manifest) []tokenEstimate {
	var out []tokenEstimate

	if n := min(len(s.projectsNeedingExtraction(root, manifest)), max(s.Max, 0)); n > 0 {
		op, fallback, model := "session-extract", defaultExtractToolsCall, "claude -p "+s.Model+" (tools)"
		if strings.EqualFold(cfg.Extract.Mode, "envelope") {
			op, fallback, model = "extract", defaultExtractEnvelopeCall, providerModelLabel(cfg.Extract.Provider, cfg.Extract.Model)
		}
		out = append(out, perCallEstimate(root, op, fallback, n, model, "project(s) × extract"))
	}

	if s.Sessions {
		normal, large := s.sessionsToMine(root, cfg)
		envelope := strings.EqualFold(cfg.SessionMine.Mode, "envelope")
		model := "claude -p " + s.Model + " (tools)"
		if envelope {
			model = providerModelLabel(cfg.SessionMine.Provider, cfg.SessionMine.Model)
		}
		if normal > 0 {
			op, fallback := "session-mine", defaultSessionToolsCall
			if envelope {
				op, fallback = "session", defaultSessionEnvCall
			}
			out = append(out, perCallEstimate(root, op, fallback, normal, model, "session(s) × mine"))
		}
		if large > 0 {
			op, fallback := "session-mine-batch", defaultSessionToolsCall
			if envelope {
				op, fallback = "large-session", defaultSessionEnvCall
			}
			out = append(out, perCallEstimate(root, op, fallback, large, model, "large session(s) × mine"))
		}
	}
	return out
}

// sessionsToMine counts the normal and large sessions a real
// `sync --sessions` would admit: the hook queue (peeked, not drained) plus
// triage candidates, through the same pre-filter and pool budgets. ccrider
// is not synced first, so the count reflects the current index.
func (s *SyncCmd) sessionsToMine(root string, cfg *ScribeConfig) (normal, large int) {
	processed := map[string]bool{}
	for _, id := range loadProcessedSessionIDs(filepath.Join(root, "wiki", "_sessions_log.json")) {
		processed[id] = true
	}
	var pending []pendingEntry
	for _, e := range peekPendingEntries() {
		if !processed[e.ID] {
			pending = append(pending, e)
		}
	}
	pending = backfillMsgCounts(cfg.CcriderDB, pending)
	pendingNormal, pendingLarge := partitionBySize(pending)

	triagedNormal := s.triageSessionsScored(s.SessionsMax*3, "--in-scope", "--message-limit", "300")
	normal = len(admitForPool(pendingNormal, triagedNormal, s.SessionsMax, cfg.PriorityLanes,
		func(ids []string) []string {
			kept, _ := preFilterSessions(root, cfg.CcriderDB, ids)
			return kept
		}))

	if largeMax := s.largeSessionBudget(); largeMax > 0 {
		triagedLarge := s.triageSessionsScored(largeMax*3, "--in-scope", "--min-messages", "301")
		large = len(admitForPool(pendingLarge, triagedLarge, largeMax, cfg.PriorityLanes,
			func(ids []string) []string { return filterSessionsByScope(root, cfg.CcriderDB, ids) }))
	}
	return normal, large
}

// perCallEstimate scales one call's median cost by n calls. The note says
// where the per-call figure came from, so a default never passes for a
// measurement.
func perCallEstimate(root, op string, fallback tokenEstimate, n int, model, what string) tokenEstimate {
	call, samples := ledgerCallMedian(root, op, estimateLedgerDays)
	source := fmt.Sprintf("median of %d %s call(s)", samples, op)
	if samples == 0 {
		call, source = fallback, "no "+op+" history — default"
	}
	return tokenEstimate{
		InputTokens:  n * call.InputTokens,
		OutputTokens: n * call.OutputTokens,
		Model:        model + "; " + source,
		Note:         fmt.Sprintf("%d %s", n, what),
	}
}

// ledgerCallMedian returns the median per-call input and output tokens of
// successful op calls in this KB's cost ledger over the last `days` days.
// Input counts cache reads (an Anthropic tools call reports most of its
// context there); rows without token fields fall back to the ledger's own
// prompt_chars/4 and /8 rules.
func ledgerCallMedian(root, op string, days int) (tokenEstimate, int) {
	files, _ := filepath.Glob(filepath.Join(root, "output", "costs", "*.jsonl"))
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	var ins, outs []int
	for _, f := range files {
		if strings.TrimSuffix(filepath.Base(f), ".jsonl") < cutoff {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			var ce CostEntry
			if json.Unmarshal([]byte(line), &ce) != nil || ce.Op != op || !ce.OK {
				continue
			}
			in, out := ce.PromptChars/4, ce.PromptChars/8
			if ce.InputTokens != nil {
				in = int(*ce.InputTokens)
				if ce.CacheReadTokens != nil {
					in += int(*ce.CacheReadTokens)
				}
			}
			if ce.OutputTokens != nil {
				out = int(*ce.OutputTokens)
			}
			ins, outs = append(ins, in), append(outs, out)
		}
	}
	if len(ins) == 0 {
		return tokenEstimate{}, 0
	}
	return tokenEstimate{InputTokens: medianInt(ins), OutputTokens: medianInt(outs)}, len(ins)
}

func medianInt(v []int) int {
	sorted := slices.Clone(v)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}

// providerModelLabel names a per-op provider/model for the estimate table,
// marking local Ollama as free.
func providerModelLabel(provider, model string) string {
	if strings.EqualFold(provider, "ollama") {
		return "ollama:" + model + " (local, free)"
	}
	if provider == "" {
		return model
	}
	return provider + "/" + model
}

// printEstimate writes a human-readable summary to stdout. Deliberately
// chunky on newlines so it's easy to eyeball in a sync log.
func printEstimate(estimates []tokenEstimate) {
	if len(estimates) == 0 {
		fmt.Println("\nestimate: nothing queued — sync would be a no-op.")
		return
	}
	fmt.Println("\nestimate (tokens, rough — actual depends on prompt shaping and output length):")
	fmt.Println()
	var totalIn, totalOut int
	for _, e := range estimates {
		fmt.Printf("  %-28s  in %6s  out %6s  [%s]\n",
			e.Note, humanTokens(e.InputTokens), humanTokens(e.OutputTokens), e.Model)
		totalIn += e.InputTokens
		totalOut += e.OutputTokens
	}
	fmt.Println("  " + strings.Repeat("─", 72))
	fmt.Printf("  %-28s  in %6s  out %6s\n", "total", humanTokens(totalIn), humanTokens(totalOut))
	fmt.Println()
	fmt.Println("  Prices drift too fast to quote — convert via current pricing at https://claude.com/pricing.")
	fmt.Println("  Switch contextualize to Ollama to zero its cost: absorb.contextualize.provider: ollama")
}

// printCeilingHeadroom compares the estimate's metered output (local
// Ollama rows are exempt, as in the ledger) against what's left of today's
// sync.daily_output_token_ceiling. Silent when no ceiling is set.
func printCeilingHeadroom(root string, cfg *ScribeConfig, estimates []tokenEstimate) {
	ceiling := effectiveOutputTokenCeiling(cfg.Sync)
	if ceiling <= 0 {
		return
	}
	var metered int
	for _, e := range estimates {
		if !strings.Contains(e.Model, "(local, free)") {
			metered += e.OutputTokens
		}
	}
	used := getDailyMeteredOutputTokens(root)
	left := max(ceiling-used, 0)
	fmt.Printf("  Daily output ceiling: %s used of %s today, %s left; this run ≈ %s metered output.\n",
		humanTokens(int(used)), humanTokens(int(ceiling)), humanTokens(int(left)), humanTokens(metered))
	if int64(metered) > left {
		fmt.Println("  The ceiling would stop this run partway; the rest waits for tomorrow's budget.")
	}
}

// unprocessedForContextualize returns paths of raw articles not yet
// recorded in _contextualized_log.json and without the inline marker.
func unprocessedForContextualize(root string) []string {
	rawDir := filepath.Join(root, "raw", "articles")
	logMap := loadJSONMap(filepath.Join(root, "wiki", "_contextualized_log.json"))
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if _, done := logMap[e.Name()]; done {
			continue
		}
		path := filepath.Join(rawDir, e.Name())
		if fileHasMarker(path, retrievalContextMarker) {
			continue
		}
		out = append(out, path)
	}
	return out
}

// unprocessedForAbsorb returns paths of raw articles not yet in the
// absorb log.
func unprocessedForAbsorb(root string) []string {
	rawDir := filepath.Join(root, "raw", "articles")
	logMap, _ := loadAbsorbLog(filepath.Join(root, "wiki", "_absorb_log.json"))
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if _, done := logMap[e.Name()]; done {
			continue
		}
		out = append(out, filepath.Join(rawDir, e.Name()))
	}
	return out
}
