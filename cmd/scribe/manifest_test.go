package main

import (
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
	if m.Projects["scriptorium"].LastSHA != "abc123" {
		t.Errorf("last_sha = %q", m.Projects["scriptorium"].LastSHA)
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
