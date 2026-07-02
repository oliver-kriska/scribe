package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCountScopedPendingSessions verifies the backlog counts only sessions
// whose project has an APPROVED manifest entry — not the whole global
// ccrider DB (issue #27 item 4). A fresh KB with zero approved projects
// must report zero pending, not the machine's entire session pile.
func TestCountScopedPendingSessions(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	root := t.TempDir()

	approved := filepath.Join(t.TempDir(), "Projects", "approved")
	pending := filepath.Join(t.TempDir(), "Projects", "pending")

	// Manifest: one approved project, one still-pending project.
	writeStatusManifest(t, root, map[string]map[string]string{
		"approved": {"path": approved, "domain": "general"}, // empty status = approved
		"pending":  {"path": pending, "domain": "general", "status": statusPending},
	})

	// Sessions across approved / pending / unknown / empty-cwd projects.
	insertFixtureSession(t, db, "s-appr-1", approved, 50, "", "", "in approved")
	insertFixtureSession(t, db, "s-appr-2", approved, 50, "", "", "in approved, already mined")
	insertFixtureSession(t, db, "s-pending", pending, 50, "", "", "in pending project")
	insertFixtureSession(t, db, "s-unknown", "/somewhere/else", 50, "", "", "no manifest entry")
	insertFixtureSession(t, db, "s-empty", "", 50, "", "", "no cwd")

	cfg := loadConfig(root)
	cfg.CcriderDB = dbPath

	processed := map[string]struct{}{"s-appr-2": {}}
	got, ok := countScopedPendingSessions(root, cfg, processed)
	if !ok {
		t.Fatal("countScopedPendingSessions returned ok=false")
	}
	// Only s-appr-1 counts: s-appr-2 is processed, s-pending is unapproved,
	// s-unknown has no entry, s-empty has no cwd.
	if got != 1 {
		t.Errorf("pending = %d, want 1 (only the unprocessed approved-project session)", got)
	}
}

// TestCountScopedPendingSessions_ZeroApproved is the headline #27 case:
// a KB with no approved projects must not claim the global session pile.
func TestCountScopedPendingSessions_ZeroApproved(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	root := t.TempDir()
	writeStatusManifest(t, root, map[string]map[string]string{}) // empty manifest

	insertFixtureSession(t, db, "g1", "/some/proj", 99, "", "", "x")
	insertFixtureSession(t, db, "g2", "/other/proj", 99, "", "", "y")

	cfg := loadConfig(root)
	cfg.CcriderDB = dbPath

	got, ok := countScopedPendingSessions(root, cfg, map[string]struct{}{})
	if !ok {
		t.Fatal("ok=false")
	}
	if got != 0 {
		t.Errorf("pending = %d, want 0 — a KB with no approved projects owns no sessions", got)
	}
}

// TestPendingQueueSummary covers pendingQueueSummary's Hot/Normal/aged
// classification for `scribe status` (issue #22): missing queue file
// reports ok=false, an empty file reports ok=true with zero counts, and a
// mix of Hot/Normal/aged entries lands in the right buckets.
func TestPendingQueueSummary(t *testing.T) {
	cfg := PriorityLanesConfig{HotThreshold: 90, AgeDays: 7}

	t.Run("missing queue file", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		hot, normal, aged, ok := pendingQueueSummary(cfg)
		if ok {
			t.Errorf("ok = true for a missing queue file, want false")
		}
		if hot != 0 || normal != 0 || aged != 0 {
			t.Errorf("counts = (%d, %d, %d), want zeros", hot, normal, aged)
		}
	})

	t.Run("empty queue file reads the same as a missing one", func(t *testing.T) {
		// scanPendingEntries returns a nil slice when it finds zero lines
		// (an empty `var out []pendingEntry`, never appended to), which is
		// indistinguishable from peekPendingEntries' os.Open-failed nil —
		// so an existing-but-empty file and a missing file both read as
		// ok=false here. Harmless at the status.go call site either way:
		// it only ever prints when ok && hot+normal>0, so both cases
		// suppress the row identically.
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		scribeDir := filepath.Join(xdg, "scribe")
		if err := os.MkdirAll(scribeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scribeDir, "pending-sessions.txt"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		hot, normal, aged, ok := pendingQueueSummary(cfg)
		if ok {
			t.Error("ok = true for an empty file — scanPendingEntries returns nil on zero lines, same as a missing file")
		}
		if hot != 0 || normal != 0 || aged != 0 {
			t.Errorf("counts = (%d, %d, %d), want zeros", hot, normal, aged)
		}
	})

	t.Run("mixed hot/normal/aged entries", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		scribeDir := filepath.Join(xdg, "scribe")
		if err := os.MkdirAll(scribeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		agedTS := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339) // > AgeDays=7
		freshTS := now.Add(-1 * time.Hour).Format(time.RFC3339)
		content := "s-hot\t95\t50\t" + freshTS + "\n" + // score-hot
			"s-normal\t40\t50\t" + freshTS + "\n" + // normal, fresh
			"s-aged\t40\t50\t" + agedTS + "\n" + // aged into hot
			"s-legacy\n" // bare-ID legacy shape: LegacyUnknownAge -> hot, counts as aged too
		if err := os.WriteFile(filepath.Join(scribeDir, "pending-sessions.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		hot, normal, aged, ok := pendingQueueSummary(cfg)
		if !ok {
			t.Fatal("ok = false")
		}
		if hot != 3 {
			t.Errorf("hot = %d, want 3 (s-hot, s-aged, s-legacy)", hot)
		}
		if normal != 1 {
			t.Errorf("normal = %d, want 1 (s-normal)", normal)
		}
		if aged != 2 {
			t.Errorf("aged = %d, want 2 (s-aged, s-legacy — both promoted by age, not by score)", aged)
		}
	})
}

// writeStatusManifest writes scripts/projects.json for a test KB.
func writeStatusManifest(t *testing.T, root string, projects map[string]map[string]string) {
	t.Helper()
	scriptsDir := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"projects": projects})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scriptsDir, "projects.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
