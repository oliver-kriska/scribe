package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestKBLockScope_StableAndUnique covers D5/D6: two different KB roots must
// map to different scopes, the same root must always map to the same
// scope, and a trailing-slash spelling of the same root must not change it
// (otherwise the same KB could silently stop contending with itself).
func TestKBLockScope_StableAndUnique(t *testing.T) {
	kbA := filepath.Join(t.TempDir(), "kb-a")
	kbB := filepath.Join(t.TempDir(), "kb-b")

	scopeA1, scopeB := kbLockScope(kbA), kbLockScope(kbB)
	if scopeA1 == scopeB {
		t.Errorf("different KB roots produced the same lock scope: %q", scopeA1)
	}
	scopeA2 := kbLockScope(kbA)
	if scopeA1 != scopeA2 {
		t.Errorf("kbLockScope is not stable across repeated calls for the same root: %q vs %q", scopeA1, scopeA2)
	}
	if got, want := kbLockScope(kbA+string(filepath.Separator)), kbLockScope(kbA); got != want {
		t.Errorf("trailing separator changed the scope: got %q, want %q", got, want)
	}
}

// TestLockPathFor_DifferentKBsGetDifferentPaths is the core gap-2 fix: two
// KBs sharing the same lock_dir (the "/tmp" default every fresh KB is
// scaffolded with) must resolve to different lock file paths.
func TestLockPathFor_DifferentKBsGetDifferentPaths(t *testing.T) {
	shared := t.TempDir()
	kbA := filepath.Join(t.TempDir(), "kb-a")
	kbB := filepath.Join(t.TempDir(), "kb-b")

	pathA := lockPathFor(shared, "sync", kbA)
	pathB := lockPathFor(shared, "sync", kbB)

	if pathA == pathB {
		t.Fatalf("different KBs produced the same lock path: %q", pathA)
	}
	for _, p := range []string{pathA, pathB} {
		if !strings.HasPrefix(filepath.Base(p), "scribe-sync-") {
			t.Errorf("lock path %q missing the scribe-sync- prefix", p)
		}
	}
}

// TestAcquireLock_DifferentKBsDoNotContend is the behavioral proof of the
// fix: before per-KB scoping, a manual invocation on one KB would silently
// no-op because it raced a scheduled run's lock on a completely unrelated
// KB. After the fix, holding kbA's lock must not block kbB's.
func TestAcquireLock_DifferentKBsDoNotContend(t *testing.T) {
	shared := t.TempDir()
	kbA := filepath.Join(t.TempDir(), "kb-a")
	kbB := filepath.Join(t.TempDir(), "kb-b")

	lf, ok, err := acquireLock(lockPathFor(shared, "sync", kbA))
	if err != nil {
		t.Fatalf("acquire kbA lock: %v", err)
	}
	if !ok {
		t.Fatal("failed to acquire kbA's lock the first time")
	}
	defer releaseLock(lf)

	lf2, ok2, err2 := acquireLock(lockPathFor(shared, "sync", kbB))
	if err2 != nil {
		t.Fatalf("acquire kbB lock: %v", err2)
	}
	if !ok2 {
		t.Fatal("kbB's lock acquisition was blocked by kbA's held lock — cross-KB contention regressed")
	}
	releaseLock(lf2)
}

// TestAcquireLock_SameKBStillContends is the regression guard for D6: the
// fix must not accidentally let a KB race itself — two acquisitions for
// the exact same root must still serialize.
func TestAcquireLock_SameKBStillContends(t *testing.T) {
	shared := t.TempDir()
	kb := filepath.Join(t.TempDir(), "kb")

	lf, ok, err := acquireLock(lockPathFor(shared, "sync", kb))
	if err != nil {
		t.Fatalf("acquire kb lock: %v", err)
	}
	if !ok {
		t.Fatal("failed to acquire the lock the first time")
	}
	defer releaseLock(lf)

	_, ok2, err2 := acquireLock(lockPathFor(shared, "sync", kb))
	if err2 != nil {
		t.Fatalf("second acquire attempt errored: %v", err2)
	}
	if ok2 {
		t.Error("same KB acquired its own lock twice concurrently — the fix broke same-KB contention")
	}
}
