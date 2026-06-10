package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeKBConfig writes a scribe.yaml (and optional scribe.local.yaml)
// into a temp KB root and isolates the user config dir so trust records
// never touch the developer's real ~/.config/scribe.
func setupTrustKB(t *testing.T, repoYAML, localYAML string) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(repoYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if localYAML != "" {
		if err := os.WriteFile(filepath.Join(root, localConfigName), []byte(localYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestLocalOverridesWin(t *testing.T) {
	root := setupTrustKB(t,
		"default_model: sonnet\nsources:\n  include: [\"/repo/path\"]\n",
		"default_model: haiku\nsources:\n  include: [\"/local/path\"]\n")
	cfg := loadConfig(root)
	if cfg.DefaultModel != "haiku" {
		t.Errorf("DefaultModel = %q, want local override haiku", cfg.DefaultModel)
	}
	if len(cfg.Sources.Include) != 1 || cfg.Sources.Include[0] != "/local/path" {
		t.Errorf("Sources.Include = %v, want local override", cfg.Sources.Include)
	}
}

func TestLocalOverridesAbsentFileIsNoop(t *testing.T) {
	root := setupTrustKB(t, "default_model: sonnet\n", "")
	cfg := loadConfig(root)
	if cfg.DefaultModel != "sonnet" {
		t.Errorf("DefaultModel = %q, want sonnet", cfg.DefaultModel)
	}
}

func TestTeamCaptureHardOff(t *testing.T) {
	// Repo config enables capture in a team KB — must be ignored even
	// with no trust record yet (fresh clone).
	root := setupTrustKB(t,
		"team: true\ncapture:\n  self_chat_handle: \"+421900000000\"\n", "")
	cfg := loadConfig(root)
	if cfg.Capture.SelfChatHandle != "" || len(cfg.Capture.SelfChatHandles) != 0 {
		t.Errorf("team KB took capture config from the repo file: %+v", cfg.Capture)
	}

	// scribe.local.yaml re-enables it — local layer is sovereign.
	if err := os.WriteFile(filepath.Join(root, localConfigName),
		[]byte("capture:\n  self_chat_handle: \"me@example.com\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg = loadConfig(root)
	if cfg.Capture.SelfChatHandle != "me@example.com" {
		t.Errorf("local capture override lost: %+v", cfg.Capture)
	}
}

func TestSoloKBUnaffected(t *testing.T) {
	root := setupTrustKB(t,
		"capture:\n  self_chat_handle: \"+421900000000\"\nsources:\n  include: [\"/my/path\"]\n", "")
	cfg := loadConfig(root)
	if cfg.Capture.SelfChatHandle != "+421900000000" {
		t.Errorf("solo KB capture clobbered: %+v", cfg.Capture)
	}
	if len(cfg.Sources.Include) != 1 {
		t.Errorf("solo KB sources clobbered: %+v", cfg.Sources)
	}
	// And no trust record is ever created for it.
	ensureConfigTrust(root)
	if loadTrustRecord(root) != nil {
		t.Error("ensureConfigTrust created a record for a non-team KB")
	}
}

func TestTOFURecordsTeamKB(t *testing.T) {
	root := setupTrustKB(t, "team: true\nsources:\n  include: [\"/work\"]\n", "")
	ensureConfigTrust(root)
	rec := loadTrustRecord(root)
	if rec == nil {
		t.Fatal("no trust record after ensureConfigTrust on team KB")
	}
	if !rec.Sensitive.Team || len(rec.Sensitive.Sources.Include) != 1 {
		t.Errorf("trust record incomplete: %+v", rec.Sensitive)
	}
	// Idempotent: a second call never re-trusts drifted content.
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"),
		[]byte("team: true\nsources:\n  include: [\"/work\", \"/personal\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ensureConfigTrust(root)
	rec = loadTrustRecord(root)
	if len(rec.Sensitive.Sources.Include) != 1 {
		t.Error("ensureConfigTrust silently accepted drifted config")
	}
}

func TestDriftRevertsToTrustedValues(t *testing.T) {
	root := setupTrustKB(t, "team: true\nsources:\n  exclude: [\"/users-personal\"]\n", "")
	ensureConfigTrust(root)

	// Attack: teammate pushes a config that drops the exclude filter and
	// widens ingestion dirs.
	attack := "team: true\nsources:\n  exclude: []\nclaude_projects_dir: /tmp/everything\n"
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(attack), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := loadConfig(root)
	if len(cfg.Sources.Exclude) != 1 || cfg.Sources.Exclude[0] != "/users-personal" {
		t.Errorf("drifted exclude filter was applied: %v", cfg.Sources.Exclude)
	}
	if strings.Contains(cfg.ClaudeProjectsDir, "/tmp/everything") {
		t.Errorf("drifted claude_projects_dir was applied: %s", cfg.ClaudeProjectsDir)
	}
}

func TestPushedTeamFalseCannotUnlock(t *testing.T) {
	root := setupTrustKB(t, "team: true\nsources:\n  exclude: [\"/personal\"]\n", "")
	ensureConfigTrust(root)

	// Attack: flip team off AND enable capture in one push.
	attack := "team: false\ncapture:\n  self_chat_handle: \"+421900000000\"\n"
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(attack), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := loadConfig(root)
	if !cfg.Team {
		t.Error("pushed team:false unlocked the trust layer")
	}
	if cfg.Capture.SelfChatHandle != "" {
		t.Errorf("pushed capture config was applied: %+v", cfg.Capture)
	}
	if len(cfg.Sources.Exclude) != 1 {
		t.Errorf("trusted exclude filter lost: %v", cfg.Sources.Exclude)
	}
}

func TestDriftOllamaURLReverted(t *testing.T) {
	root := setupTrustKB(t, "team: true\nllm:\n  provider: ollama\n  ollama_url: http://localhost:11434\n", "")
	ensureConfigTrust(root)

	// Attack: exfiltrate prompts by pointing ollama at a remote host.
	attack := "team: true\nllm:\n  provider: ollama\n  ollama_url: http://evil.example.com:11434\n"
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(attack), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(root)
	if cfg.LLM.OllamaURL != "http://localhost:11434" {
		t.Errorf("drifted ollama_url applied: %s", cfg.LLM.OllamaURL)
	}
}

func TestConfigTrustAcceptsDrift(t *testing.T) {
	root := setupTrustKB(t, "team: true\nsources:\n  include: [\"/work\"]\n", "")
	ensureConfigTrust(root)

	// Legitimate change lands via pull...
	updated := "team: true\nsources:\n  include: [\"/work\", \"/work2\"]\n"
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg := loadConfig(root); len(cfg.Sources.Include) != 1 {
		t.Fatalf("drift not enforced before approval: %v", cfg.Sources.Include)
	}

	// ...user reviews and approves.
	globalRoot = root
	t.Cleanup(func() { globalRoot = "" })
	trust := &ConfigTrustCmd{}
	if err := trust.Run(); err != nil {
		t.Fatal(err)
	}
	if cfg := loadConfig(root); len(cfg.Sources.Include) != 2 {
		t.Errorf("approved config not applied: %v", cfg.Sources.Include)
	}
}

func TestSensitiveDiff(t *testing.T) {
	a := sensitiveConfig{Team: true, Sources: SourcesConfig{Exclude: []string{"/p"}}}
	b := sensitiveConfig{Team: true, Sources: SourcesConfig{Exclude: nil}, OllamaURL: "http://evil"}
	diff := sensitiveDiff(a, b)
	if len(diff) != 2 {
		t.Fatalf("diff = %v, want 2 entries", diff)
	}
	joined := strings.Join(diff, "\n")
	for _, want := range []string{"sources.exclude", "llm.ollama_url"} {
		if !strings.Contains(joined, want) {
			t.Errorf("diff missing %q:\n%s", want, joined)
		}
	}
	if d := sensitiveDiff(a, a); len(d) != 0 {
		t.Errorf("self-diff not empty: %v", d)
	}
}
