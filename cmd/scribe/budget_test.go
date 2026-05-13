package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeBudgetFixture(t *testing.T, root, day string, entries []CostEntry) {
	t.Helper()
	dir := filepath.Join(root, "output", "costs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, day+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
}

func ptrI64(v int64) *int64 { return &v }

func TestCheckBudget_ZeroLimitDisabled(t *testing.T) {
	resetBudgetCacheForTest()
	root := t.TempDir()
	today := time.Now().UTC().Format("2006-01-02")
	writeBudgetFixture(t, root, today, []CostEntry{
		{Provider: "anthropic", OutputTokens: ptrI64(10_000_000)},
	})
	if err := checkBudget(root, 0); err != nil {
		t.Errorf("zero limit should be disabled, got %v", err)
	}
}

func TestCheckBudget_UnderLimit(t *testing.T) {
	resetBudgetCacheForTest()
	root := t.TempDir()
	today := time.Now().UTC().Format("2006-01-02")
	writeBudgetFixture(t, root, today, []CostEntry{
		{Provider: "anthropic", OutputTokens: ptrI64(500_000)},
	})
	if err := checkBudget(root, 1_000_000); err != nil {
		t.Errorf("under limit should be nil, got %v", err)
	}
}

func TestCheckBudget_OverLimit(t *testing.T) {
	resetBudgetCacheForTest()
	root := t.TempDir()
	today := time.Now().UTC().Format("2006-01-02")
	writeBudgetFixture(t, root, today, []CostEntry{
		{Provider: "anthropic", OutputTokens: ptrI64(1_500_000)},
	})
	err := checkBudget(root, 1_000_000)
	if !errors.Is(err, ErrDailyBudgetExhausted) {
		t.Errorf("expected ErrDailyBudgetExhausted, got %v", err)
	}
}

func TestCheckBudget_AtLimitFails(t *testing.T) {
	resetBudgetCacheForTest()
	root := t.TempDir()
	today := time.Now().UTC().Format("2006-01-02")
	writeBudgetFixture(t, root, today, []CostEntry{
		{Provider: "anthropic", OutputTokens: ptrI64(1_000_000)},
	})
	err := checkBudget(root, 1_000_000)
	if !errors.Is(err, ErrDailyBudgetExhausted) {
		t.Errorf("at-limit should trip (>=), got %v", err)
	}
}

func TestCheckBudget_OllamaRowsIgnored(t *testing.T) {
	resetBudgetCacheForTest()
	root := t.TempDir()
	today := time.Now().UTC().Format("2006-01-02")
	writeBudgetFixture(t, root, today, []CostEntry{
		{Provider: "ollama", OutputTokens: ptrI64(5_000_000)},
		{Provider: "anthropic", OutputTokens: ptrI64(100_000)},
	})
	if err := checkBudget(root, 1_000_000); err != nil {
		t.Errorf("ollama rows should not count, got %v", err)
	}
}

func TestCheckBudget_LegacyEmptyProviderCountsAsAnthropic(t *testing.T) {
	resetBudgetCacheForTest()
	root := t.TempDir()
	today := time.Now().UTC().Format("2006-01-02")
	writeBudgetFixture(t, root, today, []CostEntry{
		{Provider: "", OutputTokens: ptrI64(1_500_000)},
	})
	err := checkBudget(root, 1_000_000)
	if !errors.Is(err, ErrDailyBudgetExhausted) {
		t.Errorf("legacy empty-provider rows should count as anthropic, got %v", err)
	}
}

func TestCheckBudget_EnvBypass(t *testing.T) {
	resetBudgetCacheForTest()
	root := t.TempDir()
	today := time.Now().UTC().Format("2006-01-02")
	writeBudgetFixture(t, root, today, []CostEntry{
		{Provider: "anthropic", OutputTokens: ptrI64(10_000_000)},
	})
	t.Setenv(budgetBypassEnv, "1")
	if err := checkBudget(root, 1_000_000); err != nil {
		t.Errorf("SCRIBE_BYPASS_BUDGET=1 should bypass, got %v", err)
	}
}

func TestCheckBudget_MissingFileTreatedAsZero(t *testing.T) {
	resetBudgetCacheForTest()
	root := t.TempDir()
	if err := checkBudget(root, 1_000_000); err != nil {
		t.Errorf("no ledger file should be 0 used, got %v", err)
	}
}

func TestCheckBudget_CorruptLinesSkipped(t *testing.T) {
	resetBudgetCacheForTest()
	root := t.TempDir()
	today := time.Now().UTC().Format("2006-01-02")
	dir := filepath.Join(root, "output", "costs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := `{"provider":"anthropic","output_tokens":300000}
this is not json
{"provider":"anthropic","output_tokens":400000}
`
	if err := os.WriteFile(filepath.Join(dir, today+".jsonl"), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 700k under 1M limit — corrupt line shouldn't poison the sum.
	if err := checkBudget(root, 1_000_000); err != nil {
		t.Errorf("corrupt lines should be skipped, got %v", err)
	}
}

func TestCheckBudget_CacheServesRepeatCalls(t *testing.T) {
	resetBudgetCacheForTest()
	root := t.TempDir()
	today := time.Now().UTC().Format("2006-01-02")
	writeBudgetFixture(t, root, today, []CostEntry{
		{Provider: "anthropic", OutputTokens: ptrI64(500_000)},
	})
	if err := checkBudget(root, 1_000_000); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Overwrite the ledger with an over-limit value; the cache should
	// still report the old (under-limit) number until TTL expires.
	writeBudgetFixture(t, root, today, []CostEntry{
		{Provider: "anthropic", OutputTokens: ptrI64(10_000_000)},
	})
	if err := checkBudget(root, 1_000_000); err != nil {
		t.Errorf("cache should hold under-limit value; got %v", err)
	}
}

func TestCheckBudget_OnlyTodaysFileCounts(t *testing.T) {
	resetBudgetCacheForTest()
	root := t.TempDir()
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	writeBudgetFixture(t, root, yesterday, []CostEntry{
		{Provider: "anthropic", OutputTokens: ptrI64(10_000_000)},
	})
	// Today's file absent → today's used is 0.
	if err := checkBudget(root, 1_000_000); err != nil {
		t.Errorf("yesterday's rows should not gate today, got %v", err)
	}
}

func TestReadDailyAnthropicOutputTokens_NilOutputTokensSkipped(t *testing.T) {
	root := t.TempDir()
	today := time.Now().UTC().Format("2006-01-02")
	writeBudgetFixture(t, root, today, []CostEntry{
		{Provider: "anthropic", OutputTokens: nil},
		{Provider: "anthropic", OutputTokens: ptrI64(123)},
	})
	got := readDailyAnthropicOutputTokens(root, today)
	if got != 123 {
		t.Errorf("got %d, want 123", got)
	}
}
