package main

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// This file ports the manual two-teammate runbook (~/scribe-teamtest) into
// committed, self-asserting integration tests. It builds real KBs from the
// embedded templates, wires them to a shared bare remote, and drives the
// actual `scribe sync` pipeline + the real conflict resolver — so the
// team-mode features (contributor stamping, the secret-scan gate, the team
// digest, subscriptions) and both conflict outcomes (derived auto-resolve vs
// content rebase-abort) regress automatically instead of by hand.
//
// Isolation is total: HOME + XDG_CONFIG_HOME point at a temp dir (so no real
// ~/.claude scan, no real ~/.config/scribe/trust.json write), lock_dir is
// redirected off /tmp (so the test never touches the live cron's
// machine-wide sync lock), and SCRIBE_SKIP_REINDEX=1 stubs out qmd. No
// network, no LLM.

// syncBuffer is a concurrency-safe sink for the slog output sync emits, so an
// assertion can read back log lines (e.g. "subscribed:", "SECRET HELD") even
// if a phase logs from a goroutine under -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *syncBuffer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Reset()
}

// captureSlog swaps the default slog handler for one writing to a buffer and
// restores it on cleanup. logMsg() routes through slog.Info, so this captures
// every sync log line.
func captureSlog(t *testing.T) *syncBuffer {
	t.Helper()
	prev := slog.Default()
	sb := &syncBuffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(sb, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return sb
}

// isolateEnv redirects every home-derived path at a fresh temp dir so a test
// run can never read or write the developer's real machine state.
func isolateEnv(t *testing.T) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(filepath.Join(home, ".config"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig")) // hermetic git
	t.Setenv("SCRIBE_SKIP_REINDEX", "1")                             // no qmd
	t.Setenv("SCRIBE_KB", "")                                        // flipped per sync below
}

func mustGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := runGit(root, args...) // runGit is defined in init_test.go
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, root, err, out)
	}
	return out
}

func setIdentity(t *testing.T, root, name string) {
	t.Helper()
	mustGit(t, root, "config", "user.name", name)
	mustGit(t, root, "config", "user.email", strings.ToLower(name)+"@test.local")
}

// writeKBFile lives in conflicts_test.go (same package) — reused here.

func readKBFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// scaffoldTeamKB materializes a team KB from the embedded templates, then
// redirects lock_dir off /tmp so the test never contends with the real cron.
func scaffoldTeamKB(t *testing.T, root, kbName, lockDir string) {
	t.Helper()
	if err := createKBSkeleton(root); err != nil {
		t.Fatalf("createKBSkeleton: %v", err)
	}
	vars := templateVars{
		KBName:      kbName,
		KBDir:       root,
		OwnerName:   "Team",
		Domains:     []string{"backend"},
		DomainsCSV:  "backend",
		DomainsPipe: "backend",
		Today:       "2026-06-23",
		TeamMode:    true, // renders `team: true` + gitignores scripts/projects.json
	}
	if err := writeEmbeddedTemplates(root, vars); err != nil {
		t.Fatalf("writeEmbeddedTemplates: %v", err)
	}
	// The template hardcodes `lock_dir: /tmp`; point it at a per-test dir so
	// acquireLock never touches the live /tmp/scribe-sync.lock. This patched
	// scribe.yaml is committed, so the clone (bob) inherits it too.
	yamlPath := filepath.Join(root, "scribe.yaml")
	cfg, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(cfg), "lock_dir: /tmp", "lock_dir: "+lockDir, 1)
	if patched == string(cfg) {
		t.Fatalf("lock_dir line not found in rendered scribe.yaml — template changed?")
	}
	if err := os.WriteFile(yamlPath, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runSyncIn points kbDir() at kb via SCRIBE_KB and drives the real sync
// pipeline. Fields mirror kong's defaults for the no-LLM path (empty KB, no
// --sessions, no discoverable projects → discover/extract/mining are no-ops).
func runSyncIn(t *testing.T, kb string) {
	t.Helper()
	if err := os.Setenv("SCRIBE_KB", kb); err != nil {
		t.Fatal(err)
	}
	s := &SyncCmd{Max: 3, Parallel: 1, Model: "sonnet", SessionsMax: 3, SessionSort: "score"}
	if err := s.Run(); err != nil {
		t.Fatalf("sync %s: %v", kb, err)
	}
}

// TestTeamWorkflow_EndToEnd drives the full publish→subscribe round-trip
// through the real sync command and asserts the four team features by their
// durable side effects (frontmatter, git tracking, committed digest) plus the
// captured subscription log line.
func TestTeamWorkflow_EndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	isolateEnv(t)
	logs := captureSlog(t)

	tmp := t.TempDir()
	lockDir := filepath.Join(tmp, "locks")
	remote := filepath.Join(tmp, "remote.git")
	mustGit(t, tmp, "init", "--bare", "-b", "main", remote)

	// --- alice: scaffold a team KB, commit, publish to the shared remote ---
	alice := filepath.Join(tmp, "alice")
	if err := os.MkdirAll(alice, 0o755); err != nil {
		t.Fatal(err)
	}
	scaffoldTeamKB(t, alice, "alice-kb", lockDir)
	mustGit(t, alice, "init", "-b", "main")
	setIdentity(t, alice, "Alice")
	mustGit(t, alice, "add", "-A")
	mustGit(t, alice, "commit", "-q", "-m", "scaffold")
	mustGit(t, alice, "remote", "add", "origin", remote)
	mustGit(t, alice, "push", "-q", "-u", "origin", "main")

	// --- bob: clone the same remote, set a backend subscription (local-only) ---
	bob := filepath.Join(tmp, "bob")
	mustGit(t, tmp, "clone", "-q", remote, bob)
	setIdentity(t, bob, "Bob")
	writeKBFile(t, bob, "scribe.local.yaml", "subscriptions:\n  domains: [backend]\n  notify: false\n")

	// === Feature: contributor stamping + digest ===
	// alice writes a NEW backend article with no contributor frontmatter.
	writeKBFile(t, alice, "wiki/patterns/ratelimit.md",
		"---\ntitle: \"Token bucket rate limiter\"\ndomain: backend\ntags: [api]\n---\nA token bucket refills steadily.\n")
	logs.Reset()
	runSyncIn(t, alice)

	if got := readKBFile(t, alice, "wiki/patterns/ratelimit.md"); !strings.Contains(got, "contributor: 'Alice'") {
		t.Errorf("contributor not stamped on new article; frontmatter:\n%s", got)
	}
	if tracked := mustGit(t, alice, "ls-files"); !strings.Contains(tracked, "wiki/patterns/ratelimit.md") {
		t.Errorf("ratelimit.md not committed; ls-files:\n%s", tracked)
	}
	// The digest is generated every sync, but attribution lags by one sync:
	// writeDigestFile regenerates from *committed* history, and ratelimit.md
	// is only committed later in this same sync — so it surfaces in Alice's
	// activity on the next sync (asserted after the secret-gate sync below).
	if !fileExists(filepath.Join(alice, "wiki/_digest.md")) {
		t.Error("team digest wiki/_digest.md not generated")
	}

	// === Feature: secret-scan gate ===
	// A non-allowlisted fake AWS key must be held back from the commit.
	// (AKIAIOSFODNN7EXAMPLE would NOT work — it's an allowlisted stopword.)
	writeKBFile(t, alice, "wiki/patterns/leak.md",
		"---\ntitle: \"Leak\"\ndomain: backend\n---\naws_access_key_id = AKIA3QXY7TUV2WRS5LMN\n")
	logs.Reset()
	runSyncIn(t, alice)

	if !strings.Contains(logs.String(), "SECRET HELD") {
		t.Errorf("secret gate did not fire; logs:\n%s", logs.String())
	}
	if tracked := mustGit(t, alice, "ls-files"); strings.Contains(tracked, "leak.md") {
		t.Errorf("leaked credential file was committed; ls-files:\n%s", tracked)
	}
	// ratelimit.md is in committed history now, so this sync's digest
	// regeneration attributes it to Alice.
	if d := readKBFile(t, alice, "wiki/_digest.md"); !strings.Contains(d, "Alice") {
		t.Errorf("digest missing contributor attribution after article committed; digest:\n%s", d)
	}

	// === Feature: subscriptions ===
	// bob pulls; the backend article must surface attributed to Alice, and
	// the held leak.md must NOT have reached the remote.
	logs.Reset()
	runSyncIn(t, bob)

	sub := logs.String()
	if !strings.Contains(sub, "subscribed:") ||
		!strings.Contains(sub, "Token bucket rate limiter") ||
		!strings.Contains(sub, "backend") ||
		!strings.Contains(sub, "Alice") {
		t.Errorf("subscription did not surface the backend article by Alice; logs:\n%s", sub)
	}
	if !fileExists(filepath.Join(bob, "wiki/patterns/ratelimit.md")) {
		t.Error("bob did not pull the published article")
	}
	if fileExists(filepath.Join(bob, "wiki/patterns/leak.md")) {
		t.Error("held credential file leaked to the remote and reached bob")
	}
}

// TestTeamConflict_ContentFileAbortsRebase asserts the fail-safe: when two
// members edit the same content article, a pull rebase that conflicts on it
// aborts cleanly and restores the working tree — never leaving conflict
// markers on disk for a cron-driven sync.
func TestTeamConflict_ContentFileAbortsRebase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	isolateEnv(t)
	seed, bob := divergentClones(t, "wiki/patterns/shared.md")

	ok, _, err := pullRebase(bob)
	if ok || err == nil {
		t.Fatalf("expected a content conflict to abort the rebase; ok=%v err=%v", ok, err)
	}
	if !strings.Contains(err.Error(), "needs manual resolution (rebase aborted, working tree restored)") {
		t.Errorf("unexpected error message: %v", err)
	}
	if rebaseInProgress(bob) {
		t.Error("rebase left mid-flight — working tree not restored")
	}
	if got := readKBFile(t, bob, "wiki/patterns/shared.md"); got != "bob change\n" {
		t.Errorf("working tree not restored to bob's version; got %q", got)
	}
	_ = seed
}

// TestTeamConflict_DigestAutoResolves asserts the other half: a conflict
// confined to a derived/regenerable file (wiki/_digest.md) is resolved
// without a human, so the rebase completes and the file regenerates.
func TestTeamConflict_DigestAutoResolves(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	isolateEnv(t)
	_, bob := divergentClones(t, "wiki/_digest.md")

	ok, _, err := pullRebase(bob)
	if !ok || err != nil {
		t.Fatalf("expected derived-file conflict to auto-resolve; ok=%v err=%v", ok, err)
	}
	if rebaseInProgress(bob) {
		t.Error("rebase left mid-flight after auto-resolve")
	}
	if !fileExists(filepath.Join(bob, "wiki/_digest.md")) {
		t.Error("digest missing after auto-resolve (should regenerate)")
	}
}

// divergentClones builds a bare remote with a base commit containing rel, then
// returns a seed repo (which pushed an edit to rel) and a bob clone (which
// committed a different edit to rel without pulling) — i.e. a guaranteed
// same-file rebase conflict waiting for pullRebase(bob).
func divergentClones(t *testing.T, rel string) (seed, bob string) {
	t.Helper()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	mustGit(t, tmp, "init", "--bare", "-b", "main", remote)

	seed = filepath.Join(tmp, "seed")
	writeKBFile(t, seed, rel, "base line\n")
	mustGit(t, seed, "init", "-b", "main")
	setIdentity(t, seed, "Seed")
	mustGit(t, seed, "add", "-A")
	mustGit(t, seed, "commit", "-q", "-m", "base")
	mustGit(t, seed, "remote", "add", "origin", remote)
	mustGit(t, seed, "push", "-q", "-u", "origin", "main")

	bob = filepath.Join(tmp, "bob")
	mustGit(t, tmp, "clone", "-q", remote, bob)
	setIdentity(t, bob, "Bob")

	// seed advances the shared file on the remote ...
	writeKBFile(t, seed, rel, "seed change\n")
	mustGit(t, seed, "commit", "-q", "-a", "-m", "seed edit")
	mustGit(t, seed, "push", "-q")

	// ... while bob commits a conflicting change locally (no pull yet).
	writeKBFile(t, bob, rel, "bob change\n")
	mustGit(t, bob, "commit", "-q", "-a", "-m", "bob edit")
	return seed, bob
}
