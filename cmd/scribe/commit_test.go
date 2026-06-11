package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// commitTestKB builds a team-mode KB fixture with a baseline commit so
// commitRun has a HEAD to diff against and the secret gate is armed.
func commitTestKB(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := initTestGitRepo(t, "Commit Tester")
	writeTestArticle(t, root, "scribe.yaml", "owner: t\nteam: true\n")
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "baseline", "--no-gpg-sign"}} {
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

func headSubject(t *testing.T, root string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "log", "-1", "--format=%s")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestCommitMessageCountsStagedFilesOnly pins the count to the commit's
// real contents: when the secret gate holds one of two wiki files, the
// subject must say "(1 wiki)" — the pre-stage worktree count shipped
// "(2 wiki)" on a one-file commit.
func TestCommitMessageCountsStagedFilesOnly(t *testing.T) {
	root := commitTestKB(t)
	article := func(title, body string) string {
		return "---\ntitle: \"" + title + "\"\ntype: research\ndomain: general\ncreated: 2026-06-11\nupdated: 2026-06-11\nconfidence: low\ntags: []\nrelated: []\nsources: []\n---\n\n" + body + "\n"
	}
	writeTestArticle(t, root, "wiki/clean.md", article("Clean", "Nothing sensitive."))
	writeTestArticle(t, root, "wiki/leaky.md", article("Leaky", "cred "+fakeAWSKey()+" here"))

	if err := commitRun(root); err != nil {
		t.Fatalf("commitRun: %v", err)
	}
	if got := headSubject(t, root); !strings.Contains(got, "(1 wiki)") {
		t.Errorf("subject = %q, want count of the one staged file '(1 wiki)'", got)
	}
	// The held file stays in the worktree, uncommitted.
	cmd := exec.CommandContext(context.Background(), "git", "ls-files", "wiki/leaky.md")
	cmd.Dir = root
	if out, _ := cmd.Output(); strings.TrimSpace(string(out)) != "" {
		t.Error("held file was committed")
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "leaky.md")); err != nil {
		t.Error("held file vanished from worktree")
	}
}

// TestCommitHeldOnlyChangeDoesNotError: when the ONLY pending change is
// held by the gate, the staged set is empty — commitRun must return nil
// without attempting an empty git commit (which errors).
func TestCommitHeldOnlyChangeDoesNotError(t *testing.T) {
	root := commitTestKB(t)
	before := headSubject(t, root)
	writeTestArticle(t, root, "wiki/leaky.md",
		"---\ntitle: \"L\"\ntype: research\ndomain: general\ncreated: 2026-06-11\nupdated: 2026-06-11\nconfidence: low\ntags: []\nrelated: []\nsources: []\n---\n\ncred "+fakeAWSKey()+"\n")

	if err := commitRun(root); err != nil {
		t.Fatalf("held-only change must not error, got: %v", err)
	}
	if got := headSubject(t, root); got != before {
		t.Errorf("HEAD moved to %q; nothing should have been committed", got)
	}
}

// TestCommitCategoryCounts covers the raw/config buckets through the
// staged-set counter.
func TestCommitCategoryCounts(t *testing.T) {
	root := commitTestKB(t)
	writeTestArticle(t, root, "raw/articles/r.md", "raw body\n")
	if err := os.WriteFile(filepath.Join(root, "log.md"), []byte("- line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitRun(root); err != nil {
		t.Fatalf("commitRun: %v", err)
	}
	got := headSubject(t, root)
	if !strings.Contains(got, "1 raw") || !strings.Contains(got, "1 config") {
		t.Errorf("subject = %q, want '1 raw' and '1 config'", got)
	}
}
