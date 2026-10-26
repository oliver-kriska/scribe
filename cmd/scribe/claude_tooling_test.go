package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestIsClaudeToolingRelPath(t *testing.T) {
	cases := []struct {
		rel  string
		want bool
	}{
		{".claude/rules/angular.md", true},
		{".claude/agents/reviewer.md", true},
		{".claude/commands/ship.md", true},
		{".claude/skills/kb/SKILL.md", true},
		{".claude/output-styles/terse.md", true},
		// Nested checkouts are nestedCheckout's job, not this list's.
		{".claude/worktrees/feature/docs/design.md", false},
		{"pkg/.claude/rules/x.md", true},
		// Knowledge the extract prompts read on purpose.
		{".claude/plans/migration.md", false},
		{".claude/research/2026-01-01-topic.md", false},
		{".claude/solutions/fix.md", false},
		// The KB drop folder.
		{".claude/scribe/2026-01-01-drop.md", false},
		{".claude-notes/rules/x.md", false},
		{"docs/rules/x.md", false},
		{"rules/.claude", false},
	}
	for _, c := range cases {
		if got := isClaudeToolingRelPath(c.rel); got != c.want {
			t.Errorf("isClaudeToolingRelPath(%q) = %v, want %v", c.rel, got, c.want)
		}
	}
}

// Claude Code puts worktrees at <repo>/.claude/worktrees/, so a project
// checked out there must keep its files: only the part of the path inside
// the repo decides.
func TestExcludeClaudeToolingIgnoresClaudeAboveRepo(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "main", claudeToolingDir, "worktrees", "feature")
	knowledge := filepath.Join(repo, "docs", "design.md")
	tooling := filepath.Join(repo, claudeToolingDir, "rules", "angular.md")

	got := excludeClaudeTooling(repo, []string{knowledge, tooling})

	if !slices.Equal(got, []string{knowledge}) {
		t.Errorf("excludeClaudeTooling = %v, want only %s", got, knowledge)
	}
}

func relPaths(t *testing.T, root string, files []string) []string {
	t.Helper()
	rels := make([]string, 0, len(files))
	for _, f := range files {
		rel, err := filepath.Rel(root, f)
		if err != nil {
			t.Fatal(err)
		}
		rels = append(rels, filepath.ToSlash(rel))
	}
	slices.Sort(rels)
	return rels
}

func TestGitChangedFilesSkipsClaudeToolingOnBothPaths(t *testing.T) {
	repo := initTestGitRepo(t, "Extract Tester")
	writeTestArticle(t, repo, "docs/design.md", "real project knowledge\n")
	writeTestArticle(t, repo, ".claude/rules/angular.md", "tooling config\n")
	writeTestArticle(t, repo, ".claude/plans/migration.md", "a plan\n")
	writeTestArticle(t, repo, ".claude-notes/a.md", "not tooling\n")
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")

	first := relPaths(t, repo, gitChangedFiles(repo, "", []string{"*.md"}))
	want := []string{".claude-notes/a.md", ".claude/plans/migration.md", "docs/design.md"}
	if !slices.Equal(first, want) {
		t.Errorf("first extraction (ls-files) = %v, want %v", first, want)
	}

	base := strings.TrimSpace(runCmd("", "git", "-C", repo, "rev-parse", "HEAD"))
	writeTestArticle(t, repo, ".claude/rules/angular.md", "tooling config, edited\n")
	writeTestArticle(t, repo, ".claude/plans/migration.md", "a plan, edited\n")
	gitRun(t, repo, "commit", "-q", "-am", "edit")

	incremental := relPaths(t, repo, gitChangedFiles(repo, base, []string{"*.md"}))
	if !slices.Equal(incremental, []string{".claude/plans/migration.md"}) {
		t.Errorf("incremental (git diff) = %v, want only the plan", incremental)
	}
}

// Outside git, gitChangedFiles falls back to a filesystem walk, which must
// not descend into tooling dirs.
func TestGitChangedFilesFallbackWalkSkipsClaudeTooling(t *testing.T) {
	dir := t.TempDir()
	writeTestArticle(t, dir, "docs/design.md", "knowledge\n")
	writeTestArticle(t, dir, ".claude/plans/migration.md", "a plan\n")
	writeTestArticle(t, dir, ".claude/rules/angular.md", "tooling\n")
	writeTestArticle(t, dir, ".claude/skills/kb/SKILL.md", "tooling\n")

	got := relPaths(t, dir, gitChangedFiles(dir, "", []string{"*.md"}))
	want := []string{".claude/plans/migration.md", "docs/design.md"}
	if !slices.Equal(got, want) {
		t.Errorf("fallback walk = %v, want %v", got, want)
	}
}

func TestFindKnowledgeDirsSkipsClaudeTooling(t *testing.T) {
	dir := t.TempDir()
	writeTestArticle(t, dir, "docs/design.md", "knowledge\n")
	writeTestArticle(t, dir, ".claude/plans/migration.md", "a plan\n")
	writeTestArticle(t, dir, ".claude/rules/angular.md", "tooling\n")
	writeTestArticle(t, dir, ".claude/skills/kb/SKILL.md", "tooling\n")

	dirs, err := findKnowledgeDirs(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := relPaths(t, dir, dirs)
	want := []string{".claude/plans", "docs"}
	if !slices.Equal(got, want) {
		t.Errorf("findKnowledgeDirs = %v, want %v", got, want)
	}
}

// A walk rooted inside a tooling dir still walks: only directories below
// the root are skipped.
func TestFindKnowledgeDirsWalksRootInsideClaudeTooling(t *testing.T) {
	root := filepath.Join(t.TempDir(), claudeToolingDir, "skills")
	writeTestArticle(t, root, "docs/design.md", "knowledge\n")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	dirs, err := findKnowledgeDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := relPaths(t, root, dirs); !slices.Equal(got, []string{"docs"}) {
		t.Errorf("findKnowledgeDirs = %v, want [docs]", got)
	}
}

// scribe scan's manifest is what the tools-mode extract prompt tells the
// model to read, so it must skip the same directories.
func TestCollectKnowledgeFilesSkipsClaudeTooling(t *testing.T) {
	dir := t.TempDir()
	writeTestArticle(t, dir, "docs/design.md", "knowledge\n")
	writeTestArticle(t, dir, ".claude/plans/migration.md", "a plan\n")
	writeTestArticle(t, dir, ".claude/rules/angular.md", "tooling\n")
	writeTestArticle(t, dir, ".claude/skills/kb/SKILL.md", "tooling\n")

	got := collectKnowledgeFiles(dir)
	want := []string{"./.claude/plans/migration.md", "./docs/design.md"}
	if !slices.Equal(got, want) {
		t.Errorf("collectKnowledgeFiles = %v, want %v", got, want)
	}
}

// assess samples a bounded tree, and .claude sorts first, so tooling
// files would otherwise take sample slots from the real source tree.
func TestAssessSourceTreeSkipsClaudeTooling(t *testing.T) {
	dir := t.TempDir()
	writeTestArticle(t, dir, "lib/app.ex", "defmodule App do end\n")
	writeTestArticle(t, dir, ".claude/plans/migration.md", "a plan\n")
	writeTestArticle(t, dir, ".claude/skills/kb/SKILL.md", "tooling\n")

	tree := assessSourceTree(dir, 200)
	if strings.Contains(tree, "skills") {
		t.Errorf("assess tree includes a tooling dir:\n%s", tree)
	}
	for _, want := range []string{"lib/app.ex", ".claude/plans/migration.md"} {
		if !strings.Contains(tree, want) {
			t.Errorf("assess tree is missing %s:\n%s", want, tree)
		}
	}
}

// The model globs for itself in tools mode, so both extract prompts must
// name every directory the Go file selection skips.
func TestExtractPromptsNameClaudeToolingDirs(t *testing.T) {
	for _, name := range []string{"extract.md", "extract-anthropic.md"} {
		prompt, err := loadPrompt(name, map[string]string{"CLAUDE_TOOLING_DIRS": claudeToolingPromptList()})
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		for sub := range claudeToolingSubdirs {
			if want := claudeToolingDir + "/" + sub + "/"; !strings.Contains(prompt, want) {
				t.Errorf("%s does not tell the model to skip %s", name, want)
			}
		}
	}
}
