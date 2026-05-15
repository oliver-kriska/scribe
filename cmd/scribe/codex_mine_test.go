package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWalkCodexRollouts_NoDedupAndLookback locks the two ways
// walkCodexRollouts differs from walkCodexSessions: it must (1) yield
// every rollout, NOT one-per-cwd (mining wants each session), and (2)
// honor the lookback window by file mtime.
func TestWalkCodexRollouts_NoDedupAndLookback(t *testing.T) {
	root := t.TempDir()
	sameCwd := "/work/proj-a"
	// Two rollouts, SAME cwd, different days. walkCodexSessions would
	// dedupe these to one; walkCodexRollouts must surface both.
	p1 := writeCodexRollout(t, root, "2026", "05", "10", "aaaa", sameCwd)
	p2 := writeCodexRollout(t, root, "2026", "05", "11", "bbbb", sameCwd)
	// A third, distinct cwd, but stale: mtime 30 days ago.
	pOld := writeCodexRollout(t, root, "2026", "04", "01", "cccc", "/work/proj-b")
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(pOld, old, old); err != nil {
		t.Fatal(err)
	}

	collect := func(sinceHours int) map[string]bool {
		seen := map[string]bool{}
		err := walkCodexRollouts(root, sinceHours, func(path string, _ *codexSessionMeta, _ time.Time) {
			seen[path] = true
		})
		if err != nil {
			t.Fatalf("walkCodexRollouts: %v", err)
		}
		return seen
	}

	t.Run("no cwd dedup, lookback disabled yields all", func(t *testing.T) {
		seen := collect(0)
		for _, p := range []string{p1, p2, pOld} {
			if !seen[p] {
				t.Errorf("missing rollout %s (no-dedup / no-filter expected all 3)", filepath.Base(p))
			}
		}
		if len(seen) != 3 {
			t.Errorf("want 3 rollouts, got %d", len(seen))
		}
	})

	t.Run("lookback window excludes stale rollout", func(t *testing.T) {
		seen := collect(48) // 48h window
		if seen[pOld] {
			t.Error("30-day-old rollout must be excluded by a 48h lookback")
		}
		if !seen[p1] || !seen[p2] {
			t.Error("fresh same-cwd rollouts must both survive the window (no dedup)")
		}
	})
}

func TestCodexProcessedLog_Idempotent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := markCodexProcessed(root, "id1", "/work/a", ""); err != nil {
		t.Fatal(err)
	}
	if err := markCodexProcessed(root, "id2", "/work/b", "below MinScore (1 < 2)"); err != nil {
		t.Fatal(err)
	}
	// Re-mark id1 — must not duplicate, must stay present.
	if err := markCodexProcessed(root, "id1", "/work/a", ""); err != nil {
		t.Fatal(err)
	}
	// Empty id is a silent no-op (not recorded, no error).
	if err := markCodexProcessed(root, "", "/work/c", ""); err != nil {
		t.Fatalf("empty id should be a no-op, got %v", err)
	}

	set := loadProcessedCodexIDs(root)
	if !set["id1"] || !set["id2"] {
		t.Errorf("processed set missing entries: %+v", set)
	}
	if set[""] {
		t.Error("empty id must not be recorded")
	}
	if len(set) != 2 {
		t.Errorf("want exactly 2 processed ids (id1 not duplicated on re-mark), got %d: %+v", len(set), set)
	}
}

func TestApplyCodexDefaults(t *testing.T) {
	t.Run("zero value fills defaults, Mine stays opt-in false", func(t *testing.T) {
		c := CodexConfig{}
		applyCodexDefaults(&c)
		if c.Mine {
			t.Error("Mine must default to false (opt-in)")
		}
		if c.SessionsMax != 3 || c.LookbackHours != 168 || c.MinScore != 2 {
			t.Errorf("defaults wrong: %+v", c)
		}
	})

	t.Run("explicit values are preserved", func(t *testing.T) {
		c := CodexConfig{Mine: true, SessionsMax: 10, LookbackHours: 24, MinScore: 5}
		applyCodexDefaults(&c)
		if !c.Mine || c.SessionsMax != 10 || c.LookbackHours != 24 || c.MinScore != 5 {
			t.Errorf("explicit config was overwritten: %+v", c)
		}
	})
}
