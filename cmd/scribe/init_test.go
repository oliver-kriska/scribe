package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIsThrowawayPath guards the heuristic that gates global state
// rewrites in `scribe init -p ...`. The 2026-05-13 incident burned
// anthropic tokens overnight because /tmp/freshkb was treated as a
// real KB; this test pins down which prefixes trip the guard so a
// future refactor can't quietly drop /tmp/ from the list.
func TestIsThrowawayPath(t *testing.T) {
	realTmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		realTmp = os.TempDir()
	}
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"empty", "", false},
		{"tmp dir itself", "/tmp", true},
		{"tmp subdir", "/tmp/freshkb", true},
		{"private tmp", "/private/tmp/freshkb", true},
		{"var folders", "/var/folders/abc/T/scribe-test", true},
		{"private var folders", "/private/var/folders/abc/T/scribe-test", true},
		{"os.TempDir subdir", filepath.Join(realTmp, "scribe-test"), true},
		{"user home not throwaway", "/Users/oliverkriska/Projects/scriptorium", false},
		{"opt path not throwaway", "/opt/scribe/kb", false},
		{"tmpsomething not throwaway", "/tmpfile", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isThrowawayPath(tc.path)
			if got != tc.want {
				t.Errorf("isThrowawayPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestIsThrowawayPath_RealTempDir uses t.TempDir() so the test passes
// on machines where $TMPDIR resolves outside the canonical prefixes
// (some CI runners). The guard MUST trip on whatever t.TempDir() gave us.
func TestIsThrowawayPath_RealTempDir(t *testing.T) {
	dir := t.TempDir()
	if !isThrowawayPath(dir) {
		t.Errorf("t.TempDir() = %q must be classified as throwaway", dir)
	}
	if !isThrowawayPath(filepath.Join(dir, "child", "grandchild")) {
		t.Errorf("subdir of t.TempDir() must inherit the throwaway classification")
	}
}

// TestRunBootstrap_ThrowawayPathSkipsGlobals is the integration-level
// guard against the 2026-05-13 regression. Running `scribe init -p
// /tmp/...` MUST NOT call installClaudeMD or installUserConfig. We
// can't easily intercept those without refactoring, so this test
// checks the user-visible signal: a "Refusing to retarget" line in
// the stdout / a "skipping ~/.claude/CLAUDE.md" line.
//
// The test wires up a fake HOME so even if a regression slipped past
// the guard, we wouldn't damage the test runner's real CLAUDE.md.
func TestRunBootstrap_ThrowawayPathSkipsGlobals(t *testing.T) {
	if testing.Short() {
		t.Skip("uses a real tempdir + writes scaffold")
	}
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	// userConfigPath() prefers $XDG_CONFIG_HOME when set; empty makes it
	// fall back to HOME. Ubuntu CI sets XDG_CONFIG_HOME, so without this
	// the user-config write would land in the real ~/.config not fakeHome.
	t.Setenv("XDG_CONFIG_HOME", "")
	kb := filepath.Join(t.TempDir(), "smoke-kb")

	c := &InitCmd{
		Path:    kb,
		Yes:     true,
		NoCron:  true,
		NoGit:   true,
		Domains: []string{"general"},
	}

	// Capture stdout.
	r, w, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = origStdout })

	done := make(chan struct{})
	var output []byte
	go func() {
		defer close(done)
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				output = append(output, buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()

	if err := c.runBootstrap(); err != nil {
		t.Fatalf("runBootstrap: %v", err)
	}
	_ = w.Close()
	<-done

	out := string(output)
	if !strings.Contains(out, "throwaway/temp path") {
		t.Errorf("expected throwaway warning in output; got:\n%s", out)
	}
	if !strings.Contains(out, "Re-run with --bind") {
		t.Errorf("expected --bind suggestion in output; got:\n%s", out)
	}
	// CLAUDE.md must not have been written.
	claudeMD := filepath.Join(fakeHome, ".claude", "CLAUDE.md")
	if _, err := os.Stat(claudeMD); err == nil {
		t.Errorf("CLAUDE.md should not be written to fake home; found at %s", claudeMD)
	}
	// User config must not have been written.
	userCfg := filepath.Join(fakeHome, ".config", "scribe", "config.yaml")
	if _, err := os.Stat(userCfg); err == nil {
		t.Errorf("user config should not be written to fake home; found at %s", userCfg)
	}
}

// TestRunBootstrap_BindFlagAllowsThrowawayWrites confirms --bind is the
// explicit escape hatch for the rare case where a /tmp/ path really is
// meant to be primary.
func TestRunBootstrap_BindFlagAllowsThrowawayWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("writes scaffold + global state to fake home")
	}
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	// userConfigPath() prefers $XDG_CONFIG_HOME when set; empty makes it
	// fall back to HOME. Ubuntu CI sets XDG_CONFIG_HOME, so without this
	// the user-config write would land in the real ~/.config not fakeHome.
	t.Setenv("XDG_CONFIG_HOME", "")
	kb := filepath.Join(t.TempDir(), "intentional-kb")

	c := &InitCmd{
		Path:    kb,
		Yes:     true,
		Bind:    true,
		NoCron:  true,
		NoGit:   true,
		Domains: []string{"general"},
	}
	// Swallow stdout to keep test output clean.
	r, w, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = origStdout })
	go func() {
		buf := make([]byte, 32*1024)
		for {
			if _, err := r.Read(buf); err != nil {
				return
			}
		}
	}()

	if err := c.runBootstrap(); err != nil {
		t.Fatalf("runBootstrap: %v", err)
	}
	_ = w.Close()

	claudeMD := filepath.Join(fakeHome, ".claude", "CLAUDE.md")
	if _, err := os.Stat(claudeMD); err != nil {
		t.Errorf("--bind should install ~/.claude/CLAUDE.md, but %s missing: %v", claudeMD, err)
	}
	userCfg := filepath.Join(fakeHome, ".config", "scribe", "config.yaml")
	if _, err := os.Stat(userCfg); err != nil {
		t.Errorf("--bind should install ~/.config/scribe/config.yaml, but %s missing: %v", userCfg, err)
	}
}
