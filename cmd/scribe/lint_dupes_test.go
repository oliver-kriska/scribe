package main

import (
	"os"
	"path/filepath"
	"testing"
)

// setupDupKB builds a temp KB with a wiki/ dir holding a mix of canonical
// pages, self-ingestion duplicates, and a deliberately-ambiguous file that
// must NOT be touched. Returns the KB root.
func setupDupKB(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	wiki := filepath.Join(root, "wiki")
	if err := os.MkdirAll(wiki, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(wiki, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("readme.md", "# Readme\n")             // canonical
	write("readme.md.md", "# Readme dup\n")      // doubled-ext, canonical exists → remove
	write("readme_md.md", "# Readme slug dup\n") // slug-dot, canonical exists → remove
	write("orphan.md.md", "# Orphan\n")          // doubled-ext, NO canonical → rename
	write("guide_md.md", "# Legit guide_md\n")   // slug-dot but NO guide.md → leave alone
	write("normal-article.md", "# Normal\n")     // ordinary page → ignored
	write("_index.md", "# Index\n")              // generated → ignored
	return root
}

func TestFindSelfIngestionDuplicates(t *testing.T) {
	root := setupDupKB(t)
	dups := findSelfIngestionDuplicates(root)

	got := map[string]dupArtifact{}
	for _, d := range dups {
		got[filepath.Base(d.Path)] = d
	}

	// readme.md.md: doubled-ext, canonical present → remove.
	if d, ok := got["readme.md.md"]; !ok {
		t.Error("readme.md.md not detected")
	} else if d.Shape != "doubled-ext" || d.action() != "remove" {
		t.Errorf("readme.md.md: shape=%q action=%q, want doubled-ext/remove", d.Shape, d.action())
	}

	// readme_md.md: slug-dot, canonical readme.md present → remove.
	if d, ok := got["readme_md.md"]; !ok {
		t.Error("readme_md.md not detected")
	} else if d.Shape != "slug-dot" || d.action() != "remove" {
		t.Errorf("readme_md.md: shape=%q action=%q, want slug-dot/remove", d.Shape, d.action())
	}

	// orphan.md.md: doubled-ext, no canonical → rename.
	if d, ok := got["orphan.md.md"]; !ok {
		t.Error("orphan.md.md not detected")
	} else if d.action() != "rename" {
		t.Errorf("orphan.md.md: action=%q, want rename", d.action())
	}

	// Must NOT flag: legit slug without canonical, ordinary article, generated.
	for _, name := range []string{"guide_md.md", "normal-article.md", "_index.md", "readme.md"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s was wrongly flagged as a duplicate", name)
		}
	}
}

func TestFixSelfIngestionDuplicates(t *testing.T) {
	root := setupDupKB(t)
	wiki := filepath.Join(root, "wiki")

	removed, renamed := fixSelfIngestionDuplicates(root, false)
	if removed != 2 {
		t.Errorf("removed = %d, want 2 (readme.md.md, readme_md.md)", removed)
	}
	if renamed != 1 {
		t.Errorf("renamed = %d, want 1 (orphan.md.md)", renamed)
	}

	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(wiki, name))
		return err == nil
	}

	// Redundant duplicates gone; canonical untouched.
	if exists("readme.md.md") {
		t.Error("readme.md.md should have been removed")
	}
	if exists("readme_md.md") {
		t.Error("readme_md.md should have been removed")
	}
	if !exists("readme.md") {
		t.Error("canonical readme.md must survive")
	}
	// Orphan recovered under the valid name.
	if exists("orphan.md.md") {
		t.Error("orphan.md.md should have been renamed away")
	}
	if !exists("orphan.md") {
		t.Error("orphan.md.md should have been renamed to orphan.md")
	}
	// Untouched files.
	if !exists("guide_md.md") {
		t.Error("guide_md.md (legit slug, no canonical) must not be touched")
	}
	if !exists("normal-article.md") {
		t.Error("normal-article.md must not be touched")
	}
}

func TestFindSelfNamedDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("kb_name: testkb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mk := func(p string) {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk("wiki/testkb")            // self-named under wiki → flag
	mk("projects/testkb")        // self-named under projects → flag
	mk("wiki/legit-topic")       // ordinary → ignore
	mk("wiki/_sections/testkb")  // generated tree → ignore (skipped)
	mk("projects/other-project") // ordinary → ignore

	got := findSelfNamedDirs(root)
	want := map[string]bool{"projects/testkb": true, "wiki/testkb": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want keys %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected self-named dir flagged: %q", g)
		}
	}
}

func TestKeepPreferred(t *testing.T) {
	// Shallower path wins (canonical flat page over a nested/self-named copy).
	if !keepPreferred("wiki/cache.md", "wiki/scriptorium/cache.md") {
		t.Error("shallower path should be preferred")
	}
	if keepPreferred("wiki/scriptorium/cache.md", "wiki/cache.md") {
		t.Error("deeper path must not be preferred")
	}
	// Equal depth → lexicographically smaller wins (deterministic).
	if !keepPreferred("wiki/a.md", "wiki/b.md") {
		t.Error("lexicographically smaller should win at equal depth")
	}
}

// setupByteIdenticalKB builds a KB with one byte-identical pair across two
// depths, a same-body-different-frontmatter near-twin (NOT byte-identical), and
// a wholly distinct page.
func setupByteIdenticalKB(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	wiki := filepath.Join(root, "wiki")
	mustMkdir(t, filepath.Join(wiki, "scriptorium"))
	identical := "---\ntitle: Cache\n---\n\n# Cache\n\nshared cache body\n"
	mustWrite(t, filepath.Join(wiki, "cache.md"), identical)
	mustWrite(t, filepath.Join(wiki, "scriptorium", "cache.md"), identical)
	// Same normalized body, different frontmatter → must NOT be byte-identical.
	mustWrite(t, filepath.Join(wiki, "cache-copy.md"), "---\ntitle: Other\n---\n\n# Cache\n\nshared cache body\n")
	mustWrite(t, filepath.Join(wiki, "unrelated.md"), "---\ntitle: U\n---\n\n# U\n\nentirely different\n")
	return root
}

func TestFindByteIdenticalDuplicates(t *testing.T) {
	root := setupByteIdenticalKB(t)
	groups := findByteIdenticalDuplicates(root)
	if len(groups) != 1 {
		t.Fatalf("got %d byte-identical group(s), want 1: %v", len(groups), groups)
	}
	g := groups[0]
	if len(g) != 2 {
		t.Fatalf("group size = %d, want 2 (frontmatter-differing twin excluded): %v", len(g), g)
	}
	if g[0] != "wiki/cache.md" {
		t.Errorf("keep page = %q, want wiki/cache.md (shallower)", g[0])
	}
}

func TestFixByteIdenticalDuplicates(t *testing.T) {
	root := setupByteIdenticalKB(t)
	wiki := filepath.Join(root, "wiki")

	removed := fixByteIdenticalDuplicates(root, false)
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(wiki, rel))
		return err == nil
	}
	if !exists("cache.md") {
		t.Error("preferred (shallow) copy must survive")
	}
	if exists("scriptorium/cache.md") {
		t.Error("nested byte-identical copy should have been removed")
	}
	if !exists("cache-copy.md") {
		t.Error("frontmatter-differing twin must NOT be auto-removed (needs human judgment)")
	}

	// Dry run on a fresh KB writes nothing.
	root2 := setupByteIdenticalKB(t)
	if n := fixByteIdenticalDuplicates(root2, true); n != 1 {
		t.Errorf("dry-run count = %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(root2, "wiki", "scriptorium", "cache.md")); err != nil {
		t.Error("dry-run removed a file (should be untouched)")
	}
}

func TestFixSelfIngestionDuplicates_DryRunWritesNothing(t *testing.T) {
	root := setupDupKB(t)
	wiki := filepath.Join(root, "wiki")

	removed, renamed := fixSelfIngestionDuplicates(root, true)
	if removed != 2 || renamed != 1 {
		t.Errorf("dry-run counts = (%d, %d), want (2, 1)", removed, renamed)
	}
	// Dry run must leave every file in place.
	for _, name := range []string{"readme.md.md", "readme_md.md", "orphan.md.md", "readme.md"} {
		if _, err := os.Stat(filepath.Join(wiki, name)); err != nil {
			t.Errorf("dry-run removed/renamed %s (should be untouched): %v", name, err)
		}
	}
}
