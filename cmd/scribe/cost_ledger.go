package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// cost_ledger.go implements Phase 3D — per-claude-call observability.
// Every runClaude invocation appends one row to a daily JSONL file at
// output/costs/<YYYY-MM-DD>.jsonl. The `scribe cost` subcommand reads
// the ledger and prints a per-model summary (call count, wallclock,
// rough dollar estimate from a static rate table).
//
// Why ledger before token-accurate metering: switching claude -p to
// --output-format json would give us actual usage.input_tokens but
// also forces every caller to deal with a JSON envelope, separate
// stderr capture, and parse-failure modes. That's a bigger change
// than 3D should bite off. The ledger as-built captures wallclock,
// model, prompt size, and outcome — enough to spot which absorbs
// dominate the bill and enough infrastructure to drop in token
// counts later (3D.5) by extending CostEntry.
//
// Cost numbers in `scribe cost` are explicitly labeled as estimates.
// We extrapolate: input_tokens ≈ prompt_chars / 4 (the OpenAI/
// Anthropic rule of thumb; English averages ~4 chars per token), and
// output_tokens ≈ 0.5 × input_tokens (typical for summarization
// workloads — undercounts long-form pass-2). Real numbers will
// land once 3D.5 wires --output-format json.

// CostEntry is one row in the daily cost JSONL.
type CostEntry struct {
	// Timestamp when the call started. RFC3339 UTC for greppability.
	Timestamp string `json:"timestamp"`
	// Model is the alias passed to claude -p (haiku, sonnet, opus).
	// Stored verbatim so users can tell when a config change shifted
	// model choice.
	Model string `json:"model"`
	// Op is an optional caller-supplied label (absorb-pass1,
	// absorb-pass2, facts, contextualize, dream, ...). Plumbed via
	// context value so existing callers don't all need to change.
	// Empty string = unlabeled.
	Op string `json:"op,omitempty"`
	// DurationMS is wallclock from runClaude entry to exit.
	DurationMS int64 `json:"duration_ms"`
	// PromptChars is len(prompt) at call time. Rough proxy for input
	// tokens; divide by 4 for the standard estimate.
	PromptChars int `json:"prompt_chars"`
	// OK is false when the call returned an error (rate limit,
	// timeout, non-zero exit, ...).
	OK bool `json:"ok"`
	// ErrKind classifies failures: rate_limit | timeout | other.
	// Empty when OK is true.
	ErrKind string `json:"err_kind,omitempty"`
}

// opLabelKey is a context.Context key for plumbing an op label
// through to runClaude without changing its signature. Any
// dispatcher that wants to label its calls wraps the context with
// withOpLabel; runClaude reads it during ledger append.
type opLabelKey struct{}

// withOpLabel returns a child context tagged with the given op
// label. Callers that don't tag get an empty Op field on the entry.
func withOpLabel(ctx context.Context, label string) context.Context {
	return context.WithValue(ctx, opLabelKey{}, label)
}

// opLabelFromContext returns the label set by withOpLabel, or "".
func opLabelFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(opLabelKey{}).(string); ok {
		return v
	}
	return ""
}

// appendCostEntry writes one CostEntry to the day's ledger file.
// Best-effort: any I/O error returns silently — the ledger is an
// observability nice-to-have and must not block a working absorb.
//
// Files live under <root>/output/costs/<YYYY-MM-DD>.jsonl. One file
// per day keeps individual files small and gives `scribe cost
// --days N` a cheap way to bound its work.
func appendCostEntry(root string, entry CostEntry) {
	if root == "" {
		return
	}
	costsDir := filepath.Join(root, "output", "costs")
	if err := os.MkdirAll(costsDir, 0o755); err != nil {
		return
	}
	day := time.Now().UTC().Format("2006-01-02")
	dayFile := filepath.Join(costsDir, day+".jsonl")
	f, err := os.OpenFile(dayFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	fmt.Fprintln(f, string(data))
}

// modelRateUSDPerMillion maps model alias → (input, output) USD per
// 1M tokens. Sourced from Anthropic's public pricing as of late
// 2025; out-of-band models default to zero (no estimate produced).
//
// Sticking with a static table beats a config knob: the user can
// always re-run `scribe cost` against new prices by bumping the
// constants. Premature configurability for one number per model
// would just be churn.
var modelRateUSDPerMillion = map[string][2]float64{
	"haiku":  {0.80, 4.00},
	"sonnet": {3.00, 15.00},
	"opus":   {15.00, 75.00},
}

// CostSummary is the per-model rollup `scribe cost` prints.
type CostSummary struct {
	Model            string
	Calls            int
	OK               int
	WallclockSeconds float64
	PromptChars      int64
	EstUSDLow        float64
	EstUSDHigh       float64
}

// summarizeCosts reads every JSONL in output/costs/ within the
// given lookback window and aggregates by model. Returns a sorted
// slice (descending estimated cost — the first row is the biggest
// spender). days <= 0 means "all time."
//
// A bad line in the JSONL is skipped (logged-and-ignored isn't
// possible here without a logger; we just drop it). The ledger is
// not load-bearing — a corrupted line shouldn't poison the report.
func summarizeCosts(root string, days int) ([]CostSummary, error) {
	costsDir := filepath.Join(root, "output", "costs")
	entries, err := os.ReadDir(costsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read costs dir: %w", err)
	}

	var cutoff time.Time
	if days > 0 {
		cutoff = time.Now().UTC().AddDate(0, 0, -days)
	}

	byModel := map[string]*CostSummary{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		// Day-window filter on filename. <YYYY-MM-DD>.jsonl ordering
		// is ASCII-sortable; if days>0 we can short-circuit older.
		stem := strings.TrimSuffix(e.Name(), ".jsonl")
		if days > 0 {
			day, err := time.Parse("2006-01-02", stem)
			if err != nil {
				continue
			}
			if day.Before(cutoff) {
				continue
			}
		}
		path := filepath.Join(costsDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var ce CostEntry
			if err := json.Unmarshal([]byte(line), &ce); err != nil {
				continue
			}
			row := byModel[ce.Model]
			if row == nil {
				row = &CostSummary{Model: ce.Model}
				byModel[ce.Model] = row
			}
			row.Calls++
			if ce.OK {
				row.OK++
			}
			row.WallclockSeconds += float64(ce.DurationMS) / 1000.0
			row.PromptChars += int64(ce.PromptChars)
		}
	}

	// Compute USD estimates per row.
	for _, row := range byModel {
		rate, ok := modelRateUSDPerMillion[row.Model]
		if !ok {
			continue
		}
		// Token estimate: 4 chars/token. Output is bracketed:
		//   low:  output ≈ 0.25 × input
		//   high: output ≈ 1.00 × input
		// Brackets give the user a sense of uncertainty rather than
		// a single false-precision number.
		inTokens := float64(row.PromptChars) / 4.0
		row.EstUSDLow = (inTokens*rate[0] + inTokens*0.25*rate[1]) / 1_000_000.0
		row.EstUSDHigh = (inTokens*rate[0] + inTokens*1.00*rate[1]) / 1_000_000.0
	}

	out := make([]CostSummary, 0, len(byModel))
	for _, row := range byModel {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].EstUSDHigh > out[j].EstUSDHigh
	})
	return out, nil
}

// CostCmd is the `scribe cost` subcommand.
type CostCmd struct {
	Days int `help:"Look back this many days. 0 = all time." default:"7"`
}

// Run prints the cost summary in a human-readable form. JSON output
// can come later if the user asks for it; for now this is a
// scoreboard, not a programmatic API.
func (c *CostCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	rows, err := summarizeCosts(root, c.Days)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("no claude calls recorded in that window")
		return nil
	}
	window := "all time"
	if c.Days > 0 {
		window = fmt.Sprintf("last %d days", c.Days)
	}
	fmt.Printf("scribe cost — %s\n\n", window)
	fmt.Printf("  %-10s  %6s  %6s  %10s  %14s  %18s\n", "model", "calls", "ok", "wallclock", "prompt-chars", "est-usd (low–high)")
	var totalCalls, totalOK int
	var totalSec float64
	var totalChars int64
	var totalLow, totalHigh float64
	for _, r := range rows {
		fmt.Printf("  %-10s  %6d  %6d  %9.1fs  %14d  $%6.4f – $%6.4f\n",
			r.Model, r.Calls, r.OK, r.WallclockSeconds, r.PromptChars, r.EstUSDLow, r.EstUSDHigh)
		totalCalls += r.Calls
		totalOK += r.OK
		totalSec += r.WallclockSeconds
		totalChars += r.PromptChars
		totalLow += r.EstUSDLow
		totalHigh += r.EstUSDHigh
	}
	fmt.Printf("  %-10s  %6d  %6d  %9.1fs  %14d  $%6.4f – $%6.4f\n",
		"TOTAL", totalCalls, totalOK, totalSec, totalChars, totalLow, totalHigh)
	fmt.Println()
	fmt.Println("  Estimates assume ~4 chars/token, output 0.25–1.00× input. Real")
	fmt.Println("  token counts arrive once we switch claude -p to --output-format json.")
	return nil
}
