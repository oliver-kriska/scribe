package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
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
	// to set repo-local user.name. GIT_CONFIG_GLOBAL pins git to this
	// file even when the developer exports their own, and NOSYSTEM
	// blocks /etc/gitconfig surprises (core.hooksPath etc.).
	// maintenance/gc off: fetch in a fixture clone can spawn a DETACHED
	// `gc --auto` that outlives the test and races t.TempDir() cleanup
	// ("unlinkat .git/objects/pack: directory not empty" — flaked under
	// full-suite load 2026-06-11). autoDetach=false is belt-and-braces
	// in case some git path still triggers gc.
	gitconfig := "[user]\n\tname = Scribe Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n" +
		"[maintenance]\n\tauto = false\n[gc]\n\tauto = 0\n\tautoDetach = false\n"
	gitconfigPath := filepath.Join(scratch, ".gitconfig")
	if err := os.WriteFile(gitconfigPath, []byte(gitconfig), 0o644); err != nil {
		panic(err)
	}
	os.Setenv("GIT_CONFIG_GLOBAL", gitconfigPath)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, v := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		os.Unsetenv(v)
	}
	// Git exports repo-locating vars to its hooks (GIT_DIR is absolute
	// when pushing from a linked worktree), and lefthook's pre-push
	// test-race inherits them — pointing every fixture's git subprocess
	// at THIS repo instead of the fixture's temp repo (observed:
	// a fixture `git commit` reporting "On branch <this branch>", and
	// codex discovery folding a temp project into the real repo).
	// Scrub them so the suite is hermetic under hook/CI environments too.
	for _, v := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
		"GIT_OBJECT_DIRECTORY", "GIT_PREFIX", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
	} {
		os.Unsetenv(v)
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

// TestRootCommandsAreGrouped locks the --help grouping: every command field
// on the root CLI struct must carry a `group` tag whose key is registered in
// commandGroups. Without this, a new command silently lands in an untitled
// "Commands:" section at the top of --help.
func TestRootCommandsAreGrouped(t *testing.T) {
	tp := reflect.TypeOf(CLI{})
	for i := range tp.NumField() {
		f := tp.Field(i)
		if _, isCmd := f.Tag.Lookup("cmd"); !isCmd {
			continue
		}
		key, ok := f.Tag.Lookup("group")
		if !ok || key == "" {
			t.Errorf("CLI.%s has no group tag — add group:%q (or another key from commandGroups) so --help stays sectioned", f.Name, "core")
			continue
		}
		if _, known := commandGroups[key]; !known {
			t.Errorf("CLI.%s uses unknown group %q — register it in commandGroups or use an existing key", f.Name, key)
		}
	}
}

// TestHelpRendersGroupSections renders the real --help output through the
// same kong options main() uses and asserts the group section titles show
// up — i.e. the grouping is actually visible, not just present as tags.
func TestHelpRendersGroupSections(t *testing.T) {
	var out bytes.Buffer
	exited := false
	opts := append(kongOptions(),
		kong.Writers(&out, &out),
		kong.Exit(func(int) {
			exited = true
			panic(true) // fake exit; recovered below (upstream kong test pattern)
		}),
	)
	parser, err := kong.New(&CLI{}, opts...)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected --help to exit")
			}
		}()
		_, _ = parser.Parse([]string{"--help"})
	}()
	if !exited {
		t.Fatal("kong.Exit was not invoked for --help")
	}

	help := out.String()
	// Every group registered in commandGroups must render as a titled
	// section — each currently has at least one command tagged with it.
	for _, title := range commandGroups {
		if !strings.Contains(help, title) {
			t.Errorf("--help output missing group section %q", title)
		}
	}
	// Kong puts ungrouped commands under a bare "Commands:" header before
	// any grouped section — its presence means something escaped grouping.
	if strings.Contains(help, "\nCommands:\n") {
		t.Error("--help contains an ungrouped \"Commands:\" section — every root command must carry a group tag")
	}
}
