package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// EachConfig is this KB's cadence policy for the KB-agnostic scheduler
// (issue #26). `scribe each` runs every job in every registered KB on
// each launchd tick; Cadence lets a KB opt a job out of a tick when its
// last successful run (read from output/runs/*.jsonl) is younger than the
// configured interval. Empty = run every tick (the original behavior, so
// existing KBs are unaffected). NOT trust-locked: a pushed cadence only
// shapes HOW OFTEN a job runs, never what it ingests or where data goes.
//
// Keys match the run-record keys: the command path plus its first --flag,
// or the bare command — "sync --sessions", "sync --max", "dream",
// "capture --fetch", "lint --fix", "commit". A bare command key ("sync")
// also covers every mode of that command as a fallback. Values are Go
// durations ("2h", "30m", "90s") or a day count ("7d", "1.5d").
type EachConfig struct {
	Cadence map[string]string `yaml:"cadence"`
}

// EachCmd runs a scribe subcommand once per registered KB — the
// KB-agnostic execution model behind the issue #26 scheduler. LaunchAgents
// invoke `scribe each -- <job>` instead of cd-ing into one hardcoded KB, so
// a single machine-level agent set serves every registered KB. Per-KB
// behavior still comes from each KB's own scribe.yaml/scribe.local.yaml
// (e.g. capture no-ops without self-chat handles, team KBs pull first).
//
// Failure isolation: one KB erroring is logged and the loop continues, so a
// single bad KB never blocks the rest of the tick. The command exits 0
// unless it could not run at all (no registry), keeping launchd quiet.
type EachCmd struct {
	Args []string `arg:"" optional:"" passthrough:"" help:"scribe subcommand to run in every registered KB, e.g. 'each -- sync --max 2'."`
}

// eachRunner executes one scribe subcommand against a KB root. Indirected
// so tests can stub it; the default re-execs this binary with -C <root> and
// inherits stdio so child logs stream to the launchd log.
var eachRunner = func(root string, args []string) error {
	exe, _ := os.Executable()
	if exe == "" {
		exe = "scribe"
	}
	full := append([]string{"-C", root}, args...)
	cmd := exec.CommandContext(context.Background(), exe, full...)
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return cmd.Run()
}

// effectiveArgs drops the leading `--` that kong's passthrough keeps from
// `scribe each -- <sub>`, so the child gets a clean subcommand.
func (c *EachCmd) effectiveArgs() []string {
	if len(c.Args) > 0 && c.Args[0] == "--" {
		return c.Args[1:]
	}
	return c.Args
}

func (c *EachCmd) Run() error {
	args := c.effectiveArgs()
	if len(args) == 0 {
		return errors.New("usage: scribe each -- <subcommand> [args...]")
	}
	kbs := registeredKBs()
	if len(kbs) == 0 {
		return fmt.Errorf("no registered KBs — add one with `scribe kb add <path>` or set kb_dir in %s", userConfigPath())
	}
	job := strings.Join(args, " ")
	now := time.Now()
	var failed, skipped int
	for _, kb := range kbs {
		if reason := cadenceSkipReason(kb, args, now); reason != "" {
			skipped++
			logMsg("each", "%s :: skipped %s (%s)", kb, job, reason)
			continue
		}
		logMsg("each", "%s :: scribe %s", kb, job)
		if err := eachRunner(kb, args); err != nil {
			failed++
			logMsg("each", "%s :: FAILED: %v (continuing)", kb, err)
		}
	}
	if failed > 0 {
		logMsg("each", "%d of %d KB(s) errored running '%s'", failed, len(kbs), job)
	}
	if skipped > 0 {
		logMsg("each", "%d of %d KB(s) skipped '%s' (cadence not due)", skipped, len(kbs), job)
	}
	return nil // failure isolation: a per-KB error never fails the tick
}

// cadenceSkipReason reports why this KB's job is not due yet, or "" when it
// should run. A job runs (empty reason) whenever no cadence is configured,
// the run records can't be read, or the job has never completed ok — every
// uncertain case fails OPEN so cadence can only ever suppress provably
// redundant work, never silently stall a KB.
func cadenceSkipReason(kb string, args []string, now time.Time) string {
	cfg := loadConfig(kb)
	interval, ok := cadenceInterval(cfg, args)
	if !ok {
		return ""
	}
	records, err := loadRunRecords(kb)
	if err != nil {
		return ""
	}
	_, specific := eachJobKeys(args)
	last, ok := records[specific]
	if !ok || last.IsZero() {
		return ""
	}
	if age := now.Sub(last); age < interval {
		return fmt.Sprintf("last ok %s ago < cadence %s", shortDuration(age), shortDuration(interval))
	}
	return ""
}

// eachJobKeys splits an `each` subcommand into its command path and a
// more specific key that appends the first --flag — mirroring how
// loadRunRecords keys output/runs/*.jsonl, so a job's cadence reads its
// own last run. "sync --max 2" → ("sync", "sync --max"); "ingest drain"
// → ("ingest drain", "ingest drain"); "dream" → ("dream", "dream").
func eachJobKeys(args []string) (cmdPath, specific string) {
	var path []string
	var flag string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			if flag == "" && strings.HasPrefix(a, "--") {
				flag = a
			}
			continue // flags and their values are not part of the command path
		}
		if flag == "" {
			path = append(path, a) // leading bare words = the command path
		}
	}
	cmdPath = strings.Join(path, " ")
	specific = cmdPath
	if flag != "" {
		specific = cmdPath + " " + flag
	}
	return cmdPath, specific
}

// cadenceInterval resolves the configured interval for a job: the most
// specific cadence key ("sync --sessions") wins, falling back to the bare
// command ("sync") so one entry can pace a whole command family.
func cadenceInterval(cfg *ScribeConfig, args []string) (time.Duration, bool) {
	if cfg == nil || len(cfg.Each.Cadence) == 0 || len(args) == 0 {
		return 0, false
	}
	cmdPath, specific := eachJobKeys(args)
	for _, key := range []string{specific, cmdPath} {
		raw, ok := cfg.Each.Cadence[key]
		if !ok {
			continue
		}
		if d, err := parseCadenceDuration(raw); err == nil && d > 0 {
			return d, true
		}
		logMsg("each", "ignoring unparseable each.cadence[%q] = %q", key, raw)
		return 0, false
	}
	return 0, false
}

// parseCadenceDuration accepts a Go duration ("2h", "30m") or a bare day
// count ("7d", "1.5d") since Go's time package has no day unit and weekly
// cadences ("168h") read poorly.
func parseCadenceDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.ParseFloat(rest, 64)
		if err != nil {
			return 0, err
		}
		return time.Duration(days * 24 * float64(time.Hour)), nil
	}
	return time.ParseDuration(s)
}
