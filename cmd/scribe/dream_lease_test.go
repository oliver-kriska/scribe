package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDreamLeaseRoundTrip(t *testing.T) {
	root := t.TempDir()

	if l := readDreamLease(root); l != nil {
		t.Fatalf("missing lease should read nil, got %+v", l)
	}

	now := time.Now()
	lease := &dreamLease{
		Host:       "machine-a",
		By:         "Alice",
		AcquiredAt: now.UTC().Format(time.RFC3339),
		ExpiresAt:  now.Add(time.Hour).UTC().Format(time.RFC3339),
	}
	if err := writeDreamLease(root, lease); err != nil {
		t.Fatal(err)
	}
	got := readDreamLease(root)
	if got == nil || got.Host != "machine-a" || got.By != "Alice" {
		t.Errorf("round trip = %+v", got)
	}
}

func TestDreamLeaseActiveAt(t *testing.T) {
	now := time.Now()
	var nilLease *dreamLease
	if nilLease.activeAt(now) {
		t.Error("nil lease must be inactive")
	}
	active := &dreamLease{Host: "x", ExpiresAt: now.Add(time.Hour).UTC().Format(time.RFC3339)}
	if !active.activeAt(now) {
		t.Error("future expiry must be active")
	}
	expired := &dreamLease{Host: "x", ExpiresAt: now.Add(-time.Minute).UTC().Format(time.RFC3339)}
	if expired.activeAt(now) {
		t.Error("past expiry must be inactive")
	}
	corrupt := &dreamLease{Host: "x", ExpiresAt: "not-a-time"}
	if corrupt.activeAt(now) {
		t.Error("unparseable expiry must be inactive")
	}
}

func TestDreamLeaseOwnedBy(t *testing.T) {
	l := &dreamLease{Host: "MacBook-Pro", By: "alice"}
	if !l.ownedBy("MacBook-Pro", "alice") {
		t.Error("own lease not recognized")
	}
	// Default hostnames collide — the contributor must disambiguate.
	if l.ownedBy("MacBook-Pro", "bob") {
		t.Error("same hostname, different contributor treated as owned")
	}
	if l.ownedBy("other-host", "alice") {
		t.Error("different host treated as owned")
	}
	// Pre-By leases (or unknown local contributor) compare by host only.
	legacy := &dreamLease{Host: "MacBook-Pro"}
	if !legacy.ownedBy("MacBook-Pro", "bob") {
		t.Error("legacy lease without By must fall back to host compare")
	}
	var nilLease *dreamLease
	if nilLease.ownedBy("x", "y") {
		t.Error("nil lease owned by nobody")
	}
}

func TestAcquireDreamLease(t *testing.T) {
	root := initTestGitRepo(t, "Lease Tester")
	now := time.Now()

	// Fresh repo, no lease — claim succeeds and is committed.
	acquired, holder := acquireDreamLease(root, now)
	if !acquired || holder != "" {
		t.Fatalf("first acquire = (%v, %q), want (true, \"\")", acquired, holder)
	}
	lease := readDreamLease(root)
	if lease == nil || lease.Host != hostnameShort() {
		t.Fatalf("claim not written: %+v", lease)
	}
	if !lease.activeAt(now) {
		t.Error("fresh claim should be active")
	}
	if out := runCmd(root, "git", "log", "--oneline", "--", "scripts/dream-lease.json"); !strings.Contains(out, "lease claim") {
		t.Errorf("lease claim not committed; log:\n%s", out)
	}

	// Re-entrant: same host re-acquires its own active lease.
	if acquired, _ := acquireDreamLease(root, now); !acquired {
		t.Error("same host should re-acquire its own lease")
	}

	// Active lease by ANOTHER host blocks.
	other := &dreamLease{
		Host:       "other-machine",
		AcquiredAt: now.UTC().Format(time.RFC3339),
		ExpiresAt:  now.Add(time.Hour).UTC().Format(time.RFC3339),
	}
	if err := writeDreamLease(root, other); err != nil {
		t.Fatal(err)
	}
	acquired, holder = acquireDreamLease(root, now)
	if acquired || holder != "other-machine" {
		t.Errorf("acquire vs active foreign lease = (%v, %q), want (false, other-machine)", acquired, holder)
	}

	// Expired lease by another host is stealable.
	other.ExpiresAt = now.Add(-time.Minute).UTC().Format(time.RFC3339)
	if err := writeDreamLease(root, other); err != nil {
		t.Fatal(err)
	}
	if acquired, _ := acquireDreamLease(root, now); !acquired {
		t.Error("expired foreign lease should be stealable")
	}
	if got := readDreamLease(root); got == nil || got.Host != hostnameShort() {
		t.Errorf("steal did not rewrite the lease: %+v", got)
	}
}

// setupLeaseTeam simulates PR #1's two-machine setup: a bare origin and
// two clones with distinct git identities. Both tests run on one host,
// so the clones share hostnameShort() — exactly the "every fresh Mac is
// MacBook-Pro" collision the lease's By field exists to disambiguate;
// the contributor (repo-local user.name) is what tells them apart.
func setupLeaseTeam(t *testing.T) (cloneA, cloneB, origin string) {
	t.Helper()
	origin = filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, t.TempDir(), "init", "-q", "--bare", origin)

	seed := initTestGitRepo(t, "Seeder")
	writeKBFile(t, seed, "scribe.yaml", "owner_name: t\nteam: true\n")
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-q", "-m", "base")
	gitRun(t, seed, "remote", "add", "origin", origin)
	gitRun(t, seed, "push", "-q", "-u", "origin", "HEAD:main")

	cloneA = filepath.Join(t.TempDir(), "machine-a")
	gitRun(t, t.TempDir(), "clone", "-q", origin, cloneA)
	gitRun(t, cloneA, "config", "user.name", "Alice")
	gitRun(t, cloneA, "config", "user.email", "a@example.com")

	cloneB = filepath.Join(t.TempDir(), "machine-b")
	gitRun(t, t.TempDir(), "clone", "-q", origin, cloneB)
	gitRun(t, cloneB, "config", "user.name", "Bob")
	gitRun(t, cloneB, "config", "user.email", "b@example.com")
	return cloneA, cloneB, origin
}

// TestDreamLeaseTwoMachineCoordination automates PR #1's manual
// verification #3: machine A acquires the weekly dream lease and the
// claim propagates through origin; machine B's run pulls, sees the
// active lease, reports the holder, and skips without writing a claim;
// once the lease window expires, B steals it — and A then backs off.
func TestDreamLeaseTwoMachineCoordination(t *testing.T) {
	cloneA, cloneB, origin := setupLeaseTeam(t)
	now := time.Now()

	// Machine A claims the cycle; the claim is committed and pushed.
	acquired, holder := acquireDreamLease(cloneA, now)
	if !acquired || holder != "" {
		t.Fatalf("machine A acquire = (%v, %q), want (true, \"\")", acquired, holder)
	}
	originLease := runCmd(origin, "git", "show", "HEAD:scripts/dream-lease.json")
	if !strings.Contains(originLease, `"by": "Alice"`) {
		t.Fatalf("machine A's claim did not reach origin:\n%s", originLease)
	}

	// Machine B pulls, sees A's active lease, reports the holder, skips.
	acquired, holder = acquireDreamLease(cloneB, now.Add(time.Minute))
	if acquired {
		t.Fatal("machine B must skip while machine A holds the lease")
	}
	lease := readDreamLease(cloneB)
	if lease == nil || lease.By != "Alice" {
		t.Fatalf("machine B did not pull A's lease: %+v", lease)
	}
	if holder != lease.Host {
		t.Errorf("reported holder %q does not name the lease holder %q", holder, lease.Host)
	}
	// B must not have overwritten the claim on origin.
	if cur := runCmd(origin, "git", "show", "HEAD:scripts/dream-lease.json"); !strings.Contains(cur, `"by": "Alice"`) {
		t.Errorf("machine B's skip altered the origin lease:\n%s", cur)
	}

	// Expired lease is stealable: B retries after the lease window.
	later := now.Add((dreamLeaseHours + 1) * time.Hour)
	acquired, _ = acquireDreamLease(cloneB, later)
	if !acquired {
		t.Fatal("machine B must steal an expired lease")
	}
	if cur := runCmd(origin, "git", "show", "HEAD:scripts/dream-lease.json"); !strings.Contains(cur, `"by": "Bob"`) {
		t.Errorf("machine B's steal did not reach origin:\n%s", cur)
	}

	// The steal is visible back on machine A: its next run backs off.
	acquired, holder = acquireDreamLease(cloneA, later.Add(time.Minute))
	if acquired {
		t.Error("machine A must back off after B's steal")
	}
	if got := readDreamLease(cloneA); got == nil || got.By != "Bob" {
		t.Errorf("machine A did not pull B's lease: %+v", got)
	}
	if holder == "" {
		t.Error("skip must report which machine holds the lease")
	}
	if rebaseInProgress(cloneA) || rebaseInProgress(cloneB) {
		t.Error("lease coordination left a repo mid-rebase")
	}
}

func TestReleaseDreamLease(t *testing.T) {
	root := initTestGitRepo(t, "Lease Tester")
	now := time.Now()

	if acquired, _ := acquireDreamLease(root, now); !acquired {
		t.Fatal("setup acquire failed")
	}
	releaseDreamLease(root)
	if lease := readDreamLease(root); lease.activeAt(time.Now().Add(time.Second)) {
		t.Errorf("released lease still active: %+v", lease)
	}

	// Releasing someone else's lease is a no-op.
	other := &dreamLease{
		Host:      "other-machine",
		ExpiresAt: now.Add(time.Hour).UTC().Format(time.RFC3339),
	}
	if err := writeDreamLease(root, other); err != nil {
		t.Fatal(err)
	}
	releaseDreamLease(root)
	if got := readDreamLease(root); got == nil || !got.activeAt(now) {
		t.Errorf("foreign lease was touched by release: %+v", got)
	}
}
