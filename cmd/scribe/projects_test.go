package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	// pendingProjects returns manifest keys (canonical paths), sorted by
	// each entry's Name — not by the key itself.
	m := &Manifest{Projects: map[string]*ProjectEntry{
		"/z": {Path: "/z", Name: "zeta", Status: statusPending},
		"/a": {Path: "/a", Name: "alpha", Status: statusPending},
		"/m": {Path: "/m", Name: "mid"}, // approved — excluded
	}}
	got := m.pendingProjects()
	want := []string{"/a", "/z"}
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
		approvedDir: {Path: approvedDir, Name: "approved-proj"},
		pendingDir:  {Path: pendingDir, Name: "pending-proj", Status: statusPending},
	}}
	got := s.projectsNeedingExtraction(t.TempDir(), m)
	if !slices.Contains(got, approvedDir) {
		t.Errorf("approved project missing from %v", got)
	}
	if slices.Contains(got, pendingDir) {
		t.Errorf("pending project should be skipped, got %v", got)
	}

	// Explicit --extract on a pending project (by Name) must not extract
	// either.
	s.Extract = "pending-proj"
	if got := s.projectsNeedingExtraction(t.TempDir(), m); len(got) != 0 {
		t.Errorf("--extract on pending project returned %v, want none", got)
	}
}

func TestApproveProjectCreatesRepoYAML(t *testing.T) {
	root := t.TempDir()
	projDir := t.TempDir()
	m := &Manifest{Projects: map[string]*ProjectEntry{
		"myproj": {Path: projDir, Name: "myproj", Domain: "general", Status: statusPending},
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

func TestLoadManifestFreshSharedClone(t *testing.T) {
	// A shared-KB clone gitignores scripts/projects.json: the scribe.yaml
	// marker alone must yield an empty manifest, and the first save must
	// create scripts/ itself.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("kb_name: team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadManifest(root)
	if err != nil {
		t.Fatalf("loadManifest on fresh clone: %v", err)
	}
	if len(m.Projects) != 0 {
		t.Errorf("expected empty manifest, got %d projects", len(m.Projects))
	}
	m.Projects["p"] = &ProjectEntry{Path: "/p", Status: statusPending}
	if err := m.save(); err != nil {
		t.Fatalf("save on fresh clone: %v", err)
	}
	if !fileExists(filepath.Join(root, "scripts", "projects.json")) {
		t.Error("save did not create scripts/projects.json")
	}

	// Without the scribe.yaml marker a missing manifest is still an error
	// (bad -C / SCRIBE_KB must fail loudly).
	if _, err := loadManifest(t.TempDir()); err == nil {
		t.Error("loadManifest on non-KB dir should error")
	}
}

func TestDoctorWarnsOnPendingProjects(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"projects":{"waiting":{"path":"/p/waiting","domain":"general","status":"pending"}},"domain_aliases":{},"ignored_paths":[]}`
	if err := os.WriteFile(filepath.Join(root, "scripts", "projects.json"), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	checks := checkState(root, loadConfig(root))
	found := false
	for _, ck := range checks {
		if ck.Name == "pending-projects" {
			found = true
			if ck.Status != statusWarn {
				t.Errorf("pending-projects status = %s, want warn", ck.Status)
			}
			if !strings.Contains(ck.Detail, "waiting") || ck.Fix == "" {
				t.Errorf("pending-projects check missing detail/fix: %+v", ck)
			}
		}
	}
	if !found {
		t.Error("doctor checkState did not surface pending projects")
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
	// Legacy fixture is basename-keyed; migration re-keys by canonical
	// path and inherits the old key as Name.
	legacyKey := canonicalizePath("/p/legacy")
	newKey := canonicalizePath("/p/new")
	if !m.Projects[legacyKey].IsApproved() {
		t.Error("legacy entry without status must read as approved")
	}
	if m.Projects[newKey].IsApproved() {
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
	if _, has := decoded.Projects[legacyKey]["status"]; has {
		t.Error("legacy entry gained a status field on round-trip (omitempty broken)")
	}
	if got := decoded.Projects[newKey]["status"]; got != "pending" {
		t.Errorf("pending status lost on round-trip: %v", got)
	}
}

// TestProjectsApprove_ByNameAndByPath: `scribe projects approve` accepts
// either a project's short display Name or its full filesystem path — both
// route through manifest.resolve to the same entry.
func TestProjectsApprove_ByNameAndByPath(t *testing.T) {
	byName := func(t *testing.T) (root, projDir string) {
		t.Helper()
		root = t.TempDir()
		projDir = t.TempDir()
		m := &Manifest{
			Projects: map[string]*ProjectEntry{
				canonicalizePath(projDir): {Path: canonicalizePath(projDir), Name: "myproj", Domain: "general", Status: statusPending},
			},
			path: filepath.Join(root, "scripts", "projects.json"),
		}
		if err := m.save(); err != nil {
			t.Fatal(err)
		}
		return root, projDir
	}

	t.Run("by name", func(t *testing.T) {
		root, _ := byName(t)
		if err := (&ProjectsApproveCmd{Names: []string{"myproj"}}).run(root); err != nil {
			t.Fatalf("approve by name: %v", err)
		}
		m, err := loadManifest(root)
		if err != nil {
			t.Fatal(err)
		}
		e, ok := entryByName(m, "myproj")
		if !ok || !e.IsApproved() {
			t.Errorf("entry not approved after approve-by-name: %+v", e)
		}
	})

	t.Run("by path", func(t *testing.T) {
		root, projDir := byName(t)
		if err := (&ProjectsApproveCmd{Names: []string{projDir}}).run(root); err != nil {
			t.Fatalf("approve by path: %v", err)
		}
		m, err := loadManifest(root)
		if err != nil {
			t.Fatal(err)
		}
		e, ok := entryByName(m, "myproj")
		if !ok || !e.IsApproved() {
			t.Errorf("entry not approved after approve-by-path: %+v", e)
		}
	})
}

// TestProjectsList_SortsByName drives the real ProjectsListCmd.Run(): list
// output must follow each entry's Name, not its canonical-path map key —
// a key that sorts differently than its Name (a deep nested path vs. a
// short alphabetically-later Name) must still print in Name order.
func TestProjectsList_SortsByName(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("default_model: sonnet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manifest{
		Projects: map[string]*ProjectEntry{
			"/zzzz/deep/nested/path": {Path: "/zzzz/deep/nested/path", Name: "alpha", Domain: "general"},
			"/a":                     {Path: "/a", Name: "zulu", Domain: "general"},
		},
		path: filepath.Join(root, "scripts", "projects.json"),
	}
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCRIBE_KB", root)

	out := captureStdout(t, func() error { return (&ProjectsListCmd{}).Run() })
	alphaIdx := strings.Index(out, "alpha")
	zuluIdx := strings.Index(out, "zulu")
	if alphaIdx < 0 || zuluIdx < 0 {
		t.Fatalf("list output missing an entry:\n%s", out)
	}
	if alphaIdx > zuluIdx {
		t.Errorf("list output not sorted by Name (alpha should precede zulu):\n%s", out)
	}
}
