package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// lintTestKB scaffolds a KB root that kbDir() resolves via SCRIBE_KB and
// that lint's structural phases can walk: scribe.yaml marker + wiki dirs.
func lintTestKB(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("domains: [acme]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"wiki", "solutions", "scripts"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("SCRIBE_KB", root)
	return root
}

// lintValidArticle renders a frontmatter-complete article that passes
// validateFile with no size warnings (>=15 lines, <=150, index_tier set).
func lintValidArticle(title string, bodyLines int, extra ...string) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "title: %q\n", title)
	sb.WriteString("type: solution\n")
	sb.WriteString("created: 2026-04-10\n")
	sb.WriteString("updated: 2026-04-10\n")
	sb.WriteString("domain: general\n")
	sb.WriteString("confidence: high\n")
	sb.WriteString("tags: [tag1]\n")
	sb.WriteString("related: []\n")
	sb.WriteString(`sources: ["source1"]` + "\n")
	sb.WriteString(`problem: "p"` + "\n")
	sb.WriteString("index_tier: standard\n")
	for _, e := range extra {
		sb.WriteString(e + "\n")
	}
	sb.WriteString("---\n\n")
	for i := 0; i < bodyLines; i++ {
		fmt.Fprintf(&sb, "Body line %d of %s.\n", i, title)
	}
	return sb.String()
}

// captureLintStdout runs fn with os.Stdout redirected into a pipe and
// returns everything it printed.
func captureLintStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	fn()
	_ = w.Close()
	os.Stdout = orig
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestLintTargetFiles(t *testing.T) {
	root := lintTestKB(t)
	writeKBFile(t, root, "wiki/a.md", lintValidArticle("A Article", 20))
	writeKBFile(t, root, "solutions/b.md", lintValidArticle("B Article", 20))
	writeKBFile(t, root, "wiki/_index.md", "- [[A Article]]\n")

	t.Run("explicit args win", func(t *testing.T) {
		l := &LintCmd{Files: []string{"/some/explicit.md"}}
		files, err := l.targetFiles(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 || files[0] != "/some/explicit.md" {
			t.Errorf("explicit files not passed through: %v", files)
		}
	})

	t.Run("default walks articles, skipping meta", func(t *testing.T) {
		l := &LintCmd{}
		files, err := l.targetFiles(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 2 {
			t.Fatalf("want 2 articles, got %d: %v", len(files), files)
		}
		for _, f := range files {
			if strings.Contains(f, "_index") {
				t.Errorf("underscore meta file leaked into scope: %s", f)
			}
		}
	})
}

func TestLintFrontmatter(t *testing.T) {
	root := lintTestKB(t)
	good := filepath.Join(root, "wiki", "good.md")
	bad := filepath.Join(root, "wiki", "bad.md")
	writeKBFile(t, root, "wiki/good.md", lintValidArticle("Good", 20))
	writeKBFile(t, root, "wiki/bad.md", "---\ntitle: \"Bad\"\ntype: solution\n---\n\nBody.\n")

	var errors int
	out := captureLintStdout(t, func() {
		rep := newLintReport(os.Stdout, false, false)
		lintFrontmatter(rep, root, []string{good, bad})
		errors = rep.errors
	})
	// A file with several missing fields still counts once.
	if errors != 1 {
		t.Errorf("want 1 file with errors, got %d", errors)
	}
	if !strings.Contains(out, "wiki/bad.md") {
		t.Errorf("error output missing offending file:\n%s", out)
	}
	if strings.Contains(out, "good.md") {
		t.Errorf("clean file flagged:\n%s", out)
	}
}

func TestLintSizes(t *testing.T) {
	root := lintTestKB(t)

	cases := []struct {
		name     string
		rel      string
		content  string
		warnings int
		wantMsg  string
	}{
		{
			name: "thin article",
			rel:  "wiki/thin.md",
			// Size phase counts total lines including frontmatter, so a
			// minimal frontmatter keeps the file under the 15-line floor.
			content:  "---\ntitle: \"Thin\"\nindex_tier: stub\n---\n\nOne line.\n",
			warnings: 1,
			wantMsg:  "thin article",
		},
		{
			name:     "bloated article",
			rel:      "wiki/bloat.md",
			content:  lintValidArticle("Bloat", 160),
			warnings: 1,
			wantMsg:  "bloated article",
		},
		{
			name:     "good article clean",
			rel:      "wiki/good.md",
			content:  lintValidArticle("Good", 30),
			warnings: 0,
		},
		{
			name:     "missing index_tier warns",
			rel:      "wiki/notier.md",
			content:  strings.Replace(lintValidArticle("NoTier", 30), "index_tier: standard\n", "", 1),
			warnings: 1,
			wantMsg:  "index_tier missing",
		},
		{
			name:     "overgrown rolling file warns",
			rel:      "wiki/learnings.md",
			content:  "---\ntitle: \"Learnings\"\nrolling: true\n---\n\n" + strings.Repeat("entry\n", 210),
			warnings: 1,
			wantMsg:  "rolling file",
		},
		{
			name:     "rolling archive exempt from size warning",
			rel:      "wiki/learnings-archive-2025.md",
			content:  "---\ntitle: \"Learnings Archive\"\nrolling: true\n---\n\n" + strings.Repeat("entry\n", 210),
			warnings: 0,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			writeKBFile(t, root, tt.rel, tt.content)
			var got int
			out := captureLintStdout(t, func() {
				// Verbose mode prints each warning's message inline so the
				// per-class wantMsg substrings appear in the captured output;
				// default mode would defer them to the grouped flush().
				rep := newLintReport(os.Stdout, true, false)
				lintSizes(rep, root, []string{filepath.Join(root, tt.rel)})
				got = rep.warnings
			})
			if got != tt.warnings {
				t.Errorf("warnings = %d, want %d\noutput:\n%s", got, tt.warnings, out)
			}
			if tt.wantMsg != "" && !strings.Contains(out, tt.wantMsg) {
				t.Errorf("output missing %q:\n%s", tt.wantMsg, out)
			}
		})
	}
}

func TestLintOrphans(t *testing.T) {
	root := lintTestKB(t)
	// A links to B and to a missing page D. C has no inbound links.
	writeKBFile(t, root, "wiki/a.md", lintValidArticle("A Article", 14)+"\nSee [[B Article]] and [[Missing Page]].\n")
	writeKBFile(t, root, "wiki/b.md", lintValidArticle("B Article", 20))
	writeKBFile(t, root, "wiki/c.md", lintValidArticle("C Article", 20))

	var warnings int
	out := captureLintStdout(t, func() {
		rep := newLintReport(os.Stdout, false, false)
		lintOrphans(rep, root)
		warnings = rep.warnings
	})
	// One warning for orphans (A + C have no inbound links), one for the
	// missing page.
	if warnings != 2 {
		t.Errorf("warnings = %d, want 2\noutput:\n%s", warnings, out)
	}
	if !strings.Contains(out, "2 orphan articles") {
		t.Errorf("expected 2 orphan articles, output:\n%s", out)
	}
	if !strings.Contains(out, "1 missing pages") {
		t.Errorf("expected 1 missing page, output:\n%s", out)
	}
}

func TestLintOrphans_AliasResolvesLink(t *testing.T) {
	root := lintTestKB(t)
	// B is referenced only by its alias — must not count as a missing page,
	// and the alias-targeted link gives the alias entry an inbound link.
	writeKBFile(t, root, "wiki/a.md", lintValidArticle("A Article", 14)+"\nSee [[Bee]].\n")
	writeKBFile(t, root, "wiki/b.md", lintValidArticle("B Article", 20, "aliases: [Bee]"))

	out := captureLintStdout(t, func() {
		rep := newLintReport(os.Stdout, false, false)
		lintOrphans(rep, root)
	})
	if strings.Contains(out, "missing pages") {
		t.Errorf("alias-linked page reported missing:\n%s", out)
	}
}

func TestLintIndexConsistency(t *testing.T) {
	root := lintTestKB(t)
	writeKBFile(t, root, "wiki/a.md", lintValidArticle("A Article", 20))
	writeKBFile(t, root, "wiki/b.md", lintValidArticle("B Article", 20))

	indexWarnings := func(t *testing.T) (int, string) {
		t.Helper()
		var w int
		out := captureLintStdout(t, func() {
			rep := newLintReport(os.Stdout, false, false)
			lintIndexConsistency(rep, root)
			w = rep.warnings
		})
		return w, out
	}

	t.Run("no index file is silent", func(t *testing.T) {
		w, _ := indexWarnings(t)
		if w != 0 {
			t.Errorf("warnings = %d, want 0 when index missing", w)
		}
	})

	t.Run("small drift tolerated", func(t *testing.T) {
		writeKBFile(t, root, "wiki/_index.md", "- [[A Article]] -- s\n")
		w, out := indexWarnings(t)
		if w != 0 {
			t.Errorf("warnings = %d, want 0 for diff of 1\noutput:\n%s", w, out)
		}
	})

	t.Run("large drift warns", func(t *testing.T) {
		// 6 index entries vs 2 disk articles → diff -4.
		var sb strings.Builder
		for i := 0; i < 6; i++ {
			fmt.Fprintf(&sb, "- [[Article %d]] -- s\n", i)
		}
		writeKBFile(t, root, "wiki/_index.md", sb.String())
		w, out := indexWarnings(t)
		if w != 1 {
			t.Errorf("warnings = %d, want 1\noutput:\n%s", w, out)
		}
		if !strings.Contains(out, "diff: -4") {
			t.Errorf("expected diff -4 in output:\n%s", out)
		}
	})

	t.Run("second wikilink on a line not double counted", func(t *testing.T) {
		writeKBFile(t, root, "wiki/_index.md",
			"- [[A Article]] -- relates to [[B Article]] closely\n- [[B Article]] -- s\n")
		_, out := indexWarnings(t)
		if !strings.Contains(out, "index: 2 entries, disk: 2 articles") {
			t.Errorf("inline second wikilink miscounted:\n%s", out)
		}
	})
}

func TestLintConflictMarkers(t *testing.T) {
	root := lintTestKB(t)
	writeKBFile(t, root, "wiki/clean.md", lintValidArticle("Clean", 20))
	writeKBFile(t, root, "wiki/broken.md", "# B\n<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> origin/main\n")

	var errors int
	out := captureLintStdout(t, func() {
		rep := newLintReport(os.Stdout, false, false)
		lintConflictMarkers(rep, root)
		errors = rep.errors
	})
	if errors != 1 {
		t.Errorf("errors = %d, want 1\noutput:\n%s", errors, out)
	}
	if !strings.Contains(out, "wiki/broken.md:2") {
		t.Errorf("expected file:line in output:\n%s", out)
	}
}

func TestLintRun_StructuralPassAndFail(t *testing.T) {
	t.Run("clean KB passes", func(t *testing.T) {
		root := lintTestKB(t)
		writeKBFile(t, root, "wiki/a.md", lintValidArticle("A Article", 14)+"\nSee [[B Article]].\n")
		writeKBFile(t, root, "wiki/b.md", lintValidArticle("B Article", 14)+"\nSee [[A Article]].\n")
		writeKBFile(t, root, "wiki/_index.md", "- [[A Article]] -- s\n- [[B Article]] -- s\n")

		l := &LintCmd{}
		var err error
		out := captureLintStdout(t, func() { err = l.Run() })
		if err != nil {
			t.Fatalf("Run on clean KB: %v\noutput:\n%s", err, out)
		}
		if !strings.Contains(out, "PASSED") {
			t.Errorf("expected PASSED summary:\n%s", out)
		}
	})

	t.Run("frontmatter error fails the run", func(t *testing.T) {
		root := lintTestKB(t)
		writeKBFile(t, root, "wiki/bad.md", "---\ntitle: \"Bad\"\n---\n\nBody.\n")

		l := &LintCmd{}
		var err error
		captureLintStdout(t, func() { err = l.Run() })
		if err == nil {
			t.Fatal("Run should fail on frontmatter errors")
		}
		if !strings.Contains(err.Error(), "FAILED") {
			t.Errorf("error should carry FAILED summary, got: %v", err)
		}
	})

	t.Run("changed with no changes is a no-op", func(t *testing.T) {
		root := lintTestKB(t)
		gitQuick(t, root, "init")
		l := &LintCmd{Changed: true}
		var err error
		out := captureLintStdout(t, func() { err = l.Run() })
		if err != nil {
			t.Fatalf("Run --changed on empty repo: %v", err)
		}
		if !strings.Contains(out, "no changed wiki files") {
			t.Errorf("expected no-op message:\n%s", out)
		}
	})
}

// gitQuick runs a git command in dir, failing the test on error.
func gitQuick(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:noctx // test fixture, no cancellation needed
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t",
		"HOME="+dir, // ignore user gitconfig (hooks, signing)
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestChangedWikiFiles(t *testing.T) {
	root := lintTestKB(t)
	writeKBFile(t, root, "wiki/tracked.md", lintValidArticle("Tracked", 20))
	gitQuick(t, root, "init")
	gitQuick(t, root, "add", ".")
	gitQuick(t, root, "commit", "-m", "seed")

	// Modify the tracked file, add an untracked one, and an underscore
	// meta file that must be filtered out.
	writeKBFile(t, root, "wiki/tracked.md", lintValidArticle("Tracked", 22))
	writeKBFile(t, root, "wiki/untracked.md", lintValidArticle("Untracked", 20))
	writeKBFile(t, root, "wiki/_index.md", "- [[Tracked]]\n")
	writeKBFile(t, root, "wiki/notes.txt", "not markdown")

	files, err := changedWikiFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(files))
	for _, f := range files {
		got[relPath(root, f)] = true
	}
	if !got["wiki/tracked.md"] {
		t.Errorf("modified tracked file missing: %v", files)
	}
	if !got["wiki/untracked.md"] {
		t.Errorf("untracked file missing: %v", files)
	}
	if got["wiki/_index.md"] {
		t.Errorf("underscore meta file should be filtered: %v", files)
	}
	if len(files) != 2 {
		t.Errorf("want exactly 2 files, got %d: %v", len(files), files)
	}
}
