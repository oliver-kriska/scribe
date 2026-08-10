package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestGitAddWikiStagesRawSources pins the fix for the 2026-08-10 gap: a
// sync-driven commit staged wiki/_absorb_log.json (which records that a raw
// article was absorbed, by sha) but never staged raw/ itself, so the source
// existed only on the machine that ran the sync. A clone got the bookkeeping
// without the evidence.
func TestGitAddWikiStagesRawSources(t *testing.T) {
	root := initTestGitRepo(t, "Raw Stage Tester")

	for _, dir := range []string{"wiki", filepath.Join("raw", "articles")} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wikiPage := filepath.Join(root, "wiki", "page.md")
	rawArticle := filepath.Join(root, "raw", "articles", "2026-08-10-source.md")
	if err := os.WriteFile(wikiPage, []byte("---\ntitle: Page\n---\n\nBody.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rawArticle, []byte("---\ntitle: Source\n---\n\nSource body.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if ok := gitAddWiki(root); !ok {
		t.Fatal("gitAddWiki returned false (secret gate) on clean fixtures")
	}

	cmd := exec.CommandContext(context.Background(), "git", "diff", "--cached", "--name-only")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git diff --cached: %v\n%s", err, out)
	}
	staged := strings.Fields(string(out))
	if !slices.Contains(staged, "wiki/page.md") {
		t.Errorf("wiki page not staged; staged = %v", staged)
	}
	if !slices.Contains(staged, "raw/articles/2026-08-10-source.md") {
		t.Errorf("raw source not staged — absorb bookkeeping would ship without its source; staged = %v", staged)
	}
}

// TestRawIsNotAnLLMWriteTarget guards the reason raw/ is staged via
// stagedContentDirs rather than by being added to wikiDirs. wikiDirs also
// gates where absorb may write; raw/ must stay unwritable so a model can never
// mutate the source corpus it is summarizing.
func TestRawIsNotAnLLMWriteTarget(t *testing.T) {
	if slices.Contains(wikiDirs, "raw") {
		t.Fatal("raw must not be in wikiDirs — that would make the source corpus an LLM write target")
	}
	if !slices.Contains(stagedContentDirs(), "raw") {
		t.Error("stagedContentDirs must include raw so sync commits sources alongside bookkeeping")
	}

	root := t.TempDir()
	if _, err := validateActionPath(root, "raw/articles/injected.md"); !errors.Is(err, errUnknownTopDir) {
		t.Errorf("writing into raw/ must be rejected as an unknown top dir, got %v", err)
	}
}
