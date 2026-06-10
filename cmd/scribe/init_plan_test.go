package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func planTitles(plan []initAction) string {
	titles := make([]string, 0, len(plan))
	for _, a := range plan {
		titles = append(titles, a.Title)
	}
	return strings.Join(titles, "\n")
}

func TestBuildInitPlanTeamBind(t *testing.T) {
	c := &InitCmd{Team: true, Bind: true}
	vars := templateVars{LLMProvider: "anthropic"}
	plan := c.buildInitPlan("/home/u/team-kb", vars, "/home/u/old-kb", true, false)
	titles := planTitles(plan)

	for _, want := range []string{
		"write KB scaffold at /home/u/team-kb",
		"git init -b main",
		"team mode: per-machine manifest + config trust",
		"point ~/.config/scribe/config.yaml at this KB (currently /home/u/old-kb)",
		"install/refresh scribe block in ~/.claude/CLAUDE.md",
		"install/refresh scribe block in ~/.codex/AGENTS.md",
		"run dependency self-test",
	} {
		if !strings.Contains(titles, want) {
			t.Errorf("plan missing %q; plan:\n%s", want, titles)
		}
	}

	// The TOFU caveat must be disclosed in the team explain.
	var teamExplain string
	for _, a := range plan {
		if strings.HasPrefix(a.Title, "team mode") {
			teamExplain = a.Explain
		}
	}
	if !strings.Contains(teamExplain, "first sync") && !strings.Contains(teamExplain, "FIRST sync") {
		t.Errorf("team explain does not disclose the first-sync trust snapshot:\n%s", teamExplain)
	}

	// Every action must be explainable.
	for _, a := range plan {
		if strings.TrimSpace(a.Explain) == "" {
			t.Errorf("action %q has no explanation", a.Title)
		}
	}
}

func TestBuildInitPlanSkipsAndVariants(t *testing.T) {
	// No opt-in → global writes show as explicit skip lines.
	c := &InitCmd{}
	plan := c.buildInitPlan("/home/u/kb", templateVars{}, "/home/u/other", false, false)
	titles := planTitles(plan)
	if !strings.Contains(titles, "skip ~/.config/scribe/config.yaml (pass --bind") {
		t.Errorf("plan missing user-config skip disclosure:\n%s", titles)
	}
	if !strings.Contains(titles, "skip ~/.claude/CLAUDE.md block (pass --bind") {
		t.Errorf("plan missing CLAUDE.md skip disclosure:\n%s", titles)
	}
	if strings.Contains(titles, "team mode") {
		t.Errorf("non-team plan mentions team mode:\n%s", titles)
	}

	// Throwaway path gets its own wording.
	plan = c.buildInitPlan("/tmp/kb", templateVars{}, "", false, true)
	if !strings.Contains(planTitles(plan), "temp path — pass --bind") {
		t.Errorf("throwaway plan missing temp-path disclosure:\n%s", planTitles(plan))
	}

	// --no-claude-md / --no-git / ollama provider.
	c = &InitCmd{NoClaudeMD: true, NoGit: true}
	plan = c.buildInitPlan("/home/u/kb", templateVars{LLMProvider: "ollama", LLMModel: "qwen3:8b"}, "", true, false)
	titles = planTitles(plan)
	if !strings.Contains(titles, "skip ~/.claude/CLAUDE.md block (--no-claude-md)") {
		t.Errorf("plan missing --no-claude-md line:\n%s", titles)
	}
	if strings.Contains(titles, "git init") {
		t.Errorf("--no-git plan still lists git init:\n%s", titles)
	}
	if !strings.Contains(titles, "pre-pull qwen3:8b") {
		t.Errorf("ollama plan missing model pre-pull:\n%s", titles)
	}
}

// TestRunBootstrapCheckIsPlanOnly: --check prints the plan and writes
// NOTHING — not even the scaffold directory.
func TestRunBootstrapCheckIsPlanOnly(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_CONFIG_HOME", "")
	kb := filepath.Join(t.TempDir(), "planned-kb")

	c := &InitCmd{Path: kb, Check: true, Yes: true, NoGit: true, Domains: []string{"general"}}
	if err := c.runBootstrap(); err != nil {
		t.Fatalf("runBootstrap --check: %v", err)
	}

	if _, err := os.Stat(kb); err == nil {
		t.Errorf("--check created the KB directory %s", kb)
	}
	if fileExists(filepath.Join(kb, "scribe.yaml")) {
		t.Error("--check wrote scribe.yaml")
	}
}

func TestProjectArticleCount(t *testing.T) {
	root := t.TempDir()
	writeKBFile(t, root, "projects/foo/insight.md", "# a\n")
	writeKBFile(t, root, "projects/foo/deeper/nested.md", "# b\n")
	writeKBFile(t, root, "projects/foo/_index.md", "ignored\n")
	writeKBFile(t, root, "projects/foo/notes.txt", "ignored\n")

	if got := projectArticleCount(root, "foo"); got != 2 {
		t.Errorf("projectArticleCount = %d, want 2", got)
	}
	if got := projectArticleCount(root, "nonexistent"); got != 0 {
		t.Errorf("missing project dir count = %d, want 0", got)
	}
}
