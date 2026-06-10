package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain points HOME and XDG_CONFIG_HOME at a scratch directory so no
// test — present or future — can touch the developer's real global
// state (~/.config/scribe/trust.json, ~/.claude/CLAUDE.md,
// ~/.codex/AGENTS.md, ccrider's sessions DB). Hermeticity was per-test
// discipline before; this makes it the package floor. Tests that need
// their own isolated HOME still override via t.Setenv.
func TestMain(m *testing.M) {
	scratch, err := os.MkdirTemp("", "scribe-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", scratch)
	os.Unsetenv("XDG_CONFIG_HOME")
	// Deterministic git defaults: fixtures must not inherit whatever
	// the developer's real ~/.gitconfig sets (or doesn't). The push/
	// rebase tests assume `main`; identity covers fixtures that forget
	// to set repo-local user.name.
	gitconfig := "[user]\n\tname = Scribe Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n"
	if err := os.WriteFile(filepath.Join(scratch, ".gitconfig"), []byte(gitconfig), 0o644); err != nil {
		panic(err)
	}
	// A developer's shell exports (SCRIBE_KB, SCRIBE_SELF_CHAT_ID, ...)
	// must not steer test behavior either.
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "SCRIBE_") {
			os.Unsetenv(strings.SplitN(kv, "=", 2)[0])
		}
	}
	code := m.Run()
	os.RemoveAll(scratch)
	os.Exit(code)
}
