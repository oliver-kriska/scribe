package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// writeCodexRollout helps tests build synthetic rollout files. cwd is
// embedded into the session_meta payload; the second event is a tiny
// stub so the file has more than one line (matches real Codex output).
func writeCodexRollout(t *testing.T, root, year, month, day, id, cwd string) string {
	t.Helper()
	dir := filepath.Join(root, year, month, day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir rollout dir: %v", err)
	}
	name := fmt.Sprintf("rollout-%s-%s-%sT00-00-00-%s.jsonl", year, month, day, id)
	path := filepath.Join(dir, name)

	envelope := map[string]any{
		"timestamp": fmt.Sprintf("%s-%s-%sT00:00:00.000Z", year, month, day),
		"type":      "session_meta",
		"payload": map[string]any{
			"id":         id,
			"timestamp":  fmt.Sprintf("%s-%s-%sT00:00:00.000Z", year, month, day),
			"cwd":        cwd,
			"originator": "Codex CLI",
			"source":     "cli",
			"git": map[string]any{
				"branch":         "main",
				"commit_hash":    "0000000000000000000000000000000000000000",
				"repository_url": "https://example.invalid/repo.git",
			},
		},
	}
	first, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	body := make([]byte, 0, len(first)+128)
	body = append(body, first...)
	body = append(body, '\n')
	body = append(body, []byte(`{"type":"message","payload":{"role":"user","content":"hi"}}`+"\n")...)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	return path
}

// makeProjectWithMarkdown creates a directory with a README.md so
// hasSignificantContent accepts it. Returns the absolute path.
func makeProjectWithMarkdown(t *testing.T, parent, name string) string {
	t.Helper()
	p := filepath.Join(parent, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	readme := filepath.Join(p, "README.md")
	if err := os.WriteFile(readme, []byte("# "+name+"\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	return p
}

func TestReadCodexSessionMeta_HappyPath(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	cwd := makeProjectWithMarkdown(t, tmp, "happy-project")
	path := writeCodexRollout(t, tmp, "2026", "02", "07", "id-1", cwd)

	meta, err := readCodexSessionMeta(path)
	if err != nil {
		t.Fatalf("readCodexSessionMeta: %v", err)
	}
	if meta == nil {
		t.Fatal("expected non-nil meta")
	}
	if meta.Cwd != cwd {
		t.Errorf("Cwd = %q, want %q", meta.Cwd, cwd)
	}
	if meta.Git.Branch != "main" {
		t.Errorf("Git.Branch = %q, want %q", meta.Git.Branch, "main")
	}
}

func TestReadCodexSessionMeta_ObjectSource(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "rollout-object-source.jsonl")
	body := []byte(`{"type":"session_meta","payload":{"id":"subagent-1","cwd":"/tmp/project","source":{"subagent":{"thread_spawn":{"depth":1}}}}}` + "\n")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	meta, err := readCodexSessionMeta(path)
	if err != nil {
		t.Fatalf("readCodexSessionMeta object source: %v", err)
	}
	if meta == nil || meta.ID != "subagent-1" || meta.Cwd != "/tmp/project" {
		t.Fatalf("unexpected metadata: %+v", meta)
	}
}

func TestReadCodexSessionMeta_EmptyFile(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "empty.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	meta, err := readCodexSessionMeta(path)
	if err != nil {
		t.Fatalf("readCodexSessionMeta empty: %v", err)
	}
	if meta != nil {
		t.Errorf("expected nil meta on empty file, got %+v", meta)
	}
}

func TestReadCodexSessionMeta_MalformedFirstLine(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "bad.jsonl")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}
	meta, err := readCodexSessionMeta(path)
	if err == nil {
		t.Errorf("expected error on malformed line, got meta=%+v", meta)
	}
}

func TestReadCodexSessionMeta_WrongEventType(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "wrong.jsonl")
	body := []byte(`{"type":"message","payload":{"role":"user","content":"hi"}}` + "\n")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write wrong-type: %v", err)
	}
	meta, err := readCodexSessionMeta(path)
	if err != nil {
		t.Fatalf("readCodexSessionMeta wrong-type: %v", err)
	}
	if meta != nil {
		t.Errorf("expected nil meta on non-session_meta first line, got %+v", meta)
	}
}

func TestReadCodexSessionMeta_FixtureFile(t *testing.T) {
	t.Parallel()
	// The static fixture pins the on-disk shape Codex writes so a
	// silent schema change in our parser shows up as a test failure.
	meta, err := readCodexSessionMeta("testdata/codex/rollout-fixture.jsonl")
	if err != nil {
		t.Fatalf("readCodexSessionMeta fixture: %v", err)
	}
	if meta == nil {
		t.Fatal("fixture meta nil")
	}
	if meta.Cwd != "/tmp/scribe-codex-fixture" {
		t.Errorf("fixture Cwd = %q", meta.Cwd)
	}
	if meta.Originator != "Codex Desktop" {
		t.Errorf("fixture Originator = %q", meta.Originator)
	}
	if meta.Git.RepositoryURL == "" {
		t.Error("fixture git.repository_url should not be empty")
	}
}

func TestWalkCodexSessions_DedupesCwd(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	sessionsRoot := filepath.Join(tmp, "sessions")
	cwdA := makeProjectWithMarkdown(t, tmp, "proj-a")
	cwdB := makeProjectWithMarkdown(t, tmp, "proj-b")

	// Two rollouts for the same cwd (older + newer day), one for the
	// second cwd. Walk should yield each cwd exactly once.
	writeCodexRollout(t, sessionsRoot, "2026", "02", "07", "id-a-1", cwdA)
	writeCodexRollout(t, sessionsRoot, "2026", "03", "01", "id-a-2", cwdA)
	writeCodexRollout(t, sessionsRoot, "2026", "02", "07", "id-b-1", cwdB)

	var seen []string
	err := walkCodexSessions(sessionsRoot, func(m *codexSessionMeta, _ string) {
		seen = append(seen, m.Cwd)
	})
	if err != nil {
		t.Fatalf("walkCodexSessions: %v", err)
	}
	sort.Strings(seen)
	want := []string{cwdA, cwdB}
	sort.Strings(want)
	if len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Errorf("dedup: got %v want %v", seen, want)
	}
}

func TestWalkCodexSessions_DescendingOrderWinsCwdRace(t *testing.T) {
	t.Parallel()
	// Two rollouts share an `id` but disagree on cwd (e.g. a project
	// rename mid-session). The plan says: most-recent rollout wins. We
	// use distinct `id`s here because Codex `id` is per-rollout — the
	// race is on cwd alone, settled by descending date order.
	tmp := t.TempDir()
	sessionsRoot := filepath.Join(tmp, "sessions")

	oldCwd := makeProjectWithMarkdown(t, tmp, "old-name")
	newCwd := makeProjectWithMarkdown(t, tmp, "new-name")

	writeCodexRollout(t, sessionsRoot, "2026", "01", "01", "id-old", oldCwd)
	writeCodexRollout(t, sessionsRoot, "2026", "05", "01", "id-new", newCwd)

	var firstSeen string
	err := walkCodexSessions(sessionsRoot, func(m *codexSessionMeta, _ string) {
		if firstSeen == "" {
			firstSeen = m.Cwd
		}
	})
	if err != nil {
		t.Fatalf("walkCodexSessions: %v", err)
	}
	if firstSeen != newCwd {
		t.Errorf("first-seen cwd = %q, want most recent %q", firstSeen, newCwd)
	}
}

func TestWalkCodexSessions_MissingRootIsNoOp(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	gone := filepath.Join(tmp, "does-not-exist")

	called := false
	err := walkCodexSessions(gone, func(_ *codexSessionMeta, _ string) {
		called = true
	})
	if err != nil {
		t.Errorf("missing root should be a silent no-op, got: %v", err)
	}
	if called {
		t.Error("fn called for missing root")
	}
}

func TestDiscoverCodex_AddsNewProjects(t *testing.T) {
	// Hermetic ~/.codex pointing at a temp tree with two cwds + a
	// duplicate of the first. Manifest starts empty.
	tmp := t.TempDir()
	sessionsRoot := filepath.Join(tmp, "sessions")

	projects := filepath.Join(tmp, "projects-root")
	cwdA := makeProjectWithMarkdown(t, projects, "proj-a")
	cwdB := makeProjectWithMarkdown(t, projects, "proj-b")
	t.Setenv("SCRIBE_PROJECT_ROOTS", filepath.Base(projects))
	// projectName uses a package-level cache populated at init from the
	// env var. Refresh it for this test so cwd basenames win over
	// "parent-leaf".
	projectRoots = defaultProjectRoots()

	writeCodexRollout(t, sessionsRoot, "2026", "02", "07", "id-a-1", cwdA)
	writeCodexRollout(t, sessionsRoot, "2026", "02", "08", "id-a-2", cwdA)
	writeCodexRollout(t, sessionsRoot, "2026", "02", "09", "id-b-1", cwdB)

	kbRoot := filepath.Join(tmp, "kb")
	if err := os.MkdirAll(filepath.Join(kbRoot, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(kbRoot, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir kb/projects: %v", err)
	}
	manifestPath := filepath.Join(kbRoot, "scripts", "projects.json")
	if err := os.WriteFile(manifestPath, []byte(`{"projects":{},"domain_aliases":{},"ignored_paths":[]}`), 0o644); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	manifest, err := loadManifest(kbRoot)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}

	cfg := &ScribeConfig{
		CodexSessionsDir: sessionsRoot,
	}

	// projects under temp will fail manifest.isIgnored's depth >= 4
	// check on some systems; sidestep by passing a depth-friendly path
	// via an extra subdir. macOS /private/var paths satisfy this
	// naturally because TempDir expands to /var/folders/.../T/.../.
	// We still want to assert.
	if manifest.isIgnored(cwdA) {
		t.Skipf("temp dir %q is treated as ignored on this platform; cannot exercise discoverCodex here", cwdA)
	}

	s := &SyncCmd{}
	got, err := s.discoverCodex(kbRoot, manifest, cfg)
	if err != nil {
		t.Fatalf("discoverCodex: %v", err)
	}
	if got != 2 {
		t.Errorf("discovered = %d, want 2", got)
	}
	if len(manifest.Projects) != 2 {
		t.Errorf("manifest projects = %d, want 2", len(manifest.Projects))
	}
	for pname, entry := range manifest.Projects {
		if entry.DiscoveredFrom != "codex" {
			t.Errorf("project %s: DiscoveredFrom = %q, want %q", pname, entry.DiscoveredFrom, "codex")
		}
	}

	// Re-running discovery is idempotent: no new projects.
	got2, err := s.discoverCodex(kbRoot, manifest, cfg)
	if err != nil {
		t.Fatalf("discoverCodex 2nd run: %v", err)
	}
	if got2 != 0 {
		t.Errorf("2nd run discovered = %d, want 0", got2)
	}
}

func TestDiscoverCodex_PromotesClaudeOnlyToBoth(t *testing.T) {
	tmp := t.TempDir()
	sessionsRoot := filepath.Join(tmp, "sessions")
	projects := filepath.Join(tmp, "projects-root")
	cwd := makeProjectWithMarkdown(t, projects, "shared-proj")
	t.Setenv("SCRIBE_PROJECT_ROOTS", filepath.Base(projects))
	projectRoots = defaultProjectRoots()

	writeCodexRollout(t, sessionsRoot, "2026", "02", "07", "id-1", cwd)

	kbRoot := filepath.Join(tmp, "kb")
	if err := os.MkdirAll(filepath.Join(kbRoot, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(kbRoot, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir kb/projects: %v", err)
	}

	// Pre-seed manifest as if Claude had already surfaced this project.
	pname := projectName(cwd)
	manifestPath := filepath.Join(kbRoot, "scripts", "projects.json")
	seed := fmt.Sprintf(`{"projects":{%q:{"path":%q,"domain":"general","discovered_from":"claude"}},"domain_aliases":{},"ignored_paths":[]}`, pname, cwd)
	if err := os.WriteFile(manifestPath, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	manifest, err := loadManifest(kbRoot)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}

	if manifest.isIgnored(cwd) {
		t.Skip("temp dir treated as ignored; cannot test promotion here")
	}

	cfg := &ScribeConfig{CodexSessionsDir: sessionsRoot}
	s := &SyncCmd{}
	if _, err := s.discoverCodex(kbRoot, manifest, cfg); err != nil {
		t.Fatalf("discoverCodex: %v", err)
	}
	// Manifest.Projects is now keyed by canonical path (see manifest.go),
	// not the legacy projectName-derived string seeded above — resolve by
	// Name (which migration inherited 1:1 from the old key) instead.
	entry, err := manifest.resolve(pname)
	if err != nil {
		t.Fatalf("project %q gone from manifest after discovery: %v", pname, err)
	}
	if entry.DiscoveredFrom != "both" {
		t.Errorf("DiscoveredFrom = %q, want %q", entry.DiscoveredFrom, "both")
	}
}

// TestDiscoverCodex_SameBasenameBothEnroll is the basename-collision
// regression this plan fixes: two Codex-discovered cwds under different
// parent roots that derive the SAME projectName basename must both enroll
// as distinct manifest entries (auto-disambiguated Name), not have the
// second one refused/shadowed the way pre-#8 discovery did.
func TestDiscoverCodex_SameBasenameBothEnroll(t *testing.T) {
	tmp := t.TempDir()
	sessionsRoot := filepath.Join(tmp, "sessions")

	rootA := filepath.Join(tmp, "org-a", "Projects")
	rootB := filepath.Join(tmp, "org-b", "Projects")
	t.Setenv("SCRIBE_PROJECT_ROOTS", filepath.Base(rootA))
	projectRoots = defaultProjectRoots()

	cwdA := makeProjectWithMarkdown(t, rootA, "api")
	cwdB := makeProjectWithMarkdown(t, rootB, "api")

	writeCodexRollout(t, sessionsRoot, "2026", "02", "07", "id-a", cwdA)
	writeCodexRollout(t, sessionsRoot, "2026", "02", "08", "id-b", cwdB)

	kbRoot := filepath.Join(tmp, "kb")
	if err := os.MkdirAll(filepath.Join(kbRoot, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(kbRoot, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir kb/projects: %v", err)
	}
	manifestPath := filepath.Join(kbRoot, "scripts", "projects.json")
	if err := os.WriteFile(manifestPath, []byte(`{"projects":{},"domain_aliases":{},"ignored_paths":[]}`), 0o644); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	manifest, err := loadManifest(kbRoot)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if manifest.isIgnored(cwdA) {
		t.Skipf("temp dir %q is treated as ignored on this platform; cannot exercise discoverCodex here", cwdA)
	}

	cfg := &ScribeConfig{CodexSessionsDir: sessionsRoot}
	s := &SyncCmd{}
	got, err := s.discoverCodex(kbRoot, manifest, cfg)
	if err != nil {
		t.Fatalf("discoverCodex: %v", err)
	}
	if got != 2 {
		t.Fatalf("discovered = %d, want 2 (both same-basename repos enroll)", got)
	}
	if len(manifest.Projects) != 2 {
		t.Fatalf("manifest projects = %d, want 2: %v", len(manifest.Projects), manifest.Projects)
	}

	names := make([]string, 0, len(manifest.Projects))
	for _, e := range manifest.Projects {
		names = append(names, e.Name)
	}
	if names[0] == names[1] {
		t.Errorf("both entries share Name %q — uniqueName should have disambiguated", names[0])
	}
}

func TestProjectEntry_DiscoveredSourceBackCompat(t *testing.T) {
	t.Parallel()
	// Empty field (pre-existing entries) reads as "claude".
	e := &ProjectEntry{Path: "/x", Domain: "general"}
	if got := e.DiscoveredSource(); got != "claude" {
		t.Errorf("empty field = %q, want claude", got)
	}
	e.DiscoveredFrom = "codex"
	if got := e.DiscoveredSource(); got != "codex" {
		t.Errorf("explicit codex = %q", got)
	}

	// Merge: claude -> seeing codex promotes to both.
	e = &ProjectEntry{DiscoveredFrom: "claude"}
	e.MergeDiscoveredFrom("codex")
	if e.DiscoveredFrom != "both" {
		t.Errorf("claude+codex = %q, want both", e.DiscoveredFrom)
	}
	// Idempotent on repeat.
	e.MergeDiscoveredFrom("codex")
	if e.DiscoveredFrom != "both" {
		t.Errorf("both+codex repeat = %q, want both", e.DiscoveredFrom)
	}
	// Already-both stays both regardless of source.
	e.MergeDiscoveredFrom("claude")
	if e.DiscoveredFrom != "both" {
		t.Errorf("both+claude = %q, want both", e.DiscoveredFrom)
	}
}

func TestCodexRolloutCount_LimitsAndCounts(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	sessions := filepath.Join(tmp, "sessions")
	// Three rollouts across two days; one stray non-rollout file.
	writeCodexRollout(t, sessions, "2026", "02", "07", "a", "/tmp/x")
	writeCodexRollout(t, sessions, "2026", "02", "07", "b", "/tmp/y")
	writeCodexRollout(t, sessions, "2026", "02", "08", "c", "/tmp/z")
	if err := os.WriteFile(filepath.Join(sessions, "2026", "02", "08", "stray.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatalf("write stray: %v", err)
	}

	if got := codexRolloutCount(sessions, 0); got != 3 {
		t.Errorf("count unlimited = %d, want 3", got)
	}
	if got := codexRolloutCount(sessions, 2); got != 2 {
		t.Errorf("count limit=2 = %d, want 2", got)
	}
	if got := codexRolloutCount(filepath.Join(tmp, "missing"), 0); got != 0 {
		t.Errorf("missing dir count = %d, want 0", got)
	}
}

func TestCodexProbeRollout_FindsMostRecent(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	sessions := filepath.Join(tmp, "sessions")
	writeCodexRollout(t, sessions, "2026", "01", "01", "old", "/tmp/x")
	newPath := writeCodexRollout(t, sessions, "2026", "05", "01", "new", "/tmp/y")
	got := codexProbeRollout(sessions)
	if got != newPath {
		t.Errorf("probe = %q, want %q", got, newPath)
	}
	if codexProbeRollout(filepath.Join(tmp, "nope")) != "" {
		t.Error("probe of missing dir should be empty")
	}
}
