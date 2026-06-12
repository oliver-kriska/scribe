package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestPullRebaseAutoResolvesAllDerivedFiles automates PR #1's manual
// verification #4 across the FULL derived set: two machines touching
// _index.md, _backlinks.json, and _digest.md concurrently must pull
// clean — every derived file resolves without manual intervention in a
// single rebase, not just the index.
func TestPullRebaseAutoResolvesAllDerivedFiles(t *testing.T) {
	derived := []string{"wiki/_index.md", "wiki/_backlinks.json", "wiki/_digest.md"}
	clone := setupCloneWithConflict(t, derived...)

	ok, pulled, err := pullRebase(clone)
	if err != nil {
		t.Fatalf("pullRebase must auto-resolve all derived files, got: %v", err)
	}
	if !ok || !pulled {
		t.Errorf("pullRebase = (ok=%v, pulled=%v), want (true, true)", ok, pulled)
	}
	if rebaseInProgress(clone) {
		t.Error("rebase left in progress")
	}
	if got := conflictedFiles(clone); len(got) != 0 {
		t.Errorf("unmerged files remain: %v", got)
	}
	for _, rel := range derived {
		data, err := os.ReadFile(filepath.Join(clone, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s missing after auto-resolve: %v", rel, err)
			continue
		}
		if firstConflictMarkerLine(data) != 0 {
			t.Errorf("conflict markers left in %s:\n%s", rel, data)
		}
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

// setupCloneContentConflict is setupCloneWithConflict with explicit
// per-side content for a single path — needed for the semantic-merge
// tests where the merge result depends on what each side wrote.
func setupCloneContentConflict(t *testing.T, rel, base, alice, bob string) string {
	t.Helper()

	origin := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, t.TempDir(), "init", "-q", "--bare", origin)

	seed := initTestGitRepo(t, "Seeder")
	writeKBFile(t, seed, rel, base)
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-q", "-m", "base")
	gitRun(t, seed, "remote", "add", "origin", origin)
	gitRun(t, seed, "push", "-q", "-u", "origin", "HEAD:main")

	cloneA := filepath.Join(t.TempDir(), "a")
	gitRun(t, t.TempDir(), "clone", "-q", origin, cloneA)
	gitRun(t, cloneA, "config", "user.name", "Alice")
	gitRun(t, cloneA, "config", "user.email", "a@example.com")
	writeKBFile(t, cloneA, rel, alice)
	gitRun(t, cloneA, "add", ".")
	gitRun(t, cloneA, "commit", "-q", "-m", "alice change")
	gitRun(t, cloneA, "push", "-q", "origin", "HEAD:main")

	cloneB := filepath.Join(t.TempDir(), "b")
	gitRun(t, t.TempDir(), "clone", "-q", origin, cloneB)
	gitRun(t, cloneB, "config", "user.name", "Bob")
	gitRun(t, cloneB, "config", "user.email", "b@example.com")
	gitRun(t, cloneB, "reset", "-q", "--hard", "HEAD~1")
	writeKBFile(t, cloneB, rel, bob)
	gitRun(t, cloneB, "add", ".")
	gitRun(t, cloneB, "commit", "-q", "-m", "bob change")

	return cloneB
}

// TestPullRebaseMergesLedgerConflict: the normal team case — two
// members extract different repos in the same cron slot. The pull must
// merge, not wedge, and keep BOTH entries.
func TestPullRebaseMergesLedgerConflict(t *testing.T) {
	clone := setupCloneContentConflict(t, "scripts/extraction-ledger.json",
		`{"repos":{}}`+"\n",
		`{"repos":{"github.com/org/alpha":{"sha":"aaa","extracted_at":"2026-06-10T10:00:00Z","by":"alice"}}}`+"\n",
		`{"repos":{"github.com/org/beta":{"sha":"bbb","extracted_at":"2026-06-10T11:00:00Z","by":"bob"}}}`+"\n")

	ok, _, err := pullRebase(clone)
	if err != nil {
		t.Fatalf("ledger conflict must merge, got: %v", err)
	}
	if !ok || rebaseInProgress(clone) {
		t.Fatalf("pull did not complete cleanly (ok=%v)", ok)
	}
	led := loadLedger(clone)
	if _, found := led.lookup("github.com/org/alpha"); !found {
		t.Error("remote side's ledger entry lost in merge")
	}
	if e, found := led.lookup("github.com/org/beta"); !found || e.SHA != "bbb" {
		t.Errorf("local side's ledger entry lost in merge: %+v", e)
	}
}

// TestPullRebaseLedgerSameKeyNewestWins: both sides re-extracted the
// SAME repo — the newer extraction wins deterministically on every
// machine, so the merged file converges.
func TestPullRebaseLedgerSameKeyNewestWins(t *testing.T) {
	clone := setupCloneContentConflict(t, "scripts/extraction-ledger.json",
		`{"repos":{}}`+"\n",
		`{"repos":{"github.com/org/x":{"sha":"older","extracted_at":"2026-06-10T10:00:00Z","by":"alice"}}}`+"\n",
		`{"repos":{"github.com/org/x":{"sha":"newer","extracted_at":"2026-06-10T11:30:00Z","by":"bob"}}}`+"\n")

	if _, _, err := pullRebase(clone); err != nil {
		t.Fatalf("same-key ledger conflict must merge, got: %v", err)
	}
	e, found := loadLedger(clone).lookup("github.com/org/x")
	if !found || e.SHA != "newer" || e.By != "bob" {
		t.Errorf("newest entry did not win: %+v", e)
	}
}

// TestPullRebaseLeaseRemoteWins: a lease claim race resolves in the
// remote's favor (first push wins); the loser's local file then shows
// the winner so the acquire re-check backs off. The loser's replayed
// commit becomes empty, which exercises the rebase --skip path.
func TestPullRebaseLeaseRemoteWins(t *testing.T) {
	now := time.Now()
	leaseJSON := func(host string) string {
		return `{"host":"` + host + `","acquired_at":"` + now.UTC().Format(time.RFC3339) +
			`","expires_at":"` + now.Add(time.Hour).UTC().Format(time.RFC3339) + `"}` + "\n"
	}
	clone := setupCloneContentConflict(t, "scripts/dream-lease.json",
		`{"host":"nobody","acquired_at":"2026-01-01T00:00:00Z","expires_at":"2026-01-01T00:00:00Z"}`+"\n",
		leaseJSON("alice-mb"), leaseJSON("bob-mb"))

	if _, _, err := pullRebase(clone); err != nil {
		t.Fatalf("lease conflict must merge, got: %v", err)
	}
	if rebaseInProgress(clone) {
		t.Fatal("rebase left in progress")
	}
	lease := readDreamLease(clone)
	if lease == nil || lease.Host != "alice-mb" {
		t.Errorf("remote claim did not win the race: %+v", lease)
	}
}

// TestPullRebaseLogUnion: log.md is append-only from every machine —
// the merge keeps both tails.
func TestPullRebaseLogUnion(t *testing.T) {
	clone := setupCloneContentConflict(t, "log.md",
		"## log\n",
		"## log\nalice did a sync\n",
		"## log\nbob did a dream\n")

	if _, _, err := pullRebase(clone); err != nil {
		t.Fatalf("log conflict must merge, got: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(clone, "log.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alice did a sync", "bob did a dream"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("merged log lost %q:\n%s", want, data)
		}
	}
	if firstConflictMarkerLine(data) != 0 {
		t.Errorf("conflict markers in merged log:\n%s", data)
	}
}

func TestMergeUnionLines(t *testing.T) {
	got := string(mergeUnionLines([]byte("a\nb\n"), []byte("a\nc\n")))
	if got != "a\nb\nc\n" {
		t.Errorf("union = %q, want a/b/c", got)
	}
	if got := string(mergeUnionLines(nil, []byte("x\n"))); got != "x\n" {
		t.Errorf("nil ours = %q", got)
	}
	if got := string(mergeUnionLines([]byte("x\n"), nil)); got != "x\n" {
		t.Errorf("nil theirs = %q", got)
	}
	// Identical sides stay identical (no duplicated tail).
	if got := string(mergeUnionLines([]byte("a\nb\n"), []byte("a\nb\n"))); got != "a\nb\n" {
		t.Errorf("identical sides = %q", got)
	}
}

func TestLedgerEntryNewer(t *testing.T) {
	older := ledgerEntry{ExtractedAt: "2026-06-10T10:00:00Z"}
	newer := ledgerEntry{ExtractedAt: "2026-06-10T11:00:00Z"}
	if !ledgerEntryNewer(newer, older) || ledgerEntryNewer(older, newer) {
		t.Error("RFC3339 comparison wrong")
	}
	// Unparseable timestamps fall back to string compare, never panic.
	if ledgerEntryNewer(ledgerEntry{ExtractedAt: "bad"}, ledgerEntry{ExtractedAt: "worse"}) {
		t.Error("string fallback wrong")
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
