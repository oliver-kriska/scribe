package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveProbe_DreamGrant checks the monolithic dream's Bash grant against
// the real Claude Code CLI. prompts/dream.md tells the model that Bash runs
// only `wc`, `ls` and `find`, and that `find -delete` / `-exec` are refused
// (measured on Claude Code 2.1.293). That is CLI behavior, not scribe's, so
// it can drift with any CLI update; TestDreamPrompt_ToolGrantMatchesCode
// only pins the scribe side.
//
// Opt-in and never run in CI. It spends a few haiku tokens on the
// developer's own claude login:
//
//	make live-probe
//	# or: SCRIBE_LIVE_CLAUDE=1 go test -tags sqlite_fts5 -run TestLiveProbe -count=1 -v ./cmd/scribe/
//
// The call uses claudeArgs, the argv builder realRunClaude uses, with the
// dream's real grant (dreamMonolithicTools), in a t.TempDir() that holds
// only a throwaway file. The prompt goes over stdin, as in production.
func TestLiveProbe_DreamGrant(t *testing.T) {
	if !liveProbe.enabled {
		t.Skip("live probe: set SCRIBE_LIVE_CLAUDE=1 (or run `make live-probe`) to call the real claude CLI")
	}
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("live probe: claude is not on PATH")
	}
	if liveProbe.home == "" {
		t.Fatal("live probe: no HOME to find the claude login in (set SCRIBE_LIVE_CLAUDE_HOME)")
	}

	dir := t.TempDir()
	victim := filepath.Join(dir, "throwaway.txt")
	if err := os.WriteFile(victim, []byte("probe\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prompt := "This is an automated permission probe. Use the Bash tool to run these two commands, " +
		"exactly as written, each as its own Bash call, in this order, even if you expect one to be refused:\n" +
		"1. ls\n" +
		"2. find . -name '*.txt' -delete\n" +
		"Do not use any other tool and do not modify the commands. Then reply with the output of command 1 " +
		"and whether command 2 was allowed."

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, claudeBin, claudeArgs(dir, "haiku", dreamMonolithicTools)...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(prompt)
	// TestMain points HOME at a scratch dir, where claude has no login, so
	// the subprocess alone gets the real one. Scribe's own lookups in this
	// process stay sandboxed.
	cmd.Env = append(os.Environ(), "HOME="+liveProbe.home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	env, ok := parseClaudeResult(stdout.String())
	if !ok {
		t.Fatalf("no result envelope (run err: %v)\nstdout:\n%s\nstderr:\n%s", runErr, tailLines(stdout.String(), 20), tailLines(stderr.String(), 20))
	}
	t.Logf("claude -p: subtype=%s turns=%d denials=%d resolved=%v", env.Subtype, env.NumTurns, len(env.PermissionDenials), mapKeys(env.ModelUsage))

	// The load-bearing assertion: whatever the model did, the file survived.
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("find -delete was NOT refused: %s is gone (%v). The dream grant in dreamMonolithicTools no longer blocks deletion on this Claude Code version; update dream.go and prompts/dream.md", filepath.Base(victim), err)
	}

	var deleteDenied, lsDenied bool
	for _, d := range env.PermissionDenials {
		if d.ToolName != toolNameBash {
			continue
		}
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(d.ToolInput, &in) != nil {
			continue
		}
		c := strings.TrimSpace(leadingCDPrefix.ReplaceAllString(in.Command, ""))
		t.Logf("denied: %s", c)
		if strings.Contains(c, "-delete") {
			deleteDenied = true
		}
		if c == "ls" || strings.HasPrefix(c, "ls ") {
			lsDenied = true
		}
	}
	if !deleteDenied {
		t.Errorf("no permission denial for the find -delete command: the model may not have attempted it (result: %q). The probe proved nothing; rerun", truncateBytes(env.Result, 300))
	}
	if lsDenied {
		t.Errorf("plain `ls` was refused: the grant is narrower than prompts/dream.md states")
	}
	if !strings.Contains(env.Result, "throwaway") {
		t.Errorf("result does not show the `ls` output (want it to list throwaway.txt): %q", truncateBytes(env.Result, 300))
	}
}

// liveProbe holds the opt-in settings, captured by TestMain before it
// scrubs SCRIBE_* and replaces HOME for hermeticity.
var liveProbe struct {
	enabled bool
	home    string
}

func captureLiveProbeEnv() {
	liveProbe.enabled = os.Getenv("SCRIBE_LIVE_CLAUDE") == "1"
	liveProbe.home = os.Getenv("SCRIBE_LIVE_CLAUDE_HOME")
	if liveProbe.home == "" {
		liveProbe.home = os.Getenv("HOME")
	}
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
