package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
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
	Root string `help:"Override KB root directory." type:"path" short:"C"`

	Sync          SyncCmd          `cmd:"" help:"Discover, extract, mine sessions, absorb, reindex, commit."`
	Lint          LintCmd          `cmd:"" help:"Run structural KB health checks."`
	Validate      ValidateCmd      `cmd:"" help:"Validate YAML frontmatter in markdown files."`
	Backlinks     BacklinksCmd     `cmd:"" help:"Rebuild _backlinks.json from wikilinks."`
	Index         IndexCmd         `cmd:"" help:"Rebuild _index.md from disk."`
	Orphans       OrphansCmd       `cmd:"" help:"Detect orphan articles and missing wikilinks."`
	Triage        TriageCmd        `cmd:"" help:"Score sessions by knowledge density (FTS5)."`
	Scan          ScanCmd          `cmd:"" help:"Pre-scan a project for extraction."`
	Deep          DeepCmd          `cmd:"" help:"Deep-extract a project batch-by-directory."`
	Capture       CaptureCmd       `cmd:"" help:"Capture iMessage self-chat URLs/notes into raw/articles/."`
	Commit        CommitCmd        `cmd:"" help:"Auto-commit and push pending KB changes."`
	Dream         DreamCmd         `cmd:"" help:"Run structured memory consolidation (4-phase dream cycle)."`
	Link          LinkCmd          `cmd:"" help:"Link orphan articles to contextual hosts via See Also sections."`
	Cron          CronCmd          `cmd:"" help:"Manage macOS LaunchAgents for scheduled KB jobs."`
	Sessions      SessionsCmd      `cmd:"" help:"Debug and repair _sessions_log.json and the session pre-filter."`
	Debug         DebugCmd         `cmd:"" help:"Low-level diagnostics (wikilinks, backlinks, frontmatter)."`
	Ingest        IngestCmd        `cmd:"" help:"Ingest a URL into raw/articles/ (queue or synchronous)."`
	Absorb        AbsorbCmd        `cmd:"" help:"Absorb a local file (md/txt/html) into the KB end-to-end: ingest → contextualize → absorb."`
	Contextualize ContextualizeCmd `cmd:"" help:"Insert LLM-generated retrieval-context paragraph into raw articles for better qmd search."`
	Status        StatusCmd        `cmd:"" help:"One-shot KB scoreboard: raw by density, absorb/contextualize progress, last sync, Ollama health."`
	Init          InitCmd          `cmd:"" help:"Check dependencies, verify config, and prep a KB checkout."`
	Hook          HookCmd          `cmd:"" help:"Claude Code lifecycle hooks (install via ~/.claude/settings.json)."`
	Hot           HotCmd           `cmd:"" help:"Regenerate wiki/_hot.md context cache (deterministic, no LLM)."`
	Write         WriteCmd         `cmd:"" help:"Create an article or append to rolling memory (CLI write surface for skills)."`
	Watch         WatchCmd         `cmd:"" help:"Watch ccrider DB for new sessions (long-running, launchd-friendly)."`
	Assess        AssessCmd        `cmd:"" help:"One-shot parallel deep assessment of a project (5 tracks + consolidation)."`
	Doctor        DoctorCmd        `cmd:"" help:"Health check (deps, config, cron, state, run freshness). Read-only."`
	Fda           FDACmd           `cmd:"" name:"fda" help:"Probe macOS Full Disk Access for chat.db and guide the user through granting it."`
	Version       VersionCmd       `cmd:"" help:"Show version."`
}

type VersionCmd struct{}

func (v *VersionCmd) Run() error {
	println("scribe " + version)
	return nil
}

func main() {
	setupLogger()
	cli := CLI{}
	ctx := kong.Parse(&cli,
		kong.Name("scribe"),
		kong.Description("scribe — personal knowledge-base pipeline"),
		kong.UsageOnError(),
	)

	globalRoot = cli.Root

	started := time.Now()
	err := ctx.Run()
	// Command path like "sync", "ingest url", "cron install". Useful for grouping.
	cmdPath := ctx.Command()
	writeRunRecord(cmdPath, started, err)

	ctx.FatalIfErrorf(err)
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
