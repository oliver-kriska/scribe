package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeRemoteURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"ssh scp-like", "git@github.com:org/repo.git", "github.com/org/repo"},
		{"https", "https://github.com/org/repo.git", "github.com/org/repo"},
		{"https no .git", "https://github.com/org/repo", "github.com/org/repo"},
		{"ssh url scheme", "ssh://git@github.com/org/repo.git", "github.com/org/repo"},
		{"host case folded", "git@GitHub.com:Org/Repo.git", "github.com/Org/Repo"},
		{"gitlab subgroup", "git@gitlab.com:group/sub/repo.git", "gitlab.com/group/sub/repo"},
		{"https with port", "https://git.corp.example:8443/team/repo.git", "git.corp.example:8443/team/repo"},
		{"trailing slash", "https://github.com/org/repo/", "github.com/org/repo"},
		{"local path", "/srv/git/repo.git", "/srv/git/repo"},
		{"windows-ish path not split on colon", "C:/repos/thing", "C:/repos/thing"},
		{"empty", "", ""},
		{"whitespace", "  \n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeRemoteURL(tt.raw); got != tt.want {
				t.Errorf("normalizeRemoteURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}

	// Protocol variants must converge on ONE key — that's the whole point.
	variants := []string{
		"git@github.com:org/repo.git",
		"https://github.com/org/repo.git",
		"ssh://git@github.com/org/repo",
	}
	first := normalizeRemoteURL(variants[0])
	for _, v := range variants[1:] {
		if got := normalizeRemoteURL(v); got != first {
			t.Errorf("variant %q normalized to %q, want %q", v, got, first)
		}
	}
}

func TestLedgerRoundTrip(t *testing.T) {
	root := t.TempDir()

	l := loadLedger(root)
	if len(l.Repos) != 0 {
		t.Fatalf("fresh ledger not empty: %v", l.Repos)
	}

	l.record("github.com/org/repo", "abc123", "alice")
	l.record("", "abc123", "alice")         // empty key ignored
	l.record("github.com/org/x", "", "bob") // empty sha ignored
	if err := l.save(); err != nil {
		t.Fatal(err)
	}

	reloaded := loadLedger(root)
	e, ok := reloaded.lookup("github.com/org/repo")
	if !ok {
		t.Fatal("recorded entry not found after reload")
	}
	if e.SHA != "abc123" || e.By != "alice" || e.ExtractedAt == "" {
		t.Errorf("entry = %+v", e)
	}
	if len(reloaded.Repos) != 1 {
		t.Errorf("ledger has %d entries, want 1 (empty key/sha ignored)", len(reloaded.Repos))
	}
	if _, ok := reloaded.lookup(""); ok {
		t.Error("empty key lookup should miss")
	}
}

func TestLoadLedgerCorruptStartsFresh(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledgerPath(root), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := loadLedger(root)
	if len(l.Repos) != 0 {
		t.Errorf("corrupt ledger should start fresh, got %v", l.Repos)
	}
	l.record("github.com/org/repo", "def456", "")
	if err := l.save(); err != nil {
		t.Fatalf("save over corrupt file: %v", err)
	}
}

func TestRepoLedgerKey(t *testing.T) {
	repo := initTestGitRepo(t, "Ledger Tester")
	if got := repoLedgerKey(repo); got != "" {
		t.Errorf("repo without origin → key %q, want empty", got)
	}

	gitRun(t, repo, "remote", "add", "origin", "git@github.com:org/repo.git")
	if got := repoLedgerKey(repo); got != "github.com/org/repo" {
		t.Errorf("key = %q, want github.com/org/repo", got)
	}

	if got := repoLedgerKey(t.TempDir()); got != "" {
		t.Errorf("non-git dir → key %q, want empty", got)
	}
}

// TestExtractionSkippedWhenLedgerHasSHA is the team-dedupe core: a
// teammate extracted this revision (ledger says so), so this machine
// must skip it and fast-forward its local marker instead.
func TestExtractionSkippedWhenLedgerHasSHA(t *testing.T) {
	repo := initTestGitRepo(t, "Ledger Tester")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")
	gitRun(t, repo, "remote", "add", "origin", "git@github.com:org/repo.git")
	sha := gitSHA(repo)
	if sha == "" {
		t.Fatal("no HEAD sha")
	}

	kbRoot := t.TempDir()
	ledger := loadLedger(kbRoot)
	ledger.record("github.com/org/repo", sha, "alice")
	if err := ledger.save(); err != nil {
		t.Fatal(err)
	}

	entry := &ProjectEntry{Path: repo, LastSHA: "older-sha", LastExtracted: "2026-01-01T00:00:00Z"}
	m := &Manifest{
		Projects: map[string]*ProjectEntry{"repo": entry},
		path:     filepath.Join(kbRoot, "scripts", "projects.json"),
	}
	s := &SyncCmd{}

	got := s.projectsNeedingExtraction(kbRoot, m)
	if len(got) != 0 {
		t.Errorf("project should be skipped via ledger, got %v", got)
	}
	if entry.LastSHA != sha {
		t.Errorf("local marker not synced: LastSHA = %q, want %q", entry.LastSHA, sha)
	}

	// Persisted, so the next run doesn't re-check.
	reloaded, err := loadManifest(kbRoot)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Projects["repo"].LastSHA != sha {
		t.Errorf("synced marker not persisted")
	}
}

// TestExtractionNotSkippedOnLedgerMismatch: the ledger knows the repo
// but at an older revision — extraction must still run.
func TestExtractionNotSkippedOnLedgerMismatch(t *testing.T) {
	repo := initTestGitRepo(t, "Ledger Tester")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")
	gitRun(t, repo, "remote", "add", "origin", "git@github.com:org/repo.git")

	kbRoot := t.TempDir()
	ledger := loadLedger(kbRoot)
	ledger.record("github.com/org/repo", "stale-sha-from-last-week", "alice")
	if err := ledger.save(); err != nil {
		t.Fatal(err)
	}

	m := &Manifest{
		Projects: map[string]*ProjectEntry{"repo": {Path: repo, LastSHA: "older-sha"}},
		path:     filepath.Join(kbRoot, "scripts", "projects.json"),
	}
	s := &SyncCmd{}

	got := s.projectsNeedingExtraction(kbRoot, m)
	if len(got) != 1 || got[0] != "repo" {
		t.Errorf("project should still need extraction, got %v", got)
	}
}
