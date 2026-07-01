package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/alecthomas/kong"
)

var version = "dev"

// globalRoot is set from CLI.Root before any command runs, so kbDir() can use it.
var globalRoot string

// runStats holds optional per-command telemetry that writeRunRecord merges
// into the JSONL record. Commands populate it before returning.
var runStats map[string]any

type CLI struct {
	Root        string           `help:"Override KB root directory." type:"path" short:"C"`
	VersionFlag kong.VersionFlag `name:"version" short:"V" help:"Show version and exit."`

	// Core — the pipeline and its health.
	Sync   SyncCmd   `cmd:"" group:"core" help:"Discover, extract, mine sessions, absorb, reindex, commit."`
	Status StatusCmd `cmd:"" group:"core" help:"One-shot KB scoreboard: raw by density, absorb/contextualize progress, last sync, Ollama health."`
	Doctor DoctorCmd `cmd:"" group:"core" help:"Health check (deps, config, cron, state, run freshness). Read-only."`
	Commit CommitCmd `cmd:"" group:"core" help:"Auto-commit and push pending KB changes."`
	Watch  WatchCmd  `cmd:"" group:"core" help:"Watch ccrider DB for new sessions (long-running, launchd-friendly)."`

	// Content — getting knowledge into the KB.
	Write         WriteCmd         `cmd:"" group:"content" help:"Create an article or append to rolling memory (CLI write surface for skills)."`
	Drop          DropCmd          `cmd:"" group:"content" help:"Author a validated drop file for the current project's KB handoff directory."`
	Ingest        IngestCmd        `cmd:"" group:"content" help:"Ingest a URL into raw/articles/ (queue or synchronous)."`
	Absorb        AbsorbCmd        `cmd:"" group:"content" help:"Absorb a local file (md/txt/html) into the KB end-to-end: ingest → contextualize → absorb."`
	Capture       CaptureCmd       `cmd:"" group:"content" help:"Capture iMessage self-chat URLs/notes into raw/articles/."`
	Pull          PullCmd          `cmd:"" group:"content" help:"Pull bookmarks from configured integrations (Pinboard, …) into the ingest queue."`
	Contextualize ContextualizeCmd `cmd:"" group:"content" help:"Insert LLM-generated retrieval-context paragraph into raw articles for better qmd search."`
	Projects      ProjectsCmd      `cmd:"" group:"content" help:"List, approve, ignore, or review discovered projects."`
	Scan          ScanCmd          `cmd:"" group:"content" help:"Pre-scan a project for extraction."`
	Deep          DeepCmd          `cmd:"" group:"content" help:"Deep-extract a project batch-by-directory."`
	Assess        AssessCmd        `cmd:"" group:"content" help:"One-shot parallel deep assessment of a project (5 tracks + consolidation)."`

	// Quality — KB hygiene, structure, ranking, consolidation.
	Lint           LintCmd           `cmd:"" group:"quality" help:"Run structural KB health checks."`
	Validate       ValidateCmd       `cmd:"" group:"quality" help:"Validate YAML frontmatter in markdown files."`
	Link           LinkCmd           `cmd:"" group:"quality" help:"Link orphan articles to contextual hosts via See Also sections."`
	Orphans        OrphansCmd        `cmd:"" group:"quality" help:"Detect orphan articles and missing wikilinks."`
	Backlinks      BacklinksCmd      `cmd:"" group:"quality" help:"Rebuild _backlinks.json from wikilinks."`
	Index          IndexCmd          `cmd:"" group:"quality" help:"Rebuild _index.md from disk."`
	Hot            HotCmd            `cmd:"" group:"quality" help:"Regenerate wiki/_hot.md context cache (deterministic, no LLM)."`
	Tier           TierCmd           `cmd:"" group:"quality" help:"Compute, set, or backfill index_tier for ranking (Phase 5B)."`
	Sections       SectionsCmd       `cmd:"" group:"quality" help:"Build/list/get section sidecars for wiki articles (Phase 5A)."`
	Relations      RelationsCmd      `cmd:"" group:"quality" help:"Get/set/check typed relations between articles (Phase 6A)."`
	Stale          StaleCmd          `cmd:"" group:"quality" help:"Build/list/show the staleness ledger (Phase 6C)."`
	Contradictions ContradictionsCmd `cmd:"" group:"quality" help:"Build/list/show/resolve the contradiction ledger (Phase 6B)."`
	Dream          DreamCmd          `cmd:"" group:"quality" help:"Run structured memory consolidation (4-phase dream cycle)."`

	// Team — shared-KB workflows.
	Promote PromoteCmd `cmd:"" group:"team" help:"Copy an article into another scribe KB with provenance (personal → team promotion)."`
	Config  ConfigCmd  `cmd:"" group:"team" help:"Review/approve shared-KB config trust (sensitive scribe.yaml keys)."`
	Digest  DigestCmd  `cmd:"" group:"team" help:"Regenerate wiki/_digest.md — deterministic team dashboard (activity, findings, owners)."`

	// System — installation and host integration.
	Init         InitCmd         `cmd:"" group:"system" help:"Check dependencies, verify config, and prep a KB checkout."`
	Cron         CronCmd         `cmd:"" group:"system" help:"Manage macOS LaunchAgents for scheduled KB jobs."`
	Each         EachCmd         `cmd:"" group:"system" help:"Run a scribe subcommand in every registered KB (KB-agnostic scheduler)."`
	Kb           KbCmd           `cmd:"" group:"system" help:"Manage the machine's KB registry (kbs: in user config)."`
	Hook         HookCmd         `cmd:"" group:"system" help:"Claude Code lifecycle hooks (install via ~/.claude/settings.json)."`
	Fda          FDACmd          `cmd:"" name:"fda" group:"system" help:"Probe macOS Full Disk Access for chat.db and guide the user through granting it."`
	Skill        SkillCmd        `cmd:"" group:"system" help:"Install or list the embedded scribe-kb agent skill bundle (Phase 7A)."`
	InstallTools InstallToolsCmd `cmd:"" name:"install-tools" group:"system" help:"Bootstrap optional tools (uv + marker-pdf) for full PDF/DOCX/PPTX/XLSX/EPUB ingestion."`
	Version      VersionCmd      `cmd:"" group:"system" help:"Show version."`

	// Debug — diagnostics and introspection.
	Debug    DebugCmd    `cmd:"" group:"debug" help:"Low-level diagnostics (wikilinks, backlinks, frontmatter)."`
	Sessions SessionsCmd `cmd:"" group:"debug" help:"Debug and repair _sessions_log.json and the session pre-filter."`
	Triage   TriageCmd   `cmd:"" group:"debug" help:"Score sessions by knowledge density (FTS5)."`
	Cost     CostCmd     `cmd:"" group:"debug" help:"Summarize claude -p calls (count, wallclock, USD estimate) from the cost ledger."`
	View     ViewCmd     `cmd:"" group:"debug" help:"Run declarative views over wiki frontmatter (Phase 7B)."`
}

// commandGroups maps the `group` struct tags on CLI fields to the section
// titles rendered in --help. Every command field on CLI must carry one of
// these keys — TestRootCommandsAreGrouped enforces that, so a new command
// can't land ungrouped. Subcommands inherit their parent's group, so only
// root fields need tagging.
var commandGroups = kong.Groups{
	"core":    "Core commands:",
	"content": "Content commands:",
	"quality": "Quality commands:",
	"team":    "Team commands:",
	"system":  "System commands:",
	"debug":   "Debug commands:",
}

type VersionCmd struct{}

func (v *VersionCmd) Run() error {
	println("scribe " + version)
	return nil
}

// kongOptions returns the Kong configuration shared by main() and the
// help-rendering tests, so what the tests assert is what users see.
func kongOptions() []kong.Option {
	return []kong.Option{
		kong.Name("scribe"),
		kong.Description("scribe — personal knowledge-base pipeline"),
		kong.UsageOnError(),
		kong.Vars{"version": "scribe " + version},
		commandGroups,
	}
}

func main() {
	setupLogger()
	cli := CLI{}
	ctx := kong.Parse(&cli, kongOptions()...)

	globalRoot = cli.Root

	started := time.Now()
	err := ctx.Run()
	// Command path like "sync", "ingest url", "cron install". Useful for grouping.
	cmdPath := ctx.Command()
	// Read-only invocations must not append a run record — that file
	// auto-commits to the KB repo, so a `scribe doctor`/`status` or any
	// `--dry-run` would make diagnostics self-modifying (Codex finding,
	// 2026-05-15).
	if !commandIsReadOnly(ctx) {
		writeRunRecord(cmdPath, started, err)
	}

	ctx.FatalIfErrorf(err)
}

// readOnlyCmd is implemented by commands that never write KB state.
// Used for always-read-only diagnostics (doctor, status) that have no
// --dry-run flag to key off. --dry-run invocations of mutating
// commands are detected generically in commandIsReadOnly, so the bulk
// of commands need no boilerplate.
type readOnlyCmd interface{ ReadOnly() bool }

// addrOf returns an addressable pointer form of v so a pointer-receiver
// method (ReadOnly()) can be invoked. kong-bound fields are usually
// already addressable; if not, a one-shot copy is made (ReadOnly()
// returns a constant for the read-only commands, so the copy is safe).
func addrOf(v reflect.Value) reflect.Value {
	if v.Kind() == reflect.Pointer {
		return v
	}
	if v.CanAddr() {
		return v.Addr()
	}
	p := reflect.New(v.Type())
	p.Elem().Set(v)
	return p
}

// commandIsReadOnly reports whether the selected command performs no
// KB-state writes, so main() can skip the run-record append. Two
// signals, checked against the Kong-selected leaf command:
//
//  1. it implements readOnlyCmd and returns true (doctor, status), or
//  2. it carries a `DryRun bool` field that is currently true — the
//     project-wide convention for "show what would happen, write
//     nothing" (~20 commands; handled here so none need a method).
func commandIsReadOnly(ctx *kong.Context) bool {
	sel := ctx.Selected()
	if sel == nil || !sel.Target.IsValid() {
		return false
	}
	// kong's Node.Target is the command struct *value*; ReadOnly() has
	// a pointer receiver, so test the addressable pointer form too
	// (doctor/status rely solely on the interface — they have no
	// DryRun field to fall back on).
	for _, v := range []reflect.Value{addrOf(sel.Target), sel.Target} {
		if v.IsValid() && v.CanInterface() {
			if ro, ok := v.Interface().(readOnlyCmd); ok && ro.ReadOnly() {
				return true
			}
		}
	}
	v := sel.Target
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() == reflect.Struct {
		if f := v.FieldByName("DryRun"); f.IsValid() && f.Kind() == reflect.Bool && f.Bool() {
			return true
		}
	}
	return false
}

// redactArgs masks token-bearing URLs before they're persisted to
// output/runs/*.jsonl (which auto-commits to the KB repo). URL query
// strings commonly carry API tokens (?token=..., ?api_key=..., ?access_token=...);
// strip the query component wholesale when those keys are present. Headers
// like "Authorization: Bearer <token>" are not an arg pattern — those come
// from env vars and never land here.
func redactArgs(args []string) []string {
	if len(args) == 0 {
		return args
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = redactURLToken(a)
	}
	return out
}

var urlTokenRE = regexp.MustCompile(`(?i)([?&])(token|api_key|apikey|access_token|auth|key|sig|signature)=[^&\s]+`)

func redactURLToken(s string) string {
	return urlTokenRE.ReplaceAllString(s, "${1}${2}=REDACTED")
}

// writeRunRecord appends a JSON summary of a CLI invocation to output/runs/
// for observability. Silent best-effort — never blocks or errors the caller.
// Skips writing for the Version command and anything that runs before kbDir
// resolution can succeed.
func writeRunRecord(cmdPath string, started time.Time, runErr error) {
	if cmdPath == "" || strings.HasPrefix(cmdPath, "version") {
		return
	}
	root, err := kbDir()
	if err != nil {
		return
	}
	runsDir := filepath.Join(root, "output", "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return
	}

	status := "ok"
	errMsg := ""
	if runErr != nil {
		status = "error"
		errMsg = runErr.Error()
		if len(errMsg) > 500 {
			errMsg = errMsg[:500]
		}
	}

	record := map[string]any{
		"command":    cmdPath,
		"status":     status,
		"timestamp":  started.UTC().Format(time.RFC3339),
		"duration_s": time.Since(started).Seconds(),
		"args":       redactArgs(os.Args[1:]),
	}
	if errMsg != "" {
		record["error"] = errMsg
	}
	maps.Copy(record, runStats)

	// Filename: runs/YYYY-MM-DD.jsonl (one daily JSONL file, append-only).
	// JSONL keeps the file greppable and lets us reuse tooling like `jq -s`.
	dayFile := filepath.Join(runsDir, started.UTC().Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(dayFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	fmt.Fprintln(f, string(data))
}
