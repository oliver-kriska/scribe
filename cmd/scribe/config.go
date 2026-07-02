package main

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	gosync "sync"

	"gopkg.in/yaml.v3"
)

// autoFlipLogged dedupes the "<op>.provider forces mode=X" log lines so
// the auto-flip notice fires exactly once per (key, value) pair per
// process. loadConfig gets called from every subcommand entry point;
// without dedup, a single `scribe sync` printed 5 lines for every
// loadConfig call (15-30 lines per real run).
var (
	autoFlipLoggedMu gosync.Mutex
	autoFlipLogged   = map[string]bool{}
)

// logAutoFlipOnce prints `msg` (formatted with args) the first time it
// is called with a given `key` in this process. Subsequent calls with
// the same key are silent. The key should be specific enough to
// distinguish meaningful state changes (e.g. "dream:provider=ollama"
// changes are worth logging once; the same flip from the same value
// every minute is noise).
//
// SCRIBE_QUIET_CONFIG=1 suppresses every call. Sync sets the env var on
// child processes (lint, scan, backlinks, index) so the parent's sync
// log doesn't echo the same 6 config lines per subprocess. End users
// can also set it manually for very quiet cron output.
func logAutoFlipOnce(key, script, msg string, args ...any) {
	if os.Getenv("SCRIBE_QUIET_CONFIG") == "1" {
		return
	}
	autoFlipLoggedMu.Lock()
	if autoFlipLogged[key] {
		autoFlipLoggedMu.Unlock()
		return
	}
	autoFlipLogged[key] = true
	autoFlipLoggedMu.Unlock()
	logMsg(script, msg, args...)
}

// wikiDirs lists all content directories in the KB.
var wikiDirs = []string{
	"wiki", "projects", "research", "solutions", "tools",
	"decisions", "patterns", "ideas", "people", "sessions",
}

// ScribeConfig holds configuration loaded from scribe.yaml.
type ScribeConfig struct {
	// LoadErr records a scribe.yaml parse failure (duplicate key, bad
	// indent — one sloppy merge away in a team KB). The config then
	// holds pure defaults, which MUST NOT be trusted for anything with
	// teeth: defaults mean team=false (secret gate off) and
	// provider=anthropic (paid calls a KB may have routed to ollama).
	// The secret gate fails closed on it and LLM entry commands abort
	// via requireParseable; read-only commands may proceed on defaults.
	LoadErr error `yaml:"-"`

	OwnerName    string   `yaml:"owner_name"`
	OwnerContext string   `yaml:"owner_context"`
	Domains      []string `yaml:"domains"`
	// Team marks a shared KB (several people pushing to one repo). It
	// activates the config trust layer (config_trust.go): sensitive
	// repo-config keys get locked to a per-machine approved snapshot,
	// and iMessage capture only ever runs from scribe.local.yaml. The
	// flag is itself a locked key — once a machine has trusted a
	// team:true config, a pushed team:false cannot unlock it.
	Team              bool   `yaml:"team"`
	ClaudeProjectsDir string `yaml:"claude_projects_dir"`
	// CodexSessionsDir is the OpenAI Codex CLI rollouts directory
	// (~/.codex/sessions). `scribe sync --discover` walks rollouts here
	// to find projects you've touched only via Codex, parallel to
	// ClaudeProjectsDir. Missing dir is optional — Codex is not
	// required, doctor surfaces it as a WARN.
	CodexSessionsDir string `yaml:"codex_sessions_dir"`
	// Sources scopes which project paths discovery may enroll —
	// include/exclude path globs evaluated before a project ever lands
	// in the manifest. See SourcesConfig (sources.go) for semantics.
	Sources SourcesConfig `yaml:"sources"`
	// Owners routes quality findings (stale articles, contradictions,
	// conflict markers) to a named person per domain in the team digest.
	// Coarse on purpose — domain granularity, not per-file CODEOWNERS,
	// because fine-grained ownership maps themselves rot. The documented
	// killer of shared KBs is "everyone assumes someone else will update
	// the page"; a name per domain is the cheapest fix. Keys are domain
	// names; values are display names matching `contributor:`.
	Owners       map[string]string `yaml:"owners"`
	CcriderDB    string            `yaml:"ccrider_db"`
	LockDir      string            `yaml:"lock_dir"`
	DefaultModel string            `yaml:"default_model"`
	// KBName is the display-level name of this KB, used for:
	//   - the drop-file directory other projects write to
	//     (`.claude/<kb_name>/*.md`)
	//   - the `<kb_name>: true` frontmatter flag those drop files carry
	//   - log prefixes where the name adds useful disambiguation
	// Defaults to the basename of the KB root directory. Override only
	// if your folder name and the name you want to expose differ.
	KBName string `yaml:"kb_name"`
	// LLM is the top-level fallback for every per-op LLM call. Phase
	// 4A.5: when a per-op Provider/Model field is empty, the resolver
	// inherits from here. Lets the user flip the whole pipeline to
	// Ollama with a single `llm.provider: ollama` line in scribe.yaml.
	// Defaults: provider=anthropic, model="" (each op picks its own
	// sensible default — haiku for cheap passes, sonnet for prose).
	LLM     LLMConfig     `yaml:"llm"`
	Sync    SyncConfig    `yaml:"sync"`
	Deep    DeepConfig    `yaml:"deep"`
	Capture CaptureConfig `yaml:"capture"`
	// Integrations holds the non-secret config for pull adapters (`scribe
	// pull` — Pinboard, and future bookmark sources). API tokens NEVER live
	// here; they go in ~/.config/scribe/config.yaml under integration_tokens.
	// Personal source like Capture: zeroed in team mode, re-enabled locally.
	Integrations IntegrationsConfig `yaml:"integrations"`
	Triage       TriageConfig       `yaml:"triage"`
	Absorb       AbsorbConfig       `yaml:"absorb"`
	Ingest       IngestConfig       `yaml:"ingest"`
	Identities   IdentitiesConfig   `yaml:"identities"`
	Relations    RelationsConfig    `yaml:"relations"`
	SessionMine  SessionMineConfig  `yaml:"session_mine"`
	Codex        CodexConfig        `yaml:"codex"`
	Dream        DreamConfig        `yaml:"dream"`
	// Subscriptions surface teammates' incoming articles after each
	// pull — domains/tags this user cares about, typically set in the
	// gitignored scribe.local.yaml so each member subscribes
	// independently. Matches print to the sync log; notify=true also
	// fires a macOS notification (best effort).
	Subscriptions SubscriptionsConfig `yaml:"subscriptions"`
	// SecretScan tunes the team-mode credential gate (secrets.go) that
	// holds staged articles back from commit when they contain
	// real-shaped tokens. Trust-locked: a pushed `disable: true` can't
	// switch another member's gate off.
	SecretScan SecretScanConfig `yaml:"secret_scan"`
	// StopWords is the user-defined commit gate (stopwords.go): markdown
	// staged with a held word never commits, and masked words are
	// redacted in place. This is the SHARED (team-policy) half; each
	// member also unions their own ~/.config/scribe/config.yaml list,
	// which is never committed. NOT trust-locked — see StopWordsConfig.
	StopWords  StopWordsConfig  `yaml:"stop_words"`
	Assess     AssessConfig     `yaml:"assess"`
	DeepIngest DeepIngestConfig `yaml:"deep_ingest"`
	Extract    ExtractConfig    `yaml:"extract"`
	Meta       MetaConfig       `yaml:"meta"`
	// Each tunes per-KB cadence for the KB-agnostic scheduler (issue #26):
	// `scribe each` skips a job in this KB when its last ok run is younger
	// than the configured interval, so the machine-level agent set can tick
	// coarsely while each KB paces itself. See EachConfig (each.go).
	Each EachConfig `yaml:"each"`
}

// MetaConfig controls the envelope's MetaAction surface — the side-
// channel writes (log.md, sessions log, rolling memory) that don't
// fit inside the wiki-dirs sandbox. RollingTargets pins which target
// stems rolling_memory_append accepts; defaults to learnings +
// decisions-log (the original pre-4C set). Users can add domain-
// specific targets like "incidents" or "migrations-log" without code
// changes.
type MetaConfig struct {
	// RollingTargets is the closed list of file stems
	// rolling_memory_append will write under <domain>/<stem>.md.
	// Empty defaults to [learnings, decisions-log]. Names must be
	// kebab-case alphanumeric — applyMetaDefaults validates and
	// drops anything that looks like a path component.
	RollingTargets []string `yaml:"rolling_targets"`
}

// applyMetaDefaults validates and fills the meta block. Empty
// RollingTargets list inherits the historical {learnings,
// decisions-log} pair. Invalid stems (anything containing a path
// separator, slash, or `..`) get dropped with a log line so a typo
// can't open a path-traversal hole through the rolling_memory_append
// op.
func applyMetaDefaults(cfg *MetaConfig) {
	if len(cfg.RollingTargets) == 0 {
		cfg.RollingTargets = []string{"learnings", "decisions-log"}
		return
	}
	seen := map[string]bool{}
	cleaned := make([]string, 0, len(cfg.RollingTargets))
	for _, t := range cfg.RollingTargets {
		s := strings.TrimSpace(t)
		if s == "" {
			continue
		}
		if strings.ContainsAny(s, "/\\") || strings.Contains(s, "..") {
			logMsg("config", "meta.rolling_targets: dropping %q (must be a bare stem, no path separators)", t)
			continue
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		cleaned = append(cleaned, s)
	}
	if len(cleaned) == 0 {
		cleaned = []string{"learnings", "decisions-log"}
	}
	cfg.RollingTargets = cleaned
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

// ingestDefaults returns the canonical IngestConfig defaults. Each *bool
// gets its own variable — yaml.Unmarshal writes through prefilled non-nil
// pointers, so a shared default bool would alias the two knobs (see the
// absorbDefaults doc comment for the full story).
func ingestDefaults() IngestConfig {
	mpsFallback := true
	smartRouting := true
	return IngestConfig{
		InboxPath: "raw/inbox",
		Marker: IngestMarkerConfig{
			TimeoutSeconds: 300,
			MPSFallback:    &mpsFallback,
			Device:         "auto",
		},
		Converters: map[string]string{},
		SmartRouting: IngestSmartRoutingConfig{
			Enabled:     &smartRouting,
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
	// SelfChatHandle is the legacy singular form. Still honored, but new
	// configs should prefer SelfChatHandles. When both are set the lists are
	// merged and deduplicated.
	SelfChatHandle string `yaml:"self_chat_handle"`

	// SelfChatHandles lists every iMessage address the user sends to
	// themselves. Most accounts have at least two: a phone number and an
	// Apple ID email. Each maps to a distinct chat in chat.db, so capture
	// must query all of them or it silently skips messages sent to the
	// non-configured chat.
	SelfChatHandles []string `yaml:"self_chat_handles"`

	// SkipDomains: URLs containing any of these substrings are ignored during
	// capture. Useful for non-content hosts (short-form video, audiobook
	// players, etc.) that you don't want landing in raw/articles/. Defaults
	// to empty — users add their own preferences.
	SkipDomains []string `yaml:"skip_domains"`
}

// IntegrationsConfig maps an integration name (e.g. "pinboard") to its
// non-secret settings. Secrets (API tokens) never live here — they go in
// ~/.config/scribe/config.yaml under integration_tokens, so a token can never
// be committed to a shared KB. `scribe pull` iterates the source registry
// (source.go) and pulls each configured integration.
type IntegrationsConfig map[string]IntegrationConfig

// IntegrationConfig holds the shared, non-secret knobs a pull adapter reads.
type IntegrationConfig struct {
	// Enabled gates the integration; a pull run soft-skips when false.
	Enabled bool `yaml:"enabled"`
	// Scope selects which items to pull by read-state/recency. Adapter-
	// interpreted; Pinboard understands recent+unread | unread | all.
	// Empty → adapter default (recent+unread).
	Scope string `yaml:"scope"`
	// Tags is an OR filter applied by the generic driver: a bookmark is
	// ingested only if it carries at least one of these tags (case-
	// insensitive). Empty (the default) ingests everything the scope
	// returns. Orthogonal to Scope — e.g. scope: all + tags: [kb] means
	// "every bookmark I ever tagged kb".
	Tags []string `yaml:"tags"`
	// PublicOnly, when true, skips private (non-shared) bookmarks so only
	// public ones reach the KB. Default false ingests everything, since an
	// authenticated pull sees private bookmarks too. Useful when the KB may
	// be shared/promoted and private bookmarks shouldn't ride along.
	PublicOnly bool `yaml:"public_only"`
	// SkipDomains drops queued URLs containing any of these substrings —
	// the same substring filter as capture.skip_domains.
	SkipDomains []string `yaml:"skip_domains"`
}

// integrationConfig returns the config block for a named integration and
// whether it was present.
func integrationConfig(cfg *ScribeConfig, name string) (IntegrationConfig, bool) {
	if cfg == nil || cfg.Integrations == nil {
		return IntegrationConfig{}, false
	}
	ic, ok := cfg.Integrations[name]
	return ic, ok
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

	// AutoApprove restores pre-0.2.30 discovery behavior: newly
	// discovered projects enroll immediately instead of landing as
	// status=pending and waiting for `scribe projects approve`. The
	// approval gate is the default because auto-enrolling every folder
	// Claude/Codex was ever opened in pulls repos the user doesn't care
	// about into the KB (the mise/direnv-style trust model: new sources
	// need a nod first).
	AutoApprove bool `yaml:"auto_approve"`

	// AlwaysPullBeforeSync runs `git pull --rebase --autostash` at the
	// start of `scribe sync` so teammates' committed pages show up in
	// this run before extraction/absorb starts. Silently skipped when
	// the KB is not a git repo or has no remote. Default: true. Set to
	// false if the KB lives somewhere you don't want network calls (air-
	// gapped laptops, offline laptops on cron).
	AlwaysPullBeforeSync *bool `yaml:"always_pull_before_sync"`

	// DailyOutputTokenCeiling is a hard backstop against runaway spend
	// on ANY metered provider — anthropic AND hosted OpenAI-compatible
	// providers (together/groq/fireworks/huggingface/openai-compat).
	// When the sum of output_tokens in today's
	// output/costs/<date>.jsonl across metered (non-local) rows reaches
	// this number, further metered calls abort with
	// ErrDailyBudgetExhausted; sync's outer loop catches that and exits
	// cleanly so cron doesn't crashloop. Local Ollama calls are exempt
	// (free). When set (>0) this takes precedence over the legacy
	// anthropic-only field below. Zero (default) defers to that field.
	// SCRIBE_BYPASS_BUDGET=1 bypasses the check for one-off manual runs.
	DailyOutputTokenCeiling int64 `yaml:"daily_output_token_ceiling"`
	// DailyAnthropicOutputTokenCeiling is the original, anthropic-only
	// name for the ceiling, kept for backward compatibility. Prefer
	// daily_output_token_ceiling above, which also covers hosted
	// providers. When both are set, the generalized field wins. The
	// budget sum itself now counts every metered provider regardless of
	// which key names the limit — see effectiveOutputTokenCeiling.
	DailyAnthropicOutputTokenCeiling int64 `yaml:"daily_anthropic_output_token_ceiling"`
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

// fillDiscoveryDefaults sets the built-in $HOME-based discovery paths on
// any field still empty. loadConfig prefills these before unmarshal; the
// trust layer's drift revert calls it again because the trusted snapshot
// stores raw (possibly empty) values that wipe the prefill.
func fillDiscoveryDefaults(cfg *ScribeConfig) {
	if cfg.ClaudeProjectsDir == "" {
		cfg.ClaudeProjectsDir = filepath.Join(os.Getenv("HOME"), ".claude", "projects")
	}
	if cfg.CodexSessionsDir == "" {
		cfg.CodexSessionsDir = filepath.Join(os.Getenv("HOME"), ".codex", "sessions")
	}
	if cfg.CcriderDB == "" {
		cfg.CcriderDB = filepath.Join(os.Getenv("HOME"), ".config", "ccrider", "sessions.db")
	}
}

// loadConfig reads scribe.yaml from the KB root. Returns defaults if not found.
func loadConfig(root string) *ScribeConfig {
	cfg := &ScribeConfig{
		Domains:           []string{},
		ClaudeProjectsDir: filepath.Join(os.Getenv("HOME"), ".claude", "projects"),
		CodexSessionsDir:  filepath.Join(os.Getenv("HOME"), ".codex", "sessions"),
		CcriderDB:         filepath.Join(os.Getenv("HOME"), ".config", "ccrider", "sessions.db"),
		LockDir:           "/tmp",
		DefaultModel:      "sonnet",
		Sync:              SyncConfig{MaxExtractions: 3, MaxSessions: 3, MaxAbsorb: 5, ParallelExtractions: 3, CheckpointInterval: 5, MaxExtractFiles: 100},
		Deep:              DeepConfig{BatchMax: 5},
		Capture:           CaptureConfig{SelfChatHandle: ""},
		Absorb:            absorbDefaults(),
		Ingest:            ingestDefaults(),
	}
	// Pass2Mode must NOT be pre-seeded: applyAbsorbDefaultsWithLLM needs
	// "" to mean "unset in yaml" so the non-anthropic auto-flip coerces
	// the code default silently and only logs when overriding a value
	// the user wrote. The fill back to "tools" happens there.
	cfg.Absorb.Pass2Mode = ""
	// Same rationale for the per-op absorb providers: absorbDefaults()
	// pre-seeds them to "anthropic" (for the standalone, no-LLM
	// applyAbsorbDefaults path), but here they must read "" = "unset in
	// yaml" so applyAbsorbDefaultsWithLLM can cascade llm.provider into
	// them. Without this reset they're never empty, the inheritance is
	// dead code, and `llm.provider: ollama` silently fails to move the
	// (token-heavy) absorb stage off Anthropic — contradicting the
	// documented "flip one line to go fully local" behavior. The
	// resolver falls back to anthropic only when llm.provider is empty.
	cfg.Absorb.Pass1Provider = ""
	cfg.Absorb.Pass2Provider = ""
	cfg.Absorb.SinglePassProvider = ""
	cfg.Absorb.FactsProvider = ""
	// Contextualize is part of the same absorb pipeline and must cascade
	// llm.provider identically — otherwise a fully-ollama/hosted KB leaves
	// the contextualize pass pinned to the pre-seeded "anthropic" default,
	// silently billing (or, for a no-anthropic-key hosted setup, failing)
	// the one stage everyone forgets. Reset for the same "" = unset reason.
	cfg.Absorb.Contextualize.Provider = ""

	cfgPath := filepath.Join(root, "scribe.yaml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		applyLLMDefaults(&cfg.LLM)
		applyAbsorbDefaultsWithLLM(&cfg.Absorb, cfg.LLM)
		applyIngestDefaults(&cfg.Ingest)
		applyRelationsDefaults(&cfg.Relations, cfg.LLM)
		applySessionMineDefaults(&cfg.SessionMine, cfg.LLM)
		applyDreamDefaults(&cfg.Dream, cfg.LLM)
		applyAssessDefaults(&cfg.Assess, cfg.LLM)
		applyDeepIngestDefaults(&cfg.DeepIngest, cfg.LLM)
		applyExtractDefaults(&cfg.Extract, cfg.LLM)
		applyMetaDefaults(&cfg.Meta)
		applyCodexDefaults(&cfg.Codex)
		return cfg
	}
	// loadConfig used to swallow yaml.Unmarshal errors silently, which
	// meant a single duplicate key (e.g. "pass2_timeout_min" defined twice)
	// wiped every overridden field back to defaults with zero warning.
	// Log AND record the failure: read-only commands still run on
	// defaults, but LoadErr makes the secret gate fail closed and the
	// LLM-cost entry points abort (the e2e sweep proved a duplicate key
	// silently disarmed team mode and pushed a credential).
	if err := yaml.Unmarshal(data, cfg); err != nil {
		cfg.LoadErr = err
		logMsg("config", "scribe.yaml has errors — falling back to defaults: %v", err)
	}

	// Trust layer (config_trust.go), in this exact order: first judge
	// the repo-controlled view against the per-machine trust record
	// (team KBs: drifted sensitive keys revert to trusted values,
	// capture is hard-off), THEN apply the user-owned scribe.local.yaml
	// so local overrides win over both the repo file and the revert.
	enforceConfigTrust(root, cfg)
	applyLocalOverrides(root, cfg)

	// Expand ~ in paths.
	cfg.ClaudeProjectsDir = expandHome(cfg.ClaudeProjectsDir)
	cfg.CodexSessionsDir = expandHome(cfg.CodexSessionsDir)
	cfg.CcriderDB = expandHome(cfg.CcriderDB)
	cfg.LockDir = expandHome(cfg.LockDir)

	// Merge user overrides on top of absorb defaults (zero-valued fields
	// inherit). Partial user config is legal and common. LLMConfig
	// defaults are applied first so applyAbsorbDefaults can inherit
	// provider/model fall-throughs from it.
	applyLLMDefaults(&cfg.LLM)
	applyAbsorbDefaultsWithLLM(&cfg.Absorb, cfg.LLM)
	applyIngestDefaults(&cfg.Ingest)
	applyRelationsDefaults(&cfg.Relations, cfg.LLM)
	applySessionMineDefaults(&cfg.SessionMine, cfg.LLM)
	applyDreamDefaults(&cfg.Dream, cfg.LLM)
	applyAssessDefaults(&cfg.Assess, cfg.LLM)
	applyDeepIngestDefaults(&cfg.DeepIngest, cfg.LLM)
	applyExtractDefaults(&cfg.Extract, cfg.LLM)
	applyMetaDefaults(&cfg.Meta)
	applyCodexDefaults(&cfg.Codex)

	// loadConfig is pure: it never writes scribe.yaml. The first-use
	// `absorb:` backfill moved to maybeBackfillAbsorbBlock, invoked only
	// from mutating entrypoints (sync, init). Before, the backfill fired
	// from *any* loadConfig caller — so `scribe doctor`/`status` and
	// `--dry-run` silently rewrote the user's config (Codex finding,
	// 2026-05-15). Diagnostics and dry runs must not mutate state.
	return cfg
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
	"_backlinks.json":          true,
	"_absorb_log.json":         true,
	"_sessions_log.json":       true,
	"_codex_sessions_log.json": true,
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
	// KBs is the machine's KB registry (issue #26): every KB the
	// scheduler iterates and that cwd resolution can route to. Empty
	// means "single-KB install" — registeredKBs() falls back to
	// [KBDir], so existing setups migrate with zero changes. There is no
	// privileged "main" KB; KBDir degrades to an optional default for
	// bare commands run outside any project.
	KBs []string `yaml:"kbs,omitempty"`
	// Contributor overrides the identity stamped into the
	// `contributor:` frontmatter of newly created articles. Lives in
	// the per-person config (not the KB's scribe.yaml) so members of a
	// shared team KB each attribute their own extractions. Empty means
	// fall back to `git config user.name` / user.email.
	Contributor string `yaml:"contributor,omitempty"`
	// LLMAPIKey is the hosted-provider API key for OpenAI-compatible
	// providers (together/groq/fireworks/huggingface/openai-compat).
	// It lives HERE — in the per-machine user config — and never in a
	// KB's scribe.yaml, so a key can't be committed to a shared KB. A
	// single key covers the common single-provider setup; use
	// LLMAPIKeys when you route different ops to different providers.
	// Resolution order is env var first (so a one-off export still
	// overrides), then this file — so cron works with no shell exports.
	LLMAPIKey string `yaml:"llm_api_key,omitempty"`
	// LLMAPIKeys maps a provider name (together/groq/fireworks/…) to its
	// API key, for setups that use more than one hosted provider at
	// once. Checked before LLMAPIKey. Same never-in-the-KB rule.
	LLMAPIKeys map[string]string `yaml:"llm_api_keys,omitempty"`
	// StopWords is the PERSONAL half of the stop-words commit gate
	// (stopwords.go): words you never want pushed to any KB, kept here
	// — never in a shared scribe.yaml — so listing an NDA codename or a
	// private name doesn't itself leak it to teammates. Unioned with the
	// KB's shared stop_words at gate time.
	StopWords StopWordsConfig `yaml:"stop_words,omitempty"`
	// DailyOutputTokenCeiling is the MACHINE-level metered output-token
	// ceiling across every registered KB (issue #26). The per-KB
	// `sync.daily_output_token_ceiling` in scribe.yaml caps one KB; this
	// caps the whole machine, because the Anthropic/hosted bill is per API
	// key and several KBs share it. Lives here (the per-machine config),
	// not in any KB's scribe.yaml, so one KB can't set the machine budget
	// for the others. Zero = disabled. See budget.go.
	DailyOutputTokenCeiling int64 `yaml:"daily_output_token_ceiling,omitempty"`
	// IntegrationTokens maps a pull-adapter name (pinboard, …) to its API
	// token. It lives HERE — the per-machine user config — and never in a
	// KB's scribe.yaml, so a token can't be committed to a shared KB (same
	// rule as LLMAPIKeys). Env SCRIBE_<NAME>_TOKEN overrides an entry.
	IntegrationTokens map[string]string `yaml:"integration_tokens,omitempty"`
}

// integrationToken resolves the API token for a pull integration. Env wins
// (SCRIBE_<NAME>_TOKEN, so a one-off export overrides), then the per-machine
// user config (integration_tokens.<name>). Never read from a KB's scribe.yaml
// — a token must not be committable to a shared KB.
func integrationToken(name string) string {
	env := "SCRIBE_" + strings.ToUpper(name) + "_TOKEN"
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v
	}
	return strings.TrimSpace(loadUserConfig().IntegrationTokens[name])
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
	for i := range uc.KBs {
		uc.KBs[i] = expandHome(uc.KBs[i])
	}
	return uc
}

// requireParseable is the guard for commands that spend money or write
// through the pipeline: a config that failed to parse is running on
// defaults nobody chose (wrong providers, team mode off). Call it right
// after loadConfig in LLM-cost / write entry points.
func (c *ScribeConfig) requireParseable() error {
	if c.LoadErr != nil {
		return fmt.Errorf("scribe.yaml is unparseable — fix it before running this command (parse error: %w)", c.LoadErr)
	}
	return nil
}

// kbDir resolves the knowledge base root directory.
// Priority: --root flag → SCRIBE_KB env → CWD walk → user config → error.
//
// The CWD walk MUST beat the user-config default: with more than one KB
// on a machine (personal + team — the whole point of promote/team mode),
// standing inside a KB checkout means *that* KB. Until 0.2.30 the user
// config won, so `cd team-kb && scribe sync` silently synced the
// personal KB — and init_plan's promise that other KBs stay reachable
// "by running scribe inside it" was false. The user config is the
// fallback for running scribe from arbitrary directories (cron jobs cd
// into the KB explicitly and are unaffected either way).
func kbDir() (string, error) {
	// 1. Explicit --root flag
	if globalRoot != "" {
		return globalRoot, nil
	}
	// 2. Environment variable
	if d := os.Getenv("SCRIBE_KB"); d != "" {
		return d, nil
	}
	// 3. Walk up from cwd looking for a KB marker (written by `scribe init`)
	cwd, err := os.Getwd()
	if err == nil {
		for dir := cwd; dir != "/"; dir = filepath.Dir(dir) {
			if isKBRoot(dir) {
				return dir, nil
			}
		}
	}
	// 4. User-level config (~/.config/scribe/config.yaml)
	if uc := loadUserConfig(); uc.KBDir != "" {
		if isKBRoot(uc.KBDir) {
			announceDefaultKB(uc.KBDir)
			return uc.KBDir, nil
		}
	}
	if err != nil {
		return "", err
	}
	return "", errors.New("cannot find scribe KB root; run `scribe init` inside your KB checkout, use -C <path>, or set SCRIBE_KB")
}

// announceDefaultKBOnce dedups the default-KB notice across repeated
// kbDir calls within one process.
var announceDefaultKBOnce gosync.Once

// announceDefaultKB prints a one-line stderr notice when KB resolution
// fell through to the machine-wide default. Reaching that fallback
// means cwd is NOT inside any KB — with several KBs on one machine this
// is the mis-target case (#27): the user may believe location implies
// the KB. Interactive shells only — hooks, cron and pipes stay clean,
// and explicit -C/SCRIBE_KB/cwd resolutions never get here.
func announceDefaultKB(root string) {
	announceDefaultKBOnce.Do(func() {
		if fi, err := os.Stderr.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			fmt.Fprintf(os.Stderr, "scribe: KB %s (global default — use -C, SCRIBE_KB, or cd into a KB to target another)\n", root)
		}
	})
}

// isKBRoot reports whether dir is a scribe KB root. Two markers:
// scripts/projects.json (the original marker; per-machine state) or
// scribe.yaml (always committed, via isScribeKB). A fresh clone of a
// shared team KB gitignores projects.json, so scribe.yaml is what makes
// the checkout resolvable before the first sync recreates the manifest.
func isKBRoot(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "scripts", "projects.json")); err == nil {
		return true
	}
	return isScribeKB(dir)
}
