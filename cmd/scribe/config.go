package main

import (
	"bufio"
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
	LLM         LLMConfig         `yaml:"llm"`
	Sync        SyncConfig        `yaml:"sync"`
	Deep        DeepConfig        `yaml:"deep"`
	Capture     CaptureConfig     `yaml:"capture"`
	Triage      TriageConfig      `yaml:"triage"`
	Absorb      AbsorbConfig      `yaml:"absorb"`
	Ingest      IngestConfig      `yaml:"ingest"`
	Identities  IdentitiesConfig  `yaml:"identities"`
	Relations   RelationsConfig   `yaml:"relations"`
	SessionMine SessionMineConfig `yaml:"session_mine"`
	Codex       CodexConfig       `yaml:"codex"`
	Dream       DreamConfig       `yaml:"dream"`
	Assess      AssessConfig      `yaml:"assess"`
	DeepIngest  DeepIngestConfig  `yaml:"deep_ingest"`
	Extract     ExtractConfig     `yaml:"extract"`
	Meta        MetaConfig        `yaml:"meta"`
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
		OllamaURL: "http://localhost:11434",
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
		ollamaURL = "http://localhost:11434"
	}
	provider, model = coerceProviderModel(label, provider, model)
	return provider, model, ollamaURL
}

// applyRelationsDefaults fills RelationsConfig from LLMConfig + sane
// defaults (Phase 4A.4/4A.5). Empty fields inherit; alias swap runs
// at the end so a Claude alias under ollama is rewritten cleanly.
func applyRelationsDefaults(cfg *RelationsConfig, llm LLMConfig) {
	if cfg.Provider == "" {
		cfg.Provider = llm.Provider
	}
	if cfg.Provider == "" {
		cfg.Provider = "anthropic"
	}
	if cfg.Model == "" {
		cfg.Model = llm.Model
	}
	if cfg.OllamaURL == "" {
		cfg.OllamaURL = llm.OllamaURL
	}
	if cfg.OllamaURL == "" {
		cfg.OllamaURL = "http://localhost:11434"
	}
	cfg.Provider, cfg.Model = coerceProviderModel("relations", cfg.Provider, cfg.Model)
}

// applyDreamDefaults fills DreamConfig. Mode defaults to monolithic
// for backward compat; non-anthropic provider auto-flips to
// orchestrator (Phase 4D) because the legacy tools path needs
// `claude -p`.
func applyDreamDefaults(cfg *DreamConfig, llm LLMConfig) {
	if cfg.Provider == "" {
		cfg.Provider = llm.Provider
	}
	if cfg.Provider == "" {
		cfg.Provider = "anthropic"
	}
	if cfg.Model == "" {
		cfg.Model = llm.Model
	}
	if cfg.OllamaURL == "" {
		cfg.OllamaURL = llm.OllamaURL
	}
	if cfg.OllamaURL == "" {
		cfg.OllamaURL = "http://localhost:11434"
	}
	if cfg.Mode == "" {
		cfg.Mode = "monolithic"
	}
	if cfg.TimeoutMin <= 0 {
		cfg.TimeoutMin = 20
	}
	if cfg.NumCtx <= 0 {
		cfg.NumCtx = llm.NumCtx
	}
	if cfg.NumCtx <= 0 {
		// Dream's orient packet is big (log tail + inventory + stale
		// list + contradictions). 16384 keeps the conclusion of the
		// inventory from being truncated.
		cfg.NumCtx = 16384
	}
	if !strings.EqualFold(cfg.Provider, "anthropic") && !strings.EqualFold(cfg.Mode, "orchestrator") {
		logAutoFlipOnce("dream:"+cfg.Provider, "config", "dream.provider=%q forces mode=orchestrator (was %q)", cfg.Provider, cfg.Mode)
		cfg.Mode = "orchestrator"
	}
	cfg.Provider, cfg.Model = coerceProviderModel("dream", cfg.Provider, cfg.Model)
}

// applyAssessDefaults / applyDeepIngestDefaults: Phase 4E. Same shape
// as Dream — Mode defaults to tools for backward compat, non-anthropic
// provider auto-flips to envelope mode.
func applyAssessDefaults(cfg *AssessConfig, llm LLMConfig) {
	if cfg.Provider == "" {
		cfg.Provider = llm.Provider
	}
	if cfg.Provider == "" {
		cfg.Provider = "anthropic"
	}
	if cfg.Model == "" {
		cfg.Model = llm.Model
	}
	if cfg.OllamaURL == "" {
		cfg.OllamaURL = llm.OllamaURL
	}
	if cfg.OllamaURL == "" {
		cfg.OllamaURL = "http://localhost:11434"
	}
	if cfg.Mode == "" {
		cfg.Mode = "tools"
	}
	if cfg.NumCtx <= 0 {
		cfg.NumCtx = llm.NumCtx
	}
	if cfg.NumCtx <= 0 {
		// Assess inlines top-level docs (12K char cap), source tree
		// (200 entries), and recent git log. Even with truncation
		// this routinely lands at 6-8K tokens.
		cfg.NumCtx = 32768
	}
	if !strings.EqualFold(cfg.Provider, "anthropic") && !strings.EqualFold(cfg.Mode, "envelope") {
		logAutoFlipOnce("assess:"+cfg.Provider, "config", "assess.provider=%q forces mode=envelope (was %q)", cfg.Provider, cfg.Mode)
		cfg.Mode = "envelope"
	}
	cfg.Provider, cfg.Model = coerceProviderModel("assess", cfg.Provider, cfg.Model)
}

func applyDeepIngestDefaults(cfg *DeepIngestConfig, llm LLMConfig) {
	if cfg.Provider == "" {
		cfg.Provider = llm.Provider
	}
	if cfg.Provider == "" {
		cfg.Provider = "anthropic"
	}
	if cfg.Model == "" {
		cfg.Model = llm.Model
	}
	if cfg.OllamaURL == "" {
		cfg.OllamaURL = llm.OllamaURL
	}
	if cfg.OllamaURL == "" {
		cfg.OllamaURL = "http://localhost:11434"
	}
	if cfg.Mode == "" {
		cfg.Mode = "tools"
	}
	if cfg.NumCtx <= 0 {
		cfg.NumCtx = llm.NumCtx
	}
	if cfg.NumCtx <= 0 {
		// Deep inlines per-directory file contents capped at 24K
		// chars (~6K tokens) — pair with 16384 to leave headroom for
		// the prompt and the envelope output.
		cfg.NumCtx = 16384
	}
	if !strings.EqualFold(cfg.Provider, "anthropic") && !strings.EqualFold(cfg.Mode, "envelope") {
		logAutoFlipOnce("deep_ingest:"+cfg.Provider, "config", "deep_ingest.provider=%q forces mode=envelope (was %q)", cfg.Provider, cfg.Mode)
		cfg.Mode = "envelope"
	}
	cfg.Provider, cfg.Model = coerceProviderModel("deep_ingest", cfg.Provider, cfg.Model)
}

// applyExtractDefaults fills ExtractConfig (Phase 4F). Same shape as
// Dream/Assess/Deep — Mode defaults to tools for backward compat, but
// a non-anthropic provider auto-flips to envelope.
func applyExtractDefaults(cfg *ExtractConfig, llm LLMConfig) {
	if cfg.Provider == "" {
		cfg.Provider = llm.Provider
	}
	if cfg.Provider == "" {
		cfg.Provider = "anthropic"
	}
	if cfg.Model == "" {
		cfg.Model = llm.Model
	}
	if cfg.OllamaURL == "" {
		cfg.OllamaURL = llm.OllamaURL
	}
	if cfg.OllamaURL == "" {
		cfg.OllamaURL = "http://localhost:11434"
	}
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
	if cfg.NumCtx <= 0 {
		cfg.NumCtx = llm.NumCtx
	}
	if cfg.NumCtx <= 0 {
		cfg.NumCtx = 16384
	}
	if !strings.EqualFold(cfg.Provider, "anthropic") && !strings.EqualFold(cfg.Mode, "envelope") {
		logAutoFlipOnce("extract:"+cfg.Provider, "config", "extract.provider=%q forces mode=envelope (was %q)", cfg.Provider, cfg.Mode)
		cfg.Mode = "envelope"
	}
	cfg.Provider, cfg.Model = coerceProviderModel("extract", cfg.Provider, cfg.Model)
}

// applySessionMineDefaults fills SessionMineConfig + the Phase 4C
// auto-flip: a non-anthropic provider forces envelope mode because
// `claude -p` is the only backend that supports the ccrider MCP tools
// the legacy path relies on.
func applySessionMineDefaults(cfg *SessionMineConfig, llm LLMConfig) {
	if cfg.Provider == "" {
		cfg.Provider = llm.Provider
	}
	if cfg.Provider == "" {
		cfg.Provider = "anthropic"
	}
	if cfg.Model == "" {
		cfg.Model = llm.Model
	}
	if cfg.OllamaURL == "" {
		cfg.OllamaURL = llm.OllamaURL
	}
	if cfg.OllamaURL == "" {
		cfg.OllamaURL = "http://localhost:11434"
	}
	if cfg.Mode == "" {
		cfg.Mode = "tools"
	}
	if cfg.TranscriptMaxChars <= 0 {
		cfg.TranscriptMaxChars = 24000
	}
	if cfg.TimeoutMin <= 0 {
		cfg.TimeoutMin = 8
	}
	if cfg.NumCtx <= 0 {
		cfg.NumCtx = llm.NumCtx
	}
	if cfg.NumCtx <= 0 {
		// Default TranscriptMaxChars 24000 + prompt skeleton + related
		// sessions block lands at 6-7K tokens. 16384 is the floor;
		// raising TranscriptMaxChars without raising NumCtx silently
		// truncates the conclusion of the transcript on Ollama.
		cfg.NumCtx = 16384
	}
	// Non-anthropic provider has no MCP support → force envelope mode.
	if !strings.EqualFold(cfg.Provider, "anthropic") && !strings.EqualFold(cfg.Mode, "envelope") {
		logAutoFlipOnce("session_mine:"+cfg.Provider, "config", "session_mine.provider=%q forces mode=envelope (was %q)", cfg.Provider, cfg.Mode)
		cfg.Mode = "envelope"
	}
	cfg.Provider, cfg.Model = coerceProviderModel("session_mine", cfg.Provider, cfg.Model)
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

// relationsProviderModel returns the (provider, model, ollamaURL) the
// relations-migrate classifier should use. Callers pass the CLI's
// --model flag as the explicit pin; when that's a Claude alias and the
// user has flipped `llm.provider: ollama`, the recommended local model
// wins. defaultModel falls back to "haiku" when both the CLI flag and
// config are empty (matches pre-4A.4 default).
func relationsProviderModel(root, cliModel string) (provider, model, ollamaURL string) {
	cfg := loadConfig(root)
	if cfg == nil {
		return "anthropic", cliModel, "http://localhost:11434"
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
		// Pass1 default bumped 3→5 because dense long-form articles (~17K
		// chars, 200+ lines) reliably timed out on haiku at 3 min and got
		// SIGKILLed mid-stream — costing the call without delivering an
		// entity list. Existing scribe.yaml entries pinning 3 still win.
		Pass1TimeoutMin:      5,
		Pass2TimeoutMin:      5,
		Pass2Parallel:        3,
		SinglePassTimeoutMin: 5,
		ChapterAware:         &trueV,
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

// ollamaRecommendedModel is the scribe-vetted default when the user picks
// provider=ollama but leaves model blank (or accidentally copies a Claude
// alias). Chosen in research/2026-04-20-ollama-model-for-contextualize.md
// for best speed/quality balance on Apple Silicon at ~3.3 GB Q4.
const ollamaRecommendedModel = "gemma3:4b"

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
	cfg.Pass1Provider, cfg.Pass1Model = coerceProviderModel("absorb.pass1", cfg.Pass1Provider, cfg.Pass1Model)
	if cfg.SinglePassProvider == "" {
		cfg.SinglePassProvider = llm.Provider
	}
	if cfg.SinglePassProvider == "" {
		cfg.SinglePassProvider = d.FactsProvider // same anthropic default
	}
	cfg.SinglePassProvider, cfg.SinglePassModel = coerceProviderModel("absorb.single_pass", cfg.SinglePassProvider, cfg.SinglePassModel)
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
	// pass2_mode left at "tools" would silently no-op. Log so the user
	// notices the override.
	if !strings.EqualFold(cfg.Pass2Provider, "anthropic") && !strings.EqualFold(cfg.Pass2Mode, "json") {
		logAutoFlipOnce("absorb.pass2:"+cfg.Pass2Provider, "config", "absorb.pass2_provider=%q forces pass2_mode=json (was %q)", cfg.Pass2Provider, cfg.Pass2Mode)
		cfg.Pass2Mode = "json"
	}
	// Provider/model coherence: ollama + Claude alias swap, same as facts.
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
	cfg.Contextualize.Provider, cfg.Contextualize.Model = coerceProviderModel("contextualize", cfg.Contextualize.Provider, cfg.Contextualize.Model)
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

	// DailyAnthropicOutputTokenCeiling is a hard backstop against
	// runaway Anthropic spend. When the sum of output_tokens in
	// today's output/costs/<date>.jsonl (anthropic provider only)
	// reaches this number, further runClaude / anthropicProvider
	// calls abort with ErrDailyBudgetExhausted. Sync's outer loop
	// catches that and exits cleanly so cron doesn't crashloop.
	// Local-provider calls (ollama, llama.cpp) are exempt.
	// Zero (default) disables the ceiling entirely. After the
	// 2026-05-11 runaway (~7M output tokens in 35 hours), a sensible
	// production value for daily background crons is ~2_000_000.
	// SCRIBE_BYPASS_BUDGET=1 in the environment bypasses the check
	// for one-off manual runs that knowingly need to exceed it.
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
	// wiped every overridden field back to defaults with zero warning. Log
	// the failure so misconfiguration surfaces immediately — but still fall
	// through with defaults rather than crash, so a broken config doesn't
	// take down the whole binary.
	if err := yaml.Unmarshal(data, cfg); err != nil {
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
	// Contributor overrides the identity stamped into the
	// `contributor:` frontmatter of newly created articles. Lives in
	// the per-person config (not the KB's scribe.yaml) so members of a
	// shared team KB each attribute their own extractions. Empty means
	// fall back to `git config user.name` / user.email.
	Contributor string `yaml:"contributor"`
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
		if isKBRoot(uc.KBDir) {
			return uc.KBDir, nil
		}
	}
	// 4. Walk up from cwd looking for a KB marker (written by `scribe init`)
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := cwd; dir != "/"; dir = filepath.Dir(dir) {
		if isKBRoot(dir) {
			return dir, nil
		}
	}
	return "", fmt.Errorf("cannot find scribe KB root; run `scribe init` inside your KB checkout, use -C <path>, or set SCRIBE_KB")
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
	// Contributor records who first created the article — stamped
	// automatically at commit time (stampContributor) from the user
	// config or git identity. In shared team KBs this is the provenance
	// signal beyond git blame; dream's contradiction resolution may
	// consult it when weighing competing claims.
	Contributor string `yaml:"contributor,omitempty"`
	// Stack is intentionally `any`: scaffolding (project overview frontmatter,
	// LLM-written drops) sometimes ships it as a YAML list (`stack: [Go, ...]`)
	// and sometimes as a plain string ("Go + SQLite + CGO"). Both shapes are
	// valid in the corpus today; the lint pass walks frontmatter as raw maps,
	// so the typed struct only needs to *accept* the value, not normalize it.
	// Fields like Tags/Related/Sources use the same approach.
	Stack   any    `yaml:"stack,omitempty"`
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
	// IndexTier (Phase 5B) controls qmd ranking weight. Computed by
	// scribe from body length, heading count, and article type unless
	// the human pinned a specific value via IndexTierOverride.
	//   stub      — ≤80 words OR fetched_via=stub; excluded from search
	//   brief     — 81–199 words OR fxtwitter capture
	//   standard  — 200–1999 words; ordinary article
	//   deep      — 2000+ words OR ≥5 sections in sidecar
	//   reference — explicit human marker for canonical artifacts
	IndexTier         string `yaml:"index_tier,omitempty"`
	IndexTierOverride string `yaml:"index_tier_override,omitempty"`

	// Phase 6A typed relations. Each field replaces a slice of the
	// generic `related:` list with a typed edge whose semantics
	// `scribe lint --resolve` and Phase 6B contradiction detection
	// can reason about. Type-specific allowed sets:
	//   decision  — supersedes, superseded_by, contradicts
	//   solution  — applies_to (pattern), derived_from (research)
	//   pattern   — instance_of, specializes
	//   research  — extends, cited_by, informs
	// Untyped `related:` stays as the easy-out for genuinely loose
	// connections. Each typed field carries [[Wikilinks]] just like
	// related:; the typing is purely about what the edge *means*.
	Supersedes   any `yaml:"supersedes,omitempty"`
	SupersededBy any `yaml:"superseded_by,omitempty"`
	Contradicts  any `yaml:"contradicts,omitempty"`
	AppliesTo    any `yaml:"applies_to,omitempty"`
	DerivedFrom  any `yaml:"derived_from,omitempty"`
	InstanceOf   any `yaml:"instance_of,omitempty"`
	Specializes  any `yaml:"specializes,omitempty"`
	Extends      any `yaml:"extends,omitempty"`
	CitedBy      any `yaml:"cited_by,omitempty"`
	Informs      any `yaml:"informs,omitempty"`
	// RelationsLocked tells the LLM relation migrator (Phase 6A v2)
	// to leave this article alone. Useful for hand-curated cases.
	RelationsLocked bool `yaml:"relations_locked,omitempty"`
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
