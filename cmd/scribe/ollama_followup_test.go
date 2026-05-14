package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ollama_followup_test.go covers the 100%-Ollama follow-up patches:
// each test maps to a finding number in the post-implementation
// review (see commit message / docs/100-percent-ollama-plan.md).

// --- A1 / A3: time.Time + alias prefix detection ---

func TestStringFromAnyDate(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string passthrough", "2026-05-14", "2026-05-14"},
		{"time.Time formats as YYYY-MM-DD", time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC), "2026-05-14"},
		{"nil renders empty", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stringFromAny(c.in)
			if got != c.want {
				t.Errorf("stringFromAny(%#v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestIsClaudeModelAlias(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"haiku", true},
		{"sonnet", true},
		{"opus", true},
		{"claude-3-5-sonnet", true},
		{"claude-4-5-haiku", true},
		// Future names — prefix detection
		{"claude-4-7", true},
		{"claude-5-haiku", true},
		// Suffix detection
		{"some-future-sonnet", true},
		{"tiny-haiku", true},
		// Ollama models (carry a tag) must never match
		{"gemma3:4b", false},
		{"qwen2.5-coder:14b", false},
		{"haiku:custom", false},
		{"", false},
		{"random-model", false},
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			if got := isClaudeModelAlias(c.model); got != c.want {
				t.Errorf("isClaudeModelAlias(%q) = %v, want %v", c.model, got, c.want)
			}
		})
	}
}

func TestCoerceProviderModelSwapsClaudeAliasOnOllama(t *testing.T) {
	cases := []struct {
		name      string
		provider  string
		model     string
		wantModel string
	}{
		{"anthropic + sonnet stays put", "anthropic", "sonnet", "sonnet"},
		{"ollama + empty fills recommended", "ollama", "", ollamaRecommendedModel},
		{"ollama + sonnet swaps to recommended", "ollama", "sonnet", ollamaRecommendedModel},
		{"ollama + future claude name swaps", "ollama", "claude-4-7-sonnet", ollamaRecommendedModel},
		{"ollama + real ollama model stays put", "ollama", "qwen2.5-coder:14b", "qwen2.5-coder:14b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, gotModel := coerceProviderModel("test", c.provider, c.model)
			if gotModel != c.wantModel {
				t.Errorf("coerce(%s, %s) model = %q, want %q", c.provider, c.model, gotModel, c.wantModel)
			}
		})
	}
}

// --- A4: providerNameFor type-assertion ---

func TestProviderNameForUsesTypeAssertion(t *testing.T) {
	ollama := &ollamaProvider{baseURL: "http://localhost:11434", model: "gemma3:4b"}
	if got := providerNameFor(ollama); got != "ollama" {
		t.Errorf("ollama provider → %q, want ollama", got)
	}
	anthropic := &anthropicProvider{model: "sonnet"}
	if got := providerNameFor(anthropic); got != "anthropic" {
		t.Errorf("anthropic provider → %q, want anthropic", got)
	}
	if got := providerNameFor(nil); got != "anthropic" {
		t.Errorf("nil → %q, want anthropic", got)
	}
}

// --- A6: promptForProvider fallback behavior ---

func TestPromptForProviderPicksOllamaVariant(t *testing.T) {
	// session-mine has both -anthropic.md and -ollama.md variants.
	if got := promptForProvider("session-mine", "ollama"); got != "session-mine-ollama.md" {
		t.Errorf("got %q, want session-mine-ollama.md", got)
	}
	if got := promptForProvider("session-mine", "anthropic"); got != "session-mine-anthropic.md" {
		t.Errorf("got %q, want session-mine-anthropic.md", got)
	}
}

func TestPromptForProviderFallsBackToLegacy(t *testing.T) {
	// dream.md is the legacy prompt — there is no dream-anthropic.md
	// variant at the time of writing for monolithic mode? Actually
	// the followup pass did land prompts/dream-anthropic.md +
	// prompts/dream-ollama.md. Use a deliberately-unknown base to
	// exercise the fallback.
	got := promptForProvider("definitely-not-a-real-base", "ollama")
	if got != "definitely-not-a-real-base.md" {
		t.Errorf("got %q, want fallback to definitely-not-a-real-base.md", got)
	}
}

// --- B3: parseEnvelopeV2 version handling ---

func TestParseEnvelopeV2AcceptsV1(t *testing.T) {
	js := `{"actions":[{"op":"create","path":"wiki/x.md","content":"---\ntitle: x\n---\n"}]}`
	env, err := parseEnvelopeV2(js, "test")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if env.Version != 0 {
		t.Errorf("expected V1 version=0, got %d", env.Version)
	}
}

func TestParseEnvelopeV2AcceptsV2WithMeta(t *testing.T) {
	js := `{"version":2,"actions":[],"meta":[{"op":"sessions_log_append","session_id":"abc"}]}`
	env, err := parseEnvelopeV2(js, "test")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if env.Version != 2 {
		t.Errorf("want version=2, got %d", env.Version)
	}
	if len(env.Meta) != 1 {
		t.Errorf("want 1 meta op, got %d", len(env.Meta))
	}
}

func TestParseEnvelopeV2AcceptsFutureVersion(t *testing.T) {
	// version=99 should warn (in logs) but parse — be forward-compat.
	js := `{"version":99,"actions":[{"op":"create","path":"wiki/x.md","content":"y"}]}`
	if _, err := parseEnvelopeV2(js, "test"); err != nil {
		t.Errorf("expected lenient acceptance of future version, got %v", err)
	}
}

func TestParseEnvelopeAllowEmptyAcceptsEmptyActions(t *testing.T) {
	js := `{"version":2,"actions":[],"meta":[{"op":"sessions_log_append","session_id":"x"}]}`
	env, err := parseEnvelopeAllowEmpty(js)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(env.Actions) != 0 {
		t.Errorf("want empty actions, got %d", len(env.Actions))
	}
}

func TestParseEnvelopeAllowEmptyRejectsMissingFields(t *testing.T) {
	js := `{"version":2,"actions":[{"op":"create"}]}` // missing path
	if _, err := parseEnvelopeAllowEmpty(js); err == nil {
		t.Errorf("expected error for action missing path")
	}
}

// --- C1: num_ctx context plumbing ---

func TestWithOllamaNumCtx(t *testing.T) {
	ctx := context.Background()
	if got := ollamaNumCtxFromContext(ctx); got != 0 {
		t.Errorf("bare ctx → %d, want 0", got)
	}
	tagged := withOllamaNumCtx(ctx, 16384)
	if got := ollamaNumCtxFromContext(tagged); got != 16384 {
		t.Errorf("tagged ctx → %d, want 16384", got)
	}
	// Zero should be no-op (don't override the provider default).
	noop := withOllamaNumCtx(ctx, 0)
	if got := ollamaNumCtxFromContext(noop); got != 0 {
		t.Errorf("zero tag should leave ctx unchanged, got %d", got)
	}
}

// --- D1: configurable rolling_targets ---

func TestApplyMetaDefaultsFallsBackToHistoricalPair(t *testing.T) {
	cfg := &MetaConfig{}
	applyMetaDefaults(cfg)
	if len(cfg.RollingTargets) != 2 {
		t.Fatalf("default RollingTargets len = %d, want 2", len(cfg.RollingTargets))
	}
	got := strings.Join(cfg.RollingTargets, ",")
	if got != "learnings,decisions-log" {
		t.Errorf("default targets = %q, want learnings,decisions-log", got)
	}
}

func TestApplyMetaDefaultsHonorsUserList(t *testing.T) {
	cfg := &MetaConfig{RollingTargets: []string{"learnings", "incidents", "migrations-log"}}
	applyMetaDefaults(cfg)
	if len(cfg.RollingTargets) != 3 {
		t.Errorf("want 3 targets, got %d", len(cfg.RollingTargets))
	}
}

func TestApplyMetaDefaultsDropsPathTraversal(t *testing.T) {
	cfg := &MetaConfig{RollingTargets: []string{"learnings", "../escape", "good/bad", "ok"}}
	applyMetaDefaults(cfg)
	for _, t2 := range cfg.RollingTargets {
		if strings.ContainsAny(t2, "/\\") || strings.Contains(t2, "..") {
			t.Errorf("unsafe stem %q survived filter", t2)
		}
	}
	if len(cfg.RollingTargets) != 2 { // learnings + ok
		t.Errorf("want 2 safe targets, got %d (%v)", len(cfg.RollingTargets), cfg.RollingTargets)
	}
}

func TestApplyMetaDefaultsDeduplicates(t *testing.T) {
	cfg := &MetaConfig{RollingTargets: []string{"learnings", "learnings", "decisions-log"}}
	applyMetaDefaults(cfg)
	if len(cfg.RollingTargets) != 2 {
		t.Errorf("want 2 unique targets, got %d (%v)", len(cfg.RollingTargets), cfg.RollingTargets)
	}
}

// --- D1: rolling_memory_append accepts a user-configured target ---

func TestApplyMetaRollingAppendAcceptsConfiguredTarget(t *testing.T) {
	root := t.TempDir()
	// Drop a scribe.yaml that declares "incidents" as a rolling target.
	yaml := `
domains: [personal, general]
meta:
  rolling_targets: [learnings, decisions-log, incidents]
`
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write scribe.yaml: %v", err)
	}
	t.Setenv("SCRIBE_KB", root)
	env := WikiActionEnvelope{
		Version: 2,
		Meta: []MetaAction{
			{Op: "rolling_memory_append", Domain: "general", Target: "incidents", Content: "p99 spiked at 18:30."},
		},
	}
	res, err := applyWikiActions(root, env, ApplyOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	path := filepath.Join(root, "projects", "general", "incidents.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read incidents.md: %v", err)
	}
	if !strings.Contains(string(data), "p99 spiked") {
		t.Errorf("incidents.md missing content: %q", string(data))
	}
}

func TestApplyMetaRollingAppendRejectsTargetNotInConfig(t *testing.T) {
	root := t.TempDir()
	// Default config (no scribe.yaml) → only {learnings, decisions-log}.
	t.Setenv("SCRIBE_KB", root)
	env := WikiActionEnvelope{
		Version: 2,
		Meta: []MetaAction{
			{Op: "rolling_memory_append", Domain: "general", Target: "incidents", Content: "..."},
		},
	}
	res, _ := applyWikiActions(root, env, ApplyOptions{})
	if len(res.Errors) == 0 {
		t.Errorf("expected error for target not in default allow-list")
	}
}

// --- A2: tool role rendering ---

func TestRoleFromCcriderTypeRecognizesTool(t *testing.T) {
	cases := map[string]string{
		"user":        "user",
		"assistant":   "assistant",
		"system":      "system",
		"tool":        "tool",
		"tool_use":    "tool",
		"tool_result": "tool",
		"unknown":     "system",
	}
	for in, want := range cases {
		if got := roleFromCcriderType(in); got != want {
			t.Errorf("roleFromCcriderType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderTranscriptForPromptIncludesToolHeader(t *testing.T) {
	turns := []sessionTurn{
		{Role: "user", Text: "what files?"},
		{Role: "tool", ToolText: `{"command":"ls"}`},
		{Role: "assistant", Text: "Three: a, b, c."},
	}
	out := renderTranscriptForPrompt(turns, 0)
	if !strings.Contains(out, "## TOOL") {
		t.Errorf("rendered transcript missing TOOL header: %s", out)
	}
}

// --- A5 inheritProviderFromLLM chain ---

func TestInheritProviderFromLLMFallsThroughEmptyOp(t *testing.T) {
	llm := LLMConfig{Provider: "ollama", Model: "qwen2.5-coder:14b", OllamaURL: "http://localhost:11434"}
	p, m, u := inheritProviderFromLLM("test", "", "", "", llm)
	if p != "ollama" || m != "qwen2.5-coder:14b" || u != "http://localhost:11434" {
		t.Errorf("inheritance broken: got (%q, %q, %q)", p, m, u)
	}
}

func TestInheritProviderFromLLMOpOverridesTopLevel(t *testing.T) {
	llm := LLMConfig{Provider: "ollama", Model: "gemma3:4b", OllamaURL: "http://localhost:11434"}
	p, m, _ := inheritProviderFromLLM("test", "anthropic", "sonnet", "", llm)
	if p != "anthropic" {
		t.Errorf("op provider should override top-level: got %q", p)
	}
	if m != "sonnet" {
		t.Errorf("op model should override top-level: got %q", m)
	}
}

func TestInheritProviderFromLLMSwapsClaudeAliasInOllamaContext(t *testing.T) {
	llm := LLMConfig{Provider: "ollama", Model: "", OllamaURL: ""}
	// Op leaves model empty → inherits top-level (also empty) → coerce
	// fills the recommended local default.
	_, m, _ := inheritProviderFromLLM("test", "", "", "", llm)
	if m != ollamaRecommendedModel {
		t.Errorf("expected ollamaRecommendedModel, got %q", m)
	}
	// Op pins a Claude alias against the ollama provider — alias-swap
	// must kick in.
	_, m2, _ := inheritProviderFromLLM("test", "ollama", "sonnet", "", llm)
	if m2 != ollamaRecommendedModel {
		t.Errorf("alias swap missed: got %q", m2)
	}
}

// --- D1: promptBaseForSessionLabel routing ---

func TestPromptBaseForSessionLabelRouting(t *testing.T) {
	cases := map[string]string{
		"session-mine":          "session-mine",
		"session-mine-batch":    "session-mine",
		"session-extract":       "session-extract",
		"session-extract-large": "session-extract",
		"random":                "session-mine",
	}
	for in, want := range cases {
		if got := promptBaseForSessionLabel(in); got != want {
			t.Errorf("promptBaseForSessionLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- Absorb num_ctx defaults: pass2 and single_pass paths ---
//
// Pass-2 (gemma3:27b in scriptorium's config) inlines the raw article,
// the entity plan JSON, the entity block, and the facts block into a
// single prompt. At Ollama's default num_ctx=8192 the prompt's tail
// silently truncates. These tests pin the fallback chain:
//
//	per-op value (Pass2NumCtx)  →  LLMConfig.NumCtx  →  16384

func TestApplyAbsorbDefaults_Pass2NumCtxFallbackChain(t *testing.T) {
	cases := []struct {
		name      string
		opVal     int
		llmCtx    int
		wantPass2 int
		wantSP    int
	}{
		{"empty everywhere defaults to 16384", 0, 0, 16384, 16384},
		{"llm.num_ctx inherits when op empty", 0, 24576, 24576, 24576},
		{"per-op value wins over llm.num_ctx", 32768, 24576, 32768, 24576},
		{"per-op SinglePass independent of Pass2", 0, 0, 16384, 16384},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := AbsorbConfig{Pass2NumCtx: c.opVal}
			applyAbsorbDefaultsWithLLM(&cfg, LLMConfig{NumCtx: c.llmCtx})
			if cfg.Pass2NumCtx != c.wantPass2 {
				t.Errorf("Pass2NumCtx = %d, want %d", cfg.Pass2NumCtx, c.wantPass2)
			}
			if cfg.SinglePassNumCtx != c.wantSP {
				t.Errorf("SinglePassNumCtx = %d, want %d", cfg.SinglePassNumCtx, c.wantSP)
			}
		})
	}
}

func TestApplyAbsorbDefaults_SinglePassNumCtxIndependent(t *testing.T) {
	cfg := AbsorbConfig{SinglePassNumCtx: 32768}
	applyAbsorbDefaultsWithLLM(&cfg, LLMConfig{NumCtx: 16384})
	if cfg.SinglePassNumCtx != 32768 {
		t.Errorf("SinglePassNumCtx = %d, want 32768 (per-op wins)", cfg.SinglePassNumCtx)
	}
	if cfg.Pass2NumCtx != 16384 {
		t.Errorf("Pass2NumCtx = %d, want 16384 (inherits llm.num_ctx)", cfg.Pass2NumCtx)
	}
}

// --- Dream effective-model log line (orchestrator vs monolithic) ---
//
// The "starting dream cycle (model: …)" log used to print the CLI
// flag's default ("sonnet") regardless of mode, even when the resolved
// orchestrator path was running gemma3:12b on Ollama. The fix routes
// the log through cfg.Dream when mode=orchestrator so it stays honest.

func TestApplyDreamDefaults_OrchestratorResolvesProviderModel(t *testing.T) {
	cfg := DreamConfig{}
	applyDreamDefaults(&cfg, LLMConfig{Provider: "ollama", Model: "gemma3:12b"})
	if cfg.Provider != "ollama" {
		t.Errorf("Provider = %q, want ollama", cfg.Provider)
	}
	if cfg.Model != "gemma3:12b" {
		t.Errorf("Model = %q, want gemma3:12b", cfg.Model)
	}
	if !strings.EqualFold(cfg.Mode, "orchestrator") {
		t.Errorf("Mode = %q, want orchestrator (auto-flip on non-anthropic)", cfg.Mode)
	}
}

// LLMConfig.Model inheritance: dream/assess/deep_ingest/session_mine/
// relations all forgot to copy llm.model when their per-op Model was
// empty, so `llm.model: gemma3:12b` silently fell through to the
// coerceProviderModel default (gemma3:4b) for every envelope op. These
// tests pin the inheritance so the regression can't sneak back.
//
// Per-op Model wins over llm.Model when both are set.

func TestApplyDefaults_InheritsLLMModelEverywhere(t *testing.T) {
	llm := LLMConfig{Provider: "ollama", Model: "gemma3:12b"}

	d := DreamConfig{}
	applyDreamDefaults(&d, llm)
	if d.Model != "gemma3:12b" {
		t.Errorf("dream.Model = %q, want gemma3:12b", d.Model)
	}

	a := AssessConfig{}
	applyAssessDefaults(&a, llm)
	if a.Model != "gemma3:12b" {
		t.Errorf("assess.Model = %q, want gemma3:12b", a.Model)
	}

	di := DeepIngestConfig{}
	applyDeepIngestDefaults(&di, llm)
	if di.Model != "gemma3:12b" {
		t.Errorf("deep_ingest.Model = %q, want gemma3:12b", di.Model)
	}

	sm := SessionMineConfig{}
	applySessionMineDefaults(&sm, llm)
	if sm.Model != "gemma3:12b" {
		t.Errorf("session_mine.Model = %q, want gemma3:12b", sm.Model)
	}

	r := RelationsConfig{}
	applyRelationsDefaults(&r, llm)
	if r.Model != "gemma3:12b" {
		t.Errorf("relations.Model = %q, want gemma3:12b", r.Model)
	}
}

func TestApplyDefaults_PerOpModelWinsOverLLM(t *testing.T) {
	llm := LLMConfig{Provider: "ollama", Model: "gemma3:12b"}
	d := DreamConfig{Model: "qwen2.5:14b"}
	applyDreamDefaults(&d, llm)
	if d.Model != "qwen2.5:14b" {
		t.Errorf("dream.Model = %q, want qwen2.5:14b (per-op wins)", d.Model)
	}
}
