package main

import (
	"slices"
	"strings"
	"testing"
)

// dreamPromptVars lists the vars each dream prompt's loader passes
// (dream.go, dream_orchestrator.go, dream_hot.go). A placeholder outside
// this set is silently stripped by loadPrompt, so it is a prompt bug.
var dreamPromptVars = map[string][]string{
	"dream.md":               {"KB_DIR", "DATE"},
	"dream-anthropic.md":     {"KB_DIR", "TODAY", "LOG_TAIL", "INVENTORY", "STALE", "CONTRADICTIONS"},
	"dream-ollama.md":        {"KB_DIR", "TODAY", "LOG_TAIL", "INVENTORY", "STALE", "CONTRADICTIONS"},
	"dream-hot-anthropic.md": {"KB_DIR", "TODAY", "DOMAIN", "LOG_TAIL", "INVENTORY", "STALE", "CONTRADICTIONS"},
	"dream-hot-ollama.md":    {"KB_DIR", "TODAY", "DOMAIN", "LOG_TAIL", "INVENTORY", "STALE", "CONTRADICTIONS"},
}

// loadDreamPromptForTest loads a dream prompt the way its loader does and
// fails if the raw file uses a placeholder that loader never passes.
func loadDreamPromptForTest(t *testing.T, name string) string {
	t.Helper()
	vars, ok := dreamPromptVars[name]
	if !ok {
		t.Fatalf("no loader vars recorded for %s", name)
	}
	raw, err := promptFS.ReadFile("prompts/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	for _, ph := range promptPlaceholderRE.FindAllString(string(raw), -1) {
		if v := strings.Trim(ph, "{}"); !slices.Contains(vars, v) {
			t.Errorf("%s uses %s, which its loader never passes", name, ph)
		}
	}
	values := make(map[string]string, len(vars))
	for _, v := range vars {
		values[v] = "<" + strings.ToLower(v) + ">"
	}
	out, err := loadPrompt(name, values)
	if err != nil {
		t.Fatalf("loadPrompt %s: %v", name, err)
	}
	return out
}

func TestDreamPrompts_UseOnlyLoaderVars(t *testing.T) {
	for name := range dreamPromptVars {
		loadDreamPromptForTest(t, name)
	}
}

// The monolithic dream's grant (DreamCmd.Run's tools slice in dream.go, isolated by claude.go's
// --tools/--allowedTools) lets Bash run only wc, ls and find, and refuses
// find -delete/-exec. Measured 2026-10-23 on Claude Code 2.1.293. Cycles
// that didn't know this spent turns on refused cd && …; for … loops and
// python3/scribe calls. Anchors are kept to the load-bearing facts.
func TestDreamPrompt_StatesActualToolGrant(t *testing.T) {
	p := loadDreamPromptForTest(t, "dream.md")
	for _, want := range []string{"`wc`, `ls` and `find`", "no subagents", "Ignore any source ending in `_index.md`"} {
		if !strings.Contains(p, want) {
			t.Errorf("dream.md does not state %q", want)
		}
	}
}

// Nothing in the grant can delete a file (no rm; find -delete is refused),
// so the prompt must not tell the model to delete. Deletion candidates go to
// the log for a human.
func TestDreamPrompt_DoesNotAskForImpossibleDeletion(t *testing.T) {
	p := loadDreamPromptForTest(t, "dream.md")
	if strings.Contains(p, "Only then delete") {
		t.Error("dream.md still asks the model to delete files it cannot delete")
	}
	if strings.Contains(p, "Never delete more than 3") {
		t.Error("dream.md still caps deletions at 3, a cap the model cannot enforce without deleting")
	}
	if !strings.Contains(p, "for a human to delete") {
		t.Error("dream.md should hand deletion candidates to a human to delete")
	}
}

// commitDreamCycle runs `scribe backlinks` and `scribe index` after every
// dream, so asking the model to do that work wastes turns and invites edits
// that get overwritten.
func TestDreamPrompt_DropsStepsScribeRedoes(t *testing.T) {
	p := loadDreamPromptForTest(t, "dream.md")
	for _, gone := range []string{
		"Remove entries from _index.md",
		"Verify every entry in _index.md",
		"Rebuild wiki/_backlinks.json",
		"Update article_count",
	} {
		if strings.Contains(p, gone) {
			t.Errorf("dream.md still asks for %q, which scribe redoes after the cycle", gone)
		}
	}
	if !strings.Contains(p, "whose name starts with `_`") {
		t.Error("dream.md should put every generated wiki/_* file off-limits")
	}
}

// dreamStaleCandidates (dream_orchestrator.go) checks updated: only, and
// these modes see no link data, so no prompt may describe the list as
// zero-link. Marking must need positive evidence of obsolescence, because a
// model reading "not linked from this packet" as permission would mark old
// but well-linked articles. The executor's guard (wiki_actions.go) only
// blocks fresh ones.
func TestDreamOrchestratorPrompts_DoNotClaimStaleListIsZeroLink(t *testing.T) {
	for _, name := range []string{"dream-anthropic.md", "dream-ollama.md", "dream-hot-anthropic.md", "dream-hot-ollama.md"} {
		p := loadDreamPromptForTest(t, name)
		lower := strings.ToLower(p)
		if strings.Contains(lower, "zero links") {
			t.Errorf("%s still describes the stale list as zero links", name)
		}
		if !strings.Contains(lower, "link counts not checked") {
			t.Errorf("%s does not say the stale list's link counts are not checked", name)
		}
		if !strings.Contains(p, "Stale candidates") {
			t.Errorf("%s lost the \"Stale candidates\" heading its decay instruction refers to", name)
		}
		if !strings.Contains(lower, "is not evidence of decay") {
			t.Errorf("%s decay instruction does not say listing is not evidence of decay", name)
		}
		if !strings.Contains(lower, "if unsure, do not mark it") {
			t.Errorf("%s decay instruction does not say to skip marking when unsure", name)
		}
	}
}
