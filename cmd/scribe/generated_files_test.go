package main

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	gosync "sync"
	"testing"
)

// rebuildInstructionRE matches a sentence that tells the model to produce
// one of the generated wiki files itself. Scribe regenerates _index.md and
// _backlinks.json in Go after every writing path (`scribe index`,
// `scribe backlinks`), and _backlinks.json is keyed by canonical title, so
// a model rewriting it writes the wrong shape.
var rebuildInstructionRE = regexp.MustCompile(`(?i)\b(rebuild|regenerate|update|refresh|recompute|write|clean)\b[^.\n]{0,80}(_index\.md|_backlinks\.json|article_count)`)

// negatedRE marks a sentence as a prohibition ("Do not …", "NEVER …").
var negatedRE = regexp.MustCompile(`(?i)\b(not|never|don't)\b`)

// promptSentences splits a prompt into sentence-ish units: lines first,
// then ". " boundaries, so a prohibition earlier on a line can't excuse an
// instruction later on it.
func promptSentences(p string) []string {
	var out []string
	for line := range strings.SplitSeq(p, "\n") {
		out = append(out, strings.Split(line, ". ")...)
	}
	return out
}

func TestPrompts_DoNotInstructRebuildingGeneratedFiles(t *testing.T) {
	entries, err := fs.ReadDir(promptFS, "prompts")
	if err != nil {
		t.Fatalf("read prompts: %v", err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if path.Ext(name) != ".md" {
			continue
		}
		// The real loader, with no vars: placeholders are stripped, the
		// rest is exactly what a model would read.
		p, err := loadPrompt(name, nil)
		if err != nil {
			t.Fatalf("loadPrompt %s: %v", name, err)
		}
		checked++
		for _, s := range promptSentences(p) {
			if rebuildInstructionRE.MatchString(s) && !negatedRE.MatchString(s) {
				t.Errorf("%s instructs the model to produce a generated file scribe regenerates:\n  %s", name, strings.TrimSpace(s))
			}
		}
	}
	if checked < 10 {
		t.Fatalf("only %d prompts checked; the embed glob or the loop is broken", checked)
	}
}

// The matcher must still catch the instructions this test was written to
// remove, or a passing run proves nothing.
func TestRebuildInstructionRE_CatchesOldWording(t *testing.T) {
	for _, s := range []string{
		"Step 6: Rebuild /kb/wiki/_index.md and /kb/wiki/_backlinks.json",
		"6. Rebuild _index.md and _backlinks.json",
		"8. **Update `wiki/_index.md` and `wiki/_backlinks.json`.**",
		"Update article_count in the index",
	} {
		if !rebuildInstructionRE.MatchString(s) || negatedRE.MatchString(s) {
			t.Errorf("matcher misses %q", s)
		}
	}
}

// stubRebuild swaps the rebuildIndexAndBacklinks seam for a recorder.
func stubRebuild(t *testing.T) func() []string {
	t.Helper()
	var mu gosync.Mutex
	var roots []string
	orig := rebuildIndexAndBacklinks
	rebuildIndexAndBacklinks = func(root string) {
		mu.Lock()
		roots = append(roots, root)
		mu.Unlock()
	}
	t.Cleanup(func() { rebuildIndexAndBacklinks = orig })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), roots...)
	}
}

const noContextualizeYAML = "kb_name: stubkb\nabsorb:\n  contextualize:\n    enabled: false\n"

// `scribe absorb` runs outside sync, so nothing else rebuilds the index
// after the absorb prompt stopped asking the model to.
func TestAbsorbCmd_RebuildsIndexAndBacklinks(t *testing.T) {
	root := stubHarnessKB(t, noContextualizeYAML)
	stub := &stubLLM{}
	stub.Rules = []*stubRule{{MatchOp: "absorb-single", Reply: envelopeJSON(t, 1, "Note", "wiki/note.md", "Body.")}}
	installStubLLM(t, stub)
	rebuilt := stubRebuild(t)

	src := filepath.Join(t.TempDir(), "note.md")
	if err := os.WriteFile(src, []byte("# Note\n\nA short note.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (&AbsorbCmd{File: src, NoContextualize: true, Domain: "general"}).Run(); err != nil {
		t.Fatalf("AbsorbCmd.Run: %v", err)
	}
	if got := rebuilt(); len(got) != 1 || got[0] != root {
		t.Errorf("rebuildIndexAndBacklinks calls = %v, want [%s]", got, root)
	}
}

func TestContextualizeThenAbsorb_RebuildsIndexAndBacklinks(t *testing.T) {
	root := stubHarnessKB(t, noContextualizeYAML)
	stub := &stubLLM{}
	stub.Rules = []*stubRule{{MatchOp: "absorb-single", Reply: envelopeJSON(t, 1, "Note", "wiki/note.md", "Body.")}}
	installStubLLM(t, stub)
	rebuilt := stubRebuild(t)

	raw := filepath.Join(root, "raw", "articles", "2026-11-01-note.md")
	if err := os.WriteFile(raw, []byte("---\ntitle: Note\n---\n\nA short note.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := contextualizeThenAbsorb(root, raw); err != nil {
		t.Fatalf("contextualizeThenAbsorb: %v", err)
	}
	if got := rebuilt(); len(got) != 1 || got[0] != root {
		t.Errorf("rebuildIndexAndBacklinks calls = %v, want [%s]", got, root)
	}
}

// Legacy tools-mode assess only re-embedded qmd; the consolidate prompt
// used to rebuild the index itself.
func TestAssessToolsMode_RebuildsIndexAndBacklinks(t *testing.T) {
	root, _ := deepKB(t, "tools")
	rebuilt := stubRebuild(t)
	installStubClaude(t, &stubClaude{Script: func(c claudeCall) (string, error) {
		if c.Op != "assess-track" {
			return "", nil
		}
		// Each track prompt names its own output file; write it so
		// consolidation has >=3 usable tracks.
		m := regexp.MustCompile(`\S+/output/assess/\S+?\.md`).FindString(c.Prompt)
		if m == "" {
			t.Errorf("track prompt names no output file")
			return "", nil
		}
		return "", os.WriteFile(m, []byte(strings.Repeat("finding\n", 30)), 0o644)
	}})

	a := &AssessCmd{Project: "acme", Model: "sonnet", Parallel: 1, Timeout: 10}
	if err := a.Run(); err != nil {
		t.Fatalf("AssessCmd.Run: %v", err)
	}
	if got := rebuilt(); len(got) != 1 || got[0] != root {
		t.Errorf("rebuildIndexAndBacklinks calls = %v, want [%s]", got, root)
	}
}
