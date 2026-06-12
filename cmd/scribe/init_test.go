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
	if !strings.Contains(out, "Init plan") {
		t.Errorf("expected init plan in output; got:\n%s", out)
	}
	if !strings.Contains(out, "temp path — pass --bind") {
		t.Errorf("expected throwaway disclosure with --bind hint in the plan; got:\n%s", out)
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
	// Codex AGENTS.md must not have been written either — the
	// throwaway-path guard covers both agent handshakes.
	codexMD := filepath.Join(fakeHome, ".codex", "AGENTS.md")
	if _, err := os.Stat(codexMD); err == nil {
		t.Errorf("~/.codex/AGENTS.md should not be written to fake home; found at %s", codexMD)
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
	codexMD := filepath.Join(fakeHome, ".codex", "AGENTS.md")
	if _, err := os.Stat(codexMD); err != nil {
		t.Errorf("--bind should install ~/.codex/AGENTS.md, but %s missing: %v", codexMD, err)
	}
}

// minimalVars builds a templateVars sufficient to render either agent
// block template. Mirrors what collectVars produces in --yes mode.
func minimalVars(kbDir string) templateVars {
	return templateVars{
		OwnerName:   "Tester",
		KBName:      "testkb",
		KBDir:       kbDir,
		Domains:     []string{"general"},
		DomainsCSV:  "general",
		DomainsPipe: "general",
	}
}

// TestInstallCodexMD_Lifecycle exercises the four installAgentMD cases
// against ~/.codex/AGENTS.md via the Codex wrapper: create-when-missing,
// in-sync no-op, drift refresh, and user content outside the markers
// preserved on append.
func TestInstallCodexMD_Lifecycle(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	vars := minimalVars(t.TempDir())
	path := filepath.Join(fakeHome, ".codex", "AGENTS.md")

	// 1. Missing file → created with the block + markers.
	if err := installCodexMD(vars, false, true); err != nil {
		t.Fatalf("create: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after create: %v", err)
	}
	if !strings.Contains(string(data), claudeMDMarkerBegin) || !strings.Contains(string(data), claudeMDMarkerEnd) {
		t.Fatal("created AGENTS.md missing scribe markers")
	}
	if !strings.Contains(string(data), "testkb Knowledge Base") {
		t.Errorf("created AGENTS.md missing rendered KB name; got:\n%s", data)
	}
	if !strings.Contains(string(data), "shared drop-file location both Codex and Claude Code use") {
		t.Errorf("Codex template should carry the shared-drop-path note")
	}

	// 2. In-sync → byte-identical, no-op.
	before, _ := os.ReadFile(path)
	if err := installCodexMD(vars, false, true); err != nil {
		t.Fatalf("in-sync: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("in-sync run rewrote the file:\nbefore=%q\nafter=%q", before, after)
	}

	// 3. User content outside markers is preserved on refresh.
	userPrefix := "# My own Codex notes\n\nkeep me\n\n"
	userSuffix := "\n\n## trailing user section\nalso keep me\n"
	body, _ := os.ReadFile(path)
	mixed := userPrefix + string(body) + userSuffix
	if err := os.WriteFile(path, []byte(mixed), 0o644); err != nil {
		t.Fatalf("write mixed: %v", err)
	}
	// Drift the block by changing a rendered var, then refresh.
	vars2 := vars
	vars2.OwnerName = "Someone Else"
	if err := installCodexMD(vars2, false, true); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	out, _ := os.ReadFile(path)
	if !strings.Contains(string(out), "keep me") || !strings.Contains(string(out), "also keep me") {
		t.Errorf("refresh dropped user content outside markers; got:\n%s", out)
	}
	if !strings.Contains(string(out), "Someone Else") {
		t.Errorf("refresh did not update the drifted block; got:\n%s", out)
	}
	if strings.Count(string(out), claudeMDMarkerBegin) != 1 {
		t.Errorf("refresh should leave exactly one scribe block, got %d", strings.Count(string(out), claudeMDMarkerBegin))
	}
}

// TestInstallCodexMD_CheckModeNeverWrites confirms --check is read-only
// for the Codex handshake just like the Claude one.
func TestInstallCodexMD_CheckModeNeverWrites(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	vars := minimalVars(t.TempDir())
	if err := installCodexMD(vars, true, true); err != nil {
		t.Fatalf("check mode: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fakeHome, ".codex", "AGENTS.md")); err == nil {
		t.Error("check mode must not create ~/.codex/AGENTS.md")
	}
}

// TestAllowGlobalStateWrites pins issue #13: --yes answers prompts but
// must never retarget global state away from another KB — only an
// explicit --bind/--force may. Fresh machines and idempotent re-runs
// keep working with --yes alone.
func TestAllowGlobalStateWrites(t *testing.T) {
	const abs = "/Users/u/Projects/new-kb"
	cases := []struct {
		name             string
		force, yes, bind bool
		throwaway        bool
		existingKBDir    string
		want             bool
	}{
		{"fresh machine + yes", false, true, false, false, "", true},
		{"idempotent re-run + yes", false, true, false, false, abs, true},
		{"other KB + yes refuses (issue 13)", false, true, false, false, "/Users/u/Projects/primary-kb", false},
		{"other KB + bind", false, false, true, false, "/Users/u/Projects/primary-kb", true},
		{"other KB + force", true, false, false, false, "/Users/u/Projects/primary-kb", true},
		{"other KB + yes + bind", false, true, true, false, "/Users/u/Projects/primary-kb", true},
		{"no flags at all", false, false, false, false, "", false},
		{"throwaway beats force", true, false, false, true, "", false},
		{"throwaway beats yes", false, true, false, true, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := allowGlobalStateWrites(tc.force, tc.yes, tc.bind, tc.throwaway, tc.existingKBDir, abs)
			if got != tc.want {
				t.Errorf("allowGlobalStateWrites(force=%v yes=%v bind=%v throwaway=%v existing=%q) = %v, want %v",
					tc.force, tc.yes, tc.bind, tc.throwaway, tc.existingKBDir, got, tc.want)
			}
		})
	}
}

// TestRenderScribeYAML_ProviderThreadsIntoContextualize pins the
// `--provider ollama` bootstrap promise: every uncommented per-op block
// in the scaffolded scribe.yaml must follow the chosen provider. The
// contextualize block used to hardcode `provider: anthropic`, which (as
// an explicit per-op value) beat the llm-block inheritance and silently
// sent every raw article to Anthropic on an all-local KB.
func TestRenderScribeYAML_ProviderThreadsIntoContextualize(t *testing.T) {
	cases := []struct {
		name         string
		provider     string
		model        string
		wantProvider string
		wantModel    string
	}{
		{"ollama bootstrap", "ollama", "gemma3:4b", "ollama", "gemma3:4b"},
		{"default anthropic", "", "", "anthropic", "haiku"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vars := templateVars{
				OwnerName:   "Test",
				KBName:      "test-kb",
				Domains:     []string{"general"},
				LLMProvider: tc.provider,
				LLMModel:    tc.model,
			}
			out, err := renderTemplate("templates/scribe.yaml", vars)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "scribe.yaml"), []byte(out), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := loadConfig(dir)
			if cfg == nil {
				t.Fatal("loadConfig returned nil for rendered scribe.yaml")
			}
			if got := cfg.Absorb.Contextualize.Provider; got != tc.wantProvider {
				t.Errorf("contextualize provider = %q, want %q", got, tc.wantProvider)
			}
			if got := cfg.Absorb.Contextualize.Model; got != tc.wantModel {
				t.Errorf("contextualize model = %q, want %q", got, tc.wantModel)
			}
		})
	}
}
