package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// DoctorCmd is a read-only health check for a scribe KB checkout. It audits
// dependencies, config, LaunchAgents, state files, and run freshness, then
// prints each check with an exact remediation command. Doctor never mutates
// anything: it diagnoses and points, the user runs the fixes.
//
// Exit code is non-zero only on hard failures (FAIL). Freshness drift is a
// warning, not a failure — cron might simply not have fired yet because the
// Mac was asleep.
type DoctorCmd struct {
	JSON        bool          `help:"Emit structured JSON instead of text."`
	Section     string        `help:"Run only one section: deps | config | cron | state | freshness | errors | convert | contradictions | stale | vault | localmode." enum:"deps,config,cron,state,freshness,errors,convert,contradictions,stale,vault,localmode," default:""`
	ErrorWindow time.Duration `help:"How far back to scan run records for errors." default:"24h"`
}

type checkStatus string

const (
	statusOK   checkStatus = "ok"
	statusWarn checkStatus = "warn"
	statusFail checkStatus = "FAIL"
)

// check is one line of doctor's report. Each check carries enough context to
// render itself in either text or JSON mode without the formatter needing to
// know about the underlying probe.
type check struct {
	Section string      `json:"section"`
	Name    string      `json:"name"`
	Status  checkStatus `json:"status"`
	Detail  string      `json:"detail,omitempty"`
	Fix     string      `json:"fix,omitempty"`
}

// ReadOnly marks doctor as a pure diagnostic — main() skips the
// run-record append so a health check never mutates the KB it audits.
func (c *DoctorCmd) ReadOnly() bool { return true }

func (c *DoctorCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return fmt.Errorf("not inside a scribe KB checkout: %w", err)
	}
	cfg := loadConfig(root)

	sectionOrder := []string{"deps", "config", "localmode", "convert", "cron", "state", "freshness", "errors", "contradictions", "stale", "vault"}
	var all []check
	for _, name := range sectionOrder {
		if c.Section != "" && c.Section != name {
			continue
		}
		switch name {
		case "deps":
			all = append(all, checkDeps(cfg)...)
		case "config":
			all = append(all, checkConfig(root, cfg)...)
		case "localmode":
			all = append(all, checkLocalMode(cfg)...)
		case "convert":
			all = append(all, checkConvert()...)
		case "cron":
			all = append(all, checkCron(root)...)
		case "state":
			all = append(all, checkState(root, cfg)...)
		case "freshness":
			all = append(all, checkFreshness(root, time.Now())...)
		case "errors":
			all = append(all, checkRecentErrors(root, time.Now(), c.ErrorWindow)...)
		case "contradictions":
			all = append(all, checkContradictions(root)...)
		case "stale":
			all = append(all, checkStale(root)...)
		case "vault":
			all = append(all, checkVaultScaffolding(root)...)
		}
	}

	if c.JSON {
		printChecksJSON(all, root)
	} else {
		printChecksText(all, root)
		// Append the status scoreboard in text mode (skipped under --section
		// filters so section-targeted runs stay focused; skipped under --json
		// so the JSON shape stays stable).
		if c.Section == "" {
			fmt.Println()
			_ = renderStatus(os.Stdout, root)
		}
	}

	fails := 0
	for _, ck := range all {
		if ck.Status == statusFail {
			fails++
		}
	}
	if fails > 0 {
		return fmt.Errorf("%d check(s) failed — see output above", fails)
	}
	return nil
}

// ---- Dependencies ----

// runtimeGOOS is runtime.GOOS, indirected through a package var so the
// capability-aware FDA branch can be exercised for non-darwin in tests
// without a cross-compile. Production never reassigns it.
var runtimeGOOS = runtime.GOOS

func checkDeps(cfg *ScribeConfig) []check {
	var out []check
	for _, d := range scribeDeps {
		path, err := exec.LookPath(d.Binary)
		switch {
		case err == nil:
			out = append(out, check{Section: "deps", Name: d.Name, Status: statusOK, Detail: path})
		case d.Required:
			out = append(out, check{Section: "deps", Name: d.Name, Status: statusFail, Detail: "not found in PATH", Fix: d.Fix})
		default:
			out = append(out, check{Section: "deps", Name: d.Name, Status: statusWarn, Detail: "not found (optional)", Fix: d.Fix})
		}
	}

	// Full Disk Access probe is macOS- AND capture-specific. README
	// documents capture as optional and Linux as supported, and
	// `scribe fda` is macOS-only — so an unreadable chat.db is only a
	// hard FAIL when the user is actually on macOS *and* has capture
	// configured. Off that happy-path it's a skip/warn, never a FAIL
	// (Codex finding, 2026-05-15: doctor reported a false hard failure
	// on Linux and on macOS with capture intentionally unused).
	captureConfigured := len(resolveSelfChatHandles(cfg.Capture)) > 0
	switch {
	case runtimeGOOS != "darwin":
		// chat.db / TCC Full Disk Access is a macOS concept; there is
		// nothing to probe on Linux. Stay silent rather than emit a
		// confusing row on a platform where it can never apply.
	case !captureConfigured:
		out = append(out, check{
			Section: "deps", Name: "chat.db (FDA)", Status: statusWarn,
			Detail: "capture not configured (no capture.self_chat_handles) — FDA probe skipped",
			Fix:    "only needed if you want iMessage capture: set capture.self_chat_handles in scribe.yaml",
		})
	default:
		// On modern macOS (10.15+) TCC tracks the *binary being
		// executed* per inode+cdhash, not the parent Terminal, so the
		// fix is always "grant FDA to the scribe binary itself", which
		// `scribe fda` drives interactively.
		chatDB := filepath.Join(os.Getenv("HOME"), "Library", "Messages", "chat.db")
		if f, err := os.Open(chatDB); err == nil {
			_ = f.Close()
			out = append(out, check{Section: "deps", Name: "chat.db (FDA)", Status: statusOK, Detail: "readable"})
		} else {
			out = append(out, check{
				Section: "deps", Name: "chat.db (FDA)", Status: statusFail,
				Detail: "unreadable — `scribe capture` will fail",
				Fix:    "run `scribe fda` (grants Full Disk Access to the scribe binary)",
			})
		}

		// Warn about the Homebrew-Cellar / cdhash tax: the FDA grant
		// is keyed to the exact binary inode. Every `brew upgrade
		// scribe` replaces the Cellar-versioned binary, which
		// invalidates the prior grant and makes capture silently start
		// failing until the user re-runs `scribe fda`. Only relevant
		// when capture is actually in use.
		if exe, err := os.Executable(); err == nil {
			if resolved, err := filepath.EvalSymlinks(exe); err == nil && strings.Contains(resolved, "/Cellar/scribe/") {
				out = append(out, check{
					Section: "deps", Name: "FDA (brew upgrade)", Status: statusWarn,
					Detail: "running from " + resolved + " — the TCC grant is tied to this exact binary and will be invalidated by the next `brew upgrade scribe`",
					Fix:    "re-run `scribe fda` after every upgrade (until signed builds ship)",
				})
			}
		}
	}
	return out
}

// ---- Config ----

func checkConfig(root string, cfg *ScribeConfig) []check {
	var out []check

	cfgPath := filepath.Join(root, "scribe.yaml")
	if fileExists(cfgPath) {
		out = append(out, check{Section: "config", Name: "scribe.yaml", Status: statusOK, Detail: relPath(root, cfgPath)})
	} else {
		out = append(out, check{Section: "config", Name: "scribe.yaml", Status: statusWarn, Detail: "missing — using defaults", Fix: "scribe init"})
	}

	// Team-KB config trust (config_trust.go): drifted sensitive keys mean
	// scribe is deliberately ignoring part of the repo config — the user
	// must review. A team KB with no trust record yet hasn't synced since
	// the flag was set; nudge so the lock actually engages.
	if current, ok := repoSensitiveView(root); ok {
		rec := loadTrustRecord(root)
		switch {
		case rec != nil && rec.Sensitive.Team:
			if drift := sensitiveDiff(rec.Sensitive, current); len(drift) > 0 {
				out = append(out, check{
					Section: "config", Name: "config-trust", Status: statusWarn,
					Detail: fmt.Sprintf("repo scribe.yaml drifted from trusted snapshot (%d key(s)) — running on trusted values", len(drift)),
					Fix:    "review with `scribe config diff`, accept with `scribe config trust`, or revert the repo file",
				})
			} else {
				out = append(out, check{Section: "config", Name: "config-trust", Status: statusOK, Detail: "team KB — sensitive keys locked, no drift"})
			}
		case current.Team:
			out = append(out, check{
				Section: "config", Name: "config-trust", Status: statusWarn,
				Detail: "team: true but no trust record on this machine yet",
				Fix:    "run `scribe config trust` (or the next `scribe sync` records it)",
			})
		}
	}

	if dirExists(cfg.ClaudeProjectsDir) {
		out = append(out, check{Section: "config", Name: "claude_projects_dir", Status: statusOK, Detail: cfg.ClaudeProjectsDir})
	} else {
		out = append(out, check{
			Section: "config", Name: "claude_projects_dir", Status: statusFail,
			Detail: cfg.ClaudeProjectsDir + " does not exist",
			Fix:    "edit scribe.yaml or install Claude Code",
		})
	}

	// Codex sessions: optional. Missing dir / zero rollouts is WARN,
	// not FAIL — users without Codex CLI should still get a clean
	// doctor. When rollouts exist, we probe one to assert the
	// `cwd` field still parses non-empty so a future Codex schema
	// rename shows up here instead of silently breaking discovery.
	out = append(out, checkCodexSessions(cfg)...)

	if fileExists(cfg.CcriderDB) {
		out = append(out, check{Section: "config", Name: "ccrider_db", Status: statusOK, Detail: cfg.CcriderDB})
	} else {
		out = append(out, check{
			Section: "config", Name: "ccrider_db", Status: statusFail,
			Detail: cfg.CcriderDB + " does not exist",
			Fix:    "run ccrider once to initialize the database",
		})
	}

	claudeMD := filepath.Join(os.Getenv("HOME"), ".claude", "CLAUDE.md")
	data, err := os.ReadFile(claudeMD)
	switch {
	case err != nil:
		out = append(out, check{Section: "config", Name: "~/.claude/CLAUDE.md", Status: statusWarn, Detail: "not found", Fix: "scribe init"})
	case strings.Contains(string(data), claudeMDMarkerBegin) && strings.Contains(string(data), claudeMDMarkerEnd):
		if blockReferencesKB(string(data), root) {
			out = append(out, check{Section: "config", Name: "~/.claude/CLAUDE.md block", Status: statusOK, Detail: "installed"})
		} else {
			out = append(out, check{
				Section: "config", Name: "~/.claude/CLAUDE.md block", Status: statusWarn,
				Detail: "installed but references another KB — agent sessions query that one, not this",
				Fix:    "scribe init --bind  (repoints the block at this KB)",
			})
		}
	default:
		out = append(out, check{Section: "config", Name: "~/.claude/CLAUDE.md block", Status: statusWarn, Detail: "scribe block not found", Fix: "scribe init"})
	}

	// Codex CLI handshake: ~/.codex/AGENTS.md is Codex's analog of
	// ~/.claude/CLAUDE.md. WARN-only — Codex is optional, and AGENTS.md
	// is a softer contract (Codex churned codex.md → instructions.md →
	// AGENTS.md, and Desktop/managed installs may manage their own), so
	// this row reports *presence of the scribe block*, never "Codex is
	// reading it" — we can't probe the latter.
	codexMD := filepath.Join(os.Getenv("HOME"), ".codex", "AGENTS.md")
	cdata, cerr := os.ReadFile(codexMD)
	switch {
	case cerr != nil:
		out = append(out, check{Section: "config", Name: "~/.codex/AGENTS.md", Status: statusWarn, Detail: "not found (Codex CLI not set up?)", Fix: "scribe init"})
	case strings.Contains(string(cdata), claudeMDMarkerBegin) && strings.Contains(string(cdata), claudeMDMarkerEnd):
		if blockReferencesKB(string(cdata), root) {
			out = append(out, check{Section: "config", Name: "~/.codex/AGENTS.md block", Status: statusOK, Detail: "installed"})
		} else {
			out = append(out, check{
				Section: "config", Name: "~/.codex/AGENTS.md block", Status: statusWarn,
				Detail: "installed but references another KB — Codex sessions query that one, not this",
				Fix:    "scribe init --bind  (repoints the block at this KB)",
			})
		}
	default:
		out = append(out, check{Section: "config", Name: "~/.codex/AGENTS.md block", Status: statusWarn, Detail: "scribe block not found", Fix: "scribe init"})
	}

	return out
}

// blockReferencesKB reports whether the scribe handshake block mentions
// this KB's root path. The block embeds {{.KBDir}} when written, so a
// present-but-foreign block on a multi-KB machine means agent sessions
// query a different KB than the one doctor is auditing (#27).
func blockReferencesKB(data, root string) bool {
	begin := strings.Index(data, claudeMDMarkerBegin)
	end := strings.Index(data, claudeMDMarkerEnd)
	if begin < 0 || end < begin {
		return false
	}
	return strings.Contains(data[begin:end], root)
}

// checkCodexSessions probes the Codex CLI sessions root. Three states:
//
//	OK    — dir exists, contains at least one rollout-*.jsonl, and the
//	        most recent rollout's session_meta payload parses with a
//	        non-empty cwd (schema-drift sentinel — a future Codex
//	        rename of the `cwd` field would flip this to WARN).
//	WARN  — dir missing, contains zero rollouts, or the schema probe
//	        fails. Codex is optional; users without Codex CLI installed
//	        should still get a clean doctor.
//	(never FAIL — see WARN reasoning above.)
//
// Empty `codex_sessions_dir` in scribe.yaml is treated the same as a
// missing directory.
func checkCodexSessions(cfg *ScribeConfig) []check {
	dir := cfg.CodexSessionsDir
	if dir == "" {
		return []check{{
			Section: "config", Name: "codex_sessions_dir", Status: statusWarn,
			Detail: "unset in scribe.yaml — Codex discovery disabled",
			Fix:    "set codex_sessions_dir: ~/.codex/sessions or install Codex CLI",
		}}
	}
	if !dirExists(dir) {
		return []check{{
			Section: "config", Name: "codex_sessions_dir", Status: statusWarn,
			Detail: dir + " does not exist (Codex CLI not installed?)",
			Fix:    "install Codex CLI, or edit scribe.yaml to point at the right path",
		}}
	}

	probe := codexProbeRollout(dir)
	if probe == "" {
		return []check{{
			Section: "config", Name: "codex_sessions_dir", Status: statusWarn,
			Detail: dir + " — no rollouts yet",
		}}
	}
	meta, err := readCodexSessionMeta(probe)
	switch {
	case err != nil:
		return []check{{
			Section: "config", Name: "codex_sessions_dir", Status: statusWarn,
			Detail: fmt.Sprintf("%s — schema probe failed: %v", dir, err),
			Fix:    "Codex may have changed its session_meta schema — file an issue",
		}}
	case meta == nil || meta.Cwd == "":
		return []check{{
			Section: "config", Name: "codex_sessions_dir", Status: statusWarn,
			Detail: dir + " — probed rollout has empty/missing cwd (Codex schema may have changed)",
			Fix:    "file an issue at github.com/oliver-kriska/scribe",
		}}
	}

	// OK row. Capped rollout count is a cheap signal — full count for
	// huge histories is wasted work in the hot doctor path.
	count := codexRolloutCount(dir, 5000)
	suffix := fmt.Sprintf("%d rollout(s)", count)
	if count >= 5000 {
		suffix = "5000+ rollouts"
	}
	return []check{
		{
			Section: "config", Name: "codex_sessions_dir", Status: statusOK,
			Detail: fmt.Sprintf("%s (%s)", dir, suffix),
		},
		codexMiningCheck(cfg),
	}
}

// codexMiningCheck surfaces C3 session-mining status. Always statusOK:
// disabled is a deliberate opt-out, not a misconfiguration. Only
// reached when codex_sessions_dir exists, so "enabled" here means the
// pass will actually run.
func codexMiningCheck(cfg *ScribeConfig) check {
	if !cfg.Codex.Mine {
		return check{
			Section: "config", Name: "codex mining", Status: statusOK,
			Detail: "disabled (opt-in: set `codex: { mine: true }` in scribe.yaml)",
		}
	}
	return check{
		Section: "config", Name: "codex mining", Status: statusOK,
		Detail: fmt.Sprintf("enabled — lookback %dh, max %d/run, min_score %d",
			cfg.Codex.LookbackHours, cfg.Codex.SessionsMax, cfg.Codex.MinScore),
	}
}

// ---- Local-mode coherence ----

// checkLocalMode validates the absorb pipeline's local-provider knobs
// against the runtime environment. Misconfigured KBs surface here
// before a 20-min sync wastes wallclock: ollama daemon offline, the
// chosen model never pulled, atomic_facts left off when the pass-2
// provider is ollama (which fabricates fact-IDs without grounding),
// or no anthropic budget ceiling configured after the 2026-05-11
// runaway. One /api/tags probe is shared across the checks that need
// the model list. SCRIBE_DOCTOR_SKIP_OLLAMA=1 skips the network call
// for offline CI.
func checkLocalMode(cfg *ScribeConfig) []check {
	var out []check
	// Resolve the EFFECTIVE pass-2 provider through the LLMConfig
	// inheritance chain (Phase 4A.5). Raw Absorb.Pass2Provider misses
	// the common 100%-Ollama case where the user flips
	// `llm.provider: ollama` and leaves absorb.pass2_provider empty.
	effPass2Provider, effPass2Model, effOllamaURL := inheritProviderFromLLM(
		"absorb.pass2",
		cfg.Absorb.Pass2Provider, cfg.Absorb.Pass2Model, cfg.Absorb.Contextualize.OllamaURL,
		cfg.LLM,
	)
	pass2Ollama := strings.EqualFold(effPass2Provider, "ollama")
	if !pass2Ollama {
		// No local mode to validate. Only the metered-ceiling INFO
		// applies in that branch — emit it and return.
		if effectiveOutputTokenCeiling(cfg.Sync) == 0 {
			out = append(out, check{
				Section: "localmode", Name: "output_token_ceiling",
				Status: statusWarn,
				Detail: "no daily_output_token_ceiling configured",
				Fix:    "add sync.daily_output_token_ceiling: 2000000 to scribe.yaml (or larger). After the 2026-05-11 runaway this is the recommended backstop; it gates anthropic and hosted providers alike.",
			})
		}
		return out
	}

	url := effOllamaURL
	if url == "" {
		url = defaultOllamaURL
	}
	model := effPass2Model

	if os.Getenv("SCRIBE_DOCTOR_SKIP_OLLAMA") != "1" {
		probe := &ollamaProvider{baseURL: url}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		models, err := probe.listedModels(ctx)
		switch {
		case err != nil:
			out = append(out, check{
				Section: "localmode", Name: "ollama_daemon",
				Status: statusWarn,
				Detail: "unreachable at " + url + ": " + err.Error(),
				Fix:    "brew services start ollama",
			})
		default:
			out = append(out, check{
				Section: "localmode", Name: "ollama_daemon",
				Status: statusOK, Detail: url,
			})
			if model != "" && !probe.modelListContains(models, model) {
				out = append(out, check{
					Section: "localmode", Name: "pass2_model_pulled",
					Status: statusWarn,
					Detail: "absorb.pass2_model=" + model + " not present locally",
					Fix:    "ollama pull " + model,
				})
			} else if model != "" {
				out = append(out, check{
					Section: "localmode", Name: "pass2_model_pulled",
					Status: statusOK, Detail: model,
				})
			}
		}
	}

	// Phase 5: also probe the top-level LLM model. Users running the
	// 100%-Ollama config typically set `llm.model: qwen2.5-coder:14b`
	// and leave per-op model fields empty — the per-op probe above
	// would miss that case.
	if strings.EqualFold(cfg.LLM.Provider, "ollama") && cfg.LLM.Model != "" && cfg.LLM.Model != model && os.Getenv("SCRIBE_DOCTOR_SKIP_OLLAMA") != "1" {
		probe := &ollamaProvider{baseURL: url}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		models, err := probe.listedModels(ctx)
		switch {
		case err != nil:
			// Already reported above.
		case !probe.modelListContains(models, cfg.LLM.Model):
			out = append(out, check{
				Section: "localmode", Name: "llm_model_pulled",
				Status: statusWarn,
				Detail: "llm.model=" + cfg.LLM.Model + " not present locally",
				Fix:    "ollama pull " + cfg.LLM.Model,
			})
		default:
			out = append(out, check{
				Section: "localmode", Name: "llm_model_pulled",
				Status: statusOK, Detail: cfg.LLM.Model,
			})
		}
	}

	if cfg.Absorb.AtomicFacts == nil || !*cfg.Absorb.AtomicFacts {
		// Phrase the warning by *effective* routing so users on the
		// top-level `llm.provider: ollama` switch (with empty
		// absorb.pass2_provider) see the right hint.
		detail := "absorb pass-2 routed to ollama but atomic_facts is off — model will fabricate [cN-fM] citations without ground-truth fact IDs to cite"
		if strings.EqualFold(cfg.Absorb.Pass2Provider, "ollama") {
			detail = "absorb.pass2_provider=ollama but atomic_facts is off — model will fabricate [cN-fM] citations without ground-truth fact IDs to cite"
		} else if strings.EqualFold(cfg.LLM.Provider, "ollama") {
			detail = "llm.provider=ollama (inherited by pass-2) but atomic_facts is off — model will fabricate [cN-fM] citations without ground-truth fact IDs to cite"
		}
		out = append(out, check{
			Section: "localmode", Name: "atomic_facts_with_ollama",
			Status: statusWarn,
			Detail: detail,
			Fix:    "set absorb.atomic_facts: true in scribe.yaml so pass-2 has real fact IDs to ground its citations",
		})
	}

	if effectiveOutputTokenCeiling(cfg.Sync) == 0 {
		out = append(out, check{
			Section: "localmode", Name: "output_token_ceiling",
			Status: statusWarn,
			Detail: "no daily_output_token_ceiling configured",
			Fix:    "add sync.daily_output_token_ceiling: 2000000 to scribe.yaml. After the 2026-05-11 runaway this is the recommended backstop; it gates anthropic and hosted providers alike.",
		})
	}

	return out
}

// ---- Convert (file ingestion coverage) ----

// checkConvert reports per-format conversion coverage. Each row tells
// the user whether the format has a working path on this system, which
// tier handles it, and what to install if the answer is "no path".
//
// The reasoning the matrix encodes:
//
//	HTML/MD/TXT             always green (in-process)
//	PDF                     green if marker present (best quality);
//	                        yellow if tier 0 only (text-only, no OCR)
//	DOCX/EPUB               green if marker present;
//	                        yellow if tier 0 only (Phase 1B Go-native, decent on
//	                        common docs, weak on heavy styling)
//	PPTX/XLSX               green if marker present;
//	                        FAIL if marker absent (no tier 0 path)
//
// The marker probe also surfaces the binary version line so users can
// tell at a glance whether they're on the version scribe was tested
// against (relevant when Phase 5 starts pinning a known-good range).
func checkConvert() []check {
	var out []check

	// Marker presence + version. Frames every other row's verdict.
	if markerTierAvailable() {
		// The second element surfaces the device pin + retry policy so
		// users can tell at a glance whether the unattended drain has
		// the MPS-crash safety net in play. Only matters when marker is
		// installed — no point reporting a knob that's never read.
		out = append(out, check{
			Section: "convert", Name: "marker (tier 1)", Status: statusOK,
			Detail: markerVersionLine(),
		}, markerDeviceCheck())
	} else {
		out = append(out, check{
			Section: "convert", Name: "marker (tier 1)", Status: statusWarn,
			Detail: "not installed (tier 0 fallback active where supported)",
			Fix:    "pipx install marker-pdf  # for top-quality PDF/DOCX/PPTX/XLSX/EPUB",
		})
	}

	// Per-format coverage rows. routedTo describes what actually
	// happens — not what the user might assume:
	//   - "in-process" — never touches an external converter (.md, .txt)
	//   - "tier 0 (html-to-markdown)" — always tier 0 (HTML special-cased)
	//   - "marker | tier 0" — prefers marker, falls back to tier 0 PDF
	//   - "marker only" — no tier 0 path at all
	hasMarker := markerTierAvailable()
	type formatRow struct {
		ext       string
		needName  string
		routedTo  string
		needsTool bool // true if format is unusable without marker
	}
	rows := []formatRow{
		{".md", "markdown", "in-process passthrough", false},
		{".txt", "plain text", "in-process passthrough", false},
		{".html/.htm", "HTML", "tier 0 (html-to-markdown)", false},
		{".pdf", "PDF", routePDF(hasMarker), false},
		{".docx", "DOCX", routeDocOrEpub(hasMarker), false},
		{".epub", "EPUB", routeDocOrEpub(hasMarker), false},
		{".pptx", "PowerPoint", "marker only", true},
		{".xlsx", "Excel", "marker only", true},
	}
	for _, r := range rows {
		switch {
		case strings.HasPrefix(r.routedTo, "in-process") || strings.HasPrefix(r.routedTo, "tier 0"):
			out = append(out, check{
				Section: "convert", Name: r.ext, Status: statusOK,
				Detail: r.needName + " — " + r.routedTo,
			})
		case r.routedTo == "marker | tier 0":
			out = append(out, check{
				Section: "convert", Name: r.ext, Status: statusOK,
				Detail: r.needName + " — marker (best quality)",
			})
		case r.routedTo == "marker only" && hasMarker:
			out = append(out, check{
				Section: "convert", Name: r.ext, Status: statusOK,
				Detail: r.needName + " — marker (best quality)",
			})
		default:
			out = append(out, check{
				Section: "convert", Name: r.ext, Status: statusFail,
				Detail: r.needName + " — no path on this system",
				Fix:    "pipx install marker-pdf",
			})
		}
	}
	return out
}

// markerDeviceCheck reports the resolved marker device pin and whether
// the auto-retry-on-MPS-crash safety net is active. Reads scribe.yaml
// via loadConfig — defaults to "auto" so the row stays informative
// even when the user hasn't customized scribe.yaml.
func markerDeviceCheck() check {
	device := "auto"
	if root, err := kbDir(); err == nil {
		if cfg := loadConfig(root); cfg != nil && cfg.Ingest.Marker.Device != "" {
			device = cfg.Ingest.Marker.Device
		}
	}
	switch device {
	case "auto":
		return check{
			Section: "convert", Name: "marker device", Status: statusOK,
			Detail: "auto (CPU retry on MPS crash — macOS only)",
		}
	case "cpu":
		return check{
			Section: "convert", Name: "marker device", Status: statusOK,
			Detail: "cpu (forced — slower, no GPU crashes)",
		}
	case "mps":
		return check{
			Section: "convert", Name: "marker device", Status: statusWarn,
			Detail: "mps (forced — surya may crash on some PDFs; no retry)",
			Fix:    "set ingest.marker.device: auto in scribe.yaml for safety net",
		}
	case "cuda":
		return check{
			Section: "convert", Name: "marker device", Status: statusOK,
			Detail: "cuda (forced)",
		}
	default:
		return check{
			Section: "convert", Name: "marker device", Status: statusWarn,
			Detail: "unknown device: " + device,
			Fix:    "valid: auto | cpu | mps | cuda",
		}
	}
}

// routePDF picks the doctor row description for PDF based on whether
// marker is installed. Mirrors the runtime choice in convert.go: marker
// preferred, tier 0 ledongthuc/pdf as fallback.
func routePDF(hasMarker bool) string {
	if hasMarker {
		return "marker | tier 0"
	}
	return "tier 0 (text-only; install marker for tables/OCR)"
}

// routeDocOrEpub picks the doctor row description for DOCX/EPUB.
// Tier 0 has Go-native parsers (Phase 1B) but is weaker than marker
// on heavy styling; marker is preferred when present.
func routeDocOrEpub(hasMarker bool) string {
	if hasMarker {
		return "marker | tier 0"
	}
	return "tier 0 (Go-native; install marker for richer styling)"
}

// ---- Cron ----

func checkCron(root string) []check {
	var out []check
	binary := resolveScribeBinary()
	jobs := scribeJobs(binary)
	domain := guiDomain()
	own := make(map[string]bool, len(jobs))
	installed := 0
	for _, job := range jobs {
		label := plistLabel(job.Name)
		path := plistPath(job.Name)
		own[path] = true
		state := probeLaunchAgent(domain, label, path)
		switch state {
		case "loaded":
			installed++
			out = append(out, check{Section: "cron", Name: label, Status: statusOK, Detail: "loaded"})
		case "present":
			installed++
			out = append(out, check{
				Section: "cron", Name: label, Status: statusFail,
				Detail: "plist on disk but not loaded into " + domain,
				Fix:    "scribe cron install",
			})
		default: // "missing"
			out = append(out, check{
				Section: "cron", Name: label, Status: statusFail,
				Detail: "plist missing (" + path + ")",
				Fix:    "scribe cron install",
			})
		}
	}
	// KB-scope headline (issue #27 item 1): the agents above are a single
	// KB-agnostic set (issue #26) shared by every registered KB, so a bare
	// "loaded" is false confidence on a KB that isn't actually served.
	// Resolve whether cron serves THIS KB and lead the section with it —
	// only when agents exist, since "missing" rows already tell that story.
	if installed > 0 {
		out = append([]check{cronScopeCheck(root)}, out...)
	}
	agentsDir := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents")
	if foreign := foreignScribeAgents(agentsDir, binary, root, own); len(foreign) > 0 {
		out = append(out, check{
			Section: "cron", Name: "foreign-agents", Status: statusWarn,
			Detail: fmt.Sprintf("%d LaunchAgent(s) outside scribe's job set also reference this binary or KB: %s — duplicated jobs run twice per slot", len(foreign), strings.Join(foreign, ", ")),
			Fix:    "review each; if it is a stale duplicate: launchctl bootout gui/$(id -u)/<label> and move the plist out of ~/Library/LaunchAgents",
		})
	}
	return out
}

// cronScopeCheck answers the question the per-agent rows can't post-#26
// (issue #27 item 1): the cron agents are a single KB-agnostic set that
// serves every REGISTERED KB via `scribe each`, so "loaded" alone is
// false confidence on a KB that isn't enrolled. This resolves whether
// cron actually serves THIS KB.
func cronScopeCheck(root string) check {
	// Legacy pre-#26 install: a plist still embeds `cd "<other>"` and
	// serves that KB alone, so from here the loaded agents are a mirage.
	if other := otherKBServedByAgents(root); other != "" {
		return check{
			Section: "cron", Name: "kb-scope", Status: statusWarn,
			Detail: "loaded agents are pre-registry and serve " + other + " only — they do NOT serve this KB",
			Fix:    "scribe cron install   # migrate to the KB-agnostic (scribe each) scheduler",
		}
	}
	if kbRegistered(loadUserConfig(), root) {
		return check{
			Section: "cron", Name: "kb-scope", Status: statusOK,
			Detail: fmt.Sprintf("this KB is registered — cron serves it (%d KB(s) on this machine)", len(registeredKBs())),
		}
	}
	return check{
		Section: "cron", Name: "kb-scope", Status: statusWarn,
		Detail: "agents are loaded but this KB is not in the registry — `scribe each` skips it, so cron does NOT serve this KB",
		Fix:    "scribe cron install   # enrolls this KB in the registry and (re)installs the shared agents",
	}
}

// foreignScribeAgents lists LaunchAgent plists that drive this scribe
// install — they reference the scribe binary or the KB root — under a
// label outside scribe's own job set. This is the double-run incident
// class of 2026-06: a pre-rename agent set stayed loaded next to the
// current labels and every cron job fired twice for weeks, visible only
// as doubled run records and an occasional commit HEAD-lock race. The
// process locks make duplicates mostly harmless, which is exactly why
// they go unnoticed without a doctor check.
func foreignScribeAgents(agentsDir, binary, root string, own map[string]bool) []string {
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil // no LaunchAgents dir (non-macOS or fresh account)
	}
	var foreign []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".plist") {
			continue
		}
		path := filepath.Join(agentsDir, e.Name())
		if own[path] {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := string(data)
		if (binary != "" && strings.Contains(text, binary)) ||
			(root != "" && strings.Contains(text, root)) {
			foreign = append(foreign, strings.TrimSuffix(e.Name(), ".plist"))
		}
	}
	sort.Strings(foreign)
	return foreign
}

// ---- State files ----

func checkState(root string, cfg *ScribeConfig) []check {
	var out []check

	if m, err := loadManifest(root); err == nil {
		out = append(out, check{
			Section: "state", Name: "scripts/projects.json", Status: statusOK,
			Detail: fmt.Sprintf("%d projects", len(m.Projects)),
		})
		// Pending projects are invisible on cron runs (the sync hint only
		// lands in a log nobody tails) — doctor is where they must surface.
		if pending := m.pendingProjects(); len(pending) > 0 {
			names := pending
			if len(names) > 5 {
				names = append(append([]string{}, names[:5]...), "…")
			}
			out = append(out, check{
				Section: "state", Name: "pending-projects", Status: statusWarn,
				Detail: fmt.Sprintf("%d project(s) discovered but awaiting approval: %s", len(pending), strings.Join(names, ", ")),
				Fix:    "run `scribe projects review` to approve/ignore (or set sync.auto_approve: true in scribe.yaml)",
			})
		}
		// A scribe KB listed as one of its own source projects is the
		// signature of the self-extraction bug: the KB re-ingests its own
		// wiki and accumulates duplicate pages. Extraction now skips these,
		// but the duplicates already on disk need a manual sweep — surface
		// the offending entries so the user knows where to look.
		var kbProjects []string
		for pname, entry := range m.Projects {
			if entry != nil && withinScribeKB(entry.Path) {
				kbProjects = append(kbProjects, pname)
			}
		}
		if len(kbProjects) > 0 {
			sort.Strings(kbProjects)
			out = append(out, check{
				Section: "state", Name: "kb-as-project", Status: statusWarn,
				Detail: "manifest lists scribe KB(s) as source projects: " + strings.Join(kbProjects, ", ") +
					" (now skipped, but may have created duplicate wiki pages)",
				Fix: "remove the entry from scripts/projects.json and review the wiki for duplicates (e.g. *.md.md)",
			})
		}
		// Manifest entries that are themselves linked worktrees predate
		// worktree folding: they duplicate the main repo's extraction
		// (one project per ticket branch). New discoveries fold
		// automatically; existing entries need a manual ignore.
		var worktreeProjects []string
		for pname, entry := range m.Projects {
			if entry == nil || !dirExists(entry.Path) {
				continue
			}
			if main := worktreeMainRoot(entry.Path); main != "" {
				worktreeProjects = append(worktreeProjects, fmt.Sprintf("%s (worktree of %s)", pname, main))
			}
		}
		if len(worktreeProjects) > 0 {
			sort.Strings(worktreeProjects)
			out = append(out, check{
				Section: "state", Name: "worktree-projects", Status: statusWarn,
				Detail: "manifest entry is a linked git worktree, duplicating the main repo: " + strings.Join(worktreeProjects, ", "),
				Fix:    "run `scribe projects ignore <name>` — discovery folds worktrees into the main repo's entry and still collects their drop/research files",
			})
		}
	} else {
		out = append(out, check{
			Section: "state", Name: "scripts/projects.json", Status: statusFail,
			Detail: err.Error(),
			Fix:    "restore from git or rerun `scribe sync` to rebuild",
		})
	}

	// Unsubstituted template placeholders that leaked into KB paths
	// (e.g. projects/{{DOMAIN}}/…): a prompt var the extractor never filled,
	// echoed by the model into a title→path. The prompt seam now strips these,
	// but committed artifacts need a sweep. `lint --fix` removes them.
	if arts := findPlaceholderArtifacts(root); len(arts) > 0 {
		out = append(out, check{
			Section: "state", Name: "placeholder-artifacts", Status: statusWarn,
			Detail: "unsubstituted template placeholder(s) leaked into KB paths: " + strings.Join(arts, ", "),
			Fix:    "run `scribe lint --fix` to remove (git-recoverable)",
		})
	}

	// Team-mode secret scan over the KB on disk: catches both files the
	// commit gate is currently holding back AND leaks that landed in
	// the repo before the gate existed (or via another member's
	// disabled gate). Lines are rule label + file:line — never the
	// matched text.
	if cfg := loadConfig(root); cfg != nil && cfg.Team && !cfg.SecretScan.Disable {
		if findings := findSecretsInKB(root, cfg.SecretScan.Generic); len(findings) > 0 {
			shown := findings
			if len(shown) > 5 {
				shown = append(append([]string{}, shown[:5]...), "…")
			}
			out = append(out, check{
				Section: "state", Name: "secrets-in-articles", Status: statusWarn,
				Detail: fmt.Sprintf("%d credential-shaped value(s) in KB articles: %s", len(findings), strings.Join(shown, ", ")),
				Fix:    "rewrite the line (rotate the credential if real), or add 'scribe:allow' on the line for placeholders",
			})
		}
	}

	// Stop-words hold scan: unlike the secret gate above, the stop-words
	// gate applies to solo KBs too (stopwords.go), so this check is
	// unconditional.
	if findings := findHeldStopWordsInKB(root, cfg); len(findings) > 0 {
		shown := findings
		if len(shown) > 5 {
			shown = append(append([]string{}, shown[:5]...), "…")
		}
		out = append(out, check{
			Section: "state", Name: "stopword-held-articles", Status: statusWarn,
			Detail: fmt.Sprintf("%d article(s) still held back by the stop-words gate: %s", len(findings), strings.Join(shown, ", ")),
			Fix:    "remove the held word, add 'scribe:allow' on the line, or delete the file if it shouldn't exist",
		})
	}

	// Unresolved merge-conflict markers in articles — the hazard of team
	// KBs with pull-before-sync: a botched merge lands "<<<<<<< HEAD"
	// blocks that poison search and LLM context until resolved.
	if hits := findConflictMarkers(root); len(hits) > 0 {
		names := make([]string, 0, len(hits))
		for _, h := range hits {
			names = append(names, fmt.Sprintf("%s:%d", h.Rel, h.Line))
		}
		if len(names) > 5 {
			names = append(names[:5], "…")
		}
		out = append(out, check{
			Section: "state", Name: "conflict-markers", Status: statusWarn,
			Detail: fmt.Sprintf("%d file(s) contain unresolved git conflict markers: %s", len(hits), strings.Join(names, ", ")),
			Fix:    "resolve the merge by hand (search the file for '<<<<<<<'), then commit",
		})
	}

	// scribe.yaml scaffolded before an option existed never mentions it —
	// no migration needed (absent keys default safely), but the user
	// can't opt into what they can't see. OK-level: informational.
	if data, err := os.ReadFile(filepath.Join(root, "scribe.yaml")); err == nil {
		if missing := missingTemplateBlocks(string(data)); len(missing) > 0 {
			keys := make([]string, 0, len(missing))
			for _, seg := range missing {
				keys = append(keys, seg.key)
			}
			out = append(out, check{
				Section: "state", Name: "config-options", Status: statusOK,
				Detail: fmt.Sprintf("scribe.yaml predates %d option(s): %s", len(missing), strings.Join(keys, ", ")),
				Fix:    "run `scribe config update` to append commented docs (defaults unchanged)",
			})
		}
	}

	statePath := filepath.Join(root, "scripts", "imessage-state.json")
	if _, err := loadCaptureState(statePath); err == nil {
		out = append(out, check{Section: "state", Name: "scripts/imessage-state.json", Status: statusOK, Detail: "parsed"})
	} else {
		out = append(out, check{
			Section: "state", Name: "scripts/imessage-state.json", Status: statusFail,
			Detail: err.Error(),
			Fix:    "delete and rerun `scribe capture` to regenerate",
		})
	}

	// Generic JSON parse-check for the wiki-side state files.
	jsonFiles := []struct {
		rel string
		fix string
	}{
		{"wiki/_sessions_log.json", "run `scribe sync --sessions` to rebuild"},
		{"wiki/_backlinks.json", "run `scribe backlinks` to rebuild"},
	}
	for _, jf := range jsonFiles {
		out = append(out, checkJSONFile(root, jf.rel, jf.fix))
	}

	// Markdown files: exist + non-empty is enough.
	mdFiles := []struct {
		rel string
		fix string
	}{
		{"wiki/_index.md", "run `scribe index` to rebuild"},
		{"log.md", "append-only; restore from git"},
	}
	for _, mf := range mdFiles {
		abs := filepath.Join(root, mf.rel)
		info, err := os.Stat(abs)
		switch {
		case err != nil:
			out = append(out, check{Section: "state", Name: mf.rel, Status: statusFail, Detail: "missing", Fix: mf.fix})
		case info.Size() == 0:
			out = append(out, check{Section: "state", Name: mf.rel, Status: statusWarn, Detail: "empty file", Fix: mf.fix})
		default:
			out = append(out, check{Section: "state", Name: mf.rel, Status: statusOK, Detail: humanSize(info.Size())})
		}
	}

	return out
}

// checkJSONFile reads a file and attempts to json.Unmarshal it. Missing or
// corrupt files become FAILs with the caller-provided fix hint.
func checkJSONFile(root, rel, fix string) check {
	abs := filepath.Join(root, rel)
	data, err := os.ReadFile(abs)
	if err != nil {
		return check{Section: "state", Name: rel, Status: statusFail, Detail: "cannot read: " + err.Error(), Fix: fix}
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return check{Section: "state", Name: rel, Status: statusFail, Detail: "invalid JSON: " + err.Error(), Fix: fix}
	}
	return check{Section: "state", Name: rel, Status: statusOK, Detail: "parsed"}
}

// ---- Freshness ----

// freshnessSpec maps a monitored command to its max allowable gap since the
// last successful run. A nil/zero LastOk always yields a WARN — we prefer a
// loud "never ran" to silent drift.
type freshnessSpec struct {
	Command string // command path as written by writeRunRecord (e.g. "sync", "ingest drain")
	ArgFlag string // optional args-substring required (e.g. "--sessions") to distinguish modes of the same command
	Label   string
	MaxGap  time.Duration
	Fix     string
}

var freshnessSpecs = []freshnessSpec{
	{Command: "sync", Label: "sync (projects)", MaxGap: 6 * time.Hour, Fix: "scribe sync"},
	{Command: "sync", ArgFlag: "--sessions", Label: "sync (sessions)", MaxGap: 36 * time.Hour, Fix: "scribe sync --sessions"},
	{Command: "lint", Label: "lint", MaxGap: 48 * time.Hour, Fix: "scribe lint"},
	{Command: "dream", Label: "dream", MaxGap: 10 * 24 * time.Hour, Fix: "scribe dream"},
	{Command: "dream", ArgFlag: "--hot", Label: "dream (hot)", MaxGap: 36 * time.Hour, Fix: "scribe dream --hot"},
	{Command: "capture", Label: "capture", MaxGap: 12 * time.Hour, Fix: "scribe capture --fetch"},
	{Command: "commit", Label: "commit", MaxGap: 6 * time.Hour, Fix: "scribe commit"},
	{Command: "ingest drain", Label: "ingest drain", MaxGap: 3 * time.Hour, Fix: "scribe ingest drain"},
}

// runRecord mirrors the JSONL schema writeRunRecord emits in main.go.
type runRecord struct {
	Command   string   `json:"command"`
	Status    string   `json:"status"`
	Timestamp string   `json:"timestamp"`
	Args      []string `json:"args"`
}

// loadRunRecords scans output/runs/*.jsonl and returns, for each command key,
// the newest "ok" timestamp. Keys are either the bare command path ("sync")
// or "<command> <flag>" ("sync --sessions") so the freshness specs can
// distinguish the two modes that share the same command path.
//
// A missing runs directory is not an error — doctor must work on fresh
// checkouts before any scribe commands have been logged.
func loadRunRecords(root string) (map[string]time.Time, error) {
	runsDir := filepath.Join(root, "output", "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]time.Time{}, nil
		}
		return nil, err
	}
	result := map[string]time.Time{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(runsDir, e.Name()))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			var r runRecord
			if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
				continue
			}
			if r.Status != "ok" || r.Command == "" {
				continue
			}
			ts, err := time.Parse(time.RFC3339, r.Timestamp)
			if err != nil {
				continue
			}
			if prev, ok := result[r.Command]; !ok || ts.After(prev) {
				result[r.Command] = ts
			}
			// Flag-specific keys so modes like `sync --sessions` track separately.
			for _, arg := range r.Args {
				if strings.HasPrefix(arg, "--") {
					key := r.Command + " " + arg
					if prev, ok := result[key]; !ok || ts.After(prev) {
						result[key] = ts
					}
				}
			}
		}
		if err := scanner.Err(); err != nil {
			// Partial data only skews this one file's freshness reading;
			// keep auditing the rest, but say so on stderr.
			logMsg("doctor", "read %s truncated: %v", e.Name(), err)
		}
		_ = f.Close()
	}
	return result, nil
}

// classifyFreshness compares a last-ok timestamp to a threshold and returns
// the status word plus a display detail. Extracted as a pure function so the
// thresholds are unit-testable without setting up a fake filesystem.
func classifyFreshness(lastOk time.Time, now time.Time, gap time.Duration) (checkStatus, string) {
	if lastOk.IsZero() {
		return statusWarn, "never run (no record in output/runs/)"
	}
	age := now.Sub(lastOk)
	if age > gap {
		return statusWarn, fmt.Sprintf("last ok %s ago — expected ≤ %s", shortDuration(age), shortDuration(gap))
	}
	return statusOK, "last ok " + shortDuration(age) + " ago"
}

// runError captures the latest error timestamp + message for one command key.
// loadRunErrors populates these so checkRecentErrors can surface the most
// recent failure per command within the configured window.
type runError struct {
	When time.Time
	Msg  string
	Args []string
}

// loadRunErrors scans output/runs/*.jsonl and returns the newest `status:"error"`
// record per command key within `since`. A command is keyed by its base command
// name (e.g. "sync", "capture") so a cron running the same command every hour
// folds into one error line, not dozens.
func loadRunErrors(root string, since time.Time) (map[string]runError, error) {
	runsDir := filepath.Join(root, "output", "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]runError{}, nil
		}
		return nil, err
	}
	result := map[string]runError{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(runsDir, e.Name()))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			var r struct {
				Command   string   `json:"command"`
				Status    string   `json:"status"`
				Timestamp string   `json:"timestamp"`
				Error     string   `json:"error"`
				Args      []string `json:"args"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
				continue
			}
			if r.Status != "error" || r.Command == "" {
				continue
			}
			ts, err := time.Parse(time.RFC3339, r.Timestamp)
			if err != nil || ts.Before(since) {
				continue
			}
			if prev, ok := result[r.Command]; !ok || ts.After(prev.When) {
				result[r.Command] = runError{When: ts, Msg: r.Error, Args: r.Args}
			}
		}
		if err := scanner.Err(); err != nil {
			logMsg("doctor", "read %s truncated: %v", e.Name(), err)
		}
		_ = f.Close()
	}
	return result, nil
}

// checkRecentErrors reports the newest error-per-command inside the window.
// Errors are warnings by default because a single transient failure shouldn't
// fail the doctor exit code — but repeated failures (e.g. every scheduled
// triage run over a whole day) still get surfaced instead of being masked by
// the latest successful run, which was the original doctor blind spot.
func checkRecentErrors(root string, now time.Time, window time.Duration) []check {
	since := now.Add(-window)
	errs, err := loadRunErrors(root, since)
	if err != nil {
		return []check{{
			Section: "errors", Name: "output/runs", Status: statusFail,
			Detail: err.Error(),
			Fix:    "check filesystem permissions on output/runs/",
		}}
	}
	if len(errs) == 0 {
		return []check{{
			Section: "errors", Name: "recent runs", Status: statusOK,
			Detail: "no errors in last " + shortDuration(window),
		}}
	}
	// Stable order: sort command keys.
	keys := make([]string, 0, len(errs))
	for k := range errs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []check
	for _, k := range keys {
		e := errs[k]
		age := shortDuration(now.Sub(e.When))
		detail := fmt.Sprintf("last error %s ago: %s", age, truncateError(e.Msg))
		out = append(out, check{
			Section: "errors", Name: k, Status: statusWarn,
			Detail: detail,
			Fix:    fixHintForError(k, e.Msg),
		})
	}
	return out
}

// fixHintForError turns a known error signature into a runnable command the
// user can paste. Falls back to "read the jsonl" when the pattern doesn't
// match — that's still useful, just less directed.
func fixHintForError(command, msg string) string {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "operation not permitted") && command == "capture":
		return "run: scribe fda  (grants Full Disk Access interactively)"
	case strings.Contains(lower, "no such module: fts5"):
		return "rebuild with sqlite_fts5 tag: make install (or reinstall via brew)"
	case strings.Contains(lower, "self-chat handles") && strings.Contains(lower, "exist in chat.db"):
		return "fix capture.self_chat_handles in scribe.yaml"
	case strings.Contains(lower, "no self-chat handle configured"):
		return "set capture.self_chat_handles in scribe.yaml or SCRIBE_SELF_CHAT_ID"
	case strings.Contains(lower, "rate limit"):
		return "wait out Anthropic rate-limit; scribe sync resumes automatically next run"
	}
	return "inspect output/runs/"
}

// truncateError keeps the error section readable — full messages can run past
// 200 chars and break the aligned text layout in printChecksText.
func truncateError(msg string) string {
	msg = strings.ReplaceAll(msg, "\n", " ")
	const limit = 140
	if len(msg) > limit {
		return msg[:limit] + "…"
	}
	if msg == "" {
		return "(no message)"
	}
	return msg
}

func checkFreshness(root string, now time.Time) []check {
	records, err := loadRunRecords(root)
	if err != nil {
		return []check{{
			Section: "freshness", Name: "output/runs", Status: statusFail,
			Detail: err.Error(),
			Fix:    "check filesystem permissions on output/runs/",
		}}
	}
	var out []check
	for _, spec := range freshnessSpecs {
		key := spec.Command
		if spec.ArgFlag != "" {
			key = spec.Command + " " + spec.ArgFlag
		}
		lastOk := records[key]
		status, detail := classifyFreshness(lastOk, now, spec.MaxGap)
		ck := check{Section: "freshness", Name: spec.Label, Status: status, Detail: detail}
		if status != statusOK {
			ck.Fix = spec.Fix
		}
		out = append(out, ck)
	}
	return out
}

// ---- Formatting helpers ----

func shortDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		days := int(d / (24 * time.Hour))
		h := int((d % (24 * time.Hour)).Hours())
		if h == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, h)
	}
}

func humanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%dB", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(bytes)/float64(div), "KMGT"[exp])
}

func printChecksText(all []check, root string) {
	fmt.Printf("KB root: %s\n\n", root)

	sectionOrder := []string{}
	bySection := map[string][]check{}
	for _, ck := range all {
		if _, ok := bySection[ck.Section]; !ok {
			sectionOrder = append(sectionOrder, ck.Section)
		}
		bySection[ck.Section] = append(bySection[ck.Section], ck)
	}

	titles := map[string]string{
		"deps":      "Dependencies:",
		"config":    "Config:",
		"convert":   "Convert (file ingestion):",
		"cron":      "Cron (LaunchAgents):",
		"state":     "State files:",
		"freshness": "Freshness (from output/runs/):",
		"errors":    "Recent run errors:",
	}

	ok, warn, fail := 0, 0, 0
	for _, sec := range sectionOrder {
		fmt.Println(titles[sec])
		maxName := 0
		for _, ck := range bySection[sec] {
			if len(ck.Name) > maxName {
				maxName = len(ck.Name)
			}
		}
		for _, ck := range bySection[sec] {
			marker := "[ok]  "
			switch ck.Status {
			case statusWarn:
				marker = "[warn]"
				warn++
			case statusFail:
				marker = "[FAIL]"
				fail++
			default:
				ok++
			}
			fmt.Printf("  %s  %-*s  %s\n", marker, maxName, ck.Name, ck.Detail)
			if ck.Fix != "" {
				fmt.Printf("          fix: %s\n", ck.Fix)
			}
		}
		fmt.Println()
	}

	fmt.Printf("Summary: %d ok, %d warn, %d FAIL\n", ok, warn, fail)
}

func printChecksJSON(all []check, root string) {
	ok, warn, fail := 0, 0, 0
	for _, ck := range all {
		switch ck.Status {
		case statusOK:
			ok++
		case statusWarn:
			warn++
		case statusFail:
			fail++
		}
	}
	if all == nil {
		all = []check{}
	}
	payload := map[string]any{
		"kb_root": root,
		"checks":  all,
		"summary": map[string]int{"ok": ok, "warn": warn, "fail": fail},
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		logMsg("doctor", "encode json output: %v", err)
	}
}

// checkVaultScaffolding flags directories from other vault tools that have
// snuck into the KB. Logseq, in particular, will autosave a backup .md per
// edit into logseq/bak/ — left alone, that tree grows to thousands of
// orphan files that bloat the Obsidian graph and pad commit diffs.
//
// Detection is intentionally narrow: we report on dirs we are confident
// the user does not need. The fix is "git rm + .gitignore", but the user
// pulls the trigger.
func checkVaultScaffolding(root string) []check {
	var out []check
	type probe struct {
		rel    string
		label  string
		reason string
	}
	probes := []probe{
		{rel: "logseq", label: "logseq/", reason: "Logseq autosave backups + config (logseq/bak/ grows ~1 file per edit)"},
		{rel: "pages", label: "pages/", reason: "Logseq scaffolding directory (Obsidian uses the type-named dirs instead)"},
	}
	found := false
	for _, p := range probes {
		full := filepath.Join(root, p.rel)
		info, err := os.Stat(full)
		if err != nil || !info.IsDir() {
			continue
		}
		found = true
		fileCount, dirSize := dirStats(full)
		detail := fmt.Sprintf("%s — %d files, %s — %s", p.label, fileCount, humanBytes(dirSize), p.reason)
		out = append(out, check{
			Section: "vault", Name: "stray-scaffolding", Status: statusWarn,
			Detail: detail,
			Fix:    fmt.Sprintf("rm -rf %s && echo '%s' >> .gitignore", p.label, p.label),
		})
	}
	if !found {
		out = append(out, check{
			Section: "vault", Name: "scaffolding", Status: statusOK,
			Detail: "no stray vault directories",
		})
	}
	return out
}

// dirStats walks a directory and returns (file count, total bytes). Best
// effort — silently skips entries it can't stat.
func dirStats(dir string) (int, int64) {
	var files int
	var size int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil //nolint:nilerr // best-effort traversal; skip unreadable entries
		}
		if !info.IsDir() {
			files++
			size += info.Size()
		}
		return nil
	})
	return files, size
}

func humanBytes(n int64) string {
	const (
		_  = iota
		KB = 1 << (10 * iota)
		MB
		GB
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(GB))
	case n >= MB:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(MB))
	case n >= KB:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(KB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
