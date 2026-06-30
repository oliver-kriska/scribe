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
	// The trusted snapshot never set claude_projects_dir, so the revert
	// must refill the built-in default rather than leave it empty
	// (discovery would silently stop otherwise).
	if !strings.Contains(cfg.ClaudeProjectsDir, ".claude") {
		t.Errorf("revert wiped claude_projects_dir instead of refilling the default: %q", cfg.ClaudeProjectsDir)
	}
}

// TestNoPhantomDriftOnPrefilledDefaults: a team scribe.yaml that simply
// omits the $HOME-prefilled discovery keys must not register as drifted.
// The old code hashed the prefilled loadConfig view against the raw-parse
// trust record — mismatching forever, wiping the discovery paths on every
// load while `scribe config diff` showed nothing.
func TestNoPhantomDriftOnPrefilledDefaults(t *testing.T) {
	root := setupTrustKB(t, "team: true\nsources:\n  exclude: [\"/personal\"]\n", "")
	ensureConfigTrust(root)

	cfg := loadConfig(root)
	if !strings.Contains(cfg.ClaudeProjectsDir, ".claude") {
		t.Errorf("discovery path lost on unchanged config: %q", cfg.ClaudeProjectsDir)
	}
	if !strings.Contains(cfg.CcriderDB, "ccrider") {
		t.Errorf("ccrider_db lost on unchanged config: %q", cfg.CcriderDB)
	}
	view, ok := repoSensitiveView(root)
	if !ok {
		t.Fatal("repo view unreadable")
	}
	rec := loadTrustRecord(root)
	if d := sensitiveDiff(rec.Sensitive, view); len(d) != 0 {
		t.Errorf("unchanged config reports drift: %v", d)
	}
}

// TestDriftPerOpOllamaURLReverted: the per-op ollama_url keys win over
// llm.ollama_url in the resolver, so each one is an exfiltration route
// of its own and must be trust-locked like the global key.
func TestDriftPerOpOllamaURLReverted(t *testing.T) {
	root := setupTrustKB(t, "team: true\n", "")
	ensureConfigTrust(root)

	attack := "team: true\n" +
		"extract:\n  provider: ollama\n  ollama_url: http://evil.example.com:11434\n" +
		"session_mine:\n  ollama_url: http://evil.example.com:11434\n" +
		"absorb:\n  contextualize:\n    ollama_url: http://evil.example.com:11434\n"
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(attack), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(root)
	for name, got := range map[string]string{
		"extract":              cfg.Extract.OllamaURL,
		"session_mine":         cfg.SessionMine.OllamaURL,
		"absorb.contextualize": cfg.Absorb.Contextualize.OllamaURL,
	} {
		if strings.Contains(got, "evil.example.com") {
			t.Errorf("pushed %s.ollama_url survived the trust lock: %s", name, got)
		}
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

// --- #43 follow-up: LLM provider/model routing lock ---

// TestRoutingLockRevertsPushedProvider: in a team KB a pushed llm.provider
// (or per-op provider) flip to a NAMED hosted provider redirects KB content
// to that provider's built-in endpoint with no base_url to catch it. The
// trust lock must revert both the top-level and per-op routing to the trusted
// snapshot.
func TestRoutingLockRevertsPushedProvider(t *testing.T) {
	root := setupTrustKB(t, "team: true\nllm:\n  provider: ollama\n  model: gemma3:4b\n", "")
	ensureConfigTrust(root)

	attack := "team: true\n" +
		"llm:\n  provider: together\n  model: Qwen/Qwen3-235B\n" +
		"absorb:\n  pass2_provider: groq\n  pass2_model: llama-3.3-70b\n"
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(attack), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := loadConfig(root)
	if cfg.LLM.Provider == "together" {
		t.Errorf("pushed llm.provider=together survived the trust lock")
	}
	if cfg.LLM.Provider != "ollama" || cfg.LLM.Model != "gemma3:4b" {
		t.Errorf("llm routing not reverted to trusted values: provider=%q model=%q", cfg.LLM.Provider, cfg.LLM.Model)
	}
	// The per-op flip is the bypass the old comment ignored — it must revert
	// too (back to "" then cascade to the trusted ollama provider).
	if cfg.Absorb.Pass2Provider == "groq" {
		t.Errorf("pushed absorb.pass2_provider=groq survived the trust lock: %q", cfg.Absorb.Pass2Provider)
	}
}

// TestRoutingLockMigrationNoSpuriousDrift: a snapshot recorded before #43
// (nil routing maps) must NOT lock routing or report phantom drift — a
// provider change reads through unreverted (preserving pre-#43 behavior)
// until the record is upgraded. This is the safety that stops an upgrade
// from silently flipping live routing on existing enaia/scriptorium records.
func TestRoutingLockMigrationNoSpuriousDrift(t *testing.T) {
	root := setupTrustKB(t, "team: true\nllm:\n  provider: ollama\n  model: gemma3:4b\n", "")
	// Simulate a pre-#43 record: snapshot with the routing maps stripped.
	view, ok := repoSensitiveView(root)
	if !ok {
		t.Fatal("repo view unreadable")
	}
	old := view.withoutLLMRouting()
	if old.LLMProviders != nil {
		t.Fatal("withoutLLMRouting did not nil the maps")
	}
	if err := saveTrustRecord(root, trustRecord{Sensitive: old, ApprovedAt: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}

	// A pushed provider change with the old record present must read through
	// (not be reverted) — routing isn't locked until the record upgrades.
	push := "team: true\nllm:\n  provider: together\n  model: Qwen/Qwen3-235B\n"
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(push), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(root)
	if cfg.LLM.Provider != "together" {
		t.Errorf("pre-#43 record wrongly locked routing: provider=%q, want together (unreverted)", cfg.LLM.Provider)
	}
	// And `scribe config diff` shows no routing rows against an old record.
	newView, _ := repoSensitiveView(root)
	for _, line := range sensitiveDiff(old, newView) {
		if strings.Contains(line, ".provider:") || strings.Contains(line, ".model:") {
			t.Errorf("pre-#43 record produced a phantom routing diff row: %q", line)
		}
	}
}

// TestRoutingLockAutoUpgrade: the next sync (ensureConfigTrust) silently
// upgrades a pre-#43 record to lock the then-current routing, after which a
// pushed provider flip IS reverted.
func TestRoutingLockAutoUpgrade(t *testing.T) {
	root := setupTrustKB(t, "team: true\nllm:\n  provider: ollama\n  model: gemma3:4b\n", "")
	view, _ := repoSensitiveView(root)
	if err := saveTrustRecord(root, trustRecord{Sensitive: view.withoutLLMRouting(), ApprovedAt: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}

	// Upgrade fires only when the previously-locked keys still match.
	ensureConfigTrust(root)
	rec := loadTrustRecord(root)
	if rec == nil || rec.Sensitive.LLMProviders == nil {
		t.Fatalf("ensureConfigTrust did not upgrade the pre-#43 record")
	}
	if rec.Sensitive.LLMProviders["llm"] != "ollama" {
		t.Errorf("upgraded record locked the wrong provider: %q", rec.Sensitive.LLMProviders["llm"])
	}
	// ApprovedAt is preserved (not a fresh approval).
	if rec.ApprovedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("auto-upgrade reset ApprovedAt to %q", rec.ApprovedAt)
	}

	// Now routing is locked: a pushed flip reverts.
	push := "team: true\nllm:\n  provider: together\n  model: Qwen/Qwen3-235B\n"
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(push), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(root)
	if cfg.LLM.Provider != "ollama" {
		t.Errorf("after auto-upgrade a pushed provider flip was not reverted: provider=%q", cfg.LLM.Provider)
	}
}

// TestRoutingLockAutoUpgradeSkippedOnV0Drift: if the previously-locked keys
// themselves drifted, ensureConfigTrust must NOT auto-upgrade (that would
// silently bless an unreviewed change); the user resolves it explicitly.
func TestRoutingLockAutoUpgradeSkippedOnV0Drift(t *testing.T) {
	root := setupTrustKB(t, "team: true\nsources:\n  exclude: [\"/personal\"]\n", "")
	view, _ := repoSensitiveView(root)
	if err := saveTrustRecord(root, trustRecord{Sensitive: view.withoutLLMRouting(), ApprovedAt: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	// v0 drift: the exclude filter (a locked key) changes.
	drift := "team: true\nsources:\n  exclude: []\n"
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(drift), 0o644); err != nil {
		t.Fatal(err)
	}
	ensureConfigTrust(root)
	rec := loadTrustRecord(root)
	if rec.Sensitive.LLMProviders != nil {
		t.Errorf("auto-upgrade ran despite v0 drift — would bless an unreviewed change")
	}
}

// TestRoutingLockDiffShowsRows: against a #43 snapshot, sensitiveDiff surfaces
// provider/model changes so `scribe config diff` shows what to review.
func TestRoutingLockDiffShowsRows(t *testing.T) {
	root := setupTrustKB(t, "team: true\nllm:\n  provider: ollama\n  model: gemma3:4b\n", "")
	ensureConfigTrust(root)
	rec := loadTrustRecord(root)

	changed := ScribeConfig{}
	changed.LLM.Provider = "together"
	changed.LLM.Model = "Qwen/Qwen3-235B"
	current := sensitiveFrom(&changed)

	diff := sensitiveDiff(rec.Sensitive, current)
	var sawProvider, sawModel bool
	for _, line := range diff {
		if strings.HasPrefix(line, "llm.provider:") {
			sawProvider = true
		}
		if strings.HasPrefix(line, "llm.model:") {
			sawModel = true
		}
	}
	if !sawProvider || !sawModel {
		t.Errorf("config diff missing routing rows: %v", diff)
	}
}
