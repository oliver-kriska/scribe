package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestProjectEntryIsApproved(t *testing.T) {
	tests := []struct {
		name  string
		entry *ProjectEntry
		want  bool
	}{
		{"nil entry", nil, false},
		{"legacy empty status", &ProjectEntry{Path: "/p"}, true},
		{"explicit approved", &ProjectEntry{Path: "/p", Status: statusApproved}, true},
		{"pending", &ProjectEntry{Path: "/p", Status: statusPending}, false},
	}
	for _, tt := range tests {
		if got := tt.entry.IsApproved(); got != tt.want {
			t.Errorf("%s: IsApproved() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestPendingProjectsSorted(t *testing.T) {
	m := &Manifest{Projects: map[string]*ProjectEntry{
		"zeta":  {Path: "/z", Status: statusPending},
		"alpha": {Path: "/a", Status: statusPending},
		"mid":   {Path: "/m"}, // approved — excluded
	}}
	got := m.pendingProjects()
	want := []string{"alpha", "zeta"}
	if !slices.Equal(got, want) {
		t.Errorf("pendingProjects() = %v, want %v", got, want)
	}
}

func TestIgnoreProject(t *testing.T) {
	m := &Manifest{
		Projects: map[string]*ProjectEntry{
			"foo": {Path: "/Users/x/Projects/foo", Status: statusPending},
		},
	}
	m.ignoreProject("foo")
	if _, ok := m.Projects["foo"]; ok {
		t.Error("foo still in manifest after ignore")
	}
	if !slices.Contains(m.IgnoredPaths, "/Users/x/Projects/foo") {
		t.Errorf("path not in IgnoredPaths: %v", m.IgnoredPaths)
	}
	// Idempotent — no duplicate path, no panic on missing project.
	m.ignoreProject("foo")
	if n := len(m.IgnoredPaths); n != 1 {
		t.Errorf("IgnoredPaths has %d entries after double ignore, want 1", n)
	}
}

func TestDiscoveryStatus(t *testing.T) {
	if got := discoveryStatus(nil); got != statusPending {
		t.Errorf("nil cfg: got %q, want pending", got)
	}
	cfg := &ScribeConfig{}
	if got := discoveryStatus(cfg); got != statusPending {
		t.Errorf("default cfg: got %q, want pending", got)
	}
	cfg.Sync.AutoApprove = true
	if got := discoveryStatus(cfg); got != "" {
		t.Errorf("auto_approve: got %q, want empty (legacy approved)", got)
	}
}

func TestProjectsNeedingExtractionSkipsPending(t *testing.T) {
	approvedDir := t.TempDir()
	pendingDir := t.TempDir()
	s := &SyncCmd{}
	m := &Manifest{Projects: map[string]*ProjectEntry{
		"approved-proj": {Path: approvedDir},
		"pending-proj":  {Path: pendingDir, Status: statusPending},
	}}
	got := s.projectsNeedingExtraction(m)
	if !slices.Contains(got, "approved-proj") {
		t.Errorf("approved project missing from %v", got)
	}
	if slices.Contains(got, "pending-proj") {
		t.Errorf("pending project should be skipped, got %v", got)
	}

	// Explicit --extract on a pending project must not extract either.
	s.Extract = "pending-proj"
	if got := s.projectsNeedingExtraction(m); len(got) != 0 {
		t.Errorf("--extract on pending project returned %v, want none", got)
	}
}

func TestApproveProjectCreatesRepoYAML(t *testing.T) {
	root := t.TempDir()
	projDir := t.TempDir()
	m := &Manifest{Projects: map[string]*ProjectEntry{
		"myproj": {Path: projDir, Domain: "general", Status: statusPending},
	}}

	if err := approveProject(root, m, "myproj"); err != nil {
		t.Fatal(err)
	}
	if m.Projects["myproj"].Status != statusApproved {
		t.Errorf("Status = %q, want approved", m.Projects["myproj"].Status)
	}
	repoYAML := filepath.Join(root, "projects", filepath.Base(projDir), ".repo.yaml")
	if !fileExists(repoYAML) {
		t.Errorf(".repo.yaml not created at %s", repoYAML)
	}

	// Unknown project errors.
	if err := approveProject(root, m, "nope"); err == nil {
		t.Error("approve of unknown project should error")
	}
	// Re-approve is a no-op, not an error.
	if err := approveProject(root, m, "myproj"); err != nil {
		t.Errorf("re-approve errored: %v", err)
	}
}

func TestManifestStatusRoundTrip(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"projects":{"legacy":{"path":"/p/legacy","domain":"general"},"new":{"path":"/p/new","domain":"general","status":"pending"}},"domain_aliases":{},"ignored_paths":[]}`
	if err := os.WriteFile(filepath.Join(root, "scripts", "projects.json"), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := loadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Projects["legacy"].IsApproved() {
		t.Error("legacy entry without status must read as approved")
	}
	if m.Projects["new"].IsApproved() {
		t.Error("pending entry must not read as approved")
	}

	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "scripts", "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Projects map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, has := decoded.Projects["legacy"]["status"]; has {
		t.Error("legacy entry gained a status field on round-trip (omitempty broken)")
	}
	if got := decoded.Projects["new"]["status"]; got != "pending" {
		t.Errorf("pending status lost on round-trip: %v", got)
	}
}
