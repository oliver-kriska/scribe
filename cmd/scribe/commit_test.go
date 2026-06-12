package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// commitTestKB builds a team-mode KB fixture with a baseline commit so
// commitRun has a HEAD to diff against and the secret gate is armed.
//
// lock_dir MUST be isolated: the default is /tmp, the same global
// namespace a real scribe install uses — without this, a cron sync
// running on the developer machine holds /tmp/scribe-sync.lock and
// every commitRun here backs off with "blocked by active sync process"
// (exactly how these tests first failed on 2026-06-11).
func commitTestKB(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := initTestGitRepo(t, "Commit Tester")
	writeTestArticle(t, root, "scribe.yaml", "owner: t\nteam: true\nlock_dir: "+t.TempDir()+"\n")
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

// TestCommitRefusesOnUnparseableConfig pins the fail-closed contract:
// a scribe.yaml that doesn't parse (duplicate key — one bad merge away
// in a team KB) falls back to defaults with team=false, which used to
// walk straight past the secret gate. E2E-proven leak: a credential was
// committed AND pushed that way. With LoadErr set, holdSecretFiles must
// refuse and the commit must not happen.
func TestCommitRefusesOnUnparseableConfig(t *testing.T) {
	root := commitTestKB(t)
	before := headSubject(t, root)
	// Duplicate key → yaml.Unmarshal error → defaults (team=false).
	writeTestArticle(t, root, "scribe.yaml", "owner: t\nteam: true\nteam: true\n")
	writeTestArticle(t, root, "wiki/leaky.md",
		"---\ntitle: \"L\"\ntype: research\ndomain: general\ncreated: 2026-06-11\nupdated: 2026-06-11\nconfidence: low\ntags: []\nrelated: []\nsources: []\n---\n\ncred "+fakeAWSKey()+"\n")

	if err := commitRun(root); err != nil {
		t.Fatalf("commitRun must skip cleanly, got: %v", err)
	}
	if got := headSubject(t, root); got != before {
		t.Errorf("HEAD moved to %q — broken config must fail CLOSED, not commit on defaults", got)
	}
	// The file may stay STAGED (the refusal happens before any commit;
	// the next run re-gates the index) — but it must not be in HEAD.
	cmd := exec.CommandContext(context.Background(), "git", "ls-tree", "-r", "--name-only", "HEAD")
	cmd.Dir = root
	if out, _ := cmd.Output(); strings.Contains(string(out), "leaky.md") {
		t.Error("credential file reached a commit despite unparseable config")
	}
	// Assert the gate contract directly too: a broken config also loses
	// its lock_dir override (defaults to /tmp), so on a dev machine with
	// a live scribe cron the commitRun above may have been blocked by a
	// process lock instead of the gate — a vacuous pass. This check is
	// lock-independent.
	cfg := loadConfig(root)
	if cfg.LoadErr == nil {
		t.Fatal("fixture scribe.yaml must fail to parse")
	}
	if holdSecretFiles(root, cfg) {
		t.Error("holdSecretFiles must fail closed when the config is unparseable")
	}
}

// logRecorder is a slog.Handler that keeps every message in memory so
// tests can assert on operator-facing log lines (logMsg routes through
// slog.Default). Attrs/groups are irrelevant for these assertions.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *logRecorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, rec.Message)
	return nil
}
func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *logRecorder) WithGroup(string) slog.Handler      { return r }
func (r *logRecorder) joined() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// recordLogs swaps the default slog logger for an in-memory recorder
// for the duration of the test. Safe without t.Parallel(): serial tests
// never overlap, and the recorder itself is mutex-guarded.
func recordLogs(t *testing.T) func() string {
	t.Helper()
	rec := &logRecorder{}
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return rec.joined
}

// TestCommitSecretGateHoldThenAllowMarker automates PR #1's manual
// verification #1 end-to-end on the commit path: a team-mode KB with a
// runtime-assembled AWS-shaped token in an article must log SECRET HELD
// and keep the file out of the commit; after a human marks the line
// with `scribe:allow`, the very same file commits.
func TestCommitSecretGateHoldThenAllowMarker(t *testing.T) {
	root := commitTestKB(t)
	logs := recordLogs(t)
	before := headSubject(t, root)

	article := func(body string) string {
		return "---\ntitle: \"Leaky\"\ntype: research\ndomain: general\ncreated: 2026-06-12\nupdated: 2026-06-12\nconfidence: low\ntags: []\nrelated: []\nsources: []\n---\n\n" + body + "\n"
	}
	leakyLine := "the deploy used " + fakeAWSKey() + " for staging"
	writeTestArticle(t, root, "wiki/leaky.md", article(leakyLine))

	// Run 1: the gate holds the file, loudly, and commits nothing.
	if err := commitRun(root); err != nil {
		t.Fatalf("commitRun with held file must not error: %v", err)
	}
	got := logs()
	if !strings.Contains(got, "SECRET HELD: wiki/leaky.md:") {
		t.Errorf("no SECRET HELD line for wiki/leaky.md; logs:\n%s", got)
	}
	if !strings.Contains(got, "[AWS Access Key ID]") {
		t.Errorf("SECRET HELD line missing the rule label; logs:\n%s", got)
	}
	if strings.Contains(got, fakeAWSKey()) {
		t.Fatalf("log output leaked the credential value:\n%s", got)
	}
	if subj := headSubject(t, root); subj != before {
		t.Errorf("HEAD moved to %q — held-only change must not commit", subj)
	}
	lsFiles := exec.CommandContext(context.Background(), "git", "ls-files", "wiki/leaky.md")
	lsFiles.Dir = root
	if out, _ := lsFiles.Output(); strings.TrimSpace(string(out)) != "" {
		t.Error("held file reached the index/commit")
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "leaky.md")); err != nil {
		t.Fatal("held file must stay in the worktree for the human to resolve")
	}

	// Run 2: the human reviews and marks the line as a placeholder.
	writeTestArticle(t, root, "wiki/leaky.md", article(leakyLine+" <!-- scribe:allow -->"))
	if err := commitRun(root); err != nil {
		t.Fatalf("commitRun after scribe:allow: %v", err)
	}
	if subj := headSubject(t, root); !strings.Contains(subj, "(1 wiki)") {
		t.Errorf("allow-marked file did not commit; HEAD subject = %q", subj)
	}
	show := exec.CommandContext(context.Background(), "git", "show", "HEAD:wiki/leaky.md")
	show.Dir = root
	out, err := show.Output()
	if err != nil {
		t.Fatalf("allow-marked file missing from HEAD: %v", err)
	}
	if !strings.Contains(string(out), "scribe:allow") {
		t.Error("committed blob lost the scribe:allow marker")
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
