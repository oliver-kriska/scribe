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
	// Provider names the backend that ran the call: "anthropic" for
	// claude -p (runClaude + anthropicProvider), "ollama" for local
	// Ollama, etc. Empty on legacy entries written before this field
	// existed — those came from runClaude/anthropicProvider only, so
	// the daily-budget reader treats empty as "anthropic" for
	// backwards-compat.
	Provider string `json:"provider,omitempty"`
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
	// Phase 3D.5: token-accurate fields populated when claude -p is
	// invoked with --output-format json and the envelope parses
	// cleanly. Pointers so absent (legacy text-mode or failed
	// parse) is distinguishable from zero (succeeded but no usage).
	InputTokens     *int64   `json:"input_tokens,omitempty"`
	OutputTokens    *int64   `json:"output_tokens,omitempty"`
	CacheReadTokens *int64   `json:"cache_read_tokens,omitempty"`
	CostUSD         *float64 `json:"cost_usd,omitempty"`
}

// claudeResultEnvelope mirrors the JSON shape claude -p emits with
// --output-format json. Anthropic adds fields over time; we accept
// unknown keys silently (json.Unmarshal default).
type claudeResultEnvelope struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	DurationMS   int64   `json:"duration_ms"`
	NumTurns     int     `json:"num_turns"`
	Result       string  `json:"result"`
	SessionID    string  `json:"session_id"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// parseClaudeResult scans claude -p --output-format json stdout
// line-by-line and returns the first line that parses as a result
// envelope with type=="result". Returns (envelope, true) on hit,
// (zero, false) on miss.
//
// Why line-scan instead of whole-buffer parse: claude emits a
// single-line JSON object, but global hooks (CMUX session-end,
// plugin postludes, --add-dir CLAUDE.md banners) can leak text
// either before or after the envelope. A whole-buffer json.Unmarshal
// fails on any trailing/leading byte. Line-scanning is forgiving.
func parseClaudeResult(stdout string) (claudeResultEnvelope, bool) {
	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "{") {
			continue
		}
		var env claudeResultEnvelope
		if err := json.Unmarshal([]byte(trimmed), &env); err == nil && env.Type == "result" {
			return env, true
		}
	}
	return claudeResultEnvelope{}, false
}

// isRateLimitSubtype detects rate-limit errors from the JSON
// envelope's subtype field. The subtype set varies across claude
// versions; the substrings listed here cover what's been observed
// since the JSON output mode landed.
func isRateLimitSubtype(subtype string) bool {
	lower := strings.ToLower(subtype)
	return strings.Contains(lower, "rate") ||
		strings.Contains(lower, "quota") ||
		strings.Contains(lower, "limit")
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

// ollamaNumCtxKey carries a per-call num_ctx hint through to
// ollamaProvider.generate. Bigger tasks (session-mine, dream, assess,
// deep) need 16384 / 32768 — the default 8192 silently truncates the
// tail of a 24K-char transcript, dropping the conclusion exactly when
// the prompt needs it most. Pass via withOllamaNumCtx; the provider
// reads with ollamaNumCtxFromContext (defaulting to 8192).
type ollamaNumCtxKey struct{}

// withOllamaNumCtx pins the Ollama num_ctx for this call. Pass 0 to
// inherit the provider default (8192). Callers can stack this with
// withOpLabel — the two keys are independent.
func withOllamaNumCtx(ctx context.Context, numCtx int) context.Context {
	if numCtx <= 0 {
		return ctx
	}
	return context.WithValue(ctx, ollamaNumCtxKey{}, numCtx)
}

// ollamaNumCtxFromContext returns the requested num_ctx or 0 when
// the caller didn't pin one. The provider's generate() resolves 0 →
// its default (8192).
func ollamaNumCtxFromContext(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	if v, ok := ctx.Value(ollamaNumCtxKey{}).(int); ok {
		return v
	}
	return 0
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

// ErrorRecord captures the noisy context behind a claude -p failure that
// `logMsg` deliberately keeps short for terminal readability. Written to
// output/errors/<date>.jsonl alongside the compact CostEntry — same
// timestamp, same op, but with the full stderr/stdout tails the user
// needs when something actually breaks.
type ErrorRecord struct {
	Timestamp   string `json:"timestamp"`
	Op          string `json:"op"`
	Model       string `json:"model,omitempty"`
	ErrKind     string `json:"err_kind"` // timeout|rate_limit|other
	DurationMS  int64  `json:"duration_ms,omitempty"`
	PromptChars int    `json:"prompt_chars,omitempty"`
	Err         string `json:"err"`
	StderrTail  string `json:"stderr_tail,omitempty"` // up to ~50 lines
	StdoutTail  string `json:"stdout_tail,omitempty"`
}

// appendErrorRecord writes one ErrorRecord to the day's error log.
// Same best-effort contract as appendCostEntry: silent on I/O failure.
//
// Files live under <root>/output/errors/<YYYY-MM-DD>.jsonl. Rate-limit
// failures intentionally aren't logged here — they're expected,
// self-resolving, and would otherwise drown out real bugs in the file.
func appendErrorRecord(root string, rec ErrorRecord) {
	if root == "" || rec.ErrKind == "rate_limit" {
		return
	}
	dir := filepath.Join(root, "output", "errors")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	day := time.Now().UTC().Format("2006-01-02")
	dayFile := filepath.Join(dir, day+".jsonl")
	f, err := os.OpenFile(dayFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	data, err := json.Marshal(rec)
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
	Canceled         int
	Timeout          int
	RateLimit        int
	WallclockSeconds float64
	PromptChars      int64
	EstUSDLow        float64
	EstUSDHigh       float64
	// Phase 3D.5: real numbers from --output-format json envelopes.
	// CallsWithUsage tracks how many of Calls had token data; the
	// rest fall back to the char-estimate brackets.
	CallsWithUsage  int
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	ActualUSD       float64
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
			switch ce.ErrKind {
			case "canceled":
				row.Canceled++
			case "timeout":
				row.Timeout++
			case "rate_limit":
				row.RateLimit++
			}
			row.WallclockSeconds += float64(ce.DurationMS) / 1000.0
			row.PromptChars += int64(ce.PromptChars)
			// Phase 3D.5: real-number aggregation when present.
			if ce.InputTokens != nil {
				row.InputTokens += *ce.InputTokens
				row.CallsWithUsage++
			}
			if ce.OutputTokens != nil {
				row.OutputTokens += *ce.OutputTokens
			}
			if ce.CacheReadTokens != nil {
				row.CacheReadTokens += *ce.CacheReadTokens
			}
			if ce.CostUSD != nil {
				row.ActualUSD += *ce.CostUSD
			}
		}
	}

	// Compute USD estimates per row only for the prompt-chars from
	// calls WITHOUT token data — calls with real usage have their
	// cost in row.ActualUSD already. We don't double-count.
	for _, row := range byModel {
		rate, ok := modelRateUSDPerMillion[row.Model]
		if !ok {
			continue
		}
		// PromptChars covers every call. To avoid double-counting,
		// we compute the estimate only for the unmeasured share.
		// callsWithoutUsage / totalCalls is the proportion of
		// PromptChars that lacked token data.
		if row.Calls == 0 {
			continue
		}
		unmeasuredFrac := float64(row.Calls-row.CallsWithUsage) / float64(row.Calls)
		unmeasuredChars := float64(row.PromptChars) * unmeasuredFrac
		inTokens := unmeasuredChars / 4.0
		row.EstUSDLow = (inTokens*rate[0] + inTokens*0.25*rate[1]) / 1_000_000.0
		row.EstUSDHigh = (inTokens*rate[0] + inTokens*1.00*rate[1]) / 1_000_000.0
	}

	out := make([]CostSummary, 0, len(byModel))
	for _, row := range byModel {
		out = append(out, *row)
	}
	// Sort by total USD (real + estimated high) descending. Real
	// dominates when present, estimate kicks in for legacy rows.
	sort.Slice(out, func(i, j int) bool {
		ti := out[i].ActualUSD + out[i].EstUSDHigh
		tj := out[j].ActualUSD + out[j].EstUSDHigh
		return ti > tj
	})
	return out, nil
}

// CostCmd is the `scribe cost` subcommand.
type CostCmd struct {
	Days int    `help:"Look back this many days. 0 = all time." default:"7"`
	KB   string `help:"Scope to one KB by registered name or path. Default: aggregate every registered KB (matches per-API-key provider billing)."`
}

// Run prints the cost summary in a human-readable form. By default it
// aggregates EVERY registered KB, because hosted providers bill per API key
// and one key typically serves all of a machine's KBs — so a single-KB view
// silently undercounts the real spend (the exact confusion that motivated
// this: `scribe cost` in one KB never matching the provider dashboard).
// Pointing scribe at a specific KB (--kb, -C, SCRIBE_KB, or running inside a
// KB checkout) scopes the report back to that one KB.
func (c *CostCmd) Run() error {
	roots, err := c.resolveCostRoots()
	if err != nil {
		return err
	}

	// Merge every KB's per-model rollup into one table, and keep a per-KB
	// total so the aggregate stays decomposable.
	type kbReport struct {
		name  string
		total CostSummary
	}
	var reports []kbReport
	combined := map[string]*CostSummary{}
	for _, root := range roots {
		rows, err := summarizeCosts(root, c.Days)
		if err != nil {
			return err
		}
		mergeByModel(combined, rows)
		reports = append(reports, kbReport{name: kbName(root), total: totalOfCosts(rows)})
	}
	rows := sortedByCost(combined)
	if len(rows) == 0 {
		fmt.Println("no claude calls recorded in that window")
		return nil
	}

	window := "all time"
	if c.Days > 0 {
		window = fmt.Sprintf("last %d days", c.Days)
	}
	scope := reports[0].name
	if len(reports) > 1 {
		names := make([]string, len(reports))
		for i, r := range reports {
			names[i] = r.name
		}
		scope = fmt.Sprintf("%d KBs (%s)", len(reports), strings.Join(names, ", "))
	}
	fmt.Printf("scribe cost — %s — %s\n\n", window, scope)

	total := printCostTable(rows)

	// By provider — the rollup that maps to how you're actually billed:
	// anthropic (claude -p), each hosted endpoint (together/groq/…), and local
	// ollama ($0). Always shown; it's the headline cross-cut of the per-model
	// table above.
	fmt.Println()
	fmt.Println("  By provider:")
	printGroupSummary("provider", groupByProvider(rows))

	if len(reports) > 1 {
		// Per-KB subtotals: which KB drove the spend, and proof the combined
		// TOTAL above decomposes into them.
		kbRows := make([]CostSummary, len(reports))
		for i, rep := range reports {
			kbRows[i] = rep.total
			kbRows[i].Model = rep.name
		}
		fmt.Println()
		fmt.Println("  By KB:")
		printGroupSummary("KB", kbRows)
	}

	fmt.Println()
	unmeasured := total.Calls - total.CallsWithUsage
	if unmeasured > 0 && total.EstUSDHigh > 0 {
		fmt.Printf("  Coverage: %d/%d calls had token data; the other %d add ~$%.2f estimated, not shown above.\n",
			total.CallsWithUsage, total.Calls, unmeasured, total.EstUSDHigh)
	} else {
		fmt.Printf("  Coverage: %d/%d calls reported real token usage (--output-format json).\n", total.CallsWithUsage, total.Calls)
	}
	fmt.Println("  usd = measured spend from token data.  ~$lo-hi = char-based estimate for rows with no token data at all.")
	fmt.Println("  cancl = sibling-canceled (rate-limit cascade).  rate = direct rate-limit response.  tmout = ctx.DeadlineExceeded.")
	if len(reports) == 1 {
		if n := len(registeredKBs()); n > 1 {
			fmt.Printf("  Scoped to %s — drop --kb/-C and run outside a KB to aggregate all %d registered KBs.\n", reports[0].name, n)
		}
	}
	return nil
}

// resolveCostRoots decides which KB ledgers to read. Default: every
// registered KB (provider billing is per API key, which usually spans all of
// them). An explicit single-KB signal — --kb, -C, SCRIBE_KB, or running
// inside a KB checkout — scopes to that one KB.
func (c *CostCmd) resolveCostRoots() ([]string, error) {
	if c.KB != "" {
		root, err := resolveKBRef(c.KB)
		if err != nil {
			return nil, err
		}
		return []string{root}, nil
	}
	if kbScopeExplicit() {
		root, err := kbDir()
		if err != nil {
			return nil, err
		}
		return []string{root}, nil
	}
	if kbs := registeredKBs(); len(kbs) > 0 {
		return kbs, nil
	}
	// No registry — fall back to whatever single KB resolves (or error).
	root, err := kbDir()
	if err != nil {
		return nil, err
	}
	return []string{root}, nil
}

// kbScopeExplicit reports whether the user pointed scribe at a specific KB
// rather than falling through to the machine default. Mirrors kbDir's
// explicit resolution sources: -C flag, SCRIBE_KB, or cwd inside a KB.
func kbScopeExplicit() bool {
	if globalRoot != "" || os.Getenv("SCRIBE_KB") != "" {
		return true
	}
	if cwd, err := os.Getwd(); err == nil {
		for dir := cwd; dir != string(filepath.Separator) && dir != "."; dir = filepath.Dir(dir) {
			if isKBRoot(dir) {
				return true
			}
		}
	}
	return false
}

// resolveKBRef maps a --kb value to a KB root: an existing KB path, or the
// registered KB whose name (basename) matches.
func resolveKBRef(ref string) (string, error) {
	if isKBRoot(ref) {
		return filepath.Abs(ref)
	}
	kbs := registeredKBs()
	for _, kb := range kbs {
		if kb == ref || kbName(kb) == ref {
			return kb, nil
		}
	}
	names := make([]string, len(kbs))
	for i, kb := range kbs {
		names[i] = kbName(kb)
	}
	return "", fmt.Errorf("--kb %q: not a KB path and no registered KB by that name (have: %s)", ref, strings.Join(names, ", "))
}

// mergeByModel folds one KB's per-model rows into the running combined map.
func mergeByModel(dst map[string]*CostSummary, rows []CostSummary) {
	for _, r := range rows {
		d := dst[r.Model]
		if d == nil {
			d = &CostSummary{Model: r.Model}
			dst[r.Model] = d
		}
		addCost(d, r)
	}
}

// addCost accumulates src's metrics into dst. Model is the caller's to set.
func addCost(dst *CostSummary, src CostSummary) {
	dst.Calls += src.Calls
	dst.OK += src.OK
	dst.Canceled += src.Canceled
	dst.Timeout += src.Timeout
	dst.RateLimit += src.RateLimit
	dst.WallclockSeconds += src.WallclockSeconds
	dst.PromptChars += src.PromptChars
	dst.EstUSDLow += src.EstUSDLow
	dst.EstUSDHigh += src.EstUSDHigh
	dst.CallsWithUsage += src.CallsWithUsage
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.ActualUSD += src.ActualUSD
}

// totalOfCosts sums a set of per-model rows into a single CostSummary.
func totalOfCosts(rows []CostSummary) CostSummary {
	var t CostSummary
	for _, r := range rows {
		addCost(&t, r)
	}
	return t
}

// sortedByCost flattens the combined map into a slice ordered by total spend
// (real + estimated high) descending — biggest spender first.
func sortedByCost(m map[string]*CostSummary) []CostSummary {
	out := make([]CostSummary, 0, len(m))
	for _, r := range m {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ActualUSD+out[i].EstUSDHigh > out[j].ActualUSD+out[j].EstUSDHigh
	})
	return out
}

// printCostTable renders the per-model table with a TOTAL footer and returns
// the aggregate total (so the caller can report coverage and the rolled-up
// estimate). The model and usd columns size to their widest content so long
// hosted-provider slugs (e.g. "together/Qwen/Qwen3-235B-A22B-Instruct-2507-tput")
// stay aligned instead of raggedding the table.
func printCostTable(rows []CostSummary) CostSummary {
	usds := make([]string, len(rows))
	modelW := len("model") // also covers the "TOTAL" footer (5 chars)
	usdW := len("usd")
	total := totalOfCosts(rows)
	for i, r := range rows {
		usds[i] = formatRowUSD(r)
		if len(r.Model) > modelW {
			modelW = len(r.Model)
		}
		if len(usds[i]) > usdW {
			usdW = len(usds[i])
		}
	}
	totalUSD := formatRowUSD(total)
	if len(totalUSD) > usdW {
		usdW = len(totalUSD)
	}
	fmt.Printf("  %-*s  %6s  %6s  %6s  %6s  %6s  %10s  %12s  %12s  %*s\n",
		modelW, "model", "calls", "ok", "cancl", "rate", "tmout", "wallclock", "in-tokens", "out-tokens", usdW, "usd")
	for i, r := range rows {
		fmt.Printf("  %-*s  %6d  %6d  %6d  %6d  %6d  %9.1fs  %12d  %12d  %*s\n",
			modelW, r.Model, r.Calls, r.OK, r.Canceled, r.RateLimit, r.Timeout, r.WallclockSeconds, r.InputTokens, r.OutputTokens, usdW, usds[i])
	}
	fmt.Printf("  %-*s  %6d  %6d  %6d  %6d  %6d  %9.1fs  %12d  %12d  %*s\n",
		modelW, "TOTAL", total.Calls, total.OK, total.Canceled, total.RateLimit, total.Timeout, total.WallclockSeconds, total.InputTokens, total.OutputTokens, usdW, totalUSD)
	return total
}

// groupByProvider rolls per-model rows up to their backend provider, ordered
// by spend descending.
func groupByProvider(rows []CostSummary) []CostSummary {
	m := map[string]*CostSummary{}
	for _, r := range rows {
		p := providerOf(r.Model)
		d := m[p]
		if d == nil {
			d = &CostSummary{Model: p}
			m[p] = d
		}
		addCost(d, r)
	}
	return sortedByCost(m)
}

// providerOf extracts the backend from a cost-ledger model key. Hosted and
// local models are stored provider-qualified ("together/Qwen/...",
// "ollama/gemma3:4b"); bare aliases ("sonnet"/"haiku"/"opus") are claude -p,
// i.e. anthropic.
func providerOf(model string) string {
	if i := strings.IndexByte(model, '/'); i > 0 {
		return model[:i]
	}
	return "anthropic"
}

// printGroupSummary prints a compact, aligned rollup (by provider or KB) under
// the detailed per-model table: name, calls, tokens, and measured USD. Rows
// are ordered by spend descending; the name and usd columns size to content.
func printGroupSummary(colHeader string, rows []CostSummary) {
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].ActualUSD+rows[i].EstUSDHigh > rows[j].ActualUSD+rows[j].EstUSDHigh
	})
	usds := make([]string, len(rows))
	nameW := len(colHeader)
	usdW := len("usd")
	for i, r := range rows {
		usds[i] = formatRowUSD(r)
		if len(r.Model) > nameW {
			nameW = len(r.Model)
		}
		if len(usds[i]) > usdW {
			usdW = len(usds[i])
		}
	}
	fmt.Printf("    %-*s  %8s  %12s  %12s  %*s\n", nameW, colHeader, "calls", "in-tokens", "out-tokens", usdW, "usd")
	for i, r := range rows {
		fmt.Printf("    %-*s  %8d  %12d  %12d  %*s\n", nameW, r.Model, r.Calls, r.InputTokens, r.OutputTokens, usdW, usds[i])
	}
}

// formatRowUSD picks the most accurate dollar representation
// available for a row. When all calls reported usage, we print the
// real number; when none did, we print the estimate range; mixed
// rows print "real $X + est $Y–$Z" so the user can see partial
// instrumentation.
func formatRowUSD(r CostSummary) string {
	switch {
	case r.ActualUSD > 0:
		// Measured spend is the headline — one clean number. Any estimate
		// for this row's un-instrumented calls is rolled into the Coverage
		// footnote, not mashed into the cell: the old "$92.80+~$0.45" form
		// was unreadable and the estimate is sub-1% noise on a measured row.
		return usd2(r.ActualUSD)
	case r.EstUSDLow > 0 || r.EstUSDHigh > 0:
		// No token data at all (legacy rows) — show the char-based estimate,
		// flagged with a leading ~ so it never reads as a measured number.
		return fmt.Sprintf("~$%.2f-%.2f", r.EstUSDLow, r.EstUSDHigh)
	default:
		return "—"
	}
}

// usd2 formats a dollar amount at 2 decimals (cents), the granularity people
// actually read a spend table at. A positive sub-cent value floors to
// "<$0.01" so a real cost never misleadingly prints as "$0.00".
func usd2(v float64) string {
	if v > 0 && v < 0.005 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", v)
}
