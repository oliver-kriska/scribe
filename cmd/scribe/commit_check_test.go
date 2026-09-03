package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- I08: commit gate for hand commits ----

func TestStagedTextFiles_CoversTextTypesOnly(t *testing.T) {
	repo := initTestGitRepo(t, "Gate Tester")
	for _, f := range []string{"wiki/a.md", "inbox/b.url", "scripts/c.sh", "notes/d.txt", "cfg/e.json", "img/f.png", "scribe.yaml"} {
		writeKBFile(t, repo, f, "x\n")
	}
	gitRun(t, repo, "add", ".")
	got := map[string]bool{}
	for _, f := range stagedTextFiles(repo) {
		got[f] = true
	}
	for _, want := range []string{"wiki/a.md", "inbox/b.url", "scripts/c.sh", "notes/d.txt", "cfg/e.json", "scribe.yaml"} {
		if !got[want] {
			t.Errorf("%s missing from the gate's staged set: %v", want, got)
		}
	}
	if got["img/f.png"] {
		t.Error("binary file must not be scanned")
	}
}

func TestGateCheck_ReportsWithoutUnstaging(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := initTestGitRepo(t, "Gate Tester")
	writeKBFile(t, repo, "wiki/leaky.md", "---\ntitle: Leaky\n---\n\nkey: "+fakeAWSKey()+"\n")
	writeKBFile(t, repo, "inbox/queued.url", "url: https://example.test/?token="+fakeGitHubToken()+"\n")
	writeKBFile(t, repo, "wiki/held.md", "---\ntitle: Held\n---\n\nmentions projectx here\n")
	writeKBFile(t, repo, "wiki/clean.md", "---\ntitle: Clean\n---\n\nnothing\n")
	gitRun(t, repo, "add", ".")

	cfg := &ScribeConfig{Team: true, StopWords: StopWordsConfig{Hold: []string{"projectx"}}}
	findings := gateCheck(repo, cfg)
	joined := strings.Join(findings, "\n")
	for _, want := range []string{"wiki/leaky.md:", "inbox/queued.url:", "wiki/held.md:"} {
		if !strings.Contains(joined, want) {
			t.Errorf("finding for %s missing:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "wiki/clean.md") {
		t.Errorf("clean file reported:\n%s", joined)
	}
	staged := map[string]bool{}
	for _, f := range stagedTextFiles(repo) {
		staged[f] = true
	}
	if !staged["wiki/leaky.md"] || !staged["inbox/queued.url"] {
		t.Error("--check must not unstage anything")
	}

	// Solo KB: secrets are not scanned (matches the gate), held words are.
	solo := gateCheck(repo, &ScribeConfig{StopWords: StopWordsConfig{Hold: []string{"projectx"}}})
	if s := strings.Join(solo, "\n"); strings.Contains(s, "leaky") || !strings.Contains(s, "held.md") {
		t.Errorf("solo check should mirror the gate:\n%s", s)
	}
}

func TestCommitCheck_ExitsNonZeroAndIsReadOnly(t *testing.T) {
	root := commitTestKB(t)
	t.Setenv("SCRIBE_KB", root)
	globalRoot = ""
	writeTestArticle(t, root, "wiki/leaky.md", "---\ntitle: Leaky\n---\n\nkey: "+fakeAWSKey()+"\n")
	gitRun(t, root, "add", "wiki")

	c := &CommitCmd{Check: true}
	if !c.ReadOnly() {
		t.Error("--check must be read-only so the hook leaves no run record")
	}
	if err := c.Run(); err == nil || !strings.Contains(err.Error(), "commit gate") {
		t.Fatalf("want a gate error, got %v", err)
	}
	if headSubject(t, root) != "baseline" {
		t.Error("--check must not commit")
	}
	if len(stagedTextFiles(root)) != 1 {
		t.Error("--check must leave the index alone")
	}
	if _, err := os.Stat(filepath.Join(root, "output", "runs")); !os.IsNotExist(err) {
		t.Error("no run record for a read-only check")
	}
}
