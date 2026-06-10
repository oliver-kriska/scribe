package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRun runs a git command in dir, failing the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initRepoWithWorktree builds a real git repo with one commit and one
// linked worktree, returning (mainRoot, worktreeRoot). Both paths have
// symlinks resolved so they compare cleanly against git rev-parse output
// (macOS TempDir lives under /var → /private/var). The repo is nested
// two levels below TempDir because manifest.isIgnored rejects paths
// shallower than 4 segments — Linux TempDir is /tmp/TestX/001, which
// the depth floor would silently drop from discovery.
func initRepoWithWorktree(t *testing.T) (string, string) {
	t.Helper()
	main := filepath.Join(t.TempDir(), "projects", "proj")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, main, "init", "-q")
	gitRun(t, main, "config", "user.name", "Worktree Tester")
	gitRun(t, main, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(main, "README.md"), []byte("# proj\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, main, "add", ".")
	gitRun(t, main, "commit", "-q", "-m", "init")

	wt := filepath.Join(t.TempDir(), "wt")
	gitRun(t, main, "worktree", "add", "-q", "-b", "test-branch", wt)

	mainResolved, err := filepath.EvalSymlinks(main)
	if err != nil {
		t.Fatal(err)
	}
	wtResolved, err := filepath.EvalSymlinks(wt)
	if err != nil {
		t.Fatal(err)
	}
	return mainResolved, wtResolved
}

func TestWorktreeMainRoot(t *testing.T) {
	main, wt := initRepoWithWorktree(t)

	if got := worktreeMainRoot(main); got != "" {
		t.Errorf("main checkout misdetected as worktree of %q", got)
	}

	got := worktreeMainRoot(wt)
	if got == "" {
		t.Fatal("linked worktree not detected")
	}
	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatal(err)
	}
	if gotResolved != main {
		t.Errorf("main root = %q, want %q", gotResolved, main)
	}

	if got := worktreeMainRoot(t.TempDir()); got != "" {
		t.Errorf("non-git dir misdetected as worktree of %q", got)
	}
}

func TestRecordWorktree(t *testing.T) {
	e := &ProjectEntry{Path: "/repo/main"}

	if !e.recordWorktree("/repo/wt1") {
		t.Error("first record should report change")
	}
	if e.recordWorktree("/repo/wt1") {
		t.Error("duplicate record should be a no-op")
	}
	if e.recordWorktree("/repo/main") {
		t.Error("recording the main path should be a no-op")
	}
	if e.recordWorktree("") {
		t.Error("recording empty path should be a no-op")
	}
	if !e.recordWorktree("/repo/wt2") {
		t.Error("second distinct worktree should report change")
	}
	if len(e.Worktrees) != 2 {
		t.Errorf("worktrees = %v, want 2 entries", e.Worktrees)
	}

	var nilEntry *ProjectEntry
	if nilEntry.recordWorktree("/x") {
		t.Error("nil entry should be a no-op")
	}
}

func TestCollectionPaths(t *testing.T) {
	e := &ProjectEntry{Path: "/repo/main", Worktrees: []string{"/repo/wt1", "/repo/wt2"}}
	got := e.collectionPaths()
	want := []string{"/repo/main", "/repo/wt1", "/repo/wt2"}
	if len(got) != len(want) {
		t.Fatalf("collectionPaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("collectionPaths[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	bare := &ProjectEntry{Path: "/repo/main"}
	if got := bare.collectionPaths(); len(got) != 1 || got[0] != "/repo/main" {
		t.Errorf("collectionPaths without worktrees = %v", got)
	}

	var nilEntry *ProjectEntry
	if got := nilEntry.collectionPaths(); got != nil {
		t.Errorf("nil entry collectionPaths = %v, want nil", got)
	}
}

func TestFoldWorktreeIntoExistingEntry(t *testing.T) {
	main, wt := initRepoWithWorktree(t)
	kbRoot := t.TempDir()

	pname := projectName(main)
	m := &Manifest{
		Projects: map[string]*ProjectEntry{
			pname: {Path: main, Domain: "general"},
		},
		path: filepath.Join(kbRoot, "scripts", "projects.json"),
	}
	cfg := &ScribeConfig{}
	s := &SyncCmd{}

	derived := worktreeMainRoot(wt)
	n, changed := s.foldWorktree(kbRoot, m, cfg, wt, derived, "claude")
	if n != 0 || !changed {
		t.Fatalf("fold into existing entry = (%d, %v), want (0, true)", n, changed)
	}
	if len(m.Projects) != 1 {
		t.Fatalf("fold created a new project: %v", projectKeys(m))
	}
	if got := m.Projects[pname].Worktrees; len(got) != 1 || got[0] != wt {
		t.Errorf("worktrees = %v, want [%s]", got, wt)
	}

	// Second fold of the same worktree is a no-op.
	if n, changed := s.foldWorktree(kbRoot, m, cfg, wt, derived, "claude"); n != 0 || changed {
		t.Errorf("repeat fold = (%d, %v), want (0, false)", n, changed)
	}

	// Manifest was persisted with the worktree recorded.
	loaded, err := loadManifest(kbRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Projects[pname].Worktrees; len(got) != 1 || got[0] != wt {
		t.Errorf("persisted worktrees = %v, want [%s]", got, wt)
	}
}

func TestFoldWorktreeDiscoversMain(t *testing.T) {
	_, wt := initRepoWithWorktree(t)
	kbRoot := t.TempDir()

	m := &Manifest{
		Projects: map[string]*ProjectEntry{},
		path:     filepath.Join(kbRoot, "scripts", "projects.json"),
	}
	cfg := &ScribeConfig{} // no auto_approve → pending
	s := &SyncCmd{}

	derived := worktreeMainRoot(wt)
	n, changed := s.foldWorktree(kbRoot, m, cfg, wt, derived, "codex")
	if n != 1 || !changed {
		t.Fatalf("fold with unknown main = (%d, %v), want (1, true)", n, changed)
	}
	entry := m.Projects[projectName(derived)]
	if entry == nil {
		t.Fatalf("main project not created; manifest has %v", projectKeys(m))
	}
	if entry.Path != derived {
		t.Errorf("entry path = %q, want %q", entry.Path, derived)
	}
	if entry.Status != statusPending {
		t.Errorf("entry status = %q, want pending (no auto_approve)", entry.Status)
	}
	if entry.DiscoveredFrom != "codex" {
		t.Errorf("discovered_from = %q, want codex", entry.DiscoveredFrom)
	}
	if len(entry.Worktrees) != 1 || entry.Worktrees[0] != wt {
		t.Errorf("worktrees = %v, want [%s]", entry.Worktrees, wt)
	}
}

func TestFoldWorktreeSkipsLegacyWorktreeEntry(t *testing.T) {
	main, wt := initRepoWithWorktree(t)
	kbRoot := t.TempDir()

	// The worktree was enrolled as its own project before folding
	// existed — fold must not double-register it on the main entry.
	m := &Manifest{
		Projects: map[string]*ProjectEntry{
			projectName(main): {Path: main},
			projectName(wt):   {Path: wt},
		},
		path: filepath.Join(kbRoot, "scripts", "projects.json"),
	}
	s := &SyncCmd{}

	derived := worktreeMainRoot(wt)
	if n, changed := s.foldWorktree(kbRoot, m, &ScribeConfig{}, wt, derived, "claude"); n != 0 || changed {
		t.Errorf("fold with legacy entry = (%d, %v), want (0, false)", n, changed)
	}
	if got := m.Projects[projectName(main)].Worktrees; len(got) != 0 {
		t.Errorf("main entry gained worktrees %v despite legacy entry", got)
	}
}

func TestFoldWorktreeDryRun(t *testing.T) {
	main, wt := initRepoWithWorktree(t)
	kbRoot := t.TempDir()

	m := &Manifest{
		Projects: map[string]*ProjectEntry{
			projectName(main): {Path: main},
		},
		path: filepath.Join(kbRoot, "scripts", "projects.json"),
	}
	s := &SyncCmd{DryRun: true}

	derived := worktreeMainRoot(wt)
	if n, changed := s.foldWorktree(kbRoot, m, &ScribeConfig{}, wt, derived, "claude"); n != 0 || changed {
		t.Errorf("dry-run fold = (%d, %v), want (0, false)", n, changed)
	}
	if got := m.Projects[projectName(main)].Worktrees; len(got) != 0 {
		t.Errorf("dry run recorded worktrees: %v", got)
	}
}

func TestCollectDropFilesFromWorktree(t *testing.T) {
	main, wt := initRepoWithWorktree(t)
	kbRoot := t.TempDir()
	kb := kbName(kbRoot)

	// Drop file lives ONLY in the worktree — branch-specific knowledge.
	dropDir := filepath.Join(wt, ".claude", kb)
	if err := os.MkdirAll(dropDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropDir, "2026-06-10-branch-insight.md"), []byte("# insight\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pname := projectName(main)
	m := &Manifest{
		Projects: map[string]*ProjectEntry{
			pname: {Path: main, Worktrees: []string{wt}},
		},
		path: filepath.Join(kbRoot, "scripts", "projects.json"),
	}
	s := &SyncCmd{}

	if got := s.collectDropFiles(kbRoot, m); got != 1 {
		t.Fatalf("collectDropFiles = %d, want 1 (worktree drop)", got)
	}
	staged := filepath.Join(kbRoot, "output", "drops-"+pname, "2026-06-10-branch-insight.md")
	if _, err := os.Stat(staged); err != nil {
		t.Errorf("worktree drop not staged at %s: %v", staged, err)
	}
}

func TestCollectResearchFilesFromWorktree(t *testing.T) {
	main, wt := initRepoWithWorktree(t)
	kbRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(kbRoot, "raw", "articles"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Research in BOTH checkouts — both must be collected.
	for _, base := range []string{main, wt} {
		dir := filepath.Join(base, ".claude", "research")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(main, ".claude", "research", "main-topic.md"), []byte("main research\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".claude", "research", "branch-topic.md"), []byte("branch research\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pname := projectName(main)
	m := &Manifest{
		Projects: map[string]*ProjectEntry{
			pname: {Path: main, Domain: "general", Worktrees: []string{wt}},
		},
		path: filepath.Join(kbRoot, "scripts", "projects.json"),
	}
	s := &SyncCmd{}

	if got := s.collectResearchFiles(kbRoot, m); got != 2 {
		t.Fatalf("collectResearchFiles = %d, want 2 (main + worktree)", got)
	}
	for _, name := range []string{"main-topic.md", "branch-topic.md"} {
		dest := filepath.Join(kbRoot, "raw", "articles", "research-"+pname+"-"+name)
		if _, err := os.Stat(dest); err != nil {
			t.Errorf("research %s not collected: %v", name, err)
		}
	}
}

func TestDoctorWarnsOnWorktreeProjectEntry(t *testing.T) {
	main, wt := initRepoWithWorktree(t)
	kbRoot := t.TempDir()

	m := &Manifest{
		Projects: map[string]*ProjectEntry{
			projectName(main): {Path: main},
			projectName(wt):   {Path: wt},
		},
		path: filepath.Join(kbRoot, "scripts", "projects.json"),
	}
	if err := m.save(); err != nil {
		t.Fatal(err)
	}

	var found *check
	for _, c := range checkState(kbRoot) {
		if c.Name == "worktree-projects" {
			found = &c
			break
		}
	}
	if found == nil {
		t.Fatal("doctor did not flag the worktree manifest entry")
	}
	if found.Status != statusWarn {
		t.Errorf("status = %q, want WARN", found.Status)
	}
	if !strings.Contains(found.Detail, projectName(wt)) {
		t.Errorf("detail %q does not name the worktree project", found.Detail)
	}
	if strings.Contains(found.Detail, projectName(main)+" (worktree") {
		t.Errorf("detail %q flags the main checkout", found.Detail)
	}
}

// projectKeys lists manifest project names for failure messages.
func projectKeys(m *Manifest) []string {
	keys := make([]string, 0, len(m.Projects))
	for k := range m.Projects {
		keys = append(keys, k)
	}
	return keys
}
