package main

import (
	"bytes"
	"os"
	"os/exec"
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
	// The temp-dir KB root is a throwaway path, so these writes carry
	// the explicit bind consent (bound=true) the chokepoint requires —
	// the lifecycle under test is installAgentMD's, not the guard's.
	vars := minimalVars(t.TempDir())
	path := filepath.Join(fakeHome, ".codex", "AGENTS.md")

	// 1. Missing file → created with the block + markers.
	if err := installCodexMD(vars, false, true, true); err != nil {
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
	if err := installCodexMD(vars, false, true, true); err != nil {
		t.Fatalf("in-sync: %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
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
	if err := installCodexMD(vars2, false, true, true); err != nil {
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
	if err := installCodexMD(vars, true, true, false); err != nil {
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

// TestWriteGlobalStateThrowawayGuard pins the chokepoint every
// machine-global write (user config, agent handshake blocks, cron
// plists) routes through: a throwaway KB root may never own global
// state without explicit bind consent.
func TestWriteGlobalStateThrowawayGuard(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "global", "state.txt")
	throwawayKB := t.TempDir() // under os.TempDir() → throwaway

	err := writeGlobalState(throwawayKB, false, dest, []byte("x"), 0o644)
	if err == nil || !strings.Contains(err.Error(), "throwaway") {
		t.Fatalf("throwaway root without bind: want refusal, got %v", err)
	}
	if fileExists(dest) {
		t.Fatal("refused write still created the file")
	}

	// Explicit bind consent (init --bind) overrides the refusal.
	if err := writeGlobalState(throwawayKB, true, dest, []byte("bound"), 0o644); err != nil {
		t.Fatalf("bound write: %v", err)
	}
	data, _ := os.ReadFile(dest)
	if string(data) != "bound" {
		t.Errorf("bound write content = %q, want %q", data, "bound")
	}

	// A permanent KB root needs no bind.
	dest2 := filepath.Join(t.TempDir(), "state2.txt")
	if err := writeGlobalState("/Users/u/Projects/kb", false, dest2, []byte("y"), 0o644); err != nil {
		t.Fatalf("permanent root: %v", err)
	}

	// Empty servesRoot = the write is not KB-binding.
	dest3 := filepath.Join(t.TempDir(), "state3.txt")
	if err := writeGlobalState("", false, dest3, []byte("z"), 0o644); err != nil {
		t.Fatalf("non-binding write: %v", err)
	}
}

// TestInitGit_CreatesInitialCommit pins the team-onboarding fix: a fresh
// `scribe init` must leave a commit so the documented
// `git remote add origin … && git push -u origin main` works and members
// can clone. .gitignore (written by the skeleton before initGit) must keep
// the per-machine manifest out of that commit.
func TestInitGit_CreatesInitialCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "scribe.yaml"), "kb_name: t\n")
	writeFile(t, filepath.Join(root, ".gitignore"), "scripts/projects.json\n")
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "scripts", "projects.json"), "{}\n")

	if err := initGit(root); err != nil {
		t.Fatalf("initGit: %v", err)
	}

	if _, err := runGit(root, "rev-parse", "HEAD"); err != nil {
		t.Fatalf("expected an initial commit after initGit; rev-parse HEAD failed: %v", err)
	}

	tracked, err := runGit(root, "ls-files")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tracked, "scribe.yaml") {
		t.Errorf("scribe.yaml not committed; ls-files=%q", tracked)
	}
	if strings.Contains(tracked, "projects.json") {
		t.Errorf(".gitignore not honored — per-machine manifest committed; ls-files=%q", tracked)
	}
}

// TestInitGit_SkipsExistingRepo is the safety guard: when .git already
// exists, initGit must NOT add a commit or sweep untracked files into the
// existing history.
func TestInitGit_SkipsExistingRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	seed := func(args ...string) {
		full := append([]string{"-c", "user.name=seed", "-c", "user.email=seed@x"}, args...)
		if out, err := runGit(root, full...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	seed("init", "-b", "main")
	writeFile(t, filepath.Join(root, "existing.txt"), "x")
	seed("add", "-A")
	seed("commit", "-q", "-m", "seed")
	before, _ := runGit(root, "rev-parse", "HEAD")

	writeFile(t, filepath.Join(root, "untracked.txt"), "y")
	if err := initGit(root); err != nil {
		t.Fatalf("initGit: %v", err)
	}

	after, _ := runGit(root, "rev-parse", "HEAD")
	if before != after {
		t.Errorf("initGit moved HEAD in an existing repo: before=%s after=%s", before, after)
	}
	st, _ := runGit(root, "status", "--porcelain")
	if !strings.Contains(st, "untracked.txt") {
		t.Errorf("expected untracked.txt to stay uncommitted; status=%q", st)
	}
}

// runGit runs a one-shot git command rooted at root and returns combined output.
func runGit(root string, args ...string) (string, error) {
	full := append([]string{"-C", root}, args...)
	out, err := exec.Command("git", full...).CombinedOutput() //nolint:noctx // one-shot test git
	return string(out), err
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestInstallUserConfigPreservesSecretsAndRegistry is the regression for the
// data-loss bug where installUserConfig rewrote ~/.config/scribe/config.yaml
// wholesale on a kb_dir re-point — discarding the #26 KB registry (kbs:), the
// hosted-provider api keys, and the contributor identity. `scribe init --yes`
// run inside a second KB therefore wiped the machine's Together key and KB
// list. The re-point must preserve every field but kb_dir.
func TestInstallUserConfigPreservesSecretsAndRegistry(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_CONFIG_HOME", "")

	cfgDir := filepath.Join(fakeHome, ".config", "scribe")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := "" +
		"kb_dir: /old/primary-kb\n" +
		"kbs:\n" +
		"  - /old/primary-kb\n" +
		"  - /old/team-kb\n" +
		"contributor: Test Owner <owner@example.com>\n" +
		"llm_api_key: tgp_v1_SECRET_DO_NOT_DROP\n" +
		"llm_api_keys:\n" +
		"  together: tgp_v1_TOGETHER\n" +
		"  groq: gsk_GROQ\n"
	cfgPath := filepath.Join(cfgDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	// Swallow stdout.
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

	newRoot := filepath.Join(t.TempDir(), "second-kb")
	// bound=true bypasses the throwaway-path guard (newRoot is a tempdir);
	// yes=true skips the interactive confirm.
	if err := installUserConfig(newRoot, false, true, true); err != nil {
		t.Fatalf("installUserConfig: %v", err)
	}
	_ = w.Close()

	got := loadUserConfig()
	if got.KBDir != newRoot {
		t.Errorf("KBDir = %q, want %q (re-pointed)", got.KBDir, newRoot)
	}
	if len(got.KBs) != 2 || got.KBs[1] != "/old/team-kb" {
		t.Errorf("KBs = %v, want the original 2-entry registry preserved", got.KBs)
	}
	if got.Contributor != "Test Owner <owner@example.com>" {
		t.Errorf("Contributor = %q, want it preserved", got.Contributor)
	}
	if got.LLMAPIKey != "tgp_v1_SECRET_DO_NOT_DROP" {
		t.Errorf("LLMAPIKey was dropped on re-point: got %q", got.LLMAPIKey)
	}
	if got.LLMAPIKeys["together"] != "tgp_v1_TOGETHER" || got.LLMAPIKeys["groq"] != "gsk_GROQ" {
		t.Errorf("LLMAPIKeys dropped on re-point: got %v", got.LLMAPIKeys)
	}

	// A file holding a secret must not be world/group-readable.
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config perms = %o, want 0600 (file carries an api key)", perm)
	}
}
