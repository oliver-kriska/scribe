// config_absorb.go — the absorb pipeline's config block: density
// thresholds, pass models/providers, contextualize, facts, and the
// scribe.yaml backfill of the commented defaults block.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AbsorbConfig controls the absorb pipeline. All fields are optional; zero
// values fall back to defaults set in loadConfig. Users override in
// scribe.yaml under the `absorb:` key.
//
// Strictness: "low" = everything; "medium" (default) = all unabsorbed raw
// articles; "high" = only sources with `absorb: true` in frontmatter or a
// named (non-"general") domain tag.
//
// Density thresholds control classifyDensity and, transitively, whether
// absorb branches into the two-pass (entity-first) path or stays single-
// pass. A source is "dense" when word_count >= DenseThresholdWords OR
// heading_count >= DenseThresholdHeadings; "brief" when word_count <
// BriefThresholdWords AND heading_count <= BriefThresholdHeadings; else
// "standard".
//
// Model/timeout overrides let tenants tune cost vs quality. Pass2Model ""
// inherits the sync-level default_model (typically Sonnet).
type AbsorbConfig struct {
	Strictness             string              `yaml:"strictness"`
	MaxPerRun              int                 `yaml:"max_per_run"`
	DenseThresholdWords    int                 `yaml:"dense_threshold_words"`
	DenseThresholdHeadings int                 `yaml:"dense_threshold_headings"`
	BriefThresholdWords    int                 `yaml:"brief_threshold_words"`
	BriefThresholdHeadings int                 `yaml:"brief_threshold_headings"`
	Pass1Model             string              `yaml:"pass1_model"`
	Pass2Model             string              `yaml:"pass2_model"`
	Pass1TimeoutMin        int                 `yaml:"pass1_timeout_min"`
	Pass2TimeoutMin        int                 `yaml:"pass2_timeout_min"`
	Pass2Parallel          int                 `yaml:"pass2_parallel"`
	SinglePassTimeoutMin   int                 `yaml:"single_pass_timeout_min"`
	Contextualize          ContextualizeConfig `yaml:"contextualize"`

	// ChapterAware turns on the Phase 3A.5 chapter-iteration path:
	// when a raw article has a TOC sidecar with at least
	// ChapterThreshold chapters, pass-1 fans out across chapters in
	// parallel and merges the per-chapter plans before pass-2.
	// Pointer so an explicit `false` in scribe.yaml wins over the
	// default `true`.
	ChapterAware     *bool `yaml:"chapter_aware"`
	ChapterThreshold int   `yaml:"chapter_threshold"`
	// ChapterParallel caps concurrent claude -p calls during the
	// chaptered fan-out (facts pass and pass-1 chapter pass). On a
	// 60-chapter PDF the previous behavior of reusing Pass2Parallel
	// stacked 3 facts + 3 pass-1 + 3 pass-2 in flight, which made
	// rate-limit cascades easy to trigger. Default 2 is conservative
	// enough that haiku's quotas hold even on long documents.
	ChapterParallel int `yaml:"chapter_parallel"`

	// AtomicFacts turns on Phase 3B atomic-fact extraction. When on
	// (and the article qualifies for chaptered absorb), a per-chunk
	// facts pass runs before pass-1; each chunk's atomic claims get
	// merged into output/facts/<slug>.json AND injected into that
	// chunk's pass-1 prompt as grounding evidence. Off by default
	// because the cost is +1 cheap haiku call per chunk and the
	// quality win has to land per-document — not a free lunch.
	// FactsModel inherits from Pass1Model when empty; haiku is the
	// recommended pin for the facts pass since the work is
	// extraction, not reasoning.
	AtomicFacts     *bool  `yaml:"atomic_facts"`
	FactsModel      string `yaml:"facts_model"`
	FactsTimeoutMin int    `yaml:"facts_timeout_min"`

	// FactsProvider routes the facts pass through llmProviderGenerator
	// so users can keep this cheap-but-numerous pass off Anthropic
	// quota. "anthropic" (default) uses `claude -p`; "ollama" uses
	// the local Ollama HTTP server configured in
	// Contextualize.OllamaURL. The facts pass is text-in/text-out
	// (the prompt inlines the chunk and asks for one JSON document)
	// so a 4–7B local model handles it without tool use.
	//
	// When provider=ollama, FactsModel should be an Ollama model name
	// (e.g. "gemma3:4b", "qwen3:4b"). The same provider/model
	// coherence check that contextualize uses applies here — Claude
	// aliases get auto-swapped to the recommended local default with
	// a log line so misconfiguration never silently no-ops.
	FactsProvider string `yaml:"facts_provider"`

	// Pass2Mode picks the pass-2 protocol. "tools" (default) keeps the
	// historical `claude -p` path with Read/Write/Edit/Glob/Grep tool
	// access. "json" switches to the Phase 4B JSON-action-envelope
	// path: the model emits one WikiActionEnvelope JSON document, Go
	// applies the file mutations through applyWikiActions. The json
	// mode is what makes pass-2 local-model friendly.
	//
	// Auto-flip: when Pass2Provider is set to anything other than
	// "anthropic", Pass2Mode is forced to "json" — the tools path
	// only works with claude -p.
	Pass2Mode string `yaml:"pass2_mode"`

	// Pass2Provider routes pass-2 through llmProviderGenerator (json
	// mode) or `claude -p` (tools mode). Default "anthropic". Setting
	// "ollama" automatically engages json mode and reuses
	// Contextualize.OllamaURL just like FactsProvider does.
	Pass2Provider string `yaml:"pass2_provider"`

	// Pass1Provider routes the entity-list pass-1 through
	// llmProviderGenerator (Phase 4A.2). The prompt is inlined-raw
	// → JSON-plan; no tool use needed. "anthropic" (default) keeps
	// the historical `claude -p` path; "ollama" runs against the
	// local Ollama server. Same provider/model coherence rules as
	// FactsProvider — Claude alias under ollama gets swapped to the
	// recommended local default.
	Pass1Provider string `yaml:"pass1_provider"`

	// SinglePassProvider routes brief-article single-pass absorb
	// through llmProviderGenerator (Phase 4A.3). The prompt is
	// inlined-raw → WikiActionEnvelope; no tool use. Default
	// "anthropic". When empty, inherits from LLMConfig.Provider.
	SinglePassProvider string `yaml:"single_pass_provider"`
	SinglePassModel    string `yaml:"single_pass_model"`

	// Pass2NumCtx and SinglePassNumCtx override Ollama's default
	// num_ctx (8192) for the two paths that inline the raw article
	// body. The pass-2 prompt routinely lands at 5–10K tokens (entity
	// + raw body + plan JSON + facts block); without a bump, the tail
	// of the source silently truncates on Ollama. Empty fields fall
	// through to LLMConfig.NumCtx, then to 16384.
	Pass2NumCtx      int `yaml:"pass2_num_ctx"`
	SinglePassNumCtx int `yaml:"single_pass_num_ctx"`
}

// ContextualizeConfig controls the `scribe contextualize` pre-embed step.
// Enabled defaults to true; set to false in scribe.yaml to skip the phase
// entirely (Phase 1.7 in sync becomes a no-op).
//
// Provider picks the backend: "anthropic" uses `claude -p` (Sonnet/Haiku
// via the already-configured Claude CLI); "ollama" hits a local Ollama
// HTTP server for free, fully-offline generation. OllamaURL defaults to
// http://localhost:11434. When provider=ollama, Model is passed through
// as the Ollama model name (e.g., "llama3.2:3b", "gemma2:2b"); when
// provider=anthropic, Model is a Claude alias ("haiku", "sonnet").
type ContextualizeConfig struct {
	Enabled    *bool  `yaml:"enabled"` // pointer so explicit `false` wins over default `true`
	Provider   string `yaml:"provider"`
	Model      string `yaml:"model"`
	OllamaURL  string `yaml:"ollama_url"`
	MaxPerRun  int    `yaml:"max_per_run"`
	TimeoutSec int    `yaml:"timeout_sec"`
}

// absorbDefaults returns the canonical defaults for AbsorbConfig. Used by
// loadConfig to fill missing fields after yaml.Unmarshal.
//
// Every *bool default gets its OWN variable. loadConfig prefills the
// config with this struct before yaml.Unmarshal, and yaml.v3 writes
// through existing non-nil pointers instead of allocating fresh ones —
// so two fields sharing one default bool alias each other, and a user
// setting `contextualize.enabled: false` silently flipped
// `chapter_aware` off too (caught by the issue-#9 stub-harness tests;
// regression-pinned in TestAbsorbDefaults_NoBoolPointerAliasing).
func absorbDefaults() AbsorbConfig {
	chapterAware := true
	contextualizeEnabled := true
	return AbsorbConfig{
		Strictness:             "medium",
		MaxPerRun:              5,
		DenseThresholdWords:    2000,
		DenseThresholdHeadings: 4,
		BriefThresholdWords:    500,
		BriefThresholdHeadings: 1,
		Pass1Model:             "haiku",
		Pass2Model:             "", // inherit sync model
		// Pass1 default bumped 3→5 because dense long-form articles (~17K
		// chars, 200+ lines) reliably timed out on haiku at 3 min and got
		// SIGKILLed mid-stream — costing the call without delivering an
		// entity list. Existing scribe.yaml entries pinning 3 still win.
		Pass1TimeoutMin:      5,
		Pass2TimeoutMin:      5,
		Pass2Parallel:        3,
		SinglePassTimeoutMin: 5,
		ChapterAware:         &chapterAware,
		ChapterThreshold:     3,
		ChapterParallel:      2,
		// AtomicFacts off by default. Users opt in by setting
		// `absorb.atomic_facts: true` in scribe.yaml after they've
		// verified chaptered absorb works on their corpus.
		AtomicFacts:        nil,
		FactsModel:         "haiku",
		FactsTimeoutMin:    3,
		FactsProvider:      "anthropic",
		Pass2Mode:          "tools",
		Pass2Provider:      "anthropic",
		Pass1Provider:      "anthropic",
		SinglePassProvider: "anthropic",
		Contextualize: ContextualizeConfig{
			Enabled:    &contextualizeEnabled,
			Provider:   "anthropic",
			Model:      "haiku",
			OllamaURL:  defaultOllamaURL,
			MaxPerRun:  20,
			TimeoutSec: 90,
		},
	}
}

// absorbDefaultYAMLBlock renders the canonical `absorb:` section as it should
// appear in scribe.yaml. Values come from absorbDefaults() so any change to
// the defaults propagates to the template, the init-merge helper, and any
// documentation that calls this function. Leading newline included for clean
// appends. Comments explain each knob inline.
func absorbDefaultYAMLBlock() string {
	d := absorbDefaults()
	enabled := "true"
	if d.Contextualize.Enabled != nil && !*d.Contextualize.Enabled {
		enabled = "false"
	}
	return fmt.Sprintf(`
# Absorb pipeline: raw/articles/*.md → wiki pages.
# Any omitted field inherits the built-in default (shown after "# default:").
absorb:
  # Which raw articles auto-absorb during sync.
  #   high   = only articles with `+"`absorb: true`"+` in frontmatter or a named
  #            (non-"general") domain tag
  #   medium = all unabsorbed raw articles (default)
  #   low    = same as medium today, reserved
  strictness: %s              # default: medium
  max_per_run: %d                  # default: 5

  # Density classification thresholds. Raw article becomes:
  #   brief    when word_count <  brief_threshold_words AND heading_count <= brief_threshold_headings
  #   dense    when word_count >= dense_threshold_words OR  heading_count >= dense_threshold_headings
  #   standard otherwise
  # Dense sources use the two-pass (entity-first) absorb path.
  dense_threshold_words: %d     # default: 2000
  dense_threshold_headings: %d     # default: 4
  brief_threshold_words: %d      # default: 500
  brief_threshold_headings: %d     # default: 1

  # Models and timeouts for each absorb path.
  pass1_model: %s              # default: haiku (cheap entity-list pass)
  pass2_model: ""                 # default: "" = inherit default_model
  pass1_timeout_min: %d            # default: 5
  pass2_timeout_min: %d            # default: 5
  pass2_parallel: %d               # default: 3 (chapters per pass2 run; drop to 1 for slow local models)
  single_pass_timeout_min: %d      # default: 5

  # Contextualize step: inserts a retrieval-context paragraph into each raw
  # article before qmd embedding (Anthropic Contextual Retrieval pattern).
  # Idempotent via wiki/_contextualized_log.json.
  #
  # provider:
  #   anthropic — uses `+"`claude -p`"+` (Haiku ≈ $0.0001/doc). Default.
  #   ollama    — uses a local Ollama server. Fully free, fully offline.
  #               Requires only `+"`brew install ollama`"+`. scribe auto-checks
  #               the /api/tags endpoint and auto-pulls the configured model
  #               on first run via /api/pull — no manual `+"`ollama pull`"+`.
  # model:
  #   anthropic: "haiku" | "sonnet"
  #   ollama:    recommended: "gemma3:4b" (3.3 GB, fast on Apple Silicon).
  #              Alternatives: "qwen3:4b" (richer prose), "llama3.2:3b"
  #              (smaller), "phi4-mini:3.8b" (reasoning-heavy).
  contextualize:
    enabled: %s                 # default: true
    provider: %s         # default: anthropic (alternatives: ollama)
    model: %s                  # default: haiku (for ollama: e.g. llama3.2:3b)
    ollama_url: %s  # default: http://localhost:11434
    max_per_run: %d               # default: 20
    timeout_sec: %d               # default: 90

  # Phase 3B atomic-fact extraction. Off by default — opt in once
  # chaptered absorb is verified on your corpus.
  # atomic_facts: true
  # facts_model: %s              # default: haiku (for ollama: e.g. gemma3:4b)
  # facts_timeout_min: %d           # default: 3
  #
  # facts_provider routes the per-chunk facts pass through the same
  # provider abstraction as contextualize. The pass is text-in/JSON-out
  # (no tools), which suits a 4–7B local model. Anthropic stays the
  # default; flip to ollama to keep this cheap-but-numerous pass off
  # Anthropic quota. ollama_url comes from absorb.contextualize above.
  # facts_provider: %s         # default: anthropic (alternatives: ollama)

  # Phase 4B pass-2 JSON-envelope mode. The default "tools" path uses
  # `+"`claude -p`"+` with Read/Write/Edit/Glob/Grep. Setting pass2_mode=json
  # (or pass2_provider to a non-anthropic provider) switches pass-2 to
  # an inlined prompt that emits one WikiActionEnvelope JSON document;
  # scribe applies the file mutations itself. This makes pass-2 work
  # against local Ollama models (no tool-use needed).
  # pass2_mode: %s              # default: tools (alternatives: json)
  # pass2_provider: %s          # default: anthropic (alternatives: ollama)
  # pass2_model: ""                # for ollama: e.g. qwen2.5-coder:14b
`,
		d.Strictness,
		d.MaxPerRun,
		d.DenseThresholdWords,
		d.DenseThresholdHeadings,
		d.BriefThresholdWords,
		d.BriefThresholdHeadings,
		d.Pass1Model,
		d.Pass1TimeoutMin,
		d.Pass2TimeoutMin,
		d.Pass2Parallel,
		d.SinglePassTimeoutMin,
		enabled,
		d.Contextualize.Provider,
		d.Contextualize.Model,
		d.Contextualize.OllamaURL,
		d.Contextualize.MaxPerRun,
		d.Contextualize.TimeoutSec,
		d.FactsModel,
		d.FactsTimeoutMin,
		d.FactsProvider,
		d.Pass2Mode,
		d.Pass2Provider,
	)
}

// applyAbsorbDefaults fills any zero-valued AbsorbConfig field from the
// defaults. Called after yaml.Unmarshal so partial user overrides merge
// cleanly with the baseline. Also performs provider/model coherence
// fixups: ollama + Claude alias is never what the user meant, so the
// recommended local model takes over with a one-line log.
//
// Backward-compat wrapper for tests. Production callers use
// applyAbsorbDefaultsWithLLM directly so per-op fields can inherit
// from the top-level LLMConfig (Phase 4A.5).
func applyAbsorbDefaults(cfg *AbsorbConfig) {
	applyAbsorbDefaultsWithLLM(cfg, llmDefaults())
}

// applyAbsorbDefaultsWithLLM is the Phase 4A.5 inheritance-aware
// variant. Empty per-op Provider/Model fields fall through to the
// top-level LLMConfig; non-empty values stick.
func applyAbsorbDefaultsWithLLM(cfg *AbsorbConfig, llm LLMConfig) {
	d := absorbDefaults()
	if cfg.Strictness == "" {
		cfg.Strictness = d.Strictness
	}
	if cfg.MaxPerRun <= 0 {
		cfg.MaxPerRun = d.MaxPerRun
	}
	if cfg.DenseThresholdWords <= 0 {
		cfg.DenseThresholdWords = d.DenseThresholdWords
	}
	if cfg.DenseThresholdHeadings <= 0 {
		cfg.DenseThresholdHeadings = d.DenseThresholdHeadings
	}
	if cfg.BriefThresholdWords <= 0 {
		cfg.BriefThresholdWords = d.BriefThresholdWords
	}
	if cfg.BriefThresholdHeadings <= 0 {
		cfg.BriefThresholdHeadings = d.BriefThresholdHeadings
	}
	if cfg.Pass1Model == "" {
		cfg.Pass1Model = d.Pass1Model
	}
	if cfg.Pass1TimeoutMin <= 0 {
		cfg.Pass1TimeoutMin = d.Pass1TimeoutMin
	}
	if cfg.Pass2TimeoutMin <= 0 {
		cfg.Pass2TimeoutMin = d.Pass2TimeoutMin
	}
	if cfg.Pass2Parallel <= 0 {
		cfg.Pass2Parallel = d.Pass2Parallel
	}
	if cfg.SinglePassTimeoutMin <= 0 {
		cfg.SinglePassTimeoutMin = d.SinglePassTimeoutMin
	}
	if cfg.ChapterAware == nil {
		cfg.ChapterAware = d.ChapterAware
	}
	if cfg.ChapterParallel <= 0 {
		cfg.ChapterParallel = d.ChapterParallel
	}
	if cfg.ChapterThreshold <= 0 {
		cfg.ChapterThreshold = d.ChapterThreshold
	}
	// AtomicFacts: nil means inherit default (which is also nil =
	// off). We deliberately don't clobber an explicit `false` with
	// the default. A user-set `true` survives.
	if cfg.AtomicFacts == nil {
		cfg.AtomicFacts = d.AtomicFacts
	}
	if cfg.FactsModel == "" {
		cfg.FactsModel = d.FactsModel
	}
	if cfg.FactsTimeoutMin <= 0 {
		cfg.FactsTimeoutMin = d.FactsTimeoutMin
	}
	// Phase 4A.5 inheritance: empty per-op Provider falls through to
	// LLMConfig.Provider. Per-op Model already has a sensible default
	// ("haiku") for facts, so we only inherit when both layers leave
	// it blank.
	if cfg.FactsProvider == "" {
		cfg.FactsProvider = llm.Provider
	}
	if cfg.FactsProvider == "" {
		cfg.FactsProvider = d.FactsProvider
	}
	// Provider/model coherence for facts mirrors the contextualize check
	// below: ollama + Claude alias is a misconfiguration, swap to the
	// recommended local default and log so the user notices.
	cfg.FactsModel = inheritHostedModel(cfg.FactsProvider, cfg.FactsModel, llm)
	cfg.FactsProvider, cfg.FactsModel = coerceProviderModel("absorb.facts", cfg.FactsProvider, cfg.FactsModel)
	if cfg.Pass2Provider == "" {
		cfg.Pass2Provider = llm.Provider
	}
	if cfg.Pass2Provider == "" {
		cfg.Pass2Provider = d.Pass2Provider
	}
	if cfg.Pass1Provider == "" {
		cfg.Pass1Provider = llm.Provider
	}
	if cfg.Pass1Provider == "" {
		cfg.Pass1Provider = d.FactsProvider // same anthropic default
	}
	cfg.Pass1Model = inheritHostedModel(cfg.Pass1Provider, cfg.Pass1Model, llm)
	cfg.Pass1Provider, cfg.Pass1Model = coerceProviderModel("absorb.pass1", cfg.Pass1Provider, cfg.Pass1Model)
	if cfg.SinglePassProvider == "" {
		cfg.SinglePassProvider = llm.Provider
	}
	if cfg.SinglePassProvider == "" {
		cfg.SinglePassProvider = d.FactsProvider // same anthropic default
	}
	cfg.SinglePassModel = inheritHostedModel(cfg.SinglePassProvider, cfg.SinglePassModel, llm)
	cfg.SinglePassProvider, cfg.SinglePassModel = coerceProviderModel("absorb.single_pass", cfg.SinglePassProvider, cfg.SinglePassModel)
	// Track whether the mode came from the user (yaml or env) — coercing
	// the code default below is silent, overriding a user choice logs.
	pass2ModeExplicit := cfg.Pass2Mode != ""
	if cfg.Pass2Mode == "" {
		cfg.Pass2Mode = d.Pass2Mode
	}
	// Env overrides for pass-2 routing. Useful for one-shot A/B scripts
	// like scripts/absorb-compare.sh that need to flip mode/provider/model
	// without mutating scribe.yaml. Empty values are ignored. The auto-
	// flip below still wins over a mis-set SCRIBE_PASS2_MODE — e.g.
	// SCRIBE_PASS2_MODE=tools + SCRIBE_PASS2_PROVIDER=ollama still
	// engages json mode with a log line.
	if env := os.Getenv("SCRIBE_PASS2_MODE"); env != "" {
		logMsg("config", "SCRIBE_PASS2_MODE=%q overriding absorb.pass2_mode=%q", env, cfg.Pass2Mode)
		cfg.Pass2Mode = env
		pass2ModeExplicit = true
	}
	if env := os.Getenv("SCRIBE_PASS2_PROVIDER"); env != "" {
		logMsg("config", "SCRIBE_PASS2_PROVIDER=%q overriding absorb.pass2_provider=%q", env, cfg.Pass2Provider)
		cfg.Pass2Provider = env
	}
	if env := os.Getenv("SCRIBE_PASS2_MODEL"); env != "" {
		logMsg("config", "SCRIBE_PASS2_MODEL=%q overriding absorb.pass2_model=%q", env, cfg.Pass2Model)
		cfg.Pass2Model = env
	}
	// Auto-flip mode to json whenever provider is not anthropic — the
	// tools path requires `claude -p`, so a local-provider config with
	// pass2_mode left at "tools" would silently no-op. Log only when the
	// user actually set the mode; coercing the code default is noise.
	if !strings.EqualFold(cfg.Pass2Provider, "anthropic") && !strings.EqualFold(cfg.Pass2Mode, "json") {
		if pass2ModeExplicit {
			logAutoFlipOnce("absorb.pass2:"+cfg.Pass2Provider, "config", "absorb.pass2_provider=%q forces pass2_mode=json (was %q)", cfg.Pass2Provider, cfg.Pass2Mode)
		}
		cfg.Pass2Mode = "json"
	}
	// Provider/model coherence: ollama + Claude alias swap, same as facts.
	cfg.Pass2Model = inheritHostedModel(cfg.Pass2Provider, cfg.Pass2Model, llm)
	cfg.Pass2Provider, cfg.Pass2Model = coerceProviderModel("absorb.pass2", cfg.Pass2Provider, cfg.Pass2Model)

	// num_ctx defaults for the two paths that inline raw body. Empty →
	// inherit from llm.num_ctx, then 16384. Only relevant under Ollama;
	// Anthropic ignores num_ctx entirely.
	if cfg.Pass2NumCtx <= 0 {
		cfg.Pass2NumCtx = llm.NumCtx
	}
	if cfg.Pass2NumCtx <= 0 {
		cfg.Pass2NumCtx = 16384
	}
	if cfg.SinglePassNumCtx <= 0 {
		cfg.SinglePassNumCtx = llm.NumCtx
	}
	if cfg.SinglePassNumCtx <= 0 {
		cfg.SinglePassNumCtx = 16384
	}

	if cfg.Contextualize.Enabled == nil {
		cfg.Contextualize.Enabled = d.Contextualize.Enabled
	}
	if cfg.Contextualize.Provider == "" {
		cfg.Contextualize.Provider = llm.Provider
	}
	if cfg.Contextualize.Provider == "" {
		cfg.Contextualize.Provider = d.Contextualize.Provider
	}
	if cfg.Contextualize.Model == "" {
		cfg.Contextualize.Model = d.Contextualize.Model
	}
	if cfg.Contextualize.OllamaURL == "" {
		cfg.Contextualize.OllamaURL = llm.OllamaURL
	}
	if cfg.Contextualize.OllamaURL == "" {
		cfg.Contextualize.OllamaURL = d.Contextualize.OllamaURL
	}
	if cfg.Contextualize.MaxPerRun <= 0 {
		cfg.Contextualize.MaxPerRun = d.Contextualize.MaxPerRun
	}
	if cfg.Contextualize.TimeoutSec <= 0 {
		cfg.Contextualize.TimeoutSec = d.Contextualize.TimeoutSec
	}

	// Provider/model coherence: same alias swap as pass2/facts.
	cfg.Contextualize.Model = inheritHostedModel(cfg.Contextualize.Provider, cfg.Contextualize.Model, llm)
	cfg.Contextualize.Provider, cfg.Contextualize.Model = coerceProviderModel("contextualize", cfg.Contextualize.Provider, cfg.Contextualize.Model)
}

// maybeBackfillAbsorbBlock appends the commented `absorb:` defaults
// block to scribe.yaml when the file has no top-level `absorb:` key,
// so the user can discover the knobs next time they edit it. This is
// the one place config can be rewritten — call it only from commands
// that already mutate KB state (sync, init), never from loadConfig or
// any read-only path.
//
// Best-effort and silent on every failure: the runtime already has
// defaults merged in memory, so a read-only filesystem, permission
// issue, or missing file is non-fatal. Gated by
// SCRIBE_NO_CONFIG_BACKFILL=1 for users who want scribe.yaml left
// strictly untouched.
func maybeBackfillAbsorbBlock(root string) {
	if os.Getenv("SCRIBE_NO_CONFIG_BACKFILL") != "" {
		return
	}
	cfgPath := filepath.Join(root, "scribe.yaml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return
	}
	if hasTopLevelKey(string(data), "absorb") {
		return
	}
	appendAbsorbBlockQuiet(cfgPath, string(data))
}

// appendAbsorbBlockQuiet appends absorbDefaultYAMLBlock() to cfgPath. Silent
// on any error — the runtime still has a usable Config with defaults
// merged in memory, so a read-only filesystem or permission issue is
// non-fatal.
func appendAbsorbBlockQuiet(cfgPath, existing string) {
	merged := existing
	if !strings.HasSuffix(merged, "\n") {
		merged += "\n"
	}
	merged += absorbDefaultYAMLBlock()
	tmp := cfgPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(merged), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, cfgPath)
}
