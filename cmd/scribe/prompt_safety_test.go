package main

import (
	"io/fs"
	"path"
	"strings"
	"testing"
)

// Every model-facing prompt reads text scribe did not write (transcripts,
// captured pages, project files, KB articles). Each one must carry the
// shared untrusted-input + secret rule; a new prompt that forgets the
// {{INPUT_SAFETY}} placeholder fails here.
func TestEveryPromptCarriesInputSafety(t *testing.T) {
	entries, err := fs.ReadDir(promptFS, "prompts")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		if path.Ext(e.Name()) != ".md" {
			continue
		}
		out, err := loadPrompt(e.Name(), nil)
		if err != nil {
			t.Fatalf("loadPrompt %s: %v", e.Name(), err)
		}
		checked++
		if !strings.Contains(out, "## Untrusted input") || !strings.Contains(out, "Never copy a secret") {
			t.Errorf("%s lacks the {{INPUT_SAFETY}} fragment", e.Name())
		}
	}
	if checked < 10 {
		t.Fatalf("only %d prompts checked; the embed glob or the loop is broken", checked)
	}
}

// A value is data: placeholders inside an inlined transcript reach the
// model verbatim. Before the one-pass replacer, map-order ReplaceAll could
// substitute {{TODAY}} inside the transcript, and the residual-placeholder
// guard stripped {{UNKNOWN}} out of it.
func TestLoadPromptDoesNotRewriteInsideValues(t *testing.T) {
	transcript := "user: the prompt uses {{TODAY}} and {{KB_DIR}}, plus {{NOT_A_VAR}} and {{INPUT_SAFETY}}"
	for range 20 { // map iteration order is random; repeat to catch order-dependence
		out, err := loadPrompt("session-mine-anthropic.md", map[string]string{
			"TRANSCRIPT":       transcript,
			"TODAY":            "2026-11-07",
			"SESSION_ID":       "sid",
			"PROJECT_PATH":     "/p",
			"RELATED_SESSIONS": "",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, transcript) {
			t.Fatalf("transcript was rewritten during substitution")
		}
		if !strings.Contains(out, "created: 2026-11-07") {
			t.Fatalf("template placeholder {{TODAY}} was not substituted")
		}
	}
}

func TestLoadPromptCallerVarOverridesShared(t *testing.T) {
	out, err := loadPrompt("contradictions.md", map[string]string{"INPUT_SAFETY": "CUSTOM-RULE", "ARTICLES": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "CUSTOM-RULE") || strings.Contains(out, "## Untrusted input") {
		t.Errorf("caller var should win over the shared fragment")
	}
}
