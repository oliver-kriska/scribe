package main

import (
	"errors"
	"testing"
	"time"
)

// registerBudgetKBs scaffolds two KB roots, registers them in an isolated
// user config with the given machine ceiling, and returns the roots.
func registerBudgetKBs(t *testing.T, machineCeiling string) (kb1, kb2 string) {
	t.Helper()
	resetBudgetCacheForTest()
	isolateUserConfig(t)
	kb1 = makeKBRoot(t, "kb1")
	kb2 = makeKBRoot(t, "kb2")
	cfg := machineCeiling + "kbs:\n  - " + kb1 + "\n  - " + kb2 + "\n"
	writeUserCfg(t, cfg)
	return kb1, kb2
}

func TestCheckBudget_MachineCeilingAcrossKBs(t *testing.T) {
	kb1, kb2 := registerBudgetKBs(t, "daily_output_token_ceiling: 10000000\n")
	today := time.Now().UTC().Format("2006-01-02")
	// 6M + 5M = 11M machine-wide, over the 10M ceiling — even though no
	// single KB has a per-KB cap and neither alone exceeds it.
	writeBudgetFixture(t, kb1, today, []CostEntry{{Provider: "anthropic", OutputTokens: ptrI64(6_000_000)}})
	writeBudgetFixture(t, kb2, today, []CostEntry{{Provider: "anthropic", OutputTokens: ptrI64(5_000_000)}})

	if err := checkBudget(kb1, 0); !errors.Is(err, ErrDailyBudgetExhausted) {
		t.Errorf("machine ceiling not enforced (per-KB limit 0): got %v", err)
	}
}

func TestCheckBudget_MachineCeilingUnderLimitPasses(t *testing.T) {
	kb1, kb2 := registerBudgetKBs(t, "daily_output_token_ceiling: 20000000\n")
	today := time.Now().UTC().Format("2006-01-02")
	writeBudgetFixture(t, kb1, today, []CostEntry{{Provider: "anthropic", OutputTokens: ptrI64(6_000_000)}})
	writeBudgetFixture(t, kb2, today, []CostEntry{{Provider: "anthropic", OutputTokens: ptrI64(5_000_000)}})

	if err := checkBudget(kb1, 0); err != nil {
		t.Errorf("11M under a 20M machine ceiling should pass, got %v", err)
	}
}

func TestCheckBudget_NoMachineCeilingMeansPerKBOnly(t *testing.T) {
	kb1, kb2 := registerBudgetKBs(t, "") // no machine ceiling configured
	today := time.Now().UTC().Format("2006-01-02")
	writeBudgetFixture(t, kb1, today, []CostEntry{{Provider: "anthropic", OutputTokens: ptrI64(6_000_000)}})
	writeBudgetFixture(t, kb2, today, []CostEntry{{Provider: "anthropic", OutputTokens: ptrI64(5_000_000)}})

	// With no machine ceiling and no per-KB limit, nothing fires.
	if err := checkBudget(kb1, 0); err != nil {
		t.Errorf("no ceiling configured should pass, got %v", err)
	}
	// The per-KB ceiling still works independently of the machine one.
	if err := checkBudget(kb1, 5_000_000); !errors.Is(err, ErrDailyBudgetExhausted) {
		t.Errorf("per-KB ceiling should still fire (kb1 has 6M > 5M): got %v", err)
	}
}

func TestGetMachineDailyMeteredOutputTokens_SumsAndSkipsLocal(t *testing.T) {
	kb1, kb2 := registerBudgetKBs(t, "")
	today := time.Now().UTC().Format("2006-01-02")
	writeBudgetFixture(t, kb1, today, []CostEntry{
		{Provider: "anthropic", OutputTokens: ptrI64(3_000_000)},
		{Provider: "ollama", OutputTokens: ptrI64(9_000_000)}, // local — must not count
	})
	writeBudgetFixture(t, kb2, today, []CostEntry{
		{Provider: "together", OutputTokens: ptrI64(2_000_000)},
	})
	resetBudgetCacheForTest()

	if got := getMachineDailyMeteredOutputTokens(); got != 5_000_000 {
		t.Errorf("machine sum = %d, want 5000000 (3M anthropic + 2M hosted, ollama skipped)", got)
	}
}
