package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestStripUnknownFactIDs_EmptyMap(t *testing.T) {
	in := "A point about agents [c00-f1]. Another [c12-f6]."
	got, stripped := stripUnknownFactIDs(in, map[string]bool{})
	want := "A point about agents. Another."
	if got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(stripped, []string{"c00-f1", "c12-f6"}) {
		t.Errorf("stripped = %v", stripped)
	}
}

func TestStripUnknownFactIDs_AllValid(t *testing.T) {
	valid := map[string]bool{"c00-f1": true, "c01-f2": true}
	in := "Claim one [c00-f1]. Claim two [c01-f2]."
	got, stripped := stripUnknownFactIDs(in, valid)
	if got != in {
		t.Errorf("content mutated: %q", got)
	}
	if len(stripped) != 0 {
		t.Errorf("expected nothing stripped, got %v", stripped)
	}
}

func TestStripUnknownFactIDs_Mixed(t *testing.T) {
	valid := map[string]bool{"c00-f1": true}
	in := "Real [c00-f1] keeps. Fake [c99-f9] drops. Tail."
	got, stripped := stripUnknownFactIDs(in, valid)
	want := "Real [c00-f1] keeps. Fake drops. Tail."
	if got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(stripped, []string{"c99-f9"}) {
		t.Errorf("stripped = %v", stripped)
	}
}

func TestStripUnknownFactIDs_MultipleOnOneLine(t *testing.T) {
	valid := map[string]bool{"c00-f1": true}
	in := "Three brackets [c00-f1] [c00-f2] [c00-f3]."
	got, _ := stripUnknownFactIDs(in, valid)
	want := "Three brackets [c00-f1]."
	if got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

func TestStripUnknownFactIDs_DedupedStripList(t *testing.T) {
	in := "First [c99-f9]. Second [c99-f9]. Third [c99-f9]."
	_, stripped := stripUnknownFactIDs(in, map[string]bool{})
	if !reflect.DeepEqual(stripped, []string{"c99-f9"}) {
		t.Errorf("stripped = %v, want one entry", stripped)
	}
}

func TestStripUnknownFactIDs_DoesNotCrossNewlines(t *testing.T) {
	in := "Line A [c0-f1]\nLine B [c0-f1]"
	got, _ := stripUnknownFactIDs(in, map[string]bool{})
	want := "Line A\nLine B"
	if got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

func TestStripUnknownFactIDs_MalformedIgnored(t *testing.T) {
	in := "Not a fact-id: [c0-f] or [c-f1] or [abc]"
	got, stripped := stripUnknownFactIDs(in, map[string]bool{})
	if got != in {
		t.Errorf("content mutated: %q", got)
	}
	if len(stripped) != 0 {
		t.Errorf("stripped = %v", stripped)
	}
}

func TestStripUnknownFactIDs_LeadingSpaceConsumed(t *testing.T) {
	in := "Sentence ending [c99-f9]."
	got, _ := stripUnknownFactIDs(in, map[string]bool{})
	want := "Sentence ending."
	if got != want {
		t.Errorf("content = %q, want %q (no double-space before period)", got, want)
	}
}

func TestStripUnknownFactIDs_EmptyContent(t *testing.T) {
	got, stripped := stripUnknownFactIDs("", map[string]bool{"c0-f1": true})
	if got != "" || stripped != nil {
		t.Errorf("empty input should round-trip, got %q / %v", got, stripped)
	}
}

func TestStripUnknownFactIDs_HighIndexIDs(t *testing.T) {
	valid := map[string]bool{"c99-f999": true}
	in := "Edge IDs [c99-f999] real and [c100-f1000] fake."
	got, stripped := stripUnknownFactIDs(in, valid)
	want := "Edge IDs [c99-f999] real and fake."
	if got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(stripped, []string{"c100-f1000"}) {
		t.Errorf("stripped = %v", stripped)
	}
}

func TestStripUnknownFactIDs_RealisticArticleSnippet(t *testing.T) {
	valid := map[string]bool{"c00-f1": true, "c00-f2": true}
	in := strings.Join([]string{
		"## Findings",
		"",
		"> A real claim with anchor.",
		"> — Source: raw/foo.md [c00-f1]",
		"",
		"> Fabricated mid-paragraph cite [c12-f6].",
		"> — Source: raw/foo.md [c00-f2]",
	}, "\n")
	got, stripped := stripUnknownFactIDs(in, valid)
	if strings.Contains(got, "c12-f6") {
		t.Errorf("fabricated id leaked through: %q", got)
	}
	if !strings.Contains(got, "[c00-f1]") || !strings.Contains(got, "[c00-f2]") {
		t.Errorf("valid IDs got dropped: %q", got)
	}
	if !reflect.DeepEqual(stripped, []string{"c12-f6"}) {
		t.Errorf("stripped = %v", stripped)
	}
}
