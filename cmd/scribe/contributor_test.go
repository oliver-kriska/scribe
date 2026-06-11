package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initTestGitRepo creates a git repo in a temp dir with a local
// user.name so resolveContributor has an identity without touching the
// developer's global config.
//
// The repo is nested two levels below TempDir: manifest.isIgnored
// rejects paths shallower than 4 segments, and Linux TempDir is
// /tmp/TestX/001 — a fixture repo at TempDir itself silently falls
// below discovery's depth floor (the Linux-only CI failure class of
// 2026-06-10). Nesting HERE kills that class for every caller.
func initTestGitRepo(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "projects", "proj")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.name", name},
		{"config", "user.email", "test@example.com"},
	} {
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

func writeTestArticle(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStampContributor(t *testing.T) {
	// Point user config at an empty dir so a developer's real
	// ~/.config/scribe/config.yaml can't leak into the test.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := initTestGitRepo(t, "Test User")

	article := "---\ntitle: \"New Article\"\ntype: research\ndomain: general\n---\n\nBody.\n"
	withContrib := "---\ntitle: \"Authored\"\ntype: research\ncontributor: 'Someone Else'\n---\n\nBody.\n"
	noFM := "# Plain heading\n\nNo frontmatter here.\n"

	writeTestArticle(t, root, "wiki/new-article.md", article)
	writeTestArticle(t, root, "wiki/authored.md", withContrib)
	writeTestArticle(t, root, "wiki/plain.md", noFM)
	writeTestArticle(t, root, "wiki/_index.md", "---\ntitle: index\n---\n")
	writeTestArticle(t, root, "research/deep-dive.md", article)

	// A tracked, committed article must never be re-stamped.
	writeTestArticle(t, root, "wiki/old-article.md", article)
	for _, args := range [][]string{{"add", "wiki/old-article.md"}, {"commit", "-q", "-m", "seed", "--no-gpg-sign"}} {
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	stampContributor(root)

	tests := []struct {
		rel  string
		want string // substring that must be present
		not  string // substring that must be absent
	}{
		{rel: "wiki/new-article.md", want: "contributor: 'Test User'"},
		{rel: "research/deep-dive.md", want: "contributor: 'Test User'"},
		{rel: "wiki/authored.md", want: "contributor: 'Someone Else'", not: "Test User"},
		{rel: "wiki/plain.md", not: "contributor"},
		{rel: "wiki/_index.md", not: "contributor"},
		{rel: "wiki/old-article.md", not: "contributor"},
	}
	for _, tt := range tests {
		raw, err := os.ReadFile(filepath.Join(root, tt.rel))
		if err != nil {
			t.Fatalf("%s: %v", tt.rel, err)
		}
		got := string(raw)
		if tt.want != "" && !strings.Contains(got, tt.want) {
			t.Errorf("%s: missing %q in:\n%s", tt.rel, tt.want, got)
		}
		if tt.not != "" && strings.Contains(got, tt.not) {
			t.Errorf("%s: unexpected %q in:\n%s", tt.rel, tt.not, got)
		}
	}

	// Stamped frontmatter must stay parseable.
	raw, _ := os.ReadFile(filepath.Join(root, "wiki/new-article.md"))
	fm, err := parseFrontmatter(raw)
	if err != nil {
		t.Fatalf("stamped frontmatter unparseable: %v", err)
	}
	if fm.Contributor != "Test User" {
		t.Errorf("Contributor = %q, want %q", fm.Contributor, "Test User")
	}
	if fm.Title != "New Article" {
		t.Errorf("Title = %q — stamping corrupted other fields", fm.Title)
	}
}

func TestStampContributorIdempotent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := initTestGitRepo(t, "Test User")
	writeTestArticle(t, root, "wiki/a.md", "---\ntitle: A\ntype: research\n---\nBody.\n")

	stampContributor(root)
	first, _ := os.ReadFile(filepath.Join(root, "wiki/a.md"))
	stampContributor(root)
	second, _ := os.ReadFile(filepath.Join(root, "wiki/a.md"))
	if string(first) != string(second) {
		t.Errorf("second stamp changed the file:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if n := strings.Count(string(second), "contributor:"); n != 1 {
		t.Errorf("contributor key count = %d, want 1", n)
	}
}

func TestResolveContributorUserConfigOverride(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	if err := os.MkdirAll(filepath.Join(cfgHome, "scribe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgHome, "scribe", "config.yaml"),
		[]byte("contributor: \"Override Name\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := initTestGitRepo(t, "Git Name")
	if got := resolveContributor(root); got != "Override Name" {
		t.Errorf("resolveContributor = %q, want user-config override %q", got, "Override Name")
	}
}

func TestResolveContributorGitFallback(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := initTestGitRepo(t, "Git Name")
	if got := resolveContributor(root); got != "Git Name" {
		t.Errorf("resolveContributor = %q, want %q", got, "Git Name")
	}
}

func TestYamlSingleQuote(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Oliver", "'Oliver'"},
		{"Tim O'Brien", "'Tim O''Brien'"},
		{"Team: Platform", "'Team: Platform'"},
	}
	for _, tt := range tests {
		if got := yamlSingleQuote(tt.in); got != tt.want {
			t.Errorf("yamlSingleQuote(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNewWikiMarkdownFilesStatusParsing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := initTestGitRepo(t, "Test User")
	writeTestArticle(t, root, "wiki/untracked.md", "---\ntitle: U\n---\n")
	writeTestArticle(t, root, "wiki/staged.md", "---\ntitle: S\n---\n")
	writeTestArticle(t, root, "wiki/notes.txt", "not markdown")
	cmd := exec.CommandContext(context.Background(), "git", "add", "wiki/staged.md")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}

	got := newWikiMarkdownFiles(root)
	want := map[string]bool{"wiki/untracked.md": true, "wiki/staged.md": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want files %v", got, want)
	}
	for _, f := range got {
		if !want[f] {
			t.Errorf("unexpected file %q in %v", f, got)
		}
	}
}
