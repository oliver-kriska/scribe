package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

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
		return fmt.Errorf("usage: scribe each -- <subcommand> [args...]")
	}
	kbs := registeredKBs()
	if len(kbs) == 0 {
		return fmt.Errorf("no registered KBs — add one with `scribe kb add <path>` or set kb_dir in %s", userConfigPath())
	}
	job := strings.Join(args, " ")
	var failed int
	for _, kb := range kbs {
		logMsg("each", "%s :: scribe %s", kb, job)
		if err := eachRunner(kb, args); err != nil {
			failed++
			logMsg("each", "%s :: FAILED: %v (continuing)", kb, err)
		}
	}
	if failed > 0 {
		logMsg("each", "%d of %d KB(s) errored running '%s'", failed, len(kbs), job)
	}
	return nil // failure isolation: a per-KB error never fails the tick
}
