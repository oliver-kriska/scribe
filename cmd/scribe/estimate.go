package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
		return fmt.Sprintf("%d", n)
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
