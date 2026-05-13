package main

import (
	"strings"
	"testing"
)

// Phase 4B layer 2: pass2_provider defaults to anthropic and pass2_mode
// to tools, so an unset config behaves exactly like the pre-4B world.
func TestApplyAbsorbDefaults_Pass2DefaultsAnthropicTools(t *testing.T) {
	cfg := AbsorbConfig{}
	applyAbsorbDefaults(&cfg)
	if cfg.Pass2Provider != "anthropic" {
		t.Errorf("Pass2Provider default = %q, want anthropic", cfg.Pass2Provider)
	}
	if cfg.Pass2Mode != "tools" {
		t.Errorf("Pass2Mode default = %q, want tools", cfg.Pass2Mode)
	}
}

// A non-anthropic provider must auto-flip mode to json — the tools
// path only works with `claude -p`, so leaving it at "tools" would
// silently no-op every pass-2 call.
func TestApplyAbsorbDefaults_Pass2OllamaForcesJSONMode(t *testing.T) {
	cfg := AbsorbConfig{Pass2Provider: "ollama"}
	applyAbsorbDefaults(&cfg)
	if cfg.Pass2Mode != "json" {
		t.Errorf("ollama provider should force pass2_mode=json, got %q", cfg.Pass2Mode)
	}
}

// Explicit pass2_mode=json with ollama is honored (no log spam about
// override). The mode field stays "json" and we verify the provider
// fixup still ran on the model.
func TestApplyAbsorbDefaults_Pass2OllamaSwapsClaudeAlias(t *testing.T) {
	cfg := AbsorbConfig{Pass2Provider: "ollama", Pass2Model: "sonnet"}
	applyAbsorbDefaults(&cfg)
	if cfg.Pass2Model == "sonnet" {
		t.Errorf("ollama+sonnet should auto-swap, got %q", cfg.Pass2Model)
	}
	if cfg.Pass2Model != ollamaRecommendedModel {
		t.Errorf("Pass2Model after coherence fixup = %q, want %s", cfg.Pass2Model, ollamaRecommendedModel)
	}
}

// A valid (non-Claude) ollama model survives the coherence fixup.
func TestApplyAbsorbDefaults_Pass2OllamaPreservesValidModel(t *testing.T) {
	cfg := AbsorbConfig{Pass2Provider: "ollama", Pass2Model: "qwen2.5-coder:14b"}
	applyAbsorbDefaults(&cfg)
	if cfg.Pass2Model != "qwen2.5-coder:14b" {
		t.Errorf("valid ollama model should survive; got %q", cfg.Pass2Model)
	}
}

// The pass2 JSON prompt loads and every documented placeholder is
// substituted. A leftover {{NAME}} in the rendered template means the
// goroutine in absorbDenseTwoPass forgot to inline a variable, which
// would feed the model literal placeholder text — silent quality loss.
func TestPass2JSONPromptPlaceholdersFilled(t *testing.T) {
	vars := map[string]string{
		"RAW_FILE":          "/tmp/kb/raw/articles/foo.md",
		"ENTITY_LABEL":      "Test Entity",
		"ENTITY_TYPE":       "pattern",
		"ENTITY_ONE_LINE":   "A test entity used for unit tests.",
		"ENTITY_KEY_CLAIMS": "claim 1 | claim 2",
		"DOMAIN":            "general",
		"FACTS":             "[c00-f1, claim] something (\"anchor\")",
		"RAW_BODY":          "# Raw\n\nBody text here.",
		"PLAN_JSON":         `{"entities":[]}`,
		"TODAY":             "2026-05-13",
	}
	out, err := loadPrompt("absorb-pass2-json.md", vars)
	if err != nil {
		t.Fatalf("loadPrompt: %v", err)
	}
	// Sanity: each value appears at least once.
	for k, v := range vars {
		if !strings.Contains(out, v) {
			t.Errorf("rendered prompt missing substitution for %s (value %q)", k, v)
		}
	}
	// No leftover placeholder syntax — every {{...}} in the template
	// must correspond to a vars key.
	if idx := strings.Index(out, "{{"); idx >= 0 {
		end := min(idx+80, len(out))
		t.Errorf("unsubstituted placeholder remains at offset %d: %q", idx, out[idx:end])
	}
}
