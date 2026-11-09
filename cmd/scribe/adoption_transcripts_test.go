package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// jl builds Claude Code-shaped JSONL lines for fixtures.
func jlUser(cwd, text string) string {
	return `{"type":"user","cwd":"` + cwd + `","message":{"role":"user","content":"` + text + `"}}`
}

func jlTool(cwd, name, command string) string {
	input := `{}`
	if command != "" {
		input = `{"command":"` + command + `","description":"x"}`
	}
	return `{"type":"assistant","cwd":"` + cwd + `","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"` + name + `","input":` + input + `}]}}`
}

func jlText(cwd, text string) string {
	return `{"type":"assistant","cwd":"` + cwd + `","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}]}}`
}

func writeTranscript(t *testing.T, dir, name string, mtime time.Time, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyTranscriptBlocks(t *testing.T) {
	cases := []struct {
		name          string
		raw           string
		qmd, decision bool
	}{
		{"mcp qmd query", `[{"type":"tool_use","name":"mcp__plugin_qmd_qmd__query","input":{}}]`, true, false},
		{"custom alias qmd get", `[{"type":"tool_use","name":"mcp__kb__qmd__get","input":{}}]`, true, false},
		{"bash qmd search", `[{"type":"tool_use","name":"Bash","input":{"command":"qmd search \"x\""}}]`, true, false},
		{"bash unrelated", `[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]`, false, false},
		{"edit", `[{"type":"tool_use","name":"Edit","input":{}}]`, false, true},
		{"notebook edit", `[{"type":"tool_use","name":"NotebookEdit","input":{}}]`, false, true},
		{"prose mentioning qmd query is not a call", `[{"type":"text","text":"run qmd query and Edit"}]`, false, false},
		{"string content", `"qmd query"`, false, false},
		{"non-mcp tool named like qmd", `[{"type":"tool_use","name":"qmd__query","input":{}}]`, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, d := classifyTranscriptBlocks([]byte(c.raw))
			if q != c.qmd || d != c.decision {
				t.Errorf("got qmd=%v decision=%v, want %v/%v", q, d, c.qmd, c.decision)
			}
		})
	}
}

func TestComputeAdoptionFromTranscripts(t *testing.T) {
	root := t.TempDir()
	approved := filepath.Join(t.TempDir(), "Projects", "approved")
	pending := filepath.Join(t.TempDir(), "Projects", "pending")
	writeStatusManifest(t, root, map[string]map[string]string{
		"approved": {"path": approved, "domain": "general"},
		"pending":  {"path": pending, "domain": "general", "status": statusPending},
	})

	projects := t.TempDir()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-24 * time.Hour)
	old := now.Add(-10 * 24 * time.Hour)
	pdir := filepath.Join(projects, "-approved")

	// KB-first: qmd query, then Edit.
	writeTranscript(t, pdir, "kbfirst.jsonl", recent,
		jlUser(approved, "fix the bug"),
		jlTool(approved, "mcp__plugin_qmd_qmd__query", ""),
		jlTool(approved, "Read", ""),
		jlTool(approved, "Edit", ""))
	// Edit first, qmd later: a decision session that is NOT KB-first.
	writeTranscript(t, pdir, "editfirst.jsonl", recent,
		jlUser(approved, "quick change"),
		jlTool(approved, "Edit", ""),
		jlTool(approved, "Bash", "qmd query foo"))
	// No decision at all: excluded from the denominator.
	writeTranscript(t, pdir, "readonly.jsonl", recent,
		jlUser(approved, "explain"),
		jlText(approved, "here is how it works"))
	// Outside the 7d window but inside 30d: KB-first via Bash.
	writeTranscript(t, pdir, "old.jsonl", old,
		jlUser(approved, "old"),
		jlTool(approved, "Bash", "qmd search thing"),
		jlTool(approved, "Write", ""))
	// Out of scope: pending project, inside the KB, and a subagent file.
	writeTranscript(t, filepath.Join(projects, "-pending"), "p.jsonl", recent,
		jlUser(pending, "x"), jlTool(pending, "Edit", ""))
	writeTranscript(t, filepath.Join(projects, "-kb"), "kb.jsonl", recent,
		jlUser(root, "x"), jlTool(root, "Edit", ""))
	writeTranscript(t, filepath.Join(pdir, "kbfirst", "subagents"), "agent-1.jsonl", recent,
		jlUser(approved, "x"), jlTool(approved, "Edit", ""))

	r7, ok, err := computeAdoptionFromTranscripts(root, projects, 7, now)
	if err != nil || !ok {
		t.Fatalf("7d: ok=%v err=%v", ok, err)
	}
	if r7.DecisionSessions != 2 || r7.KBFirstSessions != 1 || r7.ExcludedNoDecision != 1 {
		t.Errorf("7d = %+v, want den=2 num=1 excluded=1", r7)
	}

	r30, _, err := computeAdoptionFromTranscripts(root, projects, 30, now)
	if err != nil {
		t.Fatal(err)
	}
	if r30.DecisionSessions != 3 || r30.KBFirstSessions != 2 {
		t.Errorf("30d = %+v, want den=3 num=2", r30)
	}

	if _, ok, _ := computeAdoptionFromTranscripts(root, filepath.Join(projects, "missing"), 7, now); ok {
		t.Error("missing projects dir must report ok=false so the caller falls back to ccrider")
	}
}
