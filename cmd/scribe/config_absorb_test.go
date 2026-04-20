package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAbsorbConfigDefaults confirms loadConfig fills missing absorb fields
// with absorbDefaults, so a KB with no `absorb:` section still works.
func TestAbsorbConfigDefaults(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "scribe.yaml"), []byte("owner_name: Test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(tmp)
	if cfg.Absorb.Strictness != "medium" {
		t.Errorf("Strictness = %q, want medium", cfg.Absorb.Strictness)
	}
	if cfg.Absorb.MaxPerRun != 5 {
		t.Errorf("MaxPerRun = %d, want 5", cfg.Absorb.MaxPerRun)
	}
	if cfg.Absorb.DenseThresholdWords != 2000 {
		t.Errorf("DenseThresholdWords = %d, want 2000", cfg.Absorb.DenseThresholdWords)
	}
	if cfg.Absorb.Pass1Model != "haiku" {
		t.Errorf("Pass1Model = %q, want haiku", cfg.Absorb.Pass1Model)
	}
	if cfg.Absorb.Contextualize.Enabled == nil || !*cfg.Absorb.Contextualize.Enabled {
		t.Error("Contextualize.Enabled should default to true")
	}
	if cfg.Absorb.Contextualize.MaxPerRun != 20 {
		t.Errorf("Contextualize.MaxPerRun = %d, want 20", cfg.Absorb.Contextualize.MaxPerRun)
	}
}

// TestAbsorbConfigPartialOverride verifies that a user override for one
// field does not clobber other defaults.
func TestAbsorbConfigPartialOverride(t *testing.T) {
	tmp := t.TempDir()
	yaml := `owner_name: Test
absorb:
  strictness: high
  dense_threshold_words: 3000
  contextualize:
    enabled: false
`
	if err := os.WriteFile(filepath.Join(tmp, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(tmp)
	if cfg.Absorb.Strictness != "high" {
		t.Errorf("Strictness override failed: %q", cfg.Absorb.Strictness)
	}
	if cfg.Absorb.DenseThresholdWords != 3000 {
		t.Errorf("DenseThresholdWords override failed: %d", cfg.Absorb.DenseThresholdWords)
	}
	// Fields not overridden must inherit defaults.
	if cfg.Absorb.MaxPerRun != 5 {
		t.Errorf("MaxPerRun should inherit default 5, got %d", cfg.Absorb.MaxPerRun)
	}
	if cfg.Absorb.BriefThresholdWords != 500 {
		t.Errorf("BriefThresholdWords should inherit default 500, got %d", cfg.Absorb.BriefThresholdWords)
	}
	if cfg.Absorb.Pass1Model != "haiku" {
		t.Errorf("Pass1Model should inherit default haiku, got %q", cfg.Absorb.Pass1Model)
	}
	// Explicit false survives the merge.
	if cfg.Absorb.Contextualize.Enabled == nil || *cfg.Absorb.Contextualize.Enabled {
		t.Error("Contextualize.Enabled: explicit false should win over default true")
	}
}

// TestLoadConfigBackfillsMissingAbsorbSection confirms that loadConfig
// appends the commented absorb block when scribe.yaml lacks it. Runtime
// config still returns defaults regardless; the on-disk merge is for
// user discoverability.
func TestLoadConfigBackfillsMissingAbsorbSection(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "scribe.yaml")
	original := "owner_name: Test\ndefault_model: sonnet\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = loadConfig(tmp)
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTopLevelKey(string(after), "absorb") {
		t.Error("absorb section should be backfilled into scribe.yaml")
	}
	// Original content must survive.
	if !strings.Contains(string(after), "owner_name: Test") {
		t.Error("existing content was damaged during backfill")
	}
}

// TestLoadConfigDoesNotRewriteExistingAbsorb ensures an existing absorb
// section is left alone — the user's settings are authoritative.
func TestLoadConfigDoesNotRewriteExistingAbsorb(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "scribe.yaml")
	yaml := "owner_name: Test\nabsorb:\n  strictness: high\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = loadConfig(tmp)
	after, _ := os.ReadFile(cfgPath)
	// The block must NOT be appended — only one `absorb:` line allowed.
	count := strings.Count(string(after), "\nabsorb:")
	// Account for the leading line match when absorb is at position 0.
	if strings.HasPrefix(string(after), "absorb:") {
		count++
	}
	if count > 1 {
		t.Errorf("absorb section appended on top of existing one: count=%d", count)
	}
}

// TestOllamaProviderFillsRecommendedModel confirms that switching provider
// to ollama without an explicit model gets the scribe-recommended local
// model (gemma3:4b), not the Claude default "haiku".
func TestOllamaProviderFillsRecommendedModel(t *testing.T) {
	tmp := t.TempDir()
	yaml := `owner_name: Test
absorb:
  contextualize:
    provider: ollama
`
	if err := os.WriteFile(filepath.Join(tmp, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(tmp)
	if cfg.Absorb.Contextualize.Model != ollamaRecommendedModel {
		t.Errorf("expected %s for ollama-without-model, got %q", ollamaRecommendedModel, cfg.Absorb.Contextualize.Model)
	}
}

// TestOllamaProviderOverridesClaudeAlias: if the user sets provider=ollama
// but accidentally leaves model: haiku (the Claude default), swap to the
// recommended local model so scribe doesn't send a nonsense name to Ollama.
func TestOllamaProviderOverridesClaudeAlias(t *testing.T) {
	tmp := t.TempDir()
	yaml := `owner_name: Test
absorb:
  contextualize:
    provider: ollama
    model: haiku
`
	if err := os.WriteFile(filepath.Join(tmp, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(tmp)
	if cfg.Absorb.Contextualize.Model != ollamaRecommendedModel {
		t.Errorf("expected Claude alias 'haiku' to be swapped to %s, got %q", ollamaRecommendedModel, cfg.Absorb.Contextualize.Model)
	}
}

// TestOllamaProviderRespectsExplicitModel ensures user picks win over the
// recommended default.
func TestOllamaProviderRespectsExplicitModel(t *testing.T) {
	tmp := t.TempDir()
	yaml := `owner_name: Test
absorb:
  contextualize:
    provider: ollama
    model: qwen3:4b
`
	if err := os.WriteFile(filepath.Join(tmp, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(tmp)
	if cfg.Absorb.Contextualize.Model != "qwen3:4b" {
		t.Errorf("explicit qwen3:4b should win, got %q", cfg.Absorb.Contextualize.Model)
	}
}

// TestAnthropicDefaultsUnchanged guards against the coherence fixup ever
// running for the anthropic path.
func TestAnthropicDefaultsUnchanged(t *testing.T) {
	tmp := t.TempDir()
	yaml := "owner_name: Test\n"
	if err := os.WriteFile(filepath.Join(tmp, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(tmp)
	if cfg.Absorb.Contextualize.Provider != "anthropic" {
		t.Errorf("default provider should stay anthropic, got %q", cfg.Absorb.Contextualize.Provider)
	}
	if cfg.Absorb.Contextualize.Model != "haiku" {
		t.Errorf("default anthropic model should stay haiku, got %q", cfg.Absorb.Contextualize.Model)
	}
}

func TestHasTopLevelKey(t *testing.T) {
	cases := []struct {
		yaml string
		key  string
		want bool
	}{
		{"absorb:\n  x: 1\n", "absorb", true},
		{"\nabsorb:\n", "absorb", true},
		{"# absorb: commented out\n", "absorb", false},
		{"  absorb: nested\n", "absorb", false},
		{"other: value\n", "absorb", false},
	}
	for _, tc := range cases {
		if got := hasTopLevelKey(tc.yaml, tc.key); got != tc.want {
			t.Errorf("hasTopLevelKey(%q, %q) = %v, want %v", tc.yaml, tc.key, got, tc.want)
		}
	}
}

// TestClassifyDensityWithConfig checks that user-tuned thresholds change
// the classification boundary.
func TestClassifyDensityWithConfig(t *testing.T) {
	var body strings.Builder
	for range 1500 {
		body.WriteString("word ")
	}
	// Default thresholds → 1500 words = standard.
	_, d1 := classifyDensity(body.String())
	if d1 != "standard" {
		t.Errorf("default: want standard, got %q", d1)
	}
	// Tight thresholds → 1500 words = dense.
	tight := absorbDefaults()
	tight.DenseThresholdWords = 1000
	_, d2 := classifyDensityWith(body.String(), tight)
	if d2 != "dense" {
		t.Errorf("tight: want dense, got %q", d2)
	}
}
