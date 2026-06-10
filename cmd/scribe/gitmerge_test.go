package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupCloneWithConflict builds origin + two clones and produces a
// pull conflict in every `rels` path: clone A pushes one version, clone
// B commits a different one on the same base. Returns clone B's path,
// ready for pullRebase to hit the conflict.
func setupCloneWithConflict(t *testing.T, rels ...string) string {
	t.Helper()

	origin := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, t.TempDir(), "init", "-q", "--bare", origin)

	seed := initTestGitRepo(t, "Seeder")
	for _, rel := range rels {
		writeKBFile(t, seed, rel, "base content\n")
	}
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-q", "-m", "base")
	gitRun(t, seed, "remote", "add", "origin", origin)
	gitRun(t, seed, "push", "-q", "-u", "origin", "HEAD:main")

	cloneA := filepath.Join(t.TempDir(), "a")
	gitRun(t, t.TempDir(), "clone", "-q", origin, cloneA)
	gitRun(t, cloneA, "config", "user.name", "Alice")
	gitRun(t, cloneA, "config", "user.email", "a@example.com")
	for _, rel := range rels {
		writeKBFile(t, cloneA, rel, "alice version\n")
	}
	gitRun(t, cloneA, "add", ".")
	gitRun(t, cloneA, "commit", "-q", "-m", "alice change")
	gitRun(t, cloneA, "push", "-q", "origin", "HEAD:main")

	// Clone B happens after A pushed, so rewind to the base commit and
	// commit conflicting changes there.
	cloneB := filepath.Join(t.TempDir(), "b")
	gitRun(t, t.TempDir(), "clone", "-q", origin, cloneB)
	gitRun(t, cloneB, "config", "user.name", "Bob")
	gitRun(t, cloneB, "config", "user.email", "b@example.com")
	gitRun(t, cloneB, "reset", "-q", "--hard", "HEAD~1")
	for _, rel := range rels {
		writeKBFile(t, cloneB, rel, "bob version\n")
	}
	gitRun(t, cloneB, "add", ".")
	gitRun(t, cloneB, "commit", "-q", "-m", "bob change")

	return cloneB
}

func TestPullRebaseAutoResolvesDerivedConflict(t *testing.T) {
	clone := setupCloneWithConflict(t, "wiki/_index.md")

	ok, pulled, err := pullRebase(clone)
	if err != nil {
		t.Fatalf("pullRebase should auto-resolve a derived-file conflict, got: %v", err)
	}
	if !ok || !pulled {
		t.Errorf("pullRebase = (ok=%v, pulled=%v), want (true, true)", ok, pulled)
	}
	if rebaseInProgress(clone) {
		t.Error("rebase left in progress")
	}
	data, err := os.ReadFile(filepath.Join(clone, "wiki", "_index.md"))
	if err != nil {
		t.Fatal(err)
	}
	if firstConflictMarkerLine(data) != 0 {
		t.Errorf("conflict markers left in _index.md:\n%s", data)
	}
	if got := conflictedFiles(clone); len(got) != 0 {
		t.Errorf("unmerged files remain: %v", got)
	}
}

func TestPullRebaseAbortsOnArticleConflict(t *testing.T) {
	clone := setupCloneWithConflict(t, "wiki/real-article.md")

	ok, _, err := pullRebase(clone)
	if err == nil {
		t.Fatal("pullRebase should fail on an article conflict")
	}
	if ok {
		t.Error("pullRebase reported ok despite conflict")
	}
	if !strings.Contains(err.Error(), "wiki/real-article.md") {
		t.Errorf("error %q does not name the conflicted file", err)
	}
	// The rebase must be aborted — a cron run must never leave the repo
	// mid-rebase with markers on disk.
	if rebaseInProgress(clone) {
		t.Error("rebase left in progress after abort path")
	}
	data, err := os.ReadFile(filepath.Join(clone, "wiki", "real-article.md"))
	if err != nil {
		t.Fatal(err)
	}
	if firstConflictMarkerLine(data) != 0 {
		t.Errorf("conflict markers left on disk after abort:\n%s", data)
	}
	if string(data) != "bob version\n" {
		t.Errorf("working tree not restored to local commit; got %q", data)
	}
}

func TestPullRebaseMixedConflictAborts(t *testing.T) {
	// Derived + article conflicting together: the article wins — abort,
	// never half-resolve.
	clone := setupCloneWithConflict(t, "wiki/_index.md", "wiki/real-article.md")

	ok, _, err := pullRebase(clone)
	if err == nil || ok {
		t.Fatalf("mixed conflict must fail, got (ok=%v, err=%v)", ok, err)
	}
	if rebaseInProgress(clone) {
		t.Error("rebase left in progress after mixed-conflict abort")
	}
	data, _ := os.ReadFile(filepath.Join(clone, "wiki", "real-article.md"))
	if string(data) != "bob version\n" {
		t.Errorf("working tree not restored; got %q", data)
	}
}

func TestAutoResolveNoopOnCleanRepo(t *testing.T) {
	if resolved, err := autoResolveDerivedConflicts(t.TempDir()); resolved || err != nil {
		t.Errorf("auto-resolve on non-repo = (%v, %v), want (false, nil)", resolved, err)
	}
	repo := initTestGitRepo(t, "Clean")
	if resolved, err := autoResolveDerivedConflicts(repo); resolved || err != nil {
		t.Errorf("auto-resolve on clean repo = (%v, %v), want (false, nil)", resolved, err)
	}
}
