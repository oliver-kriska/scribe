package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadEmbeddedSkillFiles_PopulatesBundle(t *testing.T) {
	got, err := readEmbeddedSkillFiles()
	if err != nil {
		t.Fatalf("read embedded: %v", err)
	}
	// Every file in the embedded tree must be readable and non-empty.
	for path, content := range got {
		if len(content) == 0 {
			t.Errorf("empty embedded file: %s", path)
		}
	}
	// Each skill's SKILL.md lives under its own <skill-name>/ prefix so
	// install recreates one subdirectory per skill.
	skillMD, ok := got["scribe-kb/SKILL.md"]
	if !ok {
		t.Fatal("scribe-kb/SKILL.md missing from embedded bundle")
	}
	if !strings.HasPrefix(string(skillMD), "---") {
		t.Errorf("SKILL.md must start with frontmatter, got: %s", strings.SplitN(string(skillMD), "\n", 2)[0])
	}
	if !strings.Contains(string(skillMD), "name: scribe-kb") {
		t.Errorf("SKILL.md missing `name: scribe-kb` in frontmatter")
	}
	if !strings.Contains(string(skillMD), "description: ") {
		t.Errorf("SKILL.md missing `description:` in frontmatter")
	}
	// References must include all six docs documented in the plan.
	wantRefs := []string{
		"scribe-kb/references/FRONTMATTER.md",
		"scribe-kb/references/WIKILINKS.md",
		"scribe-kb/references/STRUCTURE.md",
		"scribe-kb/references/DROP_FILES.md",
		"scribe-kb/references/QUERY.md",
		"scribe-kb/references/COMPAT.md",
	}
	for _, want := range wantRefs {
		if _, ok := got[want]; !ok {
			t.Errorf("expected reference file missing: %s", want)
		}
	}

	// The KB-tidy skill ships in the same bundle with its own frontmatter
	// and field-notes reference.
	tidyMD, ok := got["scribe-kb-tidy/SKILL.md"]
	if !ok {
		t.Fatal("scribe-kb-tidy/SKILL.md missing from embedded bundle")
	}
	if !strings.Contains(string(tidyMD), "name: scribe-kb-tidy") {
		t.Errorf("tidy SKILL.md missing `name: scribe-kb-tidy` in frontmatter")
	}
	if _, ok := got["scribe-kb-tidy/references/FIELD_NOTES.md"]; !ok {
		t.Errorf("expected scribe-kb-tidy/references/FIELD_NOTES.md in bundle")
	}
}

func TestSkillNames_DistinctSortedTopLevel(t *testing.T) {
	got, err := readEmbeddedSkillFiles()
	if err != nil {
		t.Fatalf("read embedded: %v", err)
	}
	names := skillNames(got)
	want := []string{"scribe-kb", "scribe-kb-tidy"}
	if len(names) != len(want) {
		t.Fatalf("skillNames = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("skillNames[%d] = %q, want %q (full: %v)", i, names[i], want[i], names)
		}
	}
}

func TestSkillInstall_WritesBothSkills(t *testing.T) {
	dir := t.TempDir()
	if err := (&SkillInstallCmd{Target: dir}).Run(); err != nil {
		t.Fatalf("install: %v", err)
	}
	for _, rel := range []string{
		filepath.Join("scribe-kb", "SKILL.md"),
		filepath.Join("scribe-kb-tidy", "SKILL.md"),
		filepath.Join("scribe-kb-tidy", "references", "FIELD_NOTES.md"),
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("expected installed file %s: %v", rel, err)
		}
	}
}

func TestSkillInstall_WritesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cmd := &SkillInstallCmd{Target: dir}
	if err := cmd.Run(); err != nil {
		t.Fatalf("install: %v", err)
	}
	// Top-level SKILL.md should exist.
	out := filepath.Join(dir, "scribe-kb", "SKILL.md")
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read written SKILL.md: %v", err)
	}
	if !strings.Contains(string(data), "name: scribe-kb") {
		t.Errorf("installed SKILL.md content unexpected:\n%s", string(data)[:200])
	}

	// Re-running must be a no-op (no errors, no changes).
	if err := cmd.Run(); err != nil {
		t.Fatalf("second install: %v", err)
	}
}

func TestSkillInstall_RespectsHandEditMarker(t *testing.T) {
	dir := t.TempDir()
	cmd := &SkillInstallCmd{Target: dir}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	// Hand-edit the file with the protective marker.
	out := filepath.Join(dir, "scribe-kb", "SKILL.md")
	custom := "---\nname: scribe-kb\n---\n\n<!-- scribe-skill: hand-edited, do not overwrite -->\n\nMy custom content.\n"
	if err := os.WriteFile(out, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	// Plain re-run preserves it.
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if !strings.Contains(string(got), "My custom content.") {
		t.Errorf("hand-edited content overwritten despite marker:\n%s", got)
	}
	// --force overwrites.
	cmd2 := &SkillInstallCmd{Target: dir, Force: true}
	if err := cmd2.Run(); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(out)
	if strings.Contains(string(got), "My custom content.") {
		t.Errorf("--force should have overwritten hand edit")
	}
}

func TestSkillInstall_CheckDetectsDrift(t *testing.T) {
	dir := t.TempDir()
	cmd := &SkillInstallCmd{Target: dir}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	// Mutate one file to introduce drift.
	out := filepath.Join(dir, "scribe-kb", "references", "QUERY.md")
	if err := os.WriteFile(out, []byte("drifted content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checkCmd := &SkillInstallCmd{Target: dir, Check: true}
	if err := checkCmd.Run(); err == nil {
		t.Errorf("expected --check to error on drift, got nil")
	}
}

func TestSkillInstall_CheckOnPristineInstallSucceeds(t *testing.T) {
	dir := t.TempDir()
	if err := (&SkillInstallCmd{Target: dir}).Run(); err != nil {
		t.Fatal(err)
	}
	if err := (&SkillInstallCmd{Target: dir, Check: true}).Run(); err != nil {
		t.Errorf("--check should succeed on pristine install: %v", err)
	}
}

func TestHasUserEdits(t *testing.T) {
	yes := []byte("# Hi\n<!-- scribe-skill: hand-edited, do not overwrite -->\n")
	if !hasUserEdits(yes) {
		t.Errorf("expected hand-edit detection")
	}
	no := []byte("# Hi\nno marker here\n")
	if hasUserEdits(no) {
		t.Errorf("false positive on plain content")
	}
}
