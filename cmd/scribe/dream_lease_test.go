package main

import (
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
