package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLooksLikePersonTarget(t *testing.T) {
	tests := []struct {
		target string
		want   bool
	}{
		{"Lisa Chen", true},
		{"Jean-Pierre Dupont", true},
		{"O'Brien Smith", true},
		{"Anna Maria Della Rosa", true}, // 4 tokens — upper bound
		{"Lisa", false},                 // single token
		{"lisa chen", false},            // lowercase lead
		{"Lisa von Chen", false},        // lowercase preposition
		{"Phoenix LiveView", false},     // CamelCase token
		{"LLM Agents", false},           // ALL-CAPS acronym
		{"One Two Three Four Five", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			if got := looksLikePersonTarget(tt.target); got != tt.want {
				t.Errorf("looksLikePersonTarget(%q) = %v, want %v", tt.target, got, tt.want)
			}
		})
	}
}

func TestKnownIdentities(t *testing.T) {
	people := []existingPerson{
		{Title: "Lisa Chen", Aliases: []string{"lchen", "Lisa C."}},
		{Title: "", Aliases: []string{"ghost"}},
	}
	set := knownIdentities(people)
	for _, want := range []string{"lisa chen", "lchen", "lisa c.", "ghost"} {
		if !set[want] {
			t.Errorf("missing %q in known set: %v", want, set)
		}
	}
	if set[""] {
		t.Error("empty title must not enter the set")
	}
}

func TestCollectExistingPeople(t *testing.T) {
	root := t.TempDir()
	writeKBFile(t, root, "people/lisa-chen.md",
		"---\ntitle: \"Lisa Chen\"\naliases: [lchen]\n---\n\nBody.\n")
	writeKBFile(t, root, "people/_template.md",
		"---\ntitle: \"Skip Me\"\n---\n")
	writeKBFile(t, root, "people/notes.txt", "not markdown")
	writeKBFile(t, root, "people/broken.md", "---\ntitle: [unclosed\n")

	people := collectExistingPeople(root)
	if len(people) != 1 {
		t.Fatalf("want 1 person, got %d: %+v", len(people), people)
	}
	p := people[0]
	if p.Title != "Lisa Chen" || p.PagePath != "people/lisa-chen.md" {
		t.Errorf("unexpected entry: %+v", p)
	}
	if len(p.Aliases) != 1 || p.Aliases[0] != "lchen" {
		t.Errorf("aliases not collected: %v", p.Aliases)
	}

	if got := collectExistingPeople(filepath.Join(root, "missing")); got != nil {
		t.Errorf("missing people dir should yield nil, got %v", got)
	}
}

func TestHarvestMentions(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("domains: [acme]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Non-person wiki page whose title must be prefiltered from wikilink hits.
	writeKBFile(t, root, "tools/plugin.md",
		"---\ntitle: \"Claude Plugin\"\n---\n\nA tool page.\n")
	// Wiki article with every mention shape.
	writeKBFile(t, root, "wiki/notes.md", `---
title: "Notes"
---

Mail from jane@corp.example and noise@example.com (stopword domain).
Handle @janedoe appears here; module attribute @moduledoc must not.
Linked person [[Mark Webber]] and [[Mark Webber]] again and [[Mark Webber]] once more.
Known [[Lisa Chen]] is already cataloged. Tool page [[Claude Plugin]] is not a person.
Rare name [[Once Only]] appears a single time.

`+"```\ncode-fence@corp.example must be ignored\n@fencehandle too\n```\n")
	// Raw articles are scanned too.
	writeKBFile(t, root, "raw/articles/cap.md", "Raw capture mentions raw@corp.example.\n")

	existing := []existingPerson{{Title: "Lisa Chen", Aliases: nil}}
	mentions := harvestMentions(root, existing)

	wantPresent := map[string]int{
		"jane@corp.example": 1,
		"@janedoe":          1,
		"Mark Webber":       3,
		"raw@corp.example":  1,
	}
	for k, n := range wantPresent {
		if mentions[k] != n {
			t.Errorf("mentions[%q] = %d, want %d (all: %v)", k, mentions[k], n, mentions)
		}
	}
	for _, absent := range []string{
		"noise@example.com",       // stopword email domain
		"@moduledoc",              // stopword handle
		"Lisa Chen",               // known person
		"Claude Plugin",           // non-person wiki title
		"Once Only",               // below the 3-occurrence bare-name floor
		"code-fence@corp.example", // inside code fence
		"@fencehandle",            // inside code fence
	} {
		if _, ok := mentions[absent]; ok {
			t.Errorf("%q should have been filtered out", absent)
		}
	}
}

func TestRenderPeopleBlock(t *testing.T) {
	if got := renderPeopleBlock(nil); got != "(none)" {
		t.Errorf("empty people = %q, want (none)", got)
	}
	got := renderPeopleBlock([]existingPerson{
		{Title: "Zed Zee", PagePath: "people/zed.md"},
		{Title: "Amy Aye", PagePath: "people/amy.md", Aliases: []string{"aa", "amy"}},
	})
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %q", got)
	}
	if !strings.HasPrefix(lines[0], "- Amy Aye") {
		t.Errorf("lines not sorted: %q", got)
	}
	if !strings.Contains(lines[0], "aliases: aa, amy") {
		t.Errorf("aliases not rendered: %q", lines[0])
	}
}

func TestRenderMentionsBlock(t *testing.T) {
	t.Run("sorted by frequency then name", func(t *testing.T) {
		got := renderMentionsBlock(map[string]int{
			"bob@x.io": 2, "@alice": 5, "Zed Zee": 5,
		})
		want := "- @alice (×5)\n- Zed Zee (×5)\n- bob@x.io (×2)"
		if got != want {
			t.Errorf("got:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("truncates at the prompt cap", func(t *testing.T) {
		counts := make(map[string]int, maxMentionsForIdentityPrompt+10)
		for i := range maxMentionsForIdentityPrompt + 10 {
			counts[fmt.Sprintf("Person%04d Mention", i)] = 1
		}
		got := renderMentionsBlock(counts)
		if n := len(strings.Split(got, "\n")); n != maxMentionsForIdentityPrompt {
			t.Errorf("rendered %d lines, want %d", n, maxMentionsForIdentityPrompt)
		}
	})
}

func TestRunIdentitiesCheck_NoMentions(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("domains: [acme]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCRIBE_KB", root)

	var err error
	out := captureLintStdout(t, func() { err = runIdentitiesCheck("", false) })
	if err != nil {
		t.Fatalf("runIdentitiesCheck: %v", err)
	}
	if !strings.Contains(out, "no unmatched person-mentions") {
		t.Errorf("expected empty-KB message, got:\n%s", out)
	}
}

func TestRunIdentitiesCheck_DryRunPrintsWithoutWriting(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("domains: [acme]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCRIBE_KB", root)
	writeKBFile(t, root, "wiki/notes.md",
		"---\ntitle: \"Notes\"\n---\n\nPing jane@corp.example today.\n")

	var err error
	out := captureLintStdout(t, func() { err = runIdentitiesCheck("", true) })
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(out, "jane@corp.example") {
		t.Errorf("dry run should list mentions:\n%s", out)
	}
	if fileExists(filepath.Join(root, "wiki", "_identity-proposals.md")) {
		t.Error("dry run must not write the proposals file")
	}
}
