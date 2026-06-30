package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
