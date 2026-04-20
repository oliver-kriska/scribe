package main

import "testing"

func TestPullBeforeSyncEnabled_DefaultsTrue(t *testing.T) {
	if !pullBeforeSyncEnabled(nil) {
		t.Fatalf("nil cfg should default to enabled")
	}
	cfg := &ScribeConfig{}
	if !pullBeforeSyncEnabled(cfg) {
		t.Fatalf("unset pointer should default to enabled")
	}
}

func TestPullBeforeSyncEnabled_ExplicitFalse(t *testing.T) {
	f := false
	cfg := &ScribeConfig{Sync: SyncConfig{AlwaysPullBeforeSync: &f}}
	if pullBeforeSyncEnabled(cfg) {
		t.Fatalf("explicit false should disable")
	}
}

func TestPullRebase_NonRepoIsNoOp(t *testing.T) {
	ok, pulled, err := pullRebase(t.TempDir())
	if err != nil {
		t.Fatalf("non-repo should not error: %v", err)
	}
	if ok || pulled {
		t.Fatalf("non-repo should return ok=false, pulled=false")
	}
}

func TestCommitDebounced_DisabledWhenZero(t *testing.T) {
	cfg := &ScribeConfig{Sync: SyncConfig{CommitDebounceMinutes: 0}}
	debounced, _, _ := commitDebounced(t.TempDir(), cfg)
	if debounced {
		t.Fatalf("expected no debounce when CommitDebounceMinutes=0")
	}
}

func TestCommitDebounced_NoRepoTreatedAsOld(t *testing.T) {
	// A directory without a git HEAD returns a very large age so callers
	// proceed to commit on first run of a fresh KB.
	cfg := &ScribeConfig{Sync: SyncConfig{CommitDebounceMinutes: 30}}
	debounced, _, _ := commitDebounced(t.TempDir(), cfg)
	if debounced {
		t.Fatalf("expected non-repo path to fall through to commit, not debounce")
	}
}
