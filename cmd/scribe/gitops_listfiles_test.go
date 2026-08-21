package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// buildIgnoreRepo lays out a repo shaped like the one in issue #86: a
// handful of real docs plus a gitignored dependency tree stuffed with
// files that match extractScanPatterns.
func buildIgnoreRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "config", "user.email", "test@example.com")
	gitRun(t, repo, "config", "user.name", "Test")
	// These assertions are the first in the suite that depend on what
	// --exclude-standard considers ignored, so the maintainer's global
	// excludes file would silently change the expected set. Signing is
	// on globally on at least one dev machine and would block the seed
	// commit wherever a key is not usable non-interactively.
	gitRun(t, repo, "config", "core.excludesFile", os.DevNull)
	gitRun(t, repo, "config", "commit.gpgsign", "false")

	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(".gitignore", ".venv/\nbuild/\n")
	write("README.md", "# readme")
	write("docs/design.md", "# design")
	write("notes.txt", "notes")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "seed")

	// Untracked but NOT ignored — a newly written doc the user has not
	// committed yet still belongs in a first extraction.
	write("docs/new-untracked.md", "# fresh")

	// Ignored dependency trees, exactly the shape that inflated the count.
	// 150 is comfortably past the default max_extract_files of 100.
	for i := range 150 {
		write(fmt.Sprintf(".venv/lib/site-packages/pkg%03d/README.md", i), "x")
	}
	write("build/generated.txt", "x")

	return repo
}

func TestGitListFiles_HonorsGitignore(t *testing.T) {
	repo := buildIgnoreRepo(t)

	got, ok := gitListFiles(repo, extractScanPatterns)
	if !ok {
		t.Fatalf("gitListFiles should succeed on a real repo")
	}

	rel := make([]string, 0, len(got))
	for _, f := range got {
		r, err := filepath.Rel(repo, f)
		if err != nil {
			t.Fatalf("Rel(%q): %v", f, err)
		}
		rel = append(rel, filepath.ToSlash(r))
	}
	slices.Sort(rel)

	want := []string{"README.md", "docs/design.md", "docs/new-untracked.md", "notes.txt"}
	if !slices.Equal(rel, want) {
		t.Fatalf("gitListFiles = %v, want %v", rel, want)
	}

	for _, r := range rel {
		if strings.HasPrefix(r, ".venv/") || strings.HasPrefix(r, "build/") {
			t.Fatalf("ignored path leaked into the result: %s", r)
		}
	}
}

// The bug: findFiles counts the gitignored tree, so the same repo trips
// sync.max_extract_files while gitListFiles stays well under it.
func TestGitChangedFiles_FirstExtractionIgnoresGitignored(t *testing.T) {
	repo := buildIgnoreRepo(t)
	cfg := &ScribeConfig{Sync: SyncConfig{MaxExtractFiles: 100}}

	changed := gitChangedFiles(repo, "", extractScanPatterns)
	if len(changed) != 4 {
		// Print a bounded sample: on regression this list is 150+ entries
		// of absolute temp paths and floods the test log.
		t.Fatalf("first extraction counted %d files, want 4 (first few: %v)",
			len(changed), changed[:min(5, len(changed))])
	}
	if exceedsExtractFileCap(cfg, len(changed)) {
		t.Fatalf("%d files should not trip max_extract_files=100", len(changed))
	}

	// Guard the regression itself: the old filesystem walk still sees the
	// ignored tree, so this test fails loudly if the fallback is ever
	// restored as the primary path.
	walked := findFiles(repo, extractScanPatterns)
	if len(walked) <= len(changed) {
		t.Fatalf("expected findFiles to over-count (got %d vs %d); "+
			"fixture no longer reproduces issue #86", len(walked), len(changed))
	}
	if !exceedsExtractFileCap(cfg, len(walked)) {
		t.Fatalf("fixture should trip the cap via findFiles (%d files)", len(walked))
	}
}

// A linked worktree's .git is a regular file, not a directory, so a
// hasGit stat reads it as "not a repo" and drops first extraction back to
// findFiles — reinstating this bug for exactly the worktree entries
// scripts/projects.json still carries from before discovery folded them
// into their main checkout.
func TestGitListFiles_LinkedWorktreeIsStillAGitRepo(t *testing.T) {
	repo := buildIgnoreRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gitRun(t, repo, "worktree", "add", "-q", "-b", "probe", wt)

	if hasGit(wt) {
		t.Fatal("fixture no longer reproduces: a linked worktree's .git should be a file, not a directory")
	}

	// The worktree only carries the tracked files; give it its own
	// ignored tree and its own uncommitted doc so both halves of the
	// rule are exercised where the checkout actually is.
	writeTestArticle(t, wt, ".venv/lib/site-packages/pkg/README.md", "x")
	writeTestArticle(t, wt, "docs/new-in-worktree.md", "# fresh")

	got, ok := gitListFiles(wt, extractScanPatterns)
	if !ok {
		t.Fatalf("linked worktree should be listable by git")
	}

	rel := make([]string, 0, len(got))
	for _, f := range got {
		r, err := filepath.Rel(wt, f)
		if err != nil {
			t.Fatalf("Rel(%q): %v", f, err)
		}
		rel = append(rel, filepath.ToSlash(r))
	}
	slices.Sort(rel)

	want := []string{"README.md", "docs/design.md", "docs/new-in-worktree.md", "notes.txt"}
	if !slices.Equal(rel, want) {
		t.Fatalf("gitListFiles in worktree = %v, want %v", rel, want)
	}
}

func TestGitListFiles_NonRepoFallsBack(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, ok := gitListFiles(dir, extractScanPatterns); ok {
		t.Fatalf("non-repo should report ok=false so the caller falls back")
	}

	// gitChangedFiles must still return the file via findFiles.
	got := gitChangedFiles(dir, "", extractScanPatterns)
	if len(got) != 1 || filepath.Base(got[0]) != "a.md" {
		t.Fatalf("non-repo first extraction = %v, want [a.md]", got)
	}
}

func TestGitListFiles_EmptyRepoIsNotAFailure(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")

	got, ok := gitListFiles(repo, extractScanPatterns)
	if !ok {
		t.Fatalf("an empty repo is still a repo; ok should be true")
	}
	if len(got) != 0 {
		t.Fatalf("empty repo should list no files, got %v", got)
	}
}
