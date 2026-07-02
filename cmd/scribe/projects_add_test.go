package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// addKB scaffolds a minimal KB with the given committed scribe.yaml, an
// isolated user config dir (so trust records stay in the sandbox), and an
// empty manifest. Returns the KB root.
func addKB(t *testing.T, scribeYAML string) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(scribeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCRIBE_KB", root)
	return root
}

// makeProjectDir creates a fake project directory under a temp parent named
// like a project root ("Projects/<name>") so projectName derives <name>.
func makeProjectDir(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "Projects", name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// --- appendIncludePath (the comment-preserving YAML editor) --------------

func TestAppendIncludePath_PreservesCommentsAndAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scribe.yaml")
	original := `# scribe config — hand commented
default_model: sonnet  # inline comment

# sources gates discovery
sources:
  include:
    - /a   # the first repo
    - /b
  exclude:
    - /junk
`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	added, err := appendIncludePath(path, "/c")
	if err != nil {
		t.Fatalf("appendIncludePath: %v", err)
	}
	if !added {
		t.Fatal("added = false, want true")
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		"# scribe config — hand commented", // top comment survives
		"# sources gates discovery",        // block comment survives
		"the first repo",                   // inline list comment survives
		"- /a",
		"- /b",
		"- /c", // appended
		"- /junk",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n---\n%s", want, got)
		}
	}

	// Re-parse to confirm the structure is valid and the include now has 3.
	cfg := loadConfigFromFile(t, path)
	if !slices.Equal(cfg.Sources.Include, []string{"/a", "/b", "/c"}) {
		t.Errorf("include = %v, want [/a /b /c]", cfg.Sources.Include)
	}
}

func TestAppendIncludePath_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scribe.yaml")
	if err := os.WriteFile(path, []byte("sources:\n  include:\n    - /a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	added, err := appendIncludePath(path, "/a")
	if err != nil {
		t.Fatalf("appendIncludePath: %v", err)
	}
	if added {
		t.Error("added = true for an entry already present, want false")
	}
}

func TestAppendIncludePath_CreatesSourcesWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scribe.yaml")
	if err := os.WriteFile(path, []byte("default_model: sonnet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	added, err := appendIncludePath(path, "/new")
	if err != nil {
		t.Fatalf("appendIncludePath: %v", err)
	}
	if !added {
		t.Fatal("added = false, want true")
	}
	cfg := loadConfigFromFile(t, path)
	if !slices.Equal(cfg.Sources.Include, []string{"/new"}) {
		t.Errorf("include = %v, want [/new]", cfg.Sources.Include)
	}
	if cfg.DefaultModel != "sonnet" {
		t.Errorf("default_model = %q, want sonnet preserved", cfg.DefaultModel)
	}
}

// loadConfigFromFile parses a standalone scribe.yaml at path through a KB
// root (no local overrides, no trust) and returns the config.
func loadConfigFromFile(t *testing.T, path string) *ScribeConfig {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := filepath.Dir(path)
	return loadConfig(root)
}

// --- ProjectsAddCmd end to end -------------------------------------------

func TestProjectsAdd_EnrollsApprovedAndWidensInclude(t *testing.T) {
	// Committed include is a non-empty allowlist — adding must widen it.
	root := addKB(t, "sources:\n  include:\n    - /already/listed\n")
	proj := makeProjectDir(t, "newrepo")

	cmd := &ProjectsAddCmd{Path: proj}
	if err := cmd.run(root); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Manifest entry: approved, manual.
	m, err := loadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := m.Projects["newrepo"]
	if !ok {
		t.Fatal("newrepo not enrolled")
	}
	if !e.IsApproved() {
		t.Errorf("status = %q, want approved", e.Status)
	}
	if e.DiscoveredFrom != "manual" {
		t.Errorf("discovered_from = %q, want manual", e.DiscoveredFrom)
	}
	if !samePath(e.Path, proj) {
		t.Errorf("path = %q, want %q", e.Path, proj)
	}

	// scribe.yaml include now covers the path.
	cfg := loadConfig(root)
	if !includeCovers(cfg.Sources.Include, proj) {
		t.Errorf("include %v does not cover %s", cfg.Sources.Include, proj)
	}
}

func TestProjectsAdd_EmptyIncludeNotNarrowed(t *testing.T) {
	// Empty include = allow-all; adding must NOT write a one-entry include.
	root := addKB(t, "default_model: sonnet\n")
	proj := makeProjectDir(t, "anyrepo")

	if err := (&ProjectsAddCmd{Path: proj}).run(root); err != nil {
		t.Fatalf("add: %v", err)
	}

	cfg := loadConfig(root)
	if len(cfg.Sources.Include) != 0 {
		t.Errorf("include = %v, want empty (allow-all must not be narrowed)", cfg.Sources.Include)
	}
	if _, ok := mustManifest(t, root).Projects["anyrepo"]; !ok {
		t.Error("anyrepo not enrolled")
	}
}

func TestProjectsAdd_LocalWritesLocalFileNotCommitted(t *testing.T) {
	root := addKB(t, "sources:\n  include:\n    - /already/listed\n")
	proj := makeProjectDir(t, "localrepo")

	if err := (&ProjectsAddCmd{Path: proj, Local: true}).run(root); err != nil {
		t.Fatalf("add --local: %v", err)
	}

	// Committed scribe.yaml is untouched; scribe.local.yaml carries the add.
	committed, err := os.ReadFile(filepath.Join(root, "scribe.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(committed), proj) {
		t.Error("committed scribe.yaml should not mention the path on --local")
	}
	local, err := os.ReadFile(filepath.Join(root, localConfigName))
	if err != nil {
		t.Fatalf("scribe.local.yaml not written: %v", err)
	}
	if !strings.Contains(string(local), proj) {
		t.Errorf("scribe.local.yaml missing the path:\n%s", local)
	}
	// Effective include unions committed + local and covers the path.
	cfg := loadConfig(root)
	if !includeCovers(cfg.Sources.Include, proj) {
		t.Errorf("merged include %v does not cover %s", cfg.Sources.Include, proj)
	}
}

func TestProjectsAdd_RejectsExcludedPath(t *testing.T) {
	proj := makeProjectDir(t, "blocked")
	root := addKB(t, "sources:\n  exclude:\n    - "+proj+"\n")
	err := (&ProjectsAddCmd{Path: proj}).run(root)
	if err == nil || !strings.Contains(err.Error(), "exclude") {
		t.Fatalf("err = %v, want an exclude rejection", err)
	}
}

func TestProjectsAdd_MissingPathErrors(t *testing.T) {
	root := addKB(t, "default_model: sonnet\n")
	err := (&ProjectsAddCmd{Path: filepath.Join(t.TempDir(), "nope")}).run(root)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v, want does-not-exist", err)
	}
}

func TestProjectsAdd_NameCollisionErrors(t *testing.T) {
	root := addKB(t, "default_model: sonnet\n")
	a := makeProjectDir(t, "dup")
	if err := (&ProjectsAddCmd{Path: a}).run(root); err != nil {
		t.Fatalf("first add: %v", err)
	}
	// A different path deriving the same name must be rejected.
	b := makeProjectDir(t, "dup")
	err := (&ProjectsAddCmd{Path: b}).run(root)
	if err == nil || !strings.Contains(err.Error(), "already maps to") {
		t.Fatalf("err = %v, want a name-collision error", err)
	}
}

func mustManifest(t *testing.T, root string) *Manifest {
	t.Helper()
	m, err := loadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// --- ProjectsAddCmd --from-sources (bulk enrollment, #28) -----------------

// gitInitDir turns an existing directory into a real git repo. hasGit only
// checks for a .git directory, but worktreeMainRoot shells out to `git
// rev-parse`, so fixtures need an actual repo rather than a bare .git stub —
// mirrors the initTestGitRepo/gitRun convention used elsewhere in this
// package (see worktree_test.go, sources_test.go).
func gitInitDir(t *testing.T, dir string) {
	t.Helper()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "Test")
}

// makeSiblingDir creates parent/name without allocating a fresh TempDir, for
// glob-pattern tests where multiple candidates must share one parent —
// unlike makeProjectDir, which gives every project its own temp parent.
func makeSiblingDir(t *testing.T, parent, name string) string {
	t.Helper()
	p := filepath.Join(parent, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProjectsAdd_FromSources_EnrollsListedRepos(t *testing.T) {
	repoA := makeProjectDir(t, "repoA")
	gitInitDir(t, repoA)
	repoB := makeProjectDir(t, "repoB")
	gitInitDir(t, repoB)

	root := addKB(t, "sources:\n  include:\n    - "+repoA+"\n    - "+repoB+"\n")

	if err := (&ProjectsAddCmd{FromSources: true}).runFromSources(root); err != nil {
		t.Fatalf("runFromSources: %v", err)
	}

	m := mustManifest(t, root)
	for name, path := range map[string]string{"repoA": repoA, "repoB": repoB} {
		e, ok := m.Projects[name]
		if !ok {
			t.Fatalf("%s not enrolled", name)
		}
		if !e.IsApproved() {
			t.Errorf("%s status = %q, want approved", name, e.Status)
		}
		if e.DiscoveredFrom != "manual" {
			t.Errorf("%s discovered_from = %q, want manual", name, e.DiscoveredFrom)
		}
		if !samePath(e.Path, path) {
			t.Errorf("%s path = %q, want %q", name, e.Path, path)
		}
	}
}

func TestProjectsAdd_FromSources_SkipsNonGitPath(t *testing.T) {
	repo := makeProjectDir(t, "gitrepo")
	gitInitDir(t, repo)
	plain := makeProjectDir(t, "plaindir") // never git-init'd

	root := addKB(t, "sources:\n  include:\n    - "+repo+"\n    - "+plain+"\n")

	out := captureStdout(t, func() error {
		return (&ProjectsAddCmd{FromSources: true}).runFromSources(root)
	})
	if !strings.Contains(out, "not a git repo") {
		t.Errorf("expected a non-git skip reason in output:\n%s", out)
	}

	m := mustManifest(t, root)
	if _, ok := m.Projects["gitrepo"]; !ok {
		t.Error("gitrepo not enrolled")
	}
	if _, ok := m.Projects["plaindir"]; ok {
		t.Error("plaindir (non-git) must not be bulk-enrolled — single-add's warn-and-continue does not apply to --from-sources")
	}
}

func TestProjectsAdd_FromSources_SkipsMissingAndExcluded(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "Projects", "ghost")
	excluded := makeProjectDir(t, "blocked")
	gitInitDir(t, excluded)

	root := addKB(t, "sources:\n  include:\n    - "+missing+"\n    - "+excluded+"\n  exclude:\n    - "+excluded+"\n")

	if err := (&ProjectsAddCmd{FromSources: true}).runFromSources(root); err != nil {
		t.Fatalf("runFromSources: %v", err)
	}

	m := mustManifest(t, root)
	if len(m.Projects) != 0 {
		t.Errorf("Projects = %v, want none enrolled (missing path + excluded path)", m.Projects)
	}
}

func TestProjectsAdd_FromSources_AlreadyEnrolledIsIdempotent(t *testing.T) {
	repoA := makeProjectDir(t, "repoA")
	gitInitDir(t, repoA)
	repoB := makeProjectDir(t, "repoB")
	gitInitDir(t, repoB)

	root := addKB(t, "sources:\n  include:\n    - "+repoA+"\n    - "+repoB+"\n")
	run := func() error { return (&ProjectsAddCmd{FromSources: true}).runFromSources(root) }

	if err := run(); err != nil {
		t.Fatalf("first run: %v", err)
	}
	manifestPath := filepath.Join(root, "scripts", "projects.json")
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := run(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("manifest changed on a repeat --from-sources run:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestProjectsAdd_FromSources_EmptyIncludeNoop(t *testing.T) {
	root := addKB(t, "default_model: sonnet\n")

	out := captureStdout(t, func() error {
		return (&ProjectsAddCmd{FromSources: true}).runFromSources(root)
	})
	if !strings.Contains(out, "allow-all") {
		t.Errorf("expected the allow-all note in output:\n%s", out)
	}

	m := mustManifest(t, root)
	if len(m.Projects) != 0 {
		t.Errorf("Projects = %v, want none (empty include must not bulk-enroll anything)", m.Projects)
	}
}

func TestProjectsAdd_FromSources_ExpandsGlobAndDedupes(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "Projects")
	clientA := makeSiblingDir(t, parent, "client-a")
	gitInitDir(t, clientA)
	clientB := makeSiblingDir(t, parent, "client-b")
	gitInitDir(t, clientB)

	// The glob and the literal entry both resolve to client-a — it must
	// enroll once, not twice.
	root := addKB(t, "sources:\n  include:\n    - "+parent+"/client-*\n    - "+clientA+"\n")

	out := captureStdout(t, func() error {
		return (&ProjectsAddCmd{FromSources: true}).runFromSources(root)
	})

	if n := strings.Count(out, "enrolled client-a ->"); n != 1 {
		t.Errorf("client-a enrolled %d times, want 1 (glob + literal entry must dedupe)\noutput:\n%s", n, out)
	}
	if !strings.Contains(out, "from-sources: 2 enrolled/confirmed, 0 skipped") {
		t.Errorf("summary line missing/wrong:\n%s", out)
	}

	m := mustManifest(t, root)
	if _, ok := m.Projects["client-a"]; !ok {
		t.Error("client-a not enrolled")
	}
	if _, ok := m.Projects["client-b"]; !ok {
		t.Error("client-b not enrolled")
	}
}

func TestProjectsAdd_FromSources_RejectsCombinedFlags(t *testing.T) {
	// lock_dir MUST be isolated off the default /tmp — Run() takes the real
	// sync lock, and /tmp is the same global namespace a real cron sync
	// uses (see commit_test.go's commitTestKB for the same footgun).
	root := addKB(t, "lock_dir: "+t.TempDir()+"\n")

	err := (&ProjectsAddCmd{FromSources: true, Local: true}).Run()
	if err == nil || !strings.Contains(err.Error(), "--from-sources cannot be combined") {
		t.Fatalf("err = %v, want a from-sources/--local conflict error", err)
	}

	if len(mustManifest(t, root).Projects) != 0 {
		t.Error("rejected flag combination must not touch the manifest")
	}
}
