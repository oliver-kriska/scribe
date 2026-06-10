package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// config_trust.go — the local trust layer for shared (team) KBs.
//
// Threat model: in a team KB the repo's scribe.yaml is writable by every
// member, and sync auto-pulls before each run. Without a guard, a pushed
// config edit silently changes what YOUR scribe ingests on the next cron
// fire — widen sources filters and personal repos get mined into the
// team repo; enable capture and your iMessages do. The defense has three
// layers, all anchored OUTSIDE the repo so a push can never disable them:
//
//  1. scribe.local.yaml (KB root, gitignored) — user-owned overrides that
//     always win over the repo scribe.yaml.
//  2. A trust record in ~/.config/scribe/trust.json — a per-machine
//     snapshot of the repo config's sensitive keys, approved by the user
//     (TOFU at first sync, explicitly via `scribe config trust`). When
//     the repo file drifts from the snapshot, scribe keeps running on
//     the TRUSTED values and warns, until the user reviews.
//  3. Team capture hard-off — in a team KB, iMessage capture from the
//     repo config is ignored entirely; only scribe.local.yaml (or the
//     SCRIBE_SELF_CHAT_ID env) can enable it.
//
// Enforcement keys off the trust record's team flag, not the incoming
// file's — so pushing `team: false` cannot bypass the lock.

// localConfigName is the gitignored, user-owned override file in the KB
// root. Values here always win over the repo scribe.yaml.
const localConfigName = "scribe.local.yaml"

// sensitiveConfig is the slice of repo-controlled config that can widen
// what scribe ingests or redirect LLM traffic. Anything here is locked
// by the trust record in team KBs. Keep this list tight and documented:
//
//   - Team: flips the whole enforcement regime — must itself be locked.
//   - Sources: include/exclude decide which project paths get mined.
//   - ClaudeProjectsDir / CodexSessionsDir / CcriderDB: discovery and
//     session-mining inputs — repointing them changes what gets read.
//   - Capture: iMessage ingestion (handles + filters) — personal data.
//   - OllamaURL (global + every per-op override): prompts (with file
//     contents) are POSTed here; a pushed remote URL would exfiltrate
//     everything the pipeline reads. The per-op keys win over the
//     global one in the resolver chain, so locking only llm.ollama_url
//     would leave seven bypass routes. Per-op `provider` flips stay
//     unlocked: with every URL locked they can only redirect to an
//     already-trusted endpoint, and providers are a routine local tune.
//   - CodexMine: enables an additional transcript source.
//   - SecretScan: the credential gate — a pushed disable/allow_paths
//     change must not weaken what another member's machine commits.
type sensitiveConfig struct {
	Team              bool             `json:"team"`
	Sources           SourcesConfig    `json:"sources"`
	ClaudeProjectsDir string           `json:"claude_projects_dir"`
	CodexSessionsDir  string           `json:"codex_sessions_dir"`
	CcriderDB         string           `json:"ccrider_db"`
	Capture           CaptureConfig    `json:"capture"`
	OllamaURL         string           `json:"ollama_url"`
	CodexMine         bool             `json:"codex_mine"`
	SecretScan        SecretScanConfig `json:"secret_scan"`

	ExtractOllamaURL       string `json:"extract_ollama_url"`
	DreamOllamaURL         string `json:"dream_ollama_url"`
	SessionMineOllamaURL   string `json:"session_mine_ollama_url"`
	AssessOllamaURL        string `json:"assess_ollama_url"`
	DeepIngestOllamaURL    string `json:"deep_ingest_ollama_url"`
	RelationsOllamaURL     string `json:"relations_ollama_url"`
	ContextualizeOllamaURL string `json:"contextualize_ollama_url"`
}

func sensitiveFrom(cfg *ScribeConfig) sensitiveConfig {
	return sensitiveConfig{
		Team:              cfg.Team,
		Sources:           cfg.Sources,
		ClaudeProjectsDir: cfg.ClaudeProjectsDir,
		CodexSessionsDir:  cfg.CodexSessionsDir,
		CcriderDB:         cfg.CcriderDB,
		Capture:           cfg.Capture,
		OllamaURL:         cfg.LLM.OllamaURL,
		CodexMine:         cfg.Codex.Mine,
		SecretScan:        cfg.SecretScan,

		ExtractOllamaURL:       cfg.Extract.OllamaURL,
		DreamOllamaURL:         cfg.Dream.OllamaURL,
		SessionMineOllamaURL:   cfg.SessionMine.OllamaURL,
		AssessOllamaURL:        cfg.Assess.OllamaURL,
		DeepIngestOllamaURL:    cfg.DeepIngest.OllamaURL,
		RelationsOllamaURL:     cfg.Relations.OllamaURL,
		ContextualizeOllamaURL: cfg.Absorb.Contextualize.OllamaURL,
	}
}

// hash returns a stable digest of the sensitive view for drift checks.
func (s sensitiveConfig) hash() string {
	data, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// applyTo writes the trusted values back over cfg — the revert that runs
// when the repo file drifted in a team KB.
func (s sensitiveConfig) applyTo(cfg *ScribeConfig) {
	cfg.Team = s.Team
	cfg.Sources = s.Sources
	cfg.ClaudeProjectsDir = s.ClaudeProjectsDir
	cfg.CodexSessionsDir = s.CodexSessionsDir
	cfg.CcriderDB = s.CcriderDB
	cfg.Capture = s.Capture
	cfg.LLM.OllamaURL = s.OllamaURL
	cfg.Codex.Mine = s.CodexMine
	cfg.SecretScan = s.SecretScan

	cfg.Extract.OllamaURL = s.ExtractOllamaURL
	cfg.Dream.OllamaURL = s.DreamOllamaURL
	cfg.SessionMine.OllamaURL = s.SessionMineOllamaURL
	cfg.Assess.OllamaURL = s.AssessOllamaURL
	cfg.DeepIngest.OllamaURL = s.DeepIngestOllamaURL
	cfg.Relations.OllamaURL = s.RelationsOllamaURL
	cfg.Absorb.Contextualize.OllamaURL = s.ContextualizeOllamaURL
}

// trustRecord is one approved sensitive snapshot for one KB root.
type trustRecord struct {
	Sensitive  sensitiveConfig `json:"sensitive"`
	ApprovedAt string          `json:"approved_at"`
}

// trustFilePath is the per-machine trust store, deliberately under the
// user config dir — never inside the KB repo, so no push can touch it.
func trustFilePath() string {
	return filepath.Join(userConfigDir(), "trust.json")
}

func loadTrustStore() map[string]trustRecord {
	store := map[string]trustRecord{}
	data, err := os.ReadFile(trustFilePath())
	if err != nil {
		return store
	}
	_ = json.Unmarshal(data, &store)
	return store
}

func loadTrustRecord(root string) *trustRecord {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil
	}
	if rec, ok := loadTrustStore()[abs]; ok {
		return &rec
	}
	return nil
}

func saveTrustRecord(root string, rec trustRecord) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	store := loadTrustStore()
	store[abs] = rec
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(userConfigDir(), 0o755); err != nil {
		return err
	}
	tmp := trustFilePath() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, trustFilePath())
}

// enforceConfigTrust runs inside loadConfig, after the repo scribe.yaml
// is unmarshaled and BEFORE scribe.local.yaml is applied — so it judges
// exactly the repo-controlled view, and local overrides stay sovereign.
// Read-only: loadConfig stays pure; TOFU recording happens in
// ensureConfigTrust from mutating entrypoints.
func enforceConfigTrust(root string, cfg *ScribeConfig) {
	rec := loadTrustRecord(root)

	// The team flag that decides enforcement comes from the local trust
	// record when one exists (a pushed `team: false` cannot unlock), and
	// from the incoming file otherwise (a fresh clone of a team KB gets
	// the capture hard-off below even before its first TOFU).
	team := cfg.Team
	if rec != nil && rec.Sensitive.Team {
		team = true
	}

	if rec != nil && rec.Sensitive.Team {
		// Compare the RAW repo view against the record — the record was
		// written from a raw parse (repoSensitiveView), while cfg here
		// already carries $HOME-prefilled discovery paths. Hashing cfg
		// would flag every scribe.yaml that legitimately omits a
		// prefilled key as drifted, forever, with `scribe config diff`
		// simultaneously showing nothing. An unreadable/unparseable file
		// counts as drift: run on the trusted values.
		repoView, ok := repoSensitiveView(root)
		if !ok || repoView.hash() != rec.Sensitive.hash() {
			rec.Sensitive.applyTo(cfg)
			// The trusted snapshot stores raw values, so keys the
			// approved scribe.yaml never set come back as empty strings —
			// refill the built-in discovery defaults the revert wiped.
			// (LLM URL defaults are applied later in loadConfig.)
			fillDiscoveryDefaults(cfg)
			logAutoFlipOnce("trust-drift:"+root, "trust",
				"scribe.yaml sensitive settings changed since last approval — running on trusted values; review with `scribe config diff`, accept with `scribe config trust`")
		}
	}

	if team {
		// Capture (iMessage) never runs off the repo config in a team
		// KB — not even off the trusted snapshot. Personal-source
		// ingestion is strictly a local decision: scribe.local.yaml or
		// SCRIBE_SELF_CHAT_ID re-enable it after this point.
		cfg.Capture = CaptureConfig{}
	}
}

// applyLocalOverrides layers scribe.local.yaml over cfg. The file is
// user-owned and gitignored, so everything in it is implicitly trusted —
// including re-enabling capture in a team KB. Absent file is the normal
// case; a parse error logs once and leaves cfg as-is.
func applyLocalOverrides(root string, cfg *ScribeConfig) {
	data, err := os.ReadFile(filepath.Join(root, localConfigName))
	if err != nil {
		return
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		logAutoFlipOnce("local-config-error:"+root, "config",
			"%s has errors — ignoring local overrides: %v", localConfigName, err)
	}
}

// repoSensitiveView parses ONLY the repo scribe.yaml (no defaults, no
// local overrides) into the sensitive view. ok=false when the file is
// missing or unparseable.
func repoSensitiveView(root string) (sensitiveConfig, bool) {
	data, err := os.ReadFile(filepath.Join(root, "scribe.yaml"))
	if err != nil {
		return sensitiveConfig{}, false
	}
	var cfg ScribeConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return sensitiveConfig{}, false
	}
	return sensitiveFrom(&cfg), true
}

// ensureConfigTrust is the TOFU writer: called from mutating entrypoints
// (sync) so the first run against a team KB records the then-current
// repo config as trusted. Solo KBs (team: false) get no record — the
// enforcement machinery stays entirely out of their way. Later drift is
// never auto-accepted; only `scribe config trust` updates a record.
func ensureConfigTrust(root string) {
	sv, ok := repoSensitiveView(root)
	if !ok || !sv.Team {
		return
	}
	if loadTrustRecord(root) != nil {
		return
	}
	if err := saveTrustRecord(root, trustRecord{Sensitive: sv, ApprovedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		logMsg("trust", "could not record config trust: %v", err)
		return
	}
	logMsg("trust", "team KB: recorded initial config trust (sensitive keys locked — `scribe config diff` shows future changes)")
}

// sensitiveDiff renders a line-per-key comparison of two sensitive
// views for `scribe config diff` and doctor. Empty slice = no drift.
func sensitiveDiff(trusted, current sensitiveConfig) []string {
	pairs := []struct {
		key      string
		old, new any
	}{
		{"team", trusted.Team, current.Team},
		{"sources.include", trusted.Sources.Include, current.Sources.Include},
		{"sources.exclude", trusted.Sources.Exclude, current.Sources.Exclude},
		{"sources.allowed_remotes", trusted.Sources.AllowedRemotes, current.Sources.AllowedRemotes},
		{"claude_projects_dir", trusted.ClaudeProjectsDir, current.ClaudeProjectsDir},
		{"codex_sessions_dir", trusted.CodexSessionsDir, current.CodexSessionsDir},
		{"ccrider_db", trusted.CcriderDB, current.CcriderDB},
		{"capture", trusted.Capture, current.Capture},
		{"llm.ollama_url", trusted.OllamaURL, current.OllamaURL},
		{"codex.mine", trusted.CodexMine, current.CodexMine},
		{"secret_scan", trusted.SecretScan, current.SecretScan},
		{"extract.ollama_url", trusted.ExtractOllamaURL, current.ExtractOllamaURL},
		{"dream.ollama_url", trusted.DreamOllamaURL, current.DreamOllamaURL},
		{"session_mine.ollama_url", trusted.SessionMineOllamaURL, current.SessionMineOllamaURL},
		{"assess.ollama_url", trusted.AssessOllamaURL, current.AssessOllamaURL},
		{"deep_ingest.ollama_url", trusted.DeepIngestOllamaURL, current.DeepIngestOllamaURL},
		{"relations.ollama_url", trusted.RelationsOllamaURL, current.RelationsOllamaURL},
		{"absorb.contextualize.ollama_url", trusted.ContextualizeOllamaURL, current.ContextualizeOllamaURL},
	}
	var out []string
	for _, p := range pairs {
		oldJSON, _ := json.Marshal(p.old)
		newJSON, _ := json.Marshal(p.new)
		if string(oldJSON) != string(newJSON) {
			out = append(out, fmt.Sprintf("%s: %s -> %s", p.key, oldJSON, newJSON))
		}
	}
	return out
}
