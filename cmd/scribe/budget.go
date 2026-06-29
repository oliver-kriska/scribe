package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"time"
)

// budget.go enforces a daily metered output-token ceiling so a
// misconfigured cron can't repeat the 2026-05-11 runaway (7M output
// tokens in 35 hours, ~12% of the weekly Claude Max quota) — on
// anthropic OR any hosted OpenAI-compatible provider, which reintroduce
// real bill risk that free local Ollama doesn't.
//
// Strategy: every metered call reads today's cost ledger
// (output/costs/<YYYY-MM-DD>.jsonl), sums output_tokens across all rows
// EXCEPT local (free) providers like ollama — anthropic, hosted
// providers, and legacy rows with empty provider (always anthropic
// before that field existed) all count — and aborts with
// ErrDailyBudgetExhausted if the configured ceiling has been reached.
// The sync command's outer loop maps that error to "commit progress,
// log, exit clean" — same shape as ErrRateLimit.
//
// The check is intentionally cheap-but-loose:
//   - 30s read cache keeps the file I/O off the hot path. Drift up to
//     ~5% past the ceiling is acceptable for a backstop.
//   - Local-provider (ollama) calls bypass the check entirely (free).
//   - SCRIBE_BYPASS_BUDGET=1 disables the check for one-off runs that
//     knowingly need to exceed the ceiling.
//   - A zero ceiling disables the feature, which is the default — the
//     knob only fires when scribe.yaml opts in.
//
// Files live under <root>/output/costs/<YYYY-MM-DD>.jsonl, same path
// the rest of the cost ledger uses.

// ErrDailyBudgetExhausted is returned by checkBudget when today's
// metered output-token sum has reached the configured ceiling.
// Errors-Is matches via errors.Is, so callers can branch the same way
// they branch on ErrRateLimit.
var ErrDailyBudgetExhausted = errors.New("daily metered output-token ceiling reached")

// localProviders are the free, on-device backends exempt from the daily
// output-token ceiling. Everything else (anthropic + hosted OpenAI-
// compatible providers) is metered and counts toward the ceiling. An
// empty provider string is legacy-anthropic and is NOT local.
var localProviders = map[string]bool{
	"ollama":    true,
	"llamacpp":  true,
	"llama.cpp": true,
}

// isLocalProvider reports whether a cost-ledger Provider value names a
// free local backend (exempt from the budget ceiling). Case-insensitive.
func isLocalProvider(provider string) bool {
	return localProviders[strings.ToLower(strings.TrimSpace(provider))]
}

// effectiveOutputTokenCeiling returns the active daily output-token
// ceiling: the generalized daily_output_token_ceiling when set (>0),
// else the legacy anthropic-only daily_anthropic_output_token_ceiling.
// Either way the budget sum counts every metered provider — the two
// keys differ only in name, not in what they meter.
func effectiveOutputTokenCeiling(s SyncConfig) int64 {
	if s.DailyOutputTokenCeiling > 0 {
		return s.DailyOutputTokenCeiling
	}
	return s.DailyAnthropicOutputTokenCeiling
}

// budgetCache memoises the last summed output-token count so repeated
// calls within a few seconds don't all re-read the ledger file. The
// cache is process-local; cron starts a fresh process, so the first
// call always reads fresh.
type budgetCache struct {
	mu          gosync.Mutex
	lastRefresh time.Time
	day         string // YYYY-MM-DD the cache is for
	usedTokens  int64
}

var budgetState budgetCache

// budgetCacheTTL is how long getDailyMeteredOutputTokens trusts a
// cached value before re-reading the ledger. Short enough that the
// ceiling acts as a real backstop on long-running absorbs, long
// enough that parallel callers don't all hit the disk per turn.
const budgetCacheTTL = 30 * time.Second

// budgetBypassEnv is the env var that disables the budget check
// entirely for one-off runs. Exported as a const so tests can flip it
// without typo risk.
const budgetBypassEnv = "SCRIBE_BYPASS_BUDGET" // #nosec G101 -- env var name, not a credential

// checkBudget returns nil unless today's metered output-token sum has
// reached limit. A zero limit disables the check; SCRIBE_BYPASS_BUDGET=1
// also bypasses it. Best-effort on ledger I/O — if the file can't be
// read we treat used as zero and let the call through (the ceiling is
// a safety net, not a correctness invariant).
func checkBudget(root string, limit int64) error {
	if limit <= 0 || os.Getenv(budgetBypassEnv) == "1" {
		return nil
	}
	used := getDailyMeteredOutputTokens(root)
	if used >= limit {
		return fmt.Errorf("%w: used %d / limit %d", ErrDailyBudgetExhausted, used, limit)
	}
	return nil
}

// getDailyMeteredOutputTokens returns the sum of output_tokens for
// today's metered (non-local) rows in the cost ledger. Cached for
// budgetCacheTTL to keep parallel callers off the disk.
//
// Legacy rows (Provider == "") are counted — before the Provider field
// existed only runClaude and anthropicProvider wrote entries, so an
// empty Provider on a CostEntry observed today is almost certainly an
// anthropic call from a binary built before this field landed.
func getDailyMeteredOutputTokens(root string) int64 {
	today := time.Now().UTC().Format("2006-01-02")
	budgetState.mu.Lock()
	defer budgetState.mu.Unlock()
	if budgetState.day == today && time.Since(budgetState.lastRefresh) < budgetCacheTTL {
		return budgetState.usedTokens
	}
	used := readDailyMeteredOutputTokens(root, today)
	budgetState.day = today
	budgetState.usedTokens = used
	budgetState.lastRefresh = time.Now()
	return used
}

// readDailyMeteredOutputTokens reads <root>/output/costs/<day>.jsonl and
// returns the sum of output_tokens across metered rows — everything
// except local (free) providers like ollama. Anthropic, hosted
// providers, and empty-provider legacy rows all count. Missing file
// returns zero. Corrupted lines are skipped silently.
func readDailyMeteredOutputTokens(root, day string) int64 {
	if root == "" {
		return 0
	}
	path := filepath.Join(root, "output", "costs", day+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var sum int64
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ce CostEntry
		if err := json.Unmarshal([]byte(line), &ce); err != nil {
			continue
		}
		if isLocalProvider(ce.Provider) {
			continue
		}
		if ce.OutputTokens != nil {
			sum += *ce.OutputTokens
		}
	}
	return sum
}

// resetBudgetCacheForTest is a hook for tests that need to force a
// re-read after writing fixture entries. Not exposed elsewhere.
func resetBudgetCacheForTest() {
	budgetState.mu.Lock()
	defer budgetState.mu.Unlock()
	budgetState.lastRefresh = time.Time{}
	budgetState.day = ""
	budgetState.usedTokens = 0
}
