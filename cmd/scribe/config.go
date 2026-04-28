package main

import (
	"bufio"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// wikiDirs lists all content directories in the KB.
var wikiDirs = []string{
	"wiki", "projects", "research", "solutions", "tools",
	"decisions", "patterns", "ideas", "people", "sessions",
}

// ScribeConfig holds configuration loaded from scribe.yaml.
type ScribeConfig struct {
	OwnerName         string   `yaml:"owner_name"`
	OwnerContext      string   `yaml:"owner_context"`
	Domains           []string `yaml:"domains"`
	ClaudeProjectsDir string   `yaml:"claude_projects_dir"`
	CcriderDB         string   `yaml:"ccrider_db"`
	LockDir           string   `yaml:"lock_dir"`
	DefaultModel      string   `yaml:"default_model"`
	// KBName is the display-level name of this KB, used for:
	//   - the drop-file directory other projects write to
	//     (`.claude/<kb_name>/*.md`)
	//   - the `<kb_name>: true` frontmatter flag those drop files carry
	//   - log prefixes where the name adds useful disambiguation
	// Defaults to the basename of the KB root directory. Override only
	// if your folder name and the name you want to expose differ.
	KBName     string           `yaml:"kb_name"`
	Sync       SyncConfig       `yaml:"sync"`
	Deep       DeepConfig       `yaml:"deep"`
	Capture    CaptureConfig    `yaml:"capture"`
	Triage     TriageConfig     `yaml:"triage"`
	Absorb     AbsorbConfig     `yaml:"absorb"`
	Ingest     IngestConfig     `yaml:"ingest"`
	Identities IdentitiesConfig `yaml:"identities"`
}

// IngestConfig controls the file-ingestion pipeline (Phase 1B+). All
// fields are optional; zero values fall back to ingestDefaults().
//
// InboxPath is the directory `scribe sync` drains during Phase 1.5
// (drop a file there, walk away, cron picks it up). Default
// "raw/inbox" relative to the KB root. Subdirectories `.processed/`
// and `.failed/` get auto-created for state tracking.
//
// Marker holds tier 1 (marker-pdf) settings. TimeoutSeconds caps a
// single-file conversion. MPSFallback toggles
// PYTORCH_ENABLE_MPS_FALLBACK=1 in the marker env — required to work
// around the surya MPS instability on Apple Silicon. Defaults: 300s
// timeout, MPS fallback on (macOS only; harmless elsewhere).
//
// Converters is a forward-compat map for the Phase 5 plugin system:
// users will be able to override per-format with `pdf: marker | tier0
// | docling | mineru`. Phase 1B reads no values from it; the field
// just claims the YAML key so config files written today survive the
// future schema bump.
type IngestConfig struct {
	InboxPath    string                   `yaml:"inbox_path"`
	Marker       IngestMarkerConfig       `yaml:"marker"`
	Converters   map[string]string        `yaml:"converters"`
	SmartRouting IngestSmartRoutingConfig `yaml:"smart_routing"`
}

type IngestMarkerConfig struct {
	TimeoutSeconds int   `yaml:"timeout_seconds"`
	MPSFallback    *bool `yaml:"mps_fallback"`
	// Device pins the torch backend marker uses. Valid values:
	//   "auto" (default) — let torch pick (MPS on Apple Silicon,
	//                       CUDA on Linux+GPU, CPU otherwise). On
	//                       macOS, scribe also enables a one-shot
	//                       retry on CPU when the surya layout
	//                       model crashes inside MPS (the
	//                       `AcceleratorError: index out of bounds`
	//                       signature observed against larger PDFs).
	//   "cpu"             — force CPU. Slower but eliminates the
	//                       MPS-crash class of failures entirely.
	//                       Recommended for unattended cron drains
	//                       on Apple Silicon.
	//   "mps" / "cuda"    — force a specific GPU backend. No retry
	//                       on crash; surfaced for power users who
	//                       know their PDFs play nice with the
	//                       layout model.
	Device string `yaml:"device"`
}

// IngestSmartRoutingConfig sends "small" PDFs to tier 0 even when
// marker is installed. Marker cold-loads ~3 GB of weights per
// invocation; on a 2-page receipt that's 50× the runtime of
// ledongthuc/pdf with no quality benefit. Defaults: 500 KB on disk
// and 5 pages — both must be true to route to tier 0.
//
// Set Enabled to false in scribe.yaml to always use marker when
// available (useful when batches are dominated by complex PDFs and
// the cold-load cost gets amortized anyway).
type IngestSmartRoutingConfig struct {
	Enabled     *bool `yaml:"enabled"`
	MaxPDFBytes int64 `yaml:"max_pdf_bytes"`
	MaxPDFPages int   `yaml:"max_pdf_pages"`
}

func ingestDefaults() IngestConfig {
	trueV := true
	return IngestConfig{
		InboxPath: "raw/inbox",
		Marker: IngestMarkerConfig{
			TimeoutSeconds: 300,
			MPSFallback:    &trueV,
			Device:         "auto",
		},
		Converters: map[string]string{},
		SmartRouting: IngestSmartRoutingConfig{
			Enabled:     &trueV,
			MaxPDFBytes: 500 * 1024, // 500 KB
			MaxPDFPages: 5,
		},
	}
}

// applyIngestDefaults merges user overrides on top of ingestDefaults.
// Mirrors applyAbsorbDefaults — zero-valued fields inherit, non-zero
// values stick. Pointer fields (MPSFallback) only inherit when nil so
// an explicit `false` in scribe.yaml wins.
func applyIngestDefaults(cfg *IngestConfig) {
	d := ingestDefaults()
	if cfg.InboxPath == "" {
		cfg.InboxPath = d.InboxPath
	}
	if cfg.Marker.TimeoutSeconds <= 0 {
		cfg.Marker.TimeoutSeconds = d.Marker.TimeoutSeconds
	}
	if cfg.Marker.MPSFallback == nil {
		cfg.Marker.MPSFallback = d.Marker.MPSFallback
	}
	if cfg.Marker.Device == "" {
		cfg.Marker.Device = d.Marker.Device
	}
	if cfg.Converters == nil {
		cfg.Converters = d.Converters
	}
	if cfg.SmartRouting.Enabled == nil {
		cfg.SmartRouting.Enabled = d.SmartRouting.Enabled
	}
	if cfg.SmartRouting.MaxPDFBytes <= 0 {
		cfg.SmartRouting.MaxPDFBytes = d.SmartRouting.MaxPDFBytes
	}
	if cfg.SmartRouting.MaxPDFPages <= 0 {
		cfg.SmartRouting.MaxPDFPages = d.SmartRouting.MaxPDFPages
	}
}

// IdentitiesConfig filters noise out of `scribe lint --identities`.
// Defaults ship with the most common Elixir module-attribute names and
// test/example email domains; users extend them in scribe.yaml. Zero
// values pick up the defaults — they merge, not override.
type IdentitiesConfig struct {
	// HandleStopwords are the bare handles (no leading `@`) that should
	// never be treated as person mentions. Common shape: Elixir module
	// attributes (@doc, @moduletag), front-end CSS utility terms (@theme,
	// @utility), Dialyzer decorators.
	HandleStopwords []string `yaml:"handle_stopwords"`

	// EmailDomainStopwords are email domains whose addresses should be
	// discarded. Test domains (example.com) and transactional senders
	// dominate.
	EmailDomainStopwords []string `yaml:"email_domain_stopwords"`
}

// identityDefaults returns the built-in stopwords shipped with scribe.
// User config values are merged (additive) rather than replacing these.
// Defaults are intentionally minimal — anything project-specific belongs
// in the user's scribe.yaml under identities.handle_stopwords.
func identityDefaults() IdentitiesConfig {
	return IdentitiesConfig{
		HandleStopwords: []string{
			// Elixir/Erlang module attributes — these are @foo tokens
			// parsed by the @handle regex but are code, not people.
			"doc", "moduledoc", "behavior", "behavior", "callback",
			"spec", "type", "typep", "opaque", "impl", "deprecated",
			"since", "dialyzer", "compile", "before_compile",
			"after_compile", "on_load", "external_resource",
			"enforce_keys", "derive", "protocol", "for",
			"fallback_to_any", "moduletag", "tag",
		},
		EmailDomainStopwords: []string{
			"example.com", "example.net", "example.org",
		},
	}
}

// mergeIdentityConfig overlays user-provided stopwords on the defaults,
// returning a config where every default stopword is retained and the
// user's additions are appended (lowercased, deduplicated).
func mergeIdentityConfig(user IdentitiesConfig) IdentitiesConfig {
	merged := identityDefaults()
	seenHandles := toLowerSet(merged.HandleStopwords)
	for _, h := range user.HandleStopwords {
		lc := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "@")))
		if lc == "" || seenHandles[lc] {
			continue
		}
		seenHandles[lc] = true
		merged.HandleStopwords = append(merged.HandleStopwords, lc)
	}
	seenDomains := toLowerSet(merged.EmailDomainStopwords)
	for _, d := range user.EmailDomainStopwords {
		lc := strings.ToLower(strings.TrimSpace(d))
		if lc == "" || seenDomains[lc] {
			continue
		}
		seenDomains[lc] = true
		merged.EmailDomainStopwords = append(merged.EmailDomainStopwords, lc)
	}
	return merged
}

func toLowerSet(in []string) map[string]bool {
	out := make(map[string]bool, len(in))
	for _, s := range in {
		out[strings.ToLower(s)] = true
	}
	return out
}

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
func absorbDefaults() AbsorbConfig {
	trueV := true
	return AbsorbConfig{
		Strictness:             "medium",
		MaxPerRun:              5,
		DenseThresholdWords:    2000,
		DenseThresholdHeadings: 4,
		BriefThresholdWords:    500,
		BriefThresholdHeadings: 1,
		Pass1Model:             "haiku",
		Pass2Model:             "", // inherit sync model
		Pass1TimeoutMin:        3,
		Pass2TimeoutMin:        5,
		Pass2Parallel:          3,
		SinglePassTimeoutMin:   5,
		ChapterAware:           &trueV,
		ChapterThreshold:       3,
		Contextualize: ContextualizeConfig{
			Enabled:    &trueV,
			Provider:   "anthropic",
			Model:      "haiku",
			OllamaURL:  "http://localhost:11434",
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
  pass1_timeout_min: %d            # default: 3
  pass2_timeout_min: %d            # default: 5
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
		d.SinglePassTimeoutMin,
		enabled,
		d.Contextualize.Provider,
		d.Contextualize.Model,
		d.Contextualize.OllamaURL,
		d.Contextualize.MaxPerRun,
		d.Contextualize.TimeoutSec,
	)
}

// ollamaRecommendedModel is the scribe-vetted default when the user picks
// provider=ollama but leaves model blank (or accidentally copies a Claude
// alias). Chosen in research/2026-04-20-ollama-model-for-contextualize.md
// for best speed/quality balance on Apple Silicon at ~3.3 GB Q4.
const ollamaRecommendedModel = "gemma3:4b"

// claudeModelAliases are the model names `claude -p` accepts. Anything in
// this list is a red flag when the provider is ollama — the user probably
// switched provider without updating model.
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

// applyAbsorbDefaults fills any zero-valued AbsorbConfig field from the
// defaults. Called after yaml.Unmarshal so partial user overrides merge
// cleanly with the baseline. Also performs provider/model coherence
// fixups: ollama + Claude alias is never what the user meant, so the
// recommended local model takes over with a one-line log.
func applyAbsorbDefaults(cfg *AbsorbConfig) {
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
	if cfg.ChapterThreshold <= 0 {
		cfg.ChapterThreshold = d.ChapterThreshold
	}
	if cfg.Contextualize.Enabled == nil {
		cfg.Contextualize.Enabled = d.Contextualize.Enabled
	}
	if cfg.Contextualize.Provider == "" {
		cfg.Contextualize.Provider = d.Contextualize.Provider
	}
	if cfg.Contextualize.Model == "" {
		cfg.Contextualize.Model = d.Contextualize.Model
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

	// Provider/model coherence: when the user opts into ollama, make sure the
	// model field makes sense for ollama. Empty → recommended default. Claude
	// alias → user probably switched provider without updating model; swap
	// to the recommended default and log so they notice.
	if strings.EqualFold(cfg.Contextualize.Provider, "ollama") {
		if cfg.Contextualize.Model == "" || claudeModelAliases[strings.ToLower(cfg.Contextualize.Model)] {
			if cfg.Contextualize.Model != "" {
				logMsg("config", "contextualize.provider=ollama but model=%q is a Claude alias — switching to %s (set an ollama model explicitly to silence this)", cfg.Contextualize.Model, ollamaRecommendedModel)
			}
			cfg.Contextualize.Model = ollamaRecommendedModel
		}
	}
}

// TriageConfig controls how `scribe triage` scores session knowledge density.
// Each category pairs a BM25 match expression (keywords) with a per-hit
// multiplier (weights). Defaults below are the original Elixir/Phoenix-biased
// set; users should edit scribe.yaml to match their stack.
type TriageConfig struct {
	Keywords map[string]string `yaml:"keywords"`
	Weights  map[string]int    `yaml:"weights"`
}

// triageCategoryOrder is the stable order the SQL builder uses when emitting
// CTEs — the column order in the result set is baked into the row scanner, so
// this list is how triage.go and scribe.yaml agree on which category is which.
var triageCategoryOrder = []string{"decision", "architecture", "research", "learning", "evaluation", "deep_work", "code_pattern"}

// defaultTriageKeywords seed `scribe init` and act as fallbacks when a
// user's scribe.yaml omits a category. FTS5 MATCH syntax: uppercase OR,
// quoted phrases preserved.
var defaultTriageKeywords = map[string]string{
	"decision":     "decided OR chose OR tradeoff OR alternative",
	"architecture": "architecture OR \"design pattern\" OR strategy OR refactor",
	"research":     "research OR paper OR benchmark OR measured OR compared",
	"learning":     "learned OR realized OR mistake OR lesson OR insight",
	"evaluation":   "evaluated OR verdict OR recommend OR comparison",
	"deep_work":    "analysis OR investigation OR \"root cause\" OR audit",
	"code_pattern": "GenServer OR LiveView OR Oban OR Ecto OR Phoenix OR migration OR Supervisor OR PubSub OR Endpoint OR Router",
}

var defaultTriageWeights = map[string]int{
	"decision":     3,
	"architecture": 2,
	"research":     3,
	"learning":     2,
	"evaluation":   2,
	"deep_work":    1,
	"code_pattern": 1,
}

// Resolve returns effective keywords and weights for this KB: the user's
// config overlaid on the defaults. Missing categories inherit the default
// wording/weight rather than disappearing from scoring.
func (t TriageConfig) Resolve() (keywords map[string]string, weights map[string]int) {
	keywords = map[string]string{}
	weights = map[string]int{}
	maps.Copy(keywords, defaultTriageKeywords)
	maps.Copy(weights, defaultTriageWeights)
	for k, v := range t.Keywords {
		if strings.TrimSpace(v) != "" {
			keywords[k] = v
		}
	}
	for k, v := range t.Weights {
		if v > 0 {
			weights[k] = v
		}
	}
	return keywords, weights
}

// CaptureConfig holds settings for `scribe capture` (iMessage self-chat).
type CaptureConfig struct {
	SelfChatHandle string `yaml:"self_chat_handle"`

	// SkipDomains: URLs containing any of these substrings are ignored during
	// capture. Useful for non-content hosts (short-form video, audiobook
	// players, etc.) that you don't want landing in raw/articles/. Defaults
	// to empty — users add their own preferences.
	SkipDomains []string `yaml:"skip_domains"`
}

type SyncConfig struct {
	MaxExtractions      int `yaml:"max_extractions"`
	MaxSessions         int `yaml:"max_sessions"`
	MaxAbsorb           int `yaml:"max_absorb"`
	ParallelExtractions int `yaml:"parallel_extractions"`
	CheckpointInterval  int `yaml:"checkpoint_interval"`
	// MaxExtractFiles gates normal `scribe sync` extraction. Projects whose
	// changed-file count exceeds this are skipped with a hint to run
	// `scribe deep <name>` — one claude -p over hundreds of files reliably
	// blows the 10-minute runClaude timeout and returns `signal: killed`.
	// Zero disables the gate.
	MaxExtractFiles int `yaml:"max_extract_files"`

	// CommitDebounceMinutes suppresses auto-commit+push when the last KB
	// commit was less than N minutes ago. Useful on busy cron cadences
	// (every 5min ingests → many tiny commits) to batch into fewer larger
	// commits. Staged changes roll over to the next run. Zero = commit
	// every run (existing behavior).
	CommitDebounceMinutes int `yaml:"commit_debounce_minutes"`

	// AlwaysPullBeforeSync runs `git pull --rebase --autostash` at the
	// start of `scribe sync` so teammates' committed pages show up in
	// this run before extraction/absorb starts. Silently skipped when
	// the KB is not a git repo or has no remote. Default: true. Set to
	// false if the KB lives somewhere you don't want network calls (air-
	// gapped laptops, offline laptops on cron).
	AlwaysPullBeforeSync *bool `yaml:"always_pull_before_sync"`
}

type DeepConfig struct {
	BatchMax int `yaml:"batch_max"`
}

// kbName returns the effective KB name for `root`. Priority: explicit
// `kb_name:` in scribe.yaml → basename of root. Never empty.
func kbName(root string) string {
	if cfg := loadConfig(root); cfg != nil && cfg.KBName != "" {
		return cfg.KBName
	}
	if root == "" {
		return "scribe"
	}
	return filepath.Base(root)
}

// pullBeforeSyncEnabled returns true unless the user has explicitly set
// sync.always_pull_before_sync: false in scribe.yaml. Default: enabled —
// we want teammates' committed pages to show up in the next sync run
// without requiring opt-in. Users on offline/air-gapped laptops flip it
// off explicitly.
func pullBeforeSyncEnabled(cfg *ScribeConfig) bool {
	if cfg == nil || cfg.Sync.AlwaysPullBeforeSync == nil {
		return true
	}
	return *cfg.Sync.AlwaysPullBeforeSync
}

// universalDomains are always accepted regardless of user config. Every KB
// inherits these even if the user's domains list is empty — they mark content
// that spans projects or has no project binding at all.
var universalDomains = []string{"personal", "general"}

// loadConfig reads scribe.yaml from the KB root. Returns defaults if not found.
func loadConfig(root string) *ScribeConfig {
	cfg := &ScribeConfig{
		Domains:           []string{},
		ClaudeProjectsDir: filepath.Join(os.Getenv("HOME"), ".claude", "projects"),
		CcriderDB:         filepath.Join(os.Getenv("HOME"), ".config", "ccrider", "sessions.db"),
		LockDir:           "/tmp",
		DefaultModel:      "sonnet",
		Sync:              SyncConfig{MaxExtractions: 3, MaxSessions: 3, MaxAbsorb: 5, ParallelExtractions: 3, CheckpointInterval: 5, MaxExtractFiles: 100},
		Deep:              DeepConfig{BatchMax: 5},
		Capture:           CaptureConfig{SelfChatHandle: ""},
		Absorb:            absorbDefaults(),
		Ingest:            ingestDefaults(),
	}

	cfgPath := filepath.Join(root, "scribe.yaml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		applyAbsorbDefaults(&cfg.Absorb)
		applyIngestDefaults(&cfg.Ingest)
		return cfg
	}
	_ = yaml.Unmarshal(data, cfg)

	// Expand ~ in paths.
	cfg.ClaudeProjectsDir = expandHome(cfg.ClaudeProjectsDir)
	cfg.CcriderDB = expandHome(cfg.CcriderDB)
	cfg.LockDir = expandHome(cfg.LockDir)

	// Merge user overrides on top of absorb defaults (zero-valued fields
	// inherit). Partial user config is legal and common.
	applyAbsorbDefaults(&cfg.Absorb)
	applyIngestDefaults(&cfg.Ingest)

	// First-use backfill: if the on-disk scribe.yaml has no `absorb:` key,
	// append the commented defaults block so the user can discover the knobs
	// next time they edit the file. Best-effort; silent on failure (runtime
	// still has defaults merged in memory). Gated by SCRIBE_NO_CONFIG_BACKFILL
	// for users who want a strictly read-only loadConfig.
	if os.Getenv("SCRIBE_NO_CONFIG_BACKFILL") == "" && !hasTopLevelKey(string(data), "absorb") {
		appendAbsorbBlockQuiet(cfgPath, string(data))
	}
	return cfg
}

// appendAbsorbBlockQuiet appends absorbDefaultYAMLBlock() to cfgPath. Silent
// on any error — loadConfig still returns a usable Config with defaults
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

// AllDomains returns the merged set of user-configured + universal domains.
// The validator calls this so literal domain values in frontmatter are checked
// against the actual KB config rather than a baked-in list.
func (c *ScribeConfig) AllDomains() []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range c.Domains {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	for _, d := range universalDomains {
		if !seen[d] {
			out = append(out, d)
			seen[d] = true
		}
	}
	return out
}

// expandHome replaces a leading ~ with $HOME. Handles both "~" alone and
// "~/relative/path" without indexing past the end of a short input.
func expandHome(path string) string {
	if path == "~" {
		return os.Getenv("HOME")
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(os.Getenv("HOME"), path[2:])
	}
	return path
}

// skipFiles are meta files with non-standard content. `_index.md` is NOT
// listed here: walkArticles still skips it via the underscore rule, but
// walkAllMarkdown reads it so hub indices contribute wikilinks.
var skipFiles = map[string]bool{
	"_backlinks.json":    true,
	"_absorb_log.json":   true,
	"_sessions_log.json": true,
}

var (
	// Exclude both `]` and `\n` from the character class: `]` terminates the
	// link, and `\n` prevents the regex from eating across a line boundary
	// when a nearby line has an unclosed `[[` (as can happen in the index if
	// a summary is truncated mid-wikilink).
	wikilinkRE = regexp.MustCompile(`\[\[([^\]\n]+)\]\]`)
	// Code-span stripping is two-pass: double-backtick escapes (``...``) first,
	// then single-backtick spans. Each pattern anchors to a single line so an
	// unmatched backtick can't eat content across line boundaries — that was
	// the bug where `[[references]]` in learnings.md leaked through because an
	// earlier unmatched backtick on line 46 paired with a backtick many lines
	// later, leaving the `[[references]]` span intact.
	codeSpanDoubleRE = regexp.MustCompile("``[^\n]*?``")
	codeSpanRE       = regexp.MustCompile("`[^`\n]+`")
	codeFenceRE      = regexp.MustCompile("(?s)```[^\n]*\n.*?```")
	titleLineRE      = regexp.MustCompile(`(?m)^title:\s*["']?(.+?)["']?\s*$`)
)

// userConfigDir returns the path to the scribe user-level config directory.
// Follows XDG: ~/.config/scribe/ on macOS/Linux.
func userConfigDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "scribe")
	}
	return filepath.Join(os.Getenv("HOME"), ".config", "scribe")
}

// userConfigPath returns the path to the scribe user-level config file.
func userConfigPath() string {
	return filepath.Join(userConfigDir(), "config.yaml")
}

// userConfig holds settings from the user-level config file (~/.config/scribe/config.yaml).
type userConfig struct {
	KBDir string `yaml:"kb_dir"`
}

// loadUserConfig reads the user-level config. Returns zero value if missing.
func loadUserConfig() userConfig {
	var uc userConfig
	data, err := os.ReadFile(userConfigPath())
	if err != nil {
		return uc
	}
	_ = yaml.Unmarshal(data, &uc)
	uc.KBDir = expandHome(uc.KBDir)
	return uc
}

// kbDir resolves the knowledge base root directory.
// Priority: --root flag → SCRIBE_KB env → user config → CWD walk → error.
func kbDir() (string, error) {
	// 1. Explicit --root flag
	if globalRoot != "" {
		return globalRoot, nil
	}
	// 2. Environment variable
	if d := os.Getenv("SCRIBE_KB"); d != "" {
		return d, nil
	}
	// 3. User-level config (~/.config/scribe/config.yaml)
	if uc := loadUserConfig(); uc.KBDir != "" {
		if _, err := os.Stat(filepath.Join(uc.KBDir, "scripts", "projects.json")); err == nil {
			return uc.KBDir, nil
		}
	}
	// 4. Walk up from cwd looking for scripts/projects.json (written by `scribe init`)
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := cwd; dir != "/"; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "scripts", "projects.json")); err == nil {
			return dir, nil
		}
	}
	return "", fmt.Errorf("cannot find scribe KB root; run `scribe init` inside your KB checkout, use -C <path>, or set SCRIBE_KB")
}

// Frontmatter represents YAML frontmatter from a wiki article.
// Note: Created/Updated use `any` because Go's YAML parser auto-converts
// YYYY-MM-DD to time.Time. We handle both string and time.Time in code.
type Frontmatter struct {
	Title      string `yaml:"title"`
	Type       string `yaml:"type"`
	Created    any    `yaml:"created"`
	Updated    any    `yaml:"updated"`
	Domain     string `yaml:"domain"`
	Confidence string `yaml:"confidence"`
	Tags       any    `yaml:"tags"`
	Related    any    `yaml:"related"`
	Sources    any    `yaml:"sources"`
	// Aliases lists alternate titles that wikilinks may use to reference
	// this article. Borrowed from zk's `aliases:` convention: lets absorbed
	// content refer to an article by a variant spelling (e.g. "Hlidac shopu"
	// vs "Hlidac Shopu") without generating a missing-link warning. Orphan
	// and lint passes treat aliases as valid existing titles.
	Aliases any    `yaml:"aliases,omitempty"`
	Status  string `yaml:"status,omitempty"`
	Rolling bool   `yaml:"rolling,omitempty"`
	Stack   string `yaml:"stack,omitempty"`
	Verdict string `yaml:"verdict,omitempty"`
	Problem string `yaml:"problem,omitempty"`
	Depth   string `yaml:"depth,omitempty"`
	// Authority marks how load-bearing an article's claims are when two
	// sources contradict. Used by `scribe lint --resolve` to pick a winner.
	//   canonical — intentional decisions/policies; wins over everything
	//   contextual — curated solutions/patterns; wins over opinion
	//   opinion — raw captures, tweets, excerpts; loses by default
	// Absent = "contextual" for wiki pages, "opinion" for raw articles.
	Authority string `yaml:"authority,omitempty"`
}

// parseFrontmatter extracts YAML frontmatter from markdown content.
func parseFrontmatter(content []byte) (*Frontmatter, error) {
	s := string(content)
	if !strings.HasPrefix(s, "---") {
		return nil, fmt.Errorf("no frontmatter delimiter")
	}
	end := strings.Index(s[3:], "\n---")
	if end < 0 {
		return nil, fmt.Errorf("no closing frontmatter delimiter")
	}
	yamlBytes := []byte(s[3 : end+3])
	var fm Frontmatter
	if err := yaml.Unmarshal(yamlBytes, &fm); err != nil {
		// Handle duplicate keys
		deduped := deduplicateYAMLKeys(string(yamlBytes))
		if err2 := yaml.Unmarshal([]byte(deduped), &fm); err2 != nil {
			return nil, fmt.Errorf("invalid YAML: %w", err)
		}
	}
	return &fm, nil
}

// parseFrontmatterRaw extracts raw YAML map for field presence checking.
// Handles duplicate keys (common in LLM-generated frontmatter) by deduplicating
// before parsing — last value wins, matching Python's yaml.safe_load behavior.
func parseFrontmatterRaw(content []byte) (map[string]any, error) {
	s := string(content)
	if !strings.HasPrefix(s, "---") {
		return nil, fmt.Errorf("no frontmatter delimiter")
	}
	end := strings.Index(s[3:], "\n---")
	if end < 0 {
		return nil, fmt.Errorf("no closing frontmatter delimiter")
	}
	yamlBytes := []byte(s[3 : end+3])
	var raw map[string]any
	if err := yaml.Unmarshal(yamlBytes, &raw); err != nil {
		// Go's yaml.v3 rejects duplicate keys. Try deduplicating.
		deduped := deduplicateYAMLKeys(string(yamlBytes))
		if err2 := yaml.Unmarshal([]byte(deduped), &raw); err2 != nil {
			return nil, fmt.Errorf("invalid YAML: %w", err)
		}
	}
	return raw, nil
}

// deduplicateYAMLKeys removes duplicate top-level keys, keeping the last occurrence.
func deduplicateYAMLKeys(yamlStr string) string {
	lines := strings.Split(yamlStr, "\n")
	seen := make(map[string]int) // key -> last line index
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Only top-level keys (no leading whitespace, has colon)
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			if colonIdx := strings.Index(trimmed, ":"); colonIdx > 0 {
				key := trimmed[:colonIdx]
				if prev, exists := seen[key]; exists {
					lines[prev] = "" // blank out earlier occurrence
				}
				seen[key] = i
			}
		}
	}
	var result []string
	for _, line := range lines {
		if line != "" || len(result) == 0 {
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}

// extractWikilinks returns all [[Target]] links from markdown content.
// Strips fenced code blocks AND inline code spans before scanning to avoid
// false positives from code. Order matters: fenced blocks → double-backtick
// spans → single-backtick spans. Double first because “ `foo` “ nests a
// single-backtick pair inside a double-backtick span, and stripping single
// first leaves a stray “ “ “ that throws off multiline parity.
// Handles piped links [[Target|Display]] by extracting just the target.
func extractWikilinks(content []byte) []string {
	cleaned := codeFenceRE.ReplaceAll(content, nil)
	cleaned = codeSpanDoubleRE.ReplaceAll(cleaned, nil)
	cleaned = codeSpanRE.ReplaceAll(cleaned, nil)
	matches := wikilinkRE.FindAllSubmatch(cleaned, -1)
	seen := make(map[string]bool)
	var links []string
	for _, m := range matches {
		target := string(m[1])
		// Handle piped links: [[Target|Display Text]]
		if idx := strings.Index(target, "|"); idx > 0 {
			target = target[:idx]
		}
		target = strings.TrimSpace(target)
		if target != "" && !seen[target] {
			seen[target] = true
			links = append(links, target)
		}
	}
	return links
}

// extractTitleFast extracts the title from frontmatter using regex.
// Faster than full YAML parsing when only the title is needed.
func extractTitleFast(content []byte) string {
	m := titleLineRE.FindSubmatch(content)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// walkArticles walks all .md files in wiki dirs, skipping meta files and _-prefixed files.
// Use this for title collection and article enumeration.
func walkArticles(root string, fn func(path string, content []byte) error) error {
	return walkMarkdown(root, true, fn)
}

// walkAllMarkdown walks all .md files in wiki dirs, including _-prefixed files.
// Use this for wikilink scanning (links in _index.md should still count).
func walkAllMarkdown(root string, fn func(path string, content []byte) error) error {
	return walkMarkdown(root, false, fn)
}

func walkMarkdown(root string, skipUnderscored bool, fn func(path string, content []byte) error) error {
	for _, dir := range wikiDirs {
		dirPath := filepath.Join(root, dir)
		if _, err := os.Stat(dirPath); os.IsNotExist(err) {
			continue
		}
		err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil //nolint:nilerr // skip unreadable, continue walk
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".md") {
				return nil
			}
			if skipFiles[info.Name()] {
				return nil
			}
			if strings.HasPrefix(info.Name(), ".") {
				return nil
			}
			if skipUnderscored && strings.HasPrefix(info.Name(), "_") {
				return nil
			}
			content, err := os.ReadFile(path) //nolint:gosec // user-supplied KB root, deliberate walk
			if err != nil {
				return nil //nolint:nilerr // skip unreadable, continue walk
			}
			return fn(path, content)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// countLines counts lines in a byte slice.
func countLines(content []byte) int {
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	n := 0
	for scanner.Scan() {
		n++
	}
	return n
}

// relPath returns path relative to root, or the original path on error.
func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}
