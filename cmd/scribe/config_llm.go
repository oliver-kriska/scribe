// config_llm.go — LLM routing config: the top-level llm block, every
// per-op provider/model block (relations, dream, assess, deep_ingest,
// extract, session_mine, codex), and the shared inheritance helpers.
package main

import (
	"strings"
)

// LLMConfig is the top-level LLM-routing knob. Set once in scribe.yaml
// to flip the whole pipeline between Anthropic and Ollama; per-op
// fields override when set, otherwise inherit. The 100%-Ollama story
// is `llm.provider: ollama` plus a pre-pulled model — no per-op edits
// needed.
type LLMConfig struct {
	// Provider is "anthropic" (default) or "ollama". Unknown values
	// fall back to anthropic with a log line (see newLLMProvider).
	Provider string `yaml:"provider"`
	// Model is the default model name used when a per-op Model is
	// empty. For provider=anthropic this is a Claude alias ("haiku",
	// "sonnet"); for ollama it's an Ollama model tag
	// ("gemma3:4b", "qwen2.5-coder:14b"). Empty is legal —
	// inheritOLLAMAModel and applyAbsorbDefaults fill the recommended
	// local default when the user picks ollama but leaves model
	// blank.
	Model string `yaml:"model"`
	// OllamaURL points at the Ollama HTTP server. Default
	// http://localhost:11434. Per-op overrides survive (e.g. the
	// existing Absorb.Contextualize.OllamaURL); when both are unset
	// the resolver falls back to this value.
	OllamaURL string `yaml:"ollama_url"`
	// NumCtx is the global Ollama num_ctx default applied to any LLM
	// call that doesn't override per-op. Zero leaves the provider's
	// own default (8192) in place. The 100%-Ollama pipeline's bigger
	// passes (session-mine, dream, assess, deep) DO override per-op
	// with larger windows; this knob mainly exists for users who
	// want every call to run at, e.g., 16384 across the board.
	NumCtx int `yaml:"num_ctx"`
}

// RelationsConfig is the per-op routing for `scribe relations migrate`.
// Phase 4A.4: the classifier path no longer needs to live on `claude -p`
// — it's pure text-in / JSON-out. Empty fields inherit from LLMConfig.
type RelationsConfig struct {
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	OllamaURL string `yaml:"ollama_url"`
}

// DreamConfig routes the weekly dream cycle. Mode picks between
// "monolithic" (legacy: one `claude -p` runs the whole 4-phase
// protocol with tools for up to an hour) and "orchestrator" (Phase
// 4D: Go runs Phase 1/4 in pure code, the LLM only sees one
// EnvelopeV2 subtask for the consolidation/contradiction/stub work).
type DreamConfig struct {
	Provider   string `yaml:"provider"`
	Model      string `yaml:"model"`
	OllamaURL  string `yaml:"ollama_url"`
	Mode       string `yaml:"mode"`        // monolithic | orchestrator
	TimeoutMin int    `yaml:"timeout_min"` // per subtask cap
	// NumCtx overrides the global LLMConfig.NumCtx for this op only.
	// Dream's orient packet (recent log + inventory + stale list +
	// contradiction pairs) is sizable; 16384 is the safe floor on
	// Ollama. Empty → falls through to LLMConfig.NumCtx, then the
	// orchestrator's own per-op default.
	NumCtx int `yaml:"num_ctx"`
}

// AssessConfig and DeepConfig route the project-ingestion commands.
// Phase 4E ports both off `claude -p` with tools onto the envelope
// orchestrator pattern. Empty fields inherit from LLMConfig.
type AssessConfig struct {
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	OllamaURL string `yaml:"ollama_url"`
	// Mode picks "tools" (legacy) or "envelope" (Phase 4E). Empty
	// defaults to tools so existing `scribe assess` calls work
	// unchanged.
	Mode string `yaml:"mode"`
	// NumCtx overrides the global LLMConfig.NumCtx for the envelope
	// path. The assess prompt inlines top docs + source tree + git
	// log so 32768 is the recommended floor on Ollama; smaller
	// projects fit in 16384.
	NumCtx int `yaml:"num_ctx"`
}

// ExtractConfig routes the per-project extraction step inside
// `scribe sync`. Phase 4F port: extractProject historically ran
// `claude -p` with full tool access (Read/Write/Edit/Glob/Grep) to
// read the project, cross-reference the wiki, and write articles.
// Envelope mode mirrors deep_orchestrator — Go gathers the context
// (CLAUDE.md, README, changed files, drops), inlines it into one
// bounded prompt, and applies a single EnvelopeV2 back.
//
// Auto-flip: non-anthropic provider forces Mode=envelope because the
// tools path requires `claude -p`.
type ExtractConfig struct {
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	OllamaURL string `yaml:"ollama_url"`
	// Mode picks "tools" (legacy claude -p path) or "envelope" (Phase
	// 4F orchestrator). Empty defaults to "tools" for backward compat.
	Mode string `yaml:"mode"`
	// MaxFileChars caps the per-file body length inlined into the
	// envelope prompt. Default 8192 — keeps the prompt under control
	// even when a project has a long README.
	MaxFileChars int `yaml:"max_file_chars"`
	// MaxTotalChars caps the whole files block. Default 32000 —
	// pairs with NumCtx 16384 default. Files are read in priority
	// order (drops → CLAUDE.md → README → docs → changed) and stop
	// when the cap is hit.
	MaxTotalChars int `yaml:"max_total_chars"`
	// TimeoutMin caps a single envelope-mode call. Default 10 minutes.
	TimeoutMin int `yaml:"timeout_min"`
	// NumCtx overrides the global LLMConfig.NumCtx for the envelope
	// path. Default 16384 — fits the typical project context window.
	NumCtx int `yaml:"num_ctx"`
}

// DeepIngestConfig is the per-op config for `scribe deep`. Named
// DeepIngestConfig to avoid the existing DeepConfig.BatchMax knob.
type DeepIngestConfig struct {
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	OllamaURL string `yaml:"ollama_url"`
	Mode      string `yaml:"mode"`
	// NumCtx overrides the global LLMConfig.NumCtx for the envelope
	// path. Deep inlines per-directory file contents (cap 24K chars),
	// so 16384 is the recommended floor on Ollama.
	NumCtx int `yaml:"num_ctx"`
}

// SessionMineConfig routes the session-mining ops (session-mine,
// session-mine-batch, session-extract). Mode picks between "tools"
// (legacy `claude -p` with ccrider MCP) and "envelope" (Phase 4C
// orchestrator: Go reads transcripts, model emits one EnvelopeV2 per
// session). Empty fields inherit from LLMConfig.
//
// Auto-flip: when Provider is non-anthropic, Mode is forced to
// "envelope" — `claude -p` is the only backend that supports MCP
// tools today.
type SessionMineConfig struct {
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	OllamaURL string `yaml:"ollama_url"`
	// Mode picks the protocol: "tools" or "envelope". Default "tools"
	// preserves the historical behavior; users flip to "envelope" to
	// engage the Phase 4C orchestrator path (which is what makes the
	// local-Ollama story work).
	Mode string `yaml:"mode"`
	// TranscriptMaxChars caps the transcript size inlined into the
	// prompt. Defaults to 24000 (~6K tokens for most tokenizers).
	// The prompt skeleton + transcript + related-sessions block can
	// approach 8K tokens — pair with NumCtx ≥ 16384 (the default
	// applySessionMineDefaults sets) so Ollama doesn't silently
	// truncate the conclusion of the transcript.
	TranscriptMaxChars int `yaml:"transcript_max_chars"`
	// TimeoutMin caps a single envelope-mode call. Defaults to 8
	// minutes — local models on a 20K-char transcript take 4-7
	// minutes wallclock on Apple Silicon.
	TimeoutMin int `yaml:"timeout_min"`
	// NumCtx overrides the global LLMConfig.NumCtx for this op only.
	// 16384 is the floor for the default TranscriptMaxChars; bump
	// alongside any TranscriptMaxChars increase.
	NumCtx int `yaml:"num_ctx"`
}

// CodexConfig controls C3 Codex session mining: distilling Codex CLI
// rollouts into the KB via the same triage→envelope→wiki path ccrider
// sessions use. Discovery (0.2.15) and the AGENTS.md handshake
// (0.2.17) are always on; mining spends LLM tokens per session, so it
// is opt-in (matches absorb.atomic_facts' opt-in precedent and avoids
// surprising existing users' token budget on upgrade). The LLM
// provider/model/prompt are inherited from session_mine: — Codex
// mining is ccrider mining with the transcript source swapped, so it
// shares one config and one prompt family.
type CodexConfig struct {
	// Mine enables the pass. Default false — set `codex: { mine: true }`
	// to turn it on. No-op when codex_sessions_dir is absent (the
	// common case for users without Codex CLI), so leaving it on in a
	// shared default config would be harmless, but opt-in is the
	// safer, more predictable contract.
	Mine bool `yaml:"mine"`
	// SessionsMax caps mined Codex sessions per sync run (parallels
	// sync.max_sessions for ccrider). Default 3.
	SessionsMax int `yaml:"sessions_max"`
	// LookbackHours bounds the rollout scan to files modified within
	// the window — the durable processed-set in
	// wiki/_codex_sessions_log.json is the real dedup; this just keeps
	// a cron pass from stat-walking all of ~/.codex/ history. Default
	// 168 (7 days).
	LookbackHours int `yaml:"lookback_hours"`
	// MinScore is the scoreText threshold a rendered transcript must
	// clear to be worth an LLM extraction. Default 2 — a genuinely
	// useful coding session trips at least one weighted category.
	MinScore int `yaml:"min_score"`
}

// applyCodexDefaults fills zero-valued CodexConfig fields. Mine is
// left as-is (its zero value false IS the default — opt-in), so a
// user's explicit `mine: true` is the only way it turns on.
func applyCodexDefaults(cfg *CodexConfig) {
	if cfg.SessionsMax <= 0 {
		cfg.SessionsMax = 3
	}
	if cfg.LookbackHours <= 0 {
		cfg.LookbackHours = 168
	}
	if cfg.MinScore <= 0 {
		cfg.MinScore = 2
	}
}

// llmDefaults returns the canonical defaults for LLMConfig. Used by
// loadConfig to fill missing fields after yaml.Unmarshal. Provider
// defaults to anthropic so an empty `llm:` block reproduces the
// pre-4A.5 behavior exactly.
func llmDefaults() LLMConfig {
	return LLMConfig{
		Provider:  "anthropic",
		Model:     "",
		OllamaURL: defaultOllamaURL,
	}
}

// applyLLMDefaults fills any zero-valued LLMConfig field from the
// defaults. Also normalizes Claude/Ollama model aliases the same way
// the per-op resolvers do — so a user who flips `llm.provider: ollama`
// without updating `llm.model` ends up on the recommended local default
// rather than a misconfigured request that 404s at Ollama.
func applyLLMDefaults(cfg *LLMConfig) {
	d := llmDefaults()
	if cfg.Provider == "" {
		cfg.Provider = d.Provider
	}
	if cfg.OllamaURL == "" {
		cfg.OllamaURL = d.OllamaURL
	}
	cfg.Provider, cfg.Model = coerceProviderModel("llm", cfg.Provider, cfg.Model)
}

// coerceProviderModel normalizes a (provider, model) pair so callers
// don't have to repeat the ollama+Claude-alias swap that absorb,
// facts, and contextualize each open-coded. Empty model under ollama
// → recommended local default; Claude alias under ollama → recommended
// local default with a log line so the user notices the swap. Returns
// the (possibly modified) provider and model. The `label` argument is
// only used in the warning log so the user can locate the misconfig.
func coerceProviderModel(label, provider, model string) (string, string) {
	if !strings.EqualFold(provider, "ollama") {
		return provider, model
	}
	if model == "" || isClaudeModelAlias(model) {
		if model != "" {
			logMsg("config", "%s.provider=ollama but model=%q is a Claude alias — switching to %s (set an ollama model explicitly to silence this)", label, model, ollamaRecommendedModel)
		}
		model = ollamaRecommendedModel
	}
	return provider, model
}

// inheritProviderFromLLM returns the effective (provider, model,
// ollamaURL) for a per-op config that may leave any field empty. The
// fallback chain is: per-op value → LLMConfig value → llmDefaults().
// Returned values pass through coerceProviderModel so callers don't
// need to repeat the alias swap themselves.
//
// label is used only for the alias-swap warning so the user can find
// the misconfigured op in scribe.yaml ("absorb.pass2", "facts", ...).
func inheritProviderFromLLM(label string, opProvider, opModel, opOllamaURL string, top LLMConfig) (provider, model, ollamaURL string) {
	provider = opProvider
	if provider == "" {
		provider = top.Provider
	}
	model = opModel
	if model == "" {
		model = top.Model
	}
	ollamaURL = opOllamaURL
	if ollamaURL == "" {
		ollamaURL = top.OllamaURL
	}
	if ollamaURL == "" {
		ollamaURL = defaultOllamaURL
	}
	provider, model = coerceProviderModel(label, provider, model)
	return provider, model, ollamaURL
}

// inheritLLMOpBase fills the trio every per-op LLM block shares:
// provider/model/ollama_url inherit op value → top-level llm block →
// hard default. Pointer parameters because the per-op config structs
// are distinct types with identical field names. The caller keeps its
// own mode/timeout/num_ctx defaults — those genuinely differ per op.
func inheritLLMOpBase(provider, model, ollamaURL *string, llm LLMConfig) {
	if *provider == "" {
		*provider = llm.Provider
	}
	if *provider == "" {
		*provider = "anthropic"
	}
	if *model == "" {
		*model = llm.Model
	}
	if *ollamaURL == "" {
		*ollamaURL = llm.OllamaURL
	}
	if *ollamaURL == "" {
		*ollamaURL = defaultOllamaURL
	}
}

// inheritNumCtx fills num_ctx: op value → top-level llm → per-op floor.
// Only Ollama reads it; Anthropic ignores num_ctx entirely.
func inheritNumCtx(numCtx *int, llm LLMConfig, floor int) {
	if *numCtx <= 0 {
		*numCtx = llm.NumCtx
	}
	if *numCtx <= 0 {
		*numCtx = floor
	}
}

// forceNonAnthropicMode auto-flips an op's mode when its provider can't
// run the `claude -p` tools path (anything non-anthropic), logging once
// so the user notices the override.
func forceNonAnthropicMode(label, provider string, mode *string, forced string) {
	if !strings.EqualFold(provider, "anthropic") && !strings.EqualFold(*mode, forced) {
		logAutoFlipOnce(label+":"+provider, "config", "%s.provider=%q forces mode=%s (was %q)", label, provider, forced, *mode)
		*mode = forced
	}
}

// applyRelationsDefaults fills RelationsConfig from LLMConfig + sane
// defaults (Phase 4A.4/4A.5). Empty fields inherit; alias swap runs
// at the end so a Claude alias under ollama is rewritten cleanly.
func applyRelationsDefaults(cfg *RelationsConfig, llm LLMConfig) {
	inheritLLMOpBase(&cfg.Provider, &cfg.Model, &cfg.OllamaURL, llm)
	cfg.Provider, cfg.Model = coerceProviderModel("relations", cfg.Provider, cfg.Model)
}

// applyDreamDefaults fills DreamConfig. Mode defaults to monolithic
// for backward compat; non-anthropic provider auto-flips to
// orchestrator (Phase 4D) because the legacy tools path needs
// `claude -p`.
func applyDreamDefaults(cfg *DreamConfig, llm LLMConfig) {
	inheritLLMOpBase(&cfg.Provider, &cfg.Model, &cfg.OllamaURL, llm)
	if cfg.Mode == "" {
		cfg.Mode = "monolithic"
	}
	if cfg.TimeoutMin <= 0 {
		cfg.TimeoutMin = 20
	}
	// Dream's orient packet is big (log tail + inventory + stale list +
	// contradictions). 16384 keeps the conclusion of the inventory from
	// being truncated.
	inheritNumCtx(&cfg.NumCtx, llm, 16384)
	forceNonAnthropicMode("dream", cfg.Provider, &cfg.Mode, "orchestrator")
	cfg.Provider, cfg.Model = coerceProviderModel("dream", cfg.Provider, cfg.Model)
}

// applyAssessDefaults / applyDeepIngestDefaults: Phase 4E. Same shape
// as Dream — Mode defaults to tools for backward compat, non-anthropic
// provider auto-flips to envelope mode.
func applyAssessDefaults(cfg *AssessConfig, llm LLMConfig) {
	inheritLLMOpBase(&cfg.Provider, &cfg.Model, &cfg.OllamaURL, llm)
	if cfg.Mode == "" {
		cfg.Mode = "tools"
	}
	// Assess inlines top-level docs (12K char cap), source tree (200
	// entries), and recent git log. Even with truncation this routinely
	// lands at 6-8K tokens.
	inheritNumCtx(&cfg.NumCtx, llm, 32768)
	forceNonAnthropicMode("assess", cfg.Provider, &cfg.Mode, "envelope")
	cfg.Provider, cfg.Model = coerceProviderModel("assess", cfg.Provider, cfg.Model)
}

func applyDeepIngestDefaults(cfg *DeepIngestConfig, llm LLMConfig) {
	inheritLLMOpBase(&cfg.Provider, &cfg.Model, &cfg.OllamaURL, llm)
	if cfg.Mode == "" {
		cfg.Mode = "tools"
	}
	// Deep inlines per-directory file contents capped at 24K chars
	// (~6K tokens) — pair with 16384 to leave headroom for the prompt
	// and the envelope output.
	inheritNumCtx(&cfg.NumCtx, llm, 16384)
	forceNonAnthropicMode("deep_ingest", cfg.Provider, &cfg.Mode, "envelope")
	cfg.Provider, cfg.Model = coerceProviderModel("deep_ingest", cfg.Provider, cfg.Model)
}

// applyExtractDefaults fills ExtractConfig (Phase 4F). Same shape as
// Dream/Assess/Deep — Mode defaults to tools for backward compat, but
// a non-anthropic provider auto-flips to envelope.
func applyExtractDefaults(cfg *ExtractConfig, llm LLMConfig) {
	inheritLLMOpBase(&cfg.Provider, &cfg.Model, &cfg.OllamaURL, llm)
	if cfg.Mode == "" {
		cfg.Mode = "tools"
	}
	if cfg.MaxFileChars <= 0 {
		cfg.MaxFileChars = 8192
	}
	if cfg.MaxTotalChars <= 0 {
		cfg.MaxTotalChars = 32000
	}
	if cfg.TimeoutMin <= 0 {
		cfg.TimeoutMin = 10
	}
	inheritNumCtx(&cfg.NumCtx, llm, 16384)
	forceNonAnthropicMode("extract", cfg.Provider, &cfg.Mode, "envelope")
	cfg.Provider, cfg.Model = coerceProviderModel("extract", cfg.Provider, cfg.Model)
}

// applySessionMineDefaults fills SessionMineConfig + the Phase 4C
// auto-flip: a non-anthropic provider forces envelope mode because
// `claude -p` is the only backend that supports the ccrider MCP tools
// the legacy path relies on.
func applySessionMineDefaults(cfg *SessionMineConfig, llm LLMConfig) {
	inheritLLMOpBase(&cfg.Provider, &cfg.Model, &cfg.OllamaURL, llm)
	if cfg.Mode == "" {
		cfg.Mode = "tools"
	}
	if cfg.TranscriptMaxChars <= 0 {
		cfg.TranscriptMaxChars = 24000
	}
	if cfg.TimeoutMin <= 0 {
		cfg.TimeoutMin = 8
	}
	// Default TranscriptMaxChars 24000 + prompt skeleton + related
	// sessions block lands at 6-7K tokens. 16384 is the floor; raising
	// TranscriptMaxChars without raising NumCtx silently truncates the
	// conclusion of the transcript on Ollama.
	inheritNumCtx(&cfg.NumCtx, llm, 16384)
	// Non-anthropic provider has no MCP support → force envelope mode.
	forceNonAnthropicMode("session_mine", cfg.Provider, &cfg.Mode, "envelope")
	cfg.Provider, cfg.Model = coerceProviderModel("session_mine", cfg.Provider, cfg.Model)
}

// relationsProviderModel returns the (provider, model, ollamaURL) the
// relations-migrate classifier should use. Callers pass the CLI's
// --model flag as the explicit pin; when that's a Claude alias and the
// user has flipped `llm.provider: ollama`, the recommended local model
// wins. defaultModel falls back to "haiku" when both the CLI flag and
// config are empty (matches pre-4A.4 default).
func relationsProviderModel(root, cliModel string) (provider, model, ollamaURL string) {
	cfg := loadConfig(root)
	if cfg == nil {
		return "anthropic", cliModel, defaultOllamaURL
	}
	// The CLI flag wins over RelationsConfig.Model when set, so users
	// can A/B without editing scribe.yaml. Empty flag → fall through
	// to config inheritance.
	opModel := cliModel
	if opModel == "" {
		opModel = cfg.Relations.Model
	}
	provider, model, ollamaURL = inheritProviderFromLLM("relations", cfg.Relations.Provider, opModel, cfg.Relations.OllamaURL, cfg.LLM)
	if model == "" {
		// Final fallback: haiku is what the pre-4A.4 CLI used as its
		// default-of-defaults. For ollama, coerceProviderModel
		// already replaced empty with ollamaRecommendedModel above.
		model = "haiku"
	}
	return provider, model, ollamaURL
}

// ollamaRecommendedModel is the scribe-vetted default when the user picks
// provider=ollama but leaves model blank (or accidentally copies a Claude
// alias). Chosen in research/2026-04-20-ollama-model-for-contextualize.md
// for best speed/quality balance on Apple Silicon at ~3.3 GB Q4.
const ollamaRecommendedModel = "gemma3:4b"

// defaultOllamaURL is the stock local Ollama server address — the final
// fallback whenever neither a per-op block nor the top-level llm block
// sets ollama_url.
const defaultOllamaURL = "http://localhost:11434"

// claudeModelAliases are the model names `claude -p` accepts. Anything in
// this list is a red flag when the provider is ollama — the user probably
// switched provider without updating model.
//
// Use isClaudeModelAlias() instead of this map directly — it covers prefix
// patterns (claude-*, *-sonnet, *-haiku, *-opus) so future model names
// (claude-4-7, claude-5-haiku, …) don't silently slip past the swap.
var claudeModelAliases = map[string]bool{
	"haiku":             true,
	"sonnet":            true,
	"opus":              true,
	"claude-haiku":      true,
	"claude-sonnet":     true,
	"claude-opus":       true,
	"claude-3-5-haiku":  true,
	"claude-3-5-sonnet": true,
	"claude-4-5-haiku":  true,
	"claude-4-5-sonnet": true,
}

// isClaudeModelAlias reports whether `model` looks like a Claude alias
// the user might have left in place after flipping `llm.provider: ollama`.
// Covers the explicit list in claudeModelAliases plus three prefix shapes
// that future model names are guaranteed to match:
//
//   - claude-*            ("claude-4-7", "claude-5-haiku-20270101")
//   - *-sonnet / *-haiku  ("claude-x-sonnet", "tier-haiku")
//   - *-opus              ("claude-y-opus")
//
// Ollama model names use the family:tag form (e.g. "qwen2.5-coder:14b"),
// so an Ollama model that contains a colon never matches this heuristic.
func isClaudeModelAlias(model string) bool {
	if model == "" {
		return false
	}
	m := strings.ToLower(strings.TrimSpace(model))
	if claudeModelAliases[m] {
		return true
	}
	// Ollama models always carry a `:tag` — colon means it's not Claude.
	if strings.Contains(m, ":") {
		return false
	}
	if strings.HasPrefix(m, "claude-") {
		return true
	}
	if strings.HasSuffix(m, "-sonnet") || strings.HasSuffix(m, "-haiku") || strings.HasSuffix(m, "-opus") {
		return true
	}
	return false
}
