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

// TestLoadConfigDoesNotBackfill_RelocatedToExplicitCall locks the
// 0.2.21 contract change at the original site: loadConfig must NOT
// rewrite scribe.yaml (it used to, which made `scribe doctor`/`status`
// and --dry-run self-modifying — Codex finding 2026-05-15). The
// discoverability backfill moved to maybeBackfillAbsorbBlock, invoked
// only from mutating entrypoints; this test proves loadConfig is inert
// and the explicit call still provides it. (Unit-level coverage of
// each half lives in readonly_contract_test.go.)
func TestLoadConfigDoesNotBackfill_RelocatedToExplicitCall(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "scribe.yaml")
	original := "owner_name: Test\ndefault_model: sonnet\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = loadConfig(tmp)
	afterLoad, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterLoad) != original {
		t.Errorf("loadConfig must not touch scribe.yaml; got:\n%s", afterLoad)
	}
	// The relocated explicit call still provides discoverability.
	maybeBackfillAbsorbBlock(tmp)
	afterBackfill, _ := os.ReadFile(cfgPath)
	if !hasTopLevelKey(string(afterBackfill), "absorb") {
		t.Error("maybeBackfillAbsorbBlock should append the absorb block")
	}
	if !strings.Contains(string(afterBackfill), "owner_name: Test") {
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

// TestLLMProviderCascadesToAbsorbPasses is the regression for the bug where
// top-level `llm.provider: ollama` silently failed to move the absorb pass
// providers off Anthropic. absorbDefaults() pre-seeds pass1/pass2/single_pass
// providers to "anthropic"; loadConfig must reset them to "" (like Pass2Mode)
// so applyAbsorbDefaultsWithLLM can cascade llm.provider into them. Before the
// fix they stayed "anthropic", so an ollama-configured KB still billed
// Anthropic (and ran the tool-use path) on every absorb — the costliest stage.
func TestLLMProviderCascadesToAbsorbPasses(t *testing.T) {
	tmp := t.TempDir()
	yaml := `owner_name: Test
llm:
  provider: ollama
  model: gemma3:4b
`
	if err := os.WriteFile(filepath.Join(tmp, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(tmp)
	for _, c := range []struct{ name, got string }{
		{"Pass1Provider", cfg.Absorb.Pass1Provider},
		{"Pass2Provider", cfg.Absorb.Pass2Provider},
		{"SinglePassProvider", cfg.Absorb.SinglePassProvider},
		{"Contextualize.Provider", cfg.Absorb.Contextualize.Provider},
	} {
		if c.got != "ollama" {
			t.Errorf("%s = %q, want ollama (cascaded from llm.provider)", c.name, c.got)
		}
	}
	// A non-anthropic provider must auto-flip the tools path to json, since
	// local models can't drive `claude -p` tool use.
	if cfg.Absorb.Pass2Mode != "json" {
		t.Errorf("Pass2Mode = %q, want json (ollama can't use the tools path)", cfg.Absorb.Pass2Mode)
	}
	// Models cascade too: the haiku alias is coerced to the local default.
	if cfg.Absorb.Pass2Model != "gemma3:4b" {
		t.Errorf("Pass2Model = %q, want gemma3:4b", cfg.Absorb.Pass2Model)
	}
}

// TestExplicitPerOpProviderWinsOverLLM confirms a per-op provider set in yaml
// still beats the llm.provider cascade — the documented "keep pass-2 on
// Anthropic" override must survive the reset.
func TestExplicitPerOpProviderWinsOverLLM(t *testing.T) {
	tmp := t.TempDir()
	yaml := `owner_name: Test
llm:
  provider: ollama
  model: gemma3:4b
absorb:
  pass2_provider: anthropic
`
	if err := os.WriteFile(filepath.Join(tmp, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(tmp)
	if cfg.Absorb.Pass2Provider != "anthropic" {
		t.Errorf("explicit pass2_provider: anthropic should win over llm.provider: ollama, got %q", cfg.Absorb.Pass2Provider)
	}
	// pass-1 (not overridden) still cascades to ollama.
	if cfg.Absorb.Pass1Provider != "ollama" {
		t.Errorf("Pass1Provider = %q, want ollama (cascaded)", cfg.Absorb.Pass1Provider)
	}
}

// TestAbsorbPassesStayAnthropicByDefault guards the no-regression direction:
// a KB with no llm block keeps the pass providers on anthropic (the reset
// must fall back to the anthropic default, not leave them empty).
func TestAbsorbPassesStayAnthropicByDefault(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "scribe.yaml"), []byte("owner_name: Test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(tmp)
	for _, c := range []struct{ name, got string }{
		{"Pass1Provider", cfg.Absorb.Pass1Provider},
		{"Pass2Provider", cfg.Absorb.Pass2Provider},
		{"SinglePassProvider", cfg.Absorb.SinglePassProvider},
	} {
		if c.got != "anthropic" {
			t.Errorf("%s = %q, want anthropic (default when no llm block)", c.name, c.got)
		}
	}
	if cfg.Absorb.Pass2Mode != "tools" {
		t.Errorf("Pass2Mode = %q, want tools (anthropic keeps the tools path)", cfg.Absorb.Pass2Mode)
	}
}

// TestHostedLLMModelCascadesToAbsorbPasses is the regression for issue #43's
// "move the whole pipeline to the cloud" config. With a hosted provider
// (together/groq/...) the per-op model defaults are anthropic-shaped
// ("haiku"/"sonnet"/"") and coerceProviderModel can't guess a hosted model
// (it only knows ollama's recommended default). Before inheritHostedModel,
// the absorb+contextualize stages would send "haiku"/"" to the hosted client,
// which rejects an unknown/empty model — so a fully-hosted KB failed on
// absorb. The top-level llm.model must cascade into every per-op model that
// the user left at the anthropic default.
func TestHostedLLMModelCascadesToAbsorbPasses(t *testing.T) {
	tmp := t.TempDir()
	yaml := `owner_name: Test
llm:
  provider: together
  model: meta-llama/Llama-3.3-70B-Instruct-Turbo
`
	if err := os.WriteFile(filepath.Join(tmp, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(tmp)
	const want = "meta-llama/Llama-3.3-70B-Instruct-Turbo"
	for _, c := range []struct{ name, got string }{
		{"Pass1Model", cfg.Absorb.Pass1Model},
		{"Pass2Model", cfg.Absorb.Pass2Model},
		{"SinglePassModel", cfg.Absorb.SinglePassModel},
		{"FactsModel", cfg.Absorb.FactsModel},
		{"Contextualize.Model", cfg.Absorb.Contextualize.Model},
	} {
		if c.got != want {
			t.Errorf("%s = %q, want %q (cascaded from llm.model)", c.name, c.got, want)
		}
	}
	// Providers cascade too, and pass-2 auto-flips off the tools path since a
	// hosted OpenAI-compatible endpoint can't drive `claude -p`.
	if cfg.Absorb.Pass2Provider != "together" {
		t.Errorf("Pass2Provider = %q, want together", cfg.Absorb.Pass2Provider)
	}
	if cfg.Absorb.Pass2Mode != "json" {
		t.Errorf("Pass2Mode = %q, want json (hosted can't use the tools path)", cfg.Absorb.Pass2Mode)
	}
}

// TestHostedLLMModelRespectsExplicitPerOpModel confirms a per-op model set in
// yaml still wins over the llm.model cascade — a user can pin one absorb pass
// to a different hosted model (e.g. a cheaper one for facts).
func TestHostedLLMModelRespectsExplicitPerOpModel(t *testing.T) {
	tmp := t.TempDir()
	yaml := `owner_name: Test
llm:
  provider: together
  model: meta-llama/Llama-3.3-70B-Instruct-Turbo
absorb:
  facts_model: meta-llama/Llama-3.1-8B-Instruct-Turbo
`
	if err := os.WriteFile(filepath.Join(tmp, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(tmp)
	if cfg.Absorb.FactsModel != "meta-llama/Llama-3.1-8B-Instruct-Turbo" {
		t.Errorf("explicit facts_model should win, got %q", cfg.Absorb.FactsModel)
	}
	// A pass the user didn't pin still inherits the top-level model.
	if cfg.Absorb.Pass1Model != "meta-llama/Llama-3.3-70B-Instruct-Turbo" {
		t.Errorf("Pass1Model = %q, want the cascaded llm.model", cfg.Absorb.Pass1Model)
	}
}

// TestAnthropicKeepsPerOpModelSplit guards the no-regression direction for
// inheritHostedModel: anthropic must keep its deliberate haiku/sonnet per-op
// split even when llm.model is set, never collapsing every pass onto one model.
func TestAnthropicKeepsPerOpModelSplit(t *testing.T) {
	tmp := t.TempDir()
	yaml := `owner_name: Test
llm:
  provider: anthropic
  model: opus
`
	if err := os.WriteFile(filepath.Join(tmp, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(tmp)
	if cfg.Absorb.FactsModel != "haiku" {
		t.Errorf("FactsModel = %q, want haiku (anthropic keeps its per-op default)", cfg.Absorb.FactsModel)
	}
	if cfg.Absorb.Contextualize.Model != "haiku" {
		t.Errorf("Contextualize.Model = %q, want haiku (anthropic per-op default)", cfg.Absorb.Contextualize.Model)
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

// TestPass2EnvOverrides verifies that SCRIBE_PASS2_{MODE,PROVIDER,MODEL}
// env vars override the corresponding scribe.yaml absorb fields. Used by
// scripts/absorb-compare.sh to flip mode without mutating yaml.
func TestPass2EnvOverrides(t *testing.T) {
	tmp := t.TempDir()
	yaml := `owner_name: Test
absorb:
  pass2_mode: tools
  pass2_provider: anthropic
  pass2_model: ""
`
	if err := os.WriteFile(filepath.Join(tmp, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCRIBE_PASS2_MODE", "json")
	t.Setenv("SCRIBE_PASS2_PROVIDER", "ollama")
	t.Setenv("SCRIBE_PASS2_MODEL", "qwen2.5-coder:14b")
	cfg := loadConfig(tmp)
	if cfg.Absorb.Pass2Mode != "json" {
		t.Errorf("Pass2Mode = %q, want json (env override)", cfg.Absorb.Pass2Mode)
	}
	if cfg.Absorb.Pass2Provider != "ollama" {
		t.Errorf("Pass2Provider = %q, want ollama (env override)", cfg.Absorb.Pass2Provider)
	}
	if cfg.Absorb.Pass2Model != "qwen2.5-coder:14b" {
		t.Errorf("Pass2Model = %q, want qwen2.5-coder:14b (env override)", cfg.Absorb.Pass2Model)
	}
}

// TestPass2EnvOverridesEmptyNoop confirms an empty env var is a no-op and
// scribe.yaml's value wins.
func TestPass2EnvOverridesEmptyNoop(t *testing.T) {
	tmp := t.TempDir()
	yaml := `owner_name: Test
absorb:
  pass2_mode: tools
  pass2_provider: anthropic
`
	if err := os.WriteFile(filepath.Join(tmp, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCRIBE_PASS2_MODE", "")
	t.Setenv("SCRIBE_PASS2_PROVIDER", "")
	t.Setenv("SCRIBE_PASS2_MODEL", "")
	cfg := loadConfig(tmp)
	if cfg.Absorb.Pass2Mode != "tools" {
		t.Errorf("Pass2Mode = %q, want tools (yaml wins on empty env)", cfg.Absorb.Pass2Mode)
	}
	if cfg.Absorb.Pass2Provider != "anthropic" {
		t.Errorf("Pass2Provider = %q, want anthropic (yaml wins on empty env)", cfg.Absorb.Pass2Provider)
	}
}

// TestPass2EnvAutoFlipStillWins verifies the post-override auto-flip
// catches a misconfigured SCRIBE_PASS2_MODE=tools with a non-anthropic
// provider — the env's "tools" loses to the auto-flip to "json".
func TestPass2EnvAutoFlipStillWins(t *testing.T) {
	tmp := t.TempDir()
	yaml := `owner_name: Test
absorb:
  pass2_provider: anthropic
`
	if err := os.WriteFile(filepath.Join(tmp, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	// User sets MODE=tools but PROVIDER=ollama via env — auto-flip should
	// pull mode back to json with a log line.
	t.Setenv("SCRIBE_PASS2_MODE", "tools")
	t.Setenv("SCRIBE_PASS2_PROVIDER", "ollama")
	cfg := loadConfig(tmp)
	if cfg.Absorb.Pass2Mode != "json" {
		t.Errorf("auto-flip should override SCRIBE_PASS2_MODE=tools to json when provider=ollama, got %q", cfg.Absorb.Pass2Mode)
	}
	if cfg.Absorb.Pass2Provider != "ollama" {
		t.Errorf("Pass2Provider = %q, want ollama", cfg.Absorb.Pass2Provider)
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
