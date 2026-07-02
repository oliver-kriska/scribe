package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProjectName documents the canonical project-name derivation used
// throughout scribe. The rule: if the parent directory basename is one of the
// configured project roots (Projects, projects, src, code, repos, work by
// default; extendable via SCRIBE_PROJECT_ROOTS), use the leaf as the name;
// otherwise use "parent-leaf". That lets nested repos (e.g. ~/org/team/repo)
// get a disambiguated name while the common case stays short.
func TestProjectName(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/home/u/Projects/scriptorium", "scriptorium"},
		{"/home/u/projects/dovolov", "dovolov"},
		{"/home/u/src/acme", "acme"},
		{"/home/u/code/app", "app"},
		{"/home/u/work/app", "app"},
		{"/tmp/random/project", "random-project"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := projectName(tc.path); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProjectName_EnvOverride(t *testing.T) {
	t.Setenv("SCRIBE_PROJECT_ROOTS", "lab:playground")
	roots := defaultProjectRoots()
	if !roots["lab"] || !roots["playground"] {
		t.Fatalf("env override not honored: %v", roots)
	}
}

// TestManifestIsIgnored covers the rules for path skipping:
// (1) fewer than 4 non-empty segments = too shallow, ignored;
// (2) exact match in IgnoredPaths list = ignored;
// (3) under a macOS TCC-protected $HOME subdir = ignored (otherwise
// auto-discovering a stray Claude session in ~/Downloads triggers a
// chain of TCC consent prompts the user never asked for).
func TestManifestIsIgnored(t *testing.T) {
	t.Setenv("HOME", "/Users/x")
	m := &Manifest{
		IgnoredPaths: []string{"/Users/x/Projects/scratch"},
	}
	cases := []struct {
		path string
		want bool
	}{
		{"/Users/x/Projects/real", false},   // 4 segments, not ignored
		{"/Users/x/Projects/scratch", true}, // explicit match
		{"/Users/x/foo", true},              // 3 segments, too shallow
		{"/tmp", true},                      // 1 segment, too shallow
		{"", true},                          // empty path
		{"/a/b/c/d/e/f/g", false},           // deep path, not in list
		{"/Users/x/Downloads/Lukas/Session/2/transcript/output", true},      // TCC: Downloads
		{"/Users/x/Documents/notes/repo", true},                             // TCC: Documents
		{"/Users/x/Desktop/scratch/repo", true},                             // TCC: Desktop
		{"/Users/x/Pictures/lib", true},                                     // TCC: Photos
		{"/Users/x/Library/Mobile Documents/com~apple~CloudDocs/foo", true}, // TCC: iCloud via Library
		{"/Users/x/Music/anything/at/all", true},                            // TCC: Music
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := m.isIgnored(tc.path); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsScribeKB asserts the marker-file detection that keeps a KB from
// extracting itself. A directory is a scribe KB iff it holds scribe.yaml.
func TestIsScribeKB(t *testing.T) {
	t.Parallel()
	kb := t.TempDir()
	if isScribeKB(kb) {
		t.Fatal("empty dir reported as scribe KB")
	}
	if err := os.WriteFile(filepath.Join(kb, "scribe.yaml"), []byte("owner_name: Test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isScribeKB(kb) {
		t.Error("dir with scribe.yaml not reported as scribe KB")
	}
}

// TestIsWithinKB covers the path-only containment check used to keep
// sessions run inside the KB out of the mining pipeline.
func TestIsWithinKB(t *testing.T) {
	t.Parallel()
	root := "/Users/x/kb"
	cases := []struct {
		path string
		want bool
	}{
		{"/Users/x/kb", true},              // the root itself
		{"/Users/x/kb/wiki", true},         // nested
		{"/Users/x/kb/projects/a/b", true}, // deeply nested
		{"/Users/x/kb-other", false},       // sibling sharing a prefix — must NOT match
		{"/Users/x/other", false},          // unrelated
		{"/Users/x/kb/../kb/wiki", true},   // cleaned to within
		{"", false},                        // empty path
	}
	for _, tc := range cases {
		if got := isWithinKB(root, tc.path); got != tc.want {
			t.Errorf("isWithinKB(%q, %q) = %v, want %v", root, tc.path, got, tc.want)
		}
	}
	if isWithinKB("", "/anything") {
		t.Error("empty root must never contain a path")
	}
}

// TestSessionInKB confirms a session is excluded from mining when its cwd is
// the active KB (or a subdir) OR any other scribe KB on disk.
func TestSessionInKB(t *testing.T) {
	t.Parallel()
	root := t.TempDir()  // pretend active KB
	other := t.TempDir() // a different KB on disk
	plain := t.TempDir() // an ordinary project
	if err := os.WriteFile(filepath.Join(other, "scribe.yaml"), []byte("owner_name: T\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sessionInKB(root, plain) {
		t.Errorf("plain project %q wrongly excluded", plain)
	}
	if !sessionInKB(root, root) {
		t.Error("active KB root not excluded")
	}
	if !sessionInKB(root, filepath.Join(root, "wiki")) {
		t.Error("subdir of active KB not excluded")
	}
	if !sessionInKB(root, other) {
		t.Errorf("other scribe KB %q not excluded", other)
	}
	// Multi-KB hardening: a session run in a SUBDIRECTORY of another KB
	// must be excluded too — the exact-root check used to miss this, so
	// KB A would mine sessions spent inside KB B's wiki.
	sub := filepath.Join(other, "wiki", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if !sessionInKB(root, sub) {
		t.Errorf("subdir %q of another scribe KB not excluded", sub)
	}
}

func TestWithinScribeKB(t *testing.T) {
	t.Parallel()
	kb := t.TempDir()
	if err := os.WriteFile(filepath.Join(kb, "scribe.yaml"), []byte("owner_name: T\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(kb, "projects", "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	plain := t.TempDir()

	if !withinScribeKB(kb) {
		t.Error("KB root not detected")
	}
	if !withinScribeKB(sub) {
		t.Error("nested KB subdir not detected")
	}
	if withinScribeKB(plain) {
		t.Errorf("plain dir %q wrongly detected as inside a KB", plain)
	}
	if withinScribeKB("") {
		t.Error("empty path must not match")
	}
}

// TestManifestIsIgnored_SkipsNestedKBPath: discovery must also refuse a
// project path that sits INSIDE another KB (e.g. a Claude session run in
// ~/team-kb/wiki/), not just the KB root itself.
func TestManifestIsIgnored_SkipsNestedKBPath(t *testing.T) {
	t.Parallel()
	kb := t.TempDir()
	sub := filepath.Join(kb, "wiki", "topic")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manifest{}
	if m.isIgnored(sub) {
		t.Skipf("temp dir %q already ignored on this platform; cannot isolate the check", sub)
	}
	if err := os.WriteFile(filepath.Join(kb, "scribe.yaml"), []byte("owner_name: Test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !m.isIgnored(sub) {
		t.Errorf("path %q inside a scribe KB not ignored — KB B would be harvested into KB A", sub)
	}
}

// TestManifestIsIgnored_SkipsScribeKB is the regression guard for the
// reported duplicate-page bug: a KB checked out under a tracked project
// root must be excluded from discovery so it never re-ingests its own
// wiki. Uses a real on-disk dir because the check stats scribe.yaml.
func TestManifestIsIgnored_SkipsScribeKB(t *testing.T) {
	t.Parallel()
	kb := t.TempDir() // deep enough to clear the segment-count + TCC checks
	m := &Manifest{}
	if m.isIgnored(kb) {
		t.Skipf("temp dir %q is already ignored on this platform (depth/TCC); cannot isolate the scribe.yaml check", kb)
	}
	if err := os.WriteFile(filepath.Join(kb, "scribe.yaml"), []byte("owner_name: Test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !m.isIgnored(kb) {
		t.Errorf("scribe KB %q not ignored — discovery would conscript it into its own pipeline", kb)
	}
}

// TestManifestResolveDomain covers the two-level alias lookup: first
// the leaf name, then "parent/leaf". Fallback is "general". This is the
// source of truth for the domain field in extracted wiki articles, so
// getting it wrong mislabels every article from that project.
func TestManifestResolveDomain(t *testing.T) {
	m := &Manifest{
		DomainAliases: map[string]string{
			"scriptorium": "personal",
			"work/acme":   "acme",
			"dovolov":     "dovolov",
		},
	}
	cases := []struct {
		path string
		want string
	}{
		{"/Users/x/Projects/scriptorium", "personal"},
		{"/Users/x/Projects/dovolov", "dovolov"},
		{"/Users/x/work/acme", "acme"},           // parent/leaf match
		{"/Users/x/work/other", "general"},       // parent known, leaf not
		{"/Users/x/Projects/unknown", "general"}, // no match at all
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := m.resolveDomain(tc.path); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLoadManifest verifies round-tripping via a real tempdir.
// scripts/projects.json is the discovery source for every sync run, so
// corrupt parsing silently breaks extraction.
func TestLoadManifest(t *testing.T) {
	root := t.TempDir()
	scriptsDir := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{
  "projects": {
    "scriptorium": {
      "path": "/Users/x/Projects/scriptorium",
      "domain": "personal",
      "last_sha": "abc123"
    }
  },
  "domain_aliases": {"scriptorium": "personal"},
  "ignored_paths": ["/tmp/junk"]
}`
	if err := os.WriteFile(filepath.Join(scriptsDir, "projects.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	// Legacy fixture is basename-keyed ("scriptorium"); loadManifest
	// migrates it in-memory to a canonical-path key, inheriting the old
	// key as Name (see migrateToPathKeys).
	entry := m.Projects[canonicalizePath("/Users/x/Projects/scriptorium")]
	if entry == nil {
		t.Fatalf("entry not found after migration; projects = %v", m.Projects)
	}
	if entry.Name != "scriptorium" {
		t.Errorf("Name = %q, want scriptorium (inherited from legacy key)", entry.Name)
	}
	if entry.LastSHA != "abc123" {
		t.Errorf("last_sha = %q", entry.LastSHA)
	}
	if m.DomainAliases["scriptorium"] != "personal" {
		t.Errorf("alias missing: %v", m.DomainAliases)
	}
	if len(m.IgnoredPaths) != 1 || m.IgnoredPaths[0] != "/tmp/junk" {
		t.Errorf("ignored_paths = %v", m.IgnoredPaths)
	}
}

func TestLoadManifest_MissingFile(t *testing.T) {
	if _, err := loadManifest(t.TempDir()); err == nil {
		t.Error("expected error for missing manifest")
	}
}

func TestLoadManifest_MalformedJSON(t *testing.T) {
	root := t.TempDir()
	scriptsDir := filepath.Join(root, "scripts")
	_ = os.MkdirAll(scriptsDir, 0o755)
	_ = os.WriteFile(filepath.Join(scriptsDir, "projects.json"), []byte("{not json"), 0o644)
	if _, err := loadManifest(root); err == nil {
		t.Error("expected parse error")
	}
}

// TestDecodeClaudePath exercises the greedy rebuild of real filesystem
// paths from Claude Code's dash-encoded project dir names. Dashes are
// ambiguous — they can mean either "/" or literal "-". The decoder
// probes dirExists at each step. Tests build a real dir tree in a
// tempdir so dirExists() hits something real.
func TestDecodeClaudePath(t *testing.T) {
	// Build: <tmp>/user/my-project and <tmp>/user/simple
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "user", "my-project"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "user", "simple"), 0o755); err != nil {
		t.Fatal(err)
	}

	// decodeClaudePath starts paths with "/". We build a relative-looking
	// encoded name from the tempdir and prefix with its own leading parts.
	// The Claude encoding of /tmp123/user/simple is -tmp123-user-simple.
	encoded := "-" + filepath.Join(tmp[1:], "user", "simple")
	// filepath.Join uses "/" on unix, so we need to turn "/" into "-".
	encodedDashes := strings.ReplaceAll(encoded, "/", "-")
	got := decodeClaudePath(encodedDashes)
	if got != filepath.Join(tmp, "user", "simple") {
		t.Errorf("simple path: got %q, want %q", got, filepath.Join(tmp, "user", "simple"))
	}

	// Now the ambiguous case: my-project encodes as ...-user-my-project.
	// The decoder must detect that "my" isn't a real dir but "my-project"
	// (with dash preserved) is.
	encoded2 := "-" + filepath.Join(tmp[1:], "user", "my-project")
	encodedDashes2 := strings.ReplaceAll(encoded2, "/", "-")
	got2 := decodeClaudePath(encodedDashes2)
	if got2 != filepath.Join(tmp, "user", "my-project") {
		t.Errorf("dashed path: got %q, want %q", got2, filepath.Join(tmp, "user", "my-project"))
	}
}

func TestDecodeClaudePath_NonExistent(t *testing.T) {
	// A path that doesn't exist on disk returns "".
	got := decodeClaudePath("-this-path-definitely-does-not-exist-12345")
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

// dashEncode mirrors the lossless part of Claude's project-dir encoding:
// the absolute path with every "/" turned into "-" and a leading "-".
// Underscores/dots are NOT collapsed here, so decodeClaudePath can still
// round-trip — used to construct the decode-fallback fixtures.
func dashEncode(absPath string) string {
	return "-" + strings.ReplaceAll(strings.TrimPrefix(absPath, "/"), "/", "-")
}

// TestResolveClaudeProjectPath_PrefersSessionCwd is the andrej_skolenia
// regression: a project whose path contains "_" can never be rebuilt by
// decodeClaudePath (Claude collapses "_" → "-"), but its session JSONL
// records the verbatim cwd. resolveClaudeProjectPath must return that.
func TestResolveClaudeProjectPath_PrefersSessionCwd(t *testing.T) {
	tmp := t.TempDir()

	projectPath := filepath.Join(tmp, "Projects", "andrej_skolenia")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Claude's real encoding would collapse the "_" to "-", so the
	// directory name is undecodable. The exact name is irrelevant — the
	// resolver should never consult it once the cwd is found.
	encodedName := strings.ReplaceAll(dashEncode(projectPath), "_", "-")
	sessionDir := filepath.Join(tmp, "claude-projects", encodedName)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonl := `{"type":"user","cwd":"` + projectPath + `","message":{"role":"user"}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "sess.jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatal(err)
	}

	// Sanity: the name-decode genuinely fails for this fixture, so the
	// test would catch a regression that silently relies on it.
	if d := decodeClaudePath(encodedName); d == projectPath {
		t.Fatalf("fixture invalid: decodeClaudePath unexpectedly recovered %q", projectPath)
	}

	if got := resolveClaudeProjectPath(sessionDir, encodedName); got != projectPath {
		t.Errorf("got %q, want verbatim cwd %q", got, projectPath)
	}
}

// TestResolveClaudeProjectPath_FallsBackToDecode: with no readable cwd in
// the session dir, the resolver must defer to decodeClaudePath.
func TestResolveClaudeProjectPath_FallsBackToDecode(t *testing.T) {
	tmp := t.TempDir()

	projectPath := filepath.Join(tmp, "user", "my-project")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	encodedName := dashEncode(projectPath)

	// Session dir exists but holds no .jsonl → no cwd → decode fallback.
	sessionDir := filepath.Join(tmp, "claude-projects", encodedName)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	got := resolveClaudeProjectPath(sessionDir, encodedName)
	if want := decodeClaudePath(encodedName); got != want || got != projectPath {
		t.Errorf("got %q, want fallback to %q (decode=%q)", got, projectPath, want)
	}
}

// TestResolveClaudeProjectPath_IgnoresStaleCwd: a cwd pointing at a
// directory that no longer exists is rejected, and the resolver falls
// back to the name-decode (which here still resolves).
func TestResolveClaudeProjectPath_IgnoresStaleCwd(t *testing.T) {
	tmp := t.TempDir()

	projectPath := filepath.Join(tmp, "user", "real-project")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	encodedName := dashEncode(projectPath)
	sessionDir := filepath.Join(tmp, "claude-projects", encodedName)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := `{"cwd":"` + filepath.Join(tmp, "moved-away") + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "sess.jsonl"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := resolveClaudeProjectPath(sessionDir, encodedName); got != projectPath {
		t.Errorf("stale cwd should be ignored; got %q, want decode result %q", got, projectPath)
	}
}

// TestReadClaudeCwd_SkipsCwdlessLeadingLine: resumed sessions can begin
// with a summary event that carries no cwd; the reader must scan past it.
func TestReadClaudeCwd_SkipsCwdlessLeadingLine(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "s.jsonl")
	content := `{"type":"summary","summary":"resumed thread"}` + "\n" +
		`{"type":"user","cwd":"/Users/x/Projects/p","message":{}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readClaudeCwd(path); got != "/Users/x/Projects/p" {
		t.Errorf("got %q, want cwd from the second line", got)
	}
}

// ---------------------------------------------------------------------
// Issue #8 — path-keyed manifest identity: canonicalizePath, migration,
// resolve, uniqueName.
// ---------------------------------------------------------------------

// TestCanonicalizePath covers the four cases the doc comment promises: a
// real directory (resolved through EvalSymlinks), a directory reached
// through a symlinked ancestor (the macOS /var vs /private/var case,
// reproduced here with an explicit symlink so the test doesn't depend on
// real system paths), a non-existent directory (falls back to the cleaned
// absolute path rather than ""), and relative / "~"-prefixed input (always
// resolves to an absolute path).
func TestCanonicalizePath(t *testing.T) {
	realDir := t.TempDir()
	if got := canonicalizePath(realDir); got != realDir {
		// t.TempDir() itself may already be behind a symlink on some
		// platforms (macOS): canonicalizePath must fully resolve it, so
		// compare against the OS's own resolution rather than the raw
		// TempDir() string.
		want, err := filepath.EvalSymlinks(realDir)
		if err != nil {
			t.Fatalf("EvalSymlinks(%s): %v", realDir, err)
		}
		if got != want {
			t.Errorf("real dir: got %q, want %q", got, want)
		}
	}

	// Symlinked ancestor: linkParent -> realParent, and we canonicalize a
	// path THROUGH the symlink. Both spellings must resolve identically.
	realParent := t.TempDir()
	realChild := filepath.Join(realParent, "child")
	if err := os.MkdirAll(realChild, 0o755); err != nil {
		t.Fatal(err)
	}
	linkParent := filepath.Join(t.TempDir(), "linked-ancestor")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Skipf("symlink not supported on this platform/filesystem: %v", err)
	}
	throughLink := filepath.Join(linkParent, "child")
	if got, want := canonicalizePath(throughLink), canonicalizePath(realChild); got != want {
		t.Errorf("symlinked ancestor: got %q, want %q (same real dir)", got, want)
	}

	// Non-existent directory: EvalSymlinks fails, so canonicalizePath must
	// fall back to the cleaned absolute path instead of "".
	gone := filepath.Join(t.TempDir(), "does-not-exist", "nested")
	got := canonicalizePath(gone)
	if got == "" {
		t.Error("non-existent path canonicalized to empty string, want fallback to cleaned abs path")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("non-existent path result %q is not absolute", got)
	}
	if got != filepath.Clean(gone) {
		t.Errorf("non-existent path = %q, want cleaned abs fallback %q", got, filepath.Clean(gone))
	}

	// Relative + "~"-prefixed input always resolves to an absolute path.
	if got := canonicalizePath("."); !filepath.IsAbs(got) {
		t.Errorf("relative input %q not resolved to absolute", got)
	}
	home := os.Getenv("HOME")
	if home != "" {
		if got := canonicalizePath("~"); got != canonicalizePath(home) {
			t.Errorf("~ = %q, want %q", got, canonicalizePath(home))
		}
	}
}

// legacyManifestJSON builds a basename-keyed (pre-#8) manifest fixture:
// no manifest_version key, no name field on entries — the exact shape a
// pre-upgrade scripts/projects.json has on disk.
func legacyManifestJSON(t *testing.T, entries map[string]*ProjectEntry) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`{"projects":{`)
	first := true
	for name, e := range entries {
		if !first {
			sb.WriteString(",")
		}
		first = false
		fmt.Fprintf(&sb, "%q:{\"path\":%q,\"domain\":%q,\"last_sha\":%q,\"last_extracted\":%q}",
			name, e.Path, e.Domain, e.LastSHA, e.LastExtracted)
	}
	sb.WriteString(`},"domain_aliases":{},"ignored_paths":[]}`)
	return sb.String()
}

// writeManifestFile writes content to <root>/scripts/projects.json,
// creating the scripts/ dir as needed, and returns the file path.
func writeManifestFile(t *testing.T, root, content string) string {
	t.Helper()
	dir := filepath.Join(root, "scripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "projects.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestManifestMigrateToPathKeys_Basic loads a legacy basename-keyed
// manifest fixture and checks the in-memory result: re-keyed by canonical
// path, Name inherited 1:1 from the old key, version bumped.
func TestManifestMigrateToPathKeys_Basic(t *testing.T) {
	root := t.TempDir()
	proj := t.TempDir()
	content := legacyManifestJSON(t, map[string]*ProjectEntry{
		"scriptorium": {Path: proj, Domain: "personal", LastSHA: "abc123"},
	})
	writeManifestFile(t, root, content)

	m, err := loadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if m.ManifestVersion != manifestPathKeyedVersion {
		t.Errorf("ManifestVersion = %d, want %d", m.ManifestVersion, manifestPathKeyedVersion)
	}
	key := canonicalizePath(proj)
	entry, ok := m.Projects[key]
	if !ok {
		t.Fatalf("no entry at canonical key %s; projects = %v", key, m.Projects)
	}
	if entry.Name != "scriptorium" {
		t.Errorf("Name = %q, want scriptorium (inherited from legacy key)", entry.Name)
	}
	if entry.Path != key {
		t.Errorf("Path = %q, want == map key %q", entry.Path, key)
	}
	if entry.LastSHA != "abc123" {
		t.Errorf("LastSHA = %q, want abc123", entry.LastSHA)
	}
}

// TestManifestMigrateToPathKeys_CollapsesDuplicates: two legacy entries
// under different names whose Path canonicalizes to the SAME real
// directory (one spelled through a symlink — reproducing the macOS /var
// vs /private/var class of bug with an explicit symlink rather than a
// real system path) collapse into exactly one surviving entry. The
// surviving Name is the one with LastExtracted set (newer); the loser's
// Worktrees are merged in, not dropped.
func TestManifestMigrateToPathKeys_CollapsesDuplicates(t *testing.T) {
	root := t.TempDir()
	realDir := t.TempDir()
	linkParent := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realDir, linkParent); err != nil {
		t.Skipf("symlink not supported on this platform/filesystem: %v", err)
	}

	wt1 := filepath.Join(t.TempDir(), "wt1")
	wt2 := filepath.Join(t.TempDir(), "wt2")

	// Build the raw JSON by hand so both worktrees + timestamps land
	// exactly as intended (legacyManifestJSON doesn't carry worktrees).
	content := fmt.Sprintf(`{"projects":{
		"never-extracted":{"path":%q,"domain":"general","worktrees":[%q]},
		"was-extracted":{"path":%q,"domain":"general","last_extracted":"2026-06-01T00:00:00Z","worktrees":[%q]}
	},"domain_aliases":{},"ignored_paths":[]}`, realDir, wt1, linkParent, wt2)
	writeManifestFile(t, root, content)

	m, err := loadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	canon := canonicalizePath(realDir)
	if got := len(m.Projects); got != 1 {
		t.Fatalf("Projects has %d entries after collapse, want 1: %v", got, m.Projects)
	}
	entry, ok := m.Projects[canon]
	if !ok {
		t.Fatalf("no entry at canonical key %s; projects = %v", canon, m.Projects)
	}
	if entry.Name != "was-extracted" {
		t.Errorf("Name = %q, want %q (the entry with LastExtracted set)", entry.Name, "was-extracted")
	}
	if len(entry.Worktrees) != 2 {
		t.Errorf("Worktrees = %v, want both wt1 and wt2 merged", entry.Worktrees)
	}
	var sawWt1, sawWt2 bool
	for _, w := range entry.Worktrees {
		if w == wt1 {
			sawWt1 = true
		}
		if w == wt2 {
			sawWt2 = true
		}
	}
	if !sawWt1 || !sawWt2 {
		t.Errorf("Worktrees = %v, want union of [%s %s]", entry.Worktrees, wt1, wt2)
	}
}

// TestManifestMigrateToPathKeys_Idempotent: migrating twice must equal
// migrating once — a manifest already at manifestPathKeyedVersion is a
// no-op on a second migrateToPathKeys call (no re-keying, no duplicate
// log, migratedCount not bumped again).
func TestManifestMigrateToPathKeys_Idempotent(t *testing.T) {
	root := t.TempDir()
	proj := t.TempDir()
	content := legacyManifestJSON(t, map[string]*ProjectEntry{
		"scriptorium": {Path: proj, Domain: "personal", LastSHA: "abc123"},
	})
	writeManifestFile(t, root, content)

	m, err := loadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	firstSave, err := os.ReadFile(filepath.Join(root, "scripts", "projects.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Reload the now-migrated (version-2) file and migrate again directly:
	// must be a complete no-op.
	reloaded, err := loadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	before := len(reloaded.Projects)
	beforeVersion := reloaded.ManifestVersion
	reloaded.migrateToPathKeys()
	if reloaded.ManifestVersion != beforeVersion {
		t.Errorf("ManifestVersion changed on second migrate: %d -> %d", beforeVersion, reloaded.ManifestVersion)
	}
	if len(reloaded.Projects) != before {
		t.Errorf("Projects count changed on second migrate: %d -> %d", before, len(reloaded.Projects))
	}
	if reloaded.migratedCount != 0 {
		t.Errorf("migratedCount = %d after a no-op migrate on an already-versioned manifest, want 0", reloaded.migratedCount)
	}

	if err := reloaded.save(); err != nil {
		t.Fatal(err)
	}
	secondSave, err := os.ReadFile(filepath.Join(root, "scripts", "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstSave, secondSave) {
		t.Errorf("migrating twice produced a different file than migrating once:\nfirst:\n%s\nsecond:\n%s", firstSave, secondSave)
	}
}

// TestLoadManifest_ReadOnlyDoesNotWriteMigratedFile mirrors the
// TestLoadConfigIsPure contract (readonly_contract_test.go): loading a
// legacy manifest transforms the in-memory struct only. Nothing on disk
// changes until a mutating command explicitly calls save().
func TestLoadManifest_ReadOnlyDoesNotWriteMigratedFile(t *testing.T) {
	root := t.TempDir()
	proj := t.TempDir()
	content := legacyManifestJSON(t, map[string]*ProjectEntry{
		"scriptorium": {Path: proj, Domain: "personal"},
	})
	path := writeManifestFile(t, root, content)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	m, err := loadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: the in-memory struct WAS migrated (else this test would pass
	// vacuously).
	if m.ManifestVersion != manifestPathKeyedVersion {
		t.Fatalf("loadManifest did not migrate in-memory struct: version = %d", m.ManifestVersion)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("loadManifest (read-only) wrote to disk:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestManifestSave_LogsMigrationOnce: the migratedCount counter that
// drives the one-line migration log is reset after the first save() that
// actually persists the migrated form, so a second save() in the same
// process doesn't re-trigger it.
func TestManifestSave_LogsMigrationOnce(t *testing.T) {
	root := t.TempDir()
	proj := t.TempDir()
	content := legacyManifestJSON(t, map[string]*ProjectEntry{
		"scriptorium": {Path: proj, Domain: "personal"},
	})
	writeManifestFile(t, root, content)

	m, err := loadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if m.migratedCount == 0 {
		t.Fatal("migratedCount should be >0 right after migrating a legacy manifest")
	}
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	if m.migratedCount != 0 {
		t.Errorf("migratedCount = %d after first save(), want 0 (reset so a second save doesn't re-log)", m.migratedCount)
	}
	// A second save must not panic or re-derive a nonzero migratedCount.
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	if m.migratedCount != 0 {
		t.Errorf("migratedCount = %d after second save(), want 0", m.migratedCount)
	}
}

// TestManifestResolve_ByPath: an entry looks up both by its exact
// canonical path and by a non-canonical (relative) spelling of the same
// directory.
func TestManifestResolve_ByPath(t *testing.T) {
	dir := t.TempDir()
	key := canonicalizePath(dir)
	entry := &ProjectEntry{Path: key, Name: "myproj"}
	m := &Manifest{Projects: map[string]*ProjectEntry{key: entry}}

	got, err := m.resolve(dir)
	if err != nil {
		t.Fatalf("resolve(%q): %v", dir, err)
	}
	if got != entry {
		t.Error("resolve by exact canonical path did not return the entry")
	}

	rel := filepath.Join(dir, "..", filepath.Base(dir))
	got2, err := m.resolve(rel)
	if err != nil {
		t.Fatalf("resolve(%q): %v", rel, err)
	}
	if got2 != entry {
		t.Error("resolve by non-canonical (relative) spelling did not return the entry")
	}
}

// TestManifestResolve_ByName: a short display Name resolves too.
func TestManifestResolve_ByName(t *testing.T) {
	entry := &ProjectEntry{Path: "/x/y", Name: "myproj"}
	m := &Manifest{Projects: map[string]*ProjectEntry{"/x/y": entry}}

	got, err := m.resolve("myproj")
	if err != nil {
		t.Fatalf("resolve(myproj): %v", err)
	}
	if got != entry {
		t.Error("resolve by Name did not return the entry")
	}
}

// TestManifestResolve_AmbiguousName: two entries sharing a Name (a
// hand-crafted fixture — uniqueName prevents this in practice) is a
// resolve error naming both paths.
func TestManifestResolve_AmbiguousName(t *testing.T) {
	m := &Manifest{Projects: map[string]*ProjectEntry{
		"/a": {Path: "/a", Name: "dup"},
		"/b": {Path: "/b", Name: "dup"},
	}}
	_, err := m.resolve("dup")
	if err == nil {
		t.Fatal("expected an ambiguous-name error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error %q does not mention ambiguous", err)
	}
	if !strings.Contains(err.Error(), "/a") || !strings.Contains(err.Error(), "/b") {
		t.Errorf("error %q does not name both paths", err)
	}
}

// TestManifestResolve_NotFound: an empty manifest's error points at
// `scribe projects list`.
func TestManifestResolve_NotFound(t *testing.T) {
	m := &Manifest{Projects: map[string]*ProjectEntry{}}
	_, err := m.resolve("nope")
	if err == nil {
		t.Fatal("expected a not-found error")
	}
	if !strings.Contains(err.Error(), "scribe projects list") {
		t.Errorf("error %q does not mention `scribe projects list`", err)
	}
}

// TestManifestUniqueName covers the three escalation tiers: passthrough
// (no collision), parent-qualified (base collides), and numeric suffix
// (even the parent-qualified form collides).
func TestManifestUniqueName(t *testing.T) {
	m := &Manifest{Projects: map[string]*ProjectEntry{}}

	// No existing entries: passthrough.
	if got := m.uniqueName("api", "/Users/x/Projects/api"); got != "api" {
		t.Errorf("passthrough = %q, want api", got)
	}

	// Seed a colliding entry at a DIFFERENT path.
	m.Projects["/Users/other/Projects/api"] = &ProjectEntry{Path: "/Users/other/Projects/api", Name: "api"}
	got := m.uniqueName("api", "/Users/x/work/api")
	want := "work-api" // parent-qualified
	if got != want {
		t.Errorf("parent-qualified = %q, want %q", got, want)
	}

	// Now seed the parent-qualified form too, at yet another path — even
	// that collides, so uniqueName must escalate to a numeric suffix.
	m.Projects["/Users/yet-another/work/api"] = &ProjectEntry{Path: "/Users/yet-another/work/api", Name: "work-api"}
	got2 := m.uniqueName("api", "/Users/x/work/api")
	if got2 != "api-2" {
		t.Errorf("double-collision = %q, want api-2", got2)
	}

	// A path that's already the canonical path of an EXISTING entry with
	// that exact name is not a collision with itself.
	m2 := &Manifest{Projects: map[string]*ProjectEntry{
		"/Users/x/Projects/api": {Path: "/Users/x/Projects/api", Name: "api"},
	}}
	if got := m2.uniqueName("api", "/Users/x/Projects/api"); got != "api" {
		t.Errorf("self-match = %q, want api (not a collision with its own entry)", got)
	}
}
