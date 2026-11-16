package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testDriverCommand points merge.scribe.driver at this test binary, which
// TestMain turns into the scribe CLI when SCRIBE_TEST_RUN_CLI=1. That way
// the integration tests run the real `merge-file` code path through git.
func testDriverCommand(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return "SCRIBE_TEST_RUN_CLI=1 " + shellQuote(exe) + " merge-file %O %A %B %P"
}

// stubMergeDriverCommand swaps the driver command ensureMergeDriver writes
// and returns the restore func.
func stubMergeDriverCommand(cmd string) func() {
	prev := mergeDriverCommand
	mergeDriverCommand = func() string { return cmd }
	return func() { mergeDriverCommand = prev }
}

func cloneKB(t *testing.T, origin, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	gitRun(t, t.TempDir(), "clone", "-q", origin, dir)
	gitRun(t, dir, "config", "user.name", name)
	gitRun(t, dir, "config", "user.email", name+"@example.com")
	return dir
}

func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", msg)
}

func readKB(t *testing.T, dir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// TestMergeDriver_TwoClonesConverge reproduces the 2026-11-09 conflicts in
// one rebase: a raw article whose auto block both machines wrote, an
// article whose related: list both machines grew, an article body both
// machines edited on the same line, and a delete/modify. Both clones must
// push and end on the same tree with nothing lost and no markers.
func TestMergeDriver_TwoClonesConverge(t *testing.T) {
	defer stubMergeDriverCommand(testDriverCommand(t))()

	origin := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, t.TempDir(), "init", "-q", "--bare", origin)
	seed := initTestGitRepo(t, "Seeder")
	if _, err := ensureGitattributes(seed); err != nil {
		t.Fatal(err)
	}
	rawBase := "---\ntitle: R\n---\n# R\n\nraw body\n"
	articleBase := "---\ntitle: X\nrelated: [\"[[A]]\"]\n---\n# X\n\nbody\n"
	writeKBFile(t, seed, "raw/articles/r.md", rawBase)
	writeKBFile(t, seed, "decisions/x.md", articleBase)
	writeKBFile(t, seed, "wiki/y.md", "# Y\n\nline one\n")
	writeKBFile(t, seed, "ideas/d.md", "keep me\n")
	commitAll(t, seed, "base")
	gitRun(t, seed, "remote", "add", "origin", origin)
	gitRun(t, seed, "push", "-q", "-u", "origin", "HEAD:main")

	alice := cloneKB(t, origin, "alice")
	bob := cloneKB(t, origin, "bob")

	rawWith := func(p string) string {
		out, err := insertRetrievalContext(rawBase, p)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	writeKBFile(t, alice, "raw/articles/r.md", rawWith("Zulu context."))
	writeKBFile(t, alice, "decisions/x.md", strings.Replace(articleBase, `"[[A]]"]`, `"[[A]]", "[[B]]"]`, 1))
	writeKBFile(t, alice, "wiki/y.md", "# Y\n\nalice line\n")
	if err := os.Remove(filepath.Join(alice, "ideas", "d.md")); err != nil {
		t.Fatal(err)
	}
	commitAll(t, alice, "alice")

	writeKBFile(t, bob, "raw/articles/r.md", rawWith("Alpha context."))
	writeKBFile(t, bob, "decisions/x.md", strings.Replace(articleBase, `"[[A]]"]`, `"[[A]]", "[[C]]"]`, 1))
	writeKBFile(t, bob, "wiki/y.md", "# Y\n\nbob line\n")
	writeKBFile(t, bob, "ideas/d.md", "keep me, edited\n")
	commitAll(t, bob, "bob")

	if err := gitPush(alice); err != nil {
		t.Fatalf("alice push: %v", err)
	}
	if err := gitPush(bob); err != nil {
		t.Fatalf("bob push must rebase through the driver and succeed: %v", err)
	}
	if ok, _, err := pullRebase(alice); !ok || err != nil {
		t.Fatalf("alice pull: ok=%v err=%v", ok, err)
	}

	headA, headB := gitSHA(alice), gitSHA(bob)
	headOrigin := runCmd(origin, "git", "rev-parse", "main")
	if headA != headB || headB != headOrigin {
		t.Fatalf("clones did not converge: alice=%s bob=%s origin=%s", headA, headB, headOrigin)
	}

	if got := readKB(t, bob, "raw/articles/r.md"); got != rawWith("Alpha context.") {
		t.Errorf("raw article:\n%s", got)
	}
	x := readKB(t, bob, "decisions/x.md")
	for _, w := range []string{"[[A]]", "[[B]]", "[[C]]"} {
		if !strings.Contains(x, w) {
			t.Errorf("decisions/x.md lost %s:\n%s", w, x)
		}
	}
	y := readKB(t, bob, "wiki/y.md")
	if !strings.Contains(y, "alice line") || !strings.Contains(y, "bob line") {
		t.Errorf("wiki/y.md lost a side:\n%s", y)
	}
	if got := readKB(t, bob, "ideas/d.md"); got != "keep me, edited\n" {
		t.Errorf("delete/modify must keep the modified side, got %q", got)
	}
	for _, dir := range []string{alice, bob} {
		if hits := findConflictMarkers(dir); len(hits) > 0 {
			t.Errorf("conflict markers in %s: %v", dir, hits)
		}
		if gitIsDirty(dir) {
			t.Errorf("%s left dirty:\n%s", dir, runCmd(dir, "git", "status", "--porcelain"))
		}
	}
}

// TestPullRebase_AutostashConflictOnDerivedFile: an uncommitted local
// edit to a derived file collides with the pulled version when the
// autostash re-applies. No rebase is in progress then, and the resolver
// used to return early and leave the markers for the next commit.
func TestPullRebase_AutostashConflictOnDerivedFile(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, t.TempDir(), "init", "-q", "--bare", origin)
	seed := initTestGitRepo(t, "Seeder")
	writeKBFile(t, seed, "wiki/_index.md", "index v0\n")
	commitAll(t, seed, "base")
	gitRun(t, seed, "remote", "add", "origin", origin)
	gitRun(t, seed, "push", "-q", "-u", "origin", "HEAD:main")

	alice := cloneKB(t, origin, "alice")
	bob := cloneKB(t, origin, "bob")
	writeKBFile(t, alice, "wiki/_index.md", "index alice\n")
	commitAll(t, alice, "alice")
	gitRun(t, alice, "push", "-q", "origin", "HEAD:main")

	writeKBFile(t, bob, "wiki/_index.md", "index bob, uncommitted\n")
	ok, _, err := pullRebase(bob)
	if !ok || err != nil {
		t.Fatalf("pullRebase: ok=%v err=%v", ok, err)
	}
	if got := conflictedFiles(bob); len(got) != 0 {
		t.Errorf("unmerged files remain: %v", got)
	}
	if line := firstConflictMarkerLine([]byte(readKB(t, bob, "wiki/_index.md"))); line != 0 {
		t.Errorf("markers left in _index.md")
	}
	if gitSHA(bob) != gitSHA(alice) {
		t.Errorf("bob did not move to the pulled commit")
	}
}

// TestPullRebase_AbortNamesEveryConflictedFile: when the fallback must
// abort, the error lists every file a human has to merge, not the first.
func TestPullRebase_AbortNamesEveryConflictedFile(t *testing.T) {
	clone := setupCloneWithConflict(t, "README.md", "notes.txt")
	_, _, err := pullRebase(clone)
	if err == nil {
		t.Fatal("unclassified conflicts must abort")
	}
	for _, f := range []string{"README.md", "notes.txt"} {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("error %q does not name %s", err, f)
		}
	}
	if rebaseInProgress(clone) {
		t.Error("rebase left in progress")
	}
}

// TestPullRebase_IgnoresCommitSigning: a global commit.gpgsign=true made
// every replayed commit fail on 2026-11-07. Scribe's pull and rebase
// steps must not sign, even when signing cannot work.
func TestPullRebase_IgnoresCommitSigning(t *testing.T) {
	clone := setupCloneWithConflict(t, "wiki/_index.md")
	gitRun(t, clone, "config", "commit.gpgsign", "true")
	gitRun(t, clone, "config", "gpg.program", "false")
	if ok, _, err := pullRebase(clone); !ok || err != nil {
		t.Fatalf("pullRebase with unusable signing: ok=%v err=%v", ok, err)
	}
	if rebaseInProgress(clone) {
		t.Error("rebase left in progress")
	}
}

// TestPullRebase_LeavesManualMergeAlone: a failed pull with no rebase
// running did not create the conflicts on disk, so the resolver must
// not touch them (a person may be mid-merge).
func TestPullRebase_LeavesManualMergeAlone(t *testing.T) {
	clone := setupCloneWithConflict(t, "wiki/_index.md")
	gitRun(t, clone, "fetch", "-q")
	cmd := exec.Command("git", "merge", "origin/main") //nolint:noctx // test fixture, expected to conflict
	cmd.Dir = clone
	_ = cmd.Run()
	if len(conflictedFiles(clone)) == 0 {
		t.Fatal("fixture: expected a manual merge conflict")
	}
	if ok, _, _ := pullRebase(clone); ok {
		t.Error("pull during an unfinished merge must fail")
	}
	if got := conflictedFiles(clone); len(got) != 1 {
		t.Errorf("resolver touched the manual merge; unmerged = %v", got)
	}
}
