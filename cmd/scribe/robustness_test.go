package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- L03 / L16: global state and shell quoting ----

func TestInstallHotHooks_RefusesUnreadableSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A directory where the file should be: ReadFile fails with EISDIR,
	// which used to fall through to "empty settings" and overwrite.
	if err := os.MkdirAll(filepath.Join(home, ".claude", "settings.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := installHotHooks(filepath.Join(home, "kb"))
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("want a refusal, got %v", err)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/usr/local/bin/scribe":          "/usr/local/bin/scribe",
		"/Users/Some One/.local/scribe":  "'/Users/Some One/.local/scribe'",
		"it's":                           `'it'\''s'`,
		"":                               "''",
		"/tmp/kb$1":                      "'/tmp/kb$1'",
		"/Users/oliver.kriska/scribe-v2": "/Users/oliver.kriska/scribe-v2",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
	plain := scribeJobs("/usr/local/bin/scribe")[0].Command
	if !strings.HasPrefix(plain, "/usr/local/bin/scribe each -- ") {
		t.Errorf("plain binary must stay unquoted for drift detection: %q", plain)
	}
	spaced := scribeJobs("/Users/Some One/.local/bin/scribe")[0].Command
	if !strings.HasPrefix(spaced, "'/Users/Some One/.local/bin/scribe' each -- ") {
		t.Errorf("binary with a space must be quoted: %q", spaced)
	}
}

// ---- L06: launchctl probe ----

func TestProbeLaunchAgent_ExitStatusWins(t *testing.T) {
	orig := runLaunchctl
	t.Cleanup(func() { runLaunchctl = orig })
	plist := filepath.Join(t.TempDir(), "com.scribe.x.plist")
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		out  string
		err  error
		want string
	}{
		{"loaded", "com.scribe.x = {\n\tstate = running\n}", nil, "loaded"},
		{"exit 113 with message", "Could not find service", errors.New("exit status 113"), "present"},
		{"non-zero exit, unexpected text", "Bad request", errors.New("exit status 1"), "present"},
		{"exit 0 empty", "", nil, "present"},
		{"exit 0 with miss text", "could not find service", nil, "present"},
	}
	for _, c := range cases {
		runLaunchctl = func(...string) (string, error) { return c.out, c.err }
		if got := probeLaunchAgent("gui/501", "com.scribe.x", plist); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
	runLaunchctl = func(...string) (string, error) { return "", errors.New("exit status 113") }
	if got := probeLaunchAgent("gui/501", "com.scribe.x", filepath.Join(t.TempDir(), "none.plist")); got != "missing" {
		t.Errorf("no plist: got %s", got)
	}
}

// ---- L04: mining stops launching calls after the first rate limit ----

func TestMineSessionBatches_StopsAfterRateLimit(t *testing.T) {
	root := testKB(t, "ccrider_db: "+filepath.Join(t.TempDir(), "none.db")+"\n")
	resetRunOutcome()
	t.Cleanup(resetRunOutcome)
	stub := &stubClaude{Script: func(claudeCall) (string, error) { return "", ErrRateLimit }}
	installStubClaude(t, stub)

	ids := make([]string, 12)
	for i := range ids {
		ids[i] = "sess-" + string(rune('a'+i))
	}
	s := &SyncCmd{Model: "haiku"}
	mined, rl := s.mineSessionBatches(root, ids, 3, time.Second, "session-extract.md", "session")
	if mined != 0 || !rl {
		t.Fatalf("mined=%d rateLimited=%v", mined, rl)
	}
	if n := len(stub.Calls()); n > 3 {
		t.Errorf("%d claude calls after the first rate limit; at most the %d in flight should run", n, 3)
	}
}

// ---- L01: dream deletion guard restores the worktree ----

func TestCommitDreamCycle_RestoresOnMassDeletion(t *testing.T) {
	repo := initTestGitRepo(t, "Dream Tester")
	for i := range 8 {
		writeKBFile(t, repo, "wiki/a"+string(rune('a'+i))+".md", lintValidArticle("Article "+string(rune('A'+i)), 12))
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "seed", "--no-gpg-sign")
	pre := countArticles(repo)
	if pre != 8 {
		t.Fatalf("fixture counts %d articles, want 8", pre)
	}
	for i := range 7 {
		if err := os.Remove(filepath.Join(repo, "wiki", "a"+string(rune('a'+i))+".md")); err != nil {
			t.Fatal(err)
		}
	}
	err := commitDreamCycle(repo, "2026-09-03", "dream", pre)
	if err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("guard must trip: %v", err)
	}
	if got := countArticles(repo); got != 8 {
		t.Errorf("worktree not restored: %d articles on disk, want 8", got)
	}
}

// ---- L11: PDF parser cannot take the drain down ----

func TestConvertPDFTier0_MalformedInputIsAnError(t *testing.T) {
	for _, in := range []string{"", "%PDF-1.4\nnot a pdf", "%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\nxref\n0 1\ntrailer\n<< /Root 1 0 R >>\nstartxref\n9999\n%%EOF"} {
		if _, err := convertPDFTier0([]byte(in)); err == nil {
			t.Errorf("%q: want an error", truncateBytes(in, 20))
		}
		_ = pdfPageCount([]byte(in))
	}
}
