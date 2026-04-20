package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestScanRollingFile covers the H2-date heading parser. If this regresses,
// _hot.md becomes blank and every new Claude Code session loses cross-project
// orientation.
func TestScanRollingFile(t *testing.T) {
	dir := t.TempDir()
	file := "decisions-log.md"
	content := `---
title: Acme Decisions
rolling: true
---

## 2026-04-08 | Chose postgres over dynamodb

Body text.

---

## 2026-04-05 | Switched to Phoenix 1.7

Body.

---

## not-a-date | Should be skipped

## 2026-04-03 | With trailing spaces

More body.
`
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := scanRollingFile(dir, "acme", file, "decision")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(entries), entries)
	}

	// First entry: check all fields.
	if entries[0].Project != "acme" {
		t.Errorf("project: got %q want %q", entries[0].Project, "acme")
	}
	if entries[0].Kind != "decision" {
		t.Errorf("kind: got %q want %q", entries[0].Kind, "decision")
	}
	if entries[0].Title != "Chose postgres over dynamodb" {
		t.Errorf("title: got %q", entries[0].Title)
	}
	wantDate, _ := time.Parse("2006-01-02", "2026-04-08")
	if !entries[0].Date.Equal(wantDate) {
		t.Errorf("date: got %v want %v", entries[0].Date, wantDate)
	}

	// Title whitespace trimmed.
	if entries[2].Title != "With trailing spaces" {
		t.Errorf("trailing-spaces entry not trimmed: %q", entries[2].Title)
	}
}

func TestScanRollingFileMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := scanRollingFile(dir, "nobody", "missing.md", "decision")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

// TestGenerateHotMDEmpty covers the "no activity" paths. Important because
// a fresh KB with no rolling files must still produce a valid hot cache,
// not crash.
func TestGenerateHotMDEmpty(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := generateHotMD(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Hot Cache",
		"## Active threads",
		"No project activity in the last 7 days",
		"## Recent decisions",
		"No decisions logged in the last 7 days",
		"## Open questions",
		"None flagged",
		"## Last updated",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing section %q", want)
		}
	}
}

// TestGenerateHotMDWithActivity is the realistic path: multiple projects
// with recent entries, one older entry past the 7-day cutoff that must be
// excluded, and one title that should land in Open questions.
func TestGenerateHotMDWithActivity(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}

	today := time.Now().Format("2006-01-02")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	oldDate := time.Now().AddDate(0, 0, -30).Format("2006-01-02") // outside 7-day window

	writeRolling := func(project, filename, body string) {
		dir := filepath.Join(projects, project)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		full := "---\nrolling: true\n---\n\n" + body
		if err := os.WriteFile(filepath.Join(dir, filename), []byte(full), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeRolling("acme", "decisions-log.md",
		"## "+today+" | Chose pgvector over qdrant\n\nBody.\n\n---\n\n"+
			"## "+oldDate+" | Ancient decision should not appear\n\nBody.\n")

	writeRolling("sposal", "learnings.md",
		"## "+yesterday+" | How should we scale the RSVP form?\n\nBody.\n")

	got, err := generateHotMD(root)
	if err != nil {
		t.Fatal(err)
	}

	// Active threads must include both projects.
	if !strings.Contains(got, "**acme**") {
		t.Errorf("missing acme in active threads:\n%s", got)
	}
	if !strings.Contains(got, "**sposal**") {
		t.Errorf("missing sposal in active threads:\n%s", got)
	}

	// Recent decision present.
	if !strings.Contains(got, "Chose pgvector over qdrant") {
		t.Errorf("missing recent decision:\n%s", got)
	}

	// Old decision outside cutoff must be absent.
	if strings.Contains(got, "Ancient decision") {
		t.Errorf("old decision leaked past cutoff:\n%s", got)
	}

	// Learning with '?' must appear in Open questions section.
	openIdx := strings.Index(got, "## Open questions")
	lastIdx := strings.Index(got, "## Last updated")
	if openIdx < 0 || lastIdx < 0 || openIdx >= lastIdx {
		t.Fatal("Open questions section missing or misordered")
	}
	openSection := got[openIdx:lastIdx]
	if !strings.Contains(openSection, "How should we scale the RSVP form?") {
		t.Errorf("question not promoted to Open questions:\n%s", openSection)
	}
}

// TestTruncateUTF8 covers the rune-safe clipping used for title lines in
// the hot cache. Must not split multi-byte characters.
func TestTruncateUTF8(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"short passthrough", "hello", 10, "hello"},
		{"exact length", "hello", 5, "hello"},
		{"truncate ascii", "abcdefghij", 7, "abcd..."},
		{"truncate multibyte", "žžžžžžžžžž", 5, "žž..."},
		{"too small for ellipsis", "abcdef", 3, "abc"},
		{"empty", "", 10, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateUTF8(tc.in, tc.n); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// TestAddHookIfMissing is the idempotency guard for installHotHooks.
// If this regresses, running `scribe hot --install` twice duplicates the
// hook in ~/.claude/settings.json.
func TestAddHookIfMissing(t *testing.T) {
	cmd := "cat /tmp/scribe-kb/wiki/_hot.md"
	entry := map[string]any{
		"hooks": []any{
			map[string]any{"type": "command", "command": cmd},
		},
	}

	t.Run("nil existing adds entry", func(t *testing.T) {
		got := addHookIfMissing(nil, entry, cmd)
		if len(got) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(got))
		}
	})

	t.Run("empty slice adds entry", func(t *testing.T) {
		got := addHookIfMissing([]any{}, entry, cmd)
		if len(got) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(got))
		}
	})

	t.Run("duplicate command is not re-added", func(t *testing.T) {
		existing := []any{entry}
		got := addHookIfMissing(existing, entry, cmd)
		if len(got) != 1 {
			t.Errorf("duplicate added — got %d entries", len(got))
		}
	})

	t.Run("different command is added alongside", func(t *testing.T) {
		otherCmd := "echo other"
		otherEntry := map[string]any{
			"hooks": []any{
				map[string]any{"type": "command", "command": otherCmd},
			},
		}
		existing := []any{otherEntry}
		got := addHookIfMissing(existing, entry, cmd)
		if len(got) != 2 {
			t.Errorf("expected 2 entries, got %d", len(got))
		}
	})

	t.Run("result is valid JSON (round-trip)", func(t *testing.T) {
		got := addHookIfMissing(nil, entry, cmd)
		wrapper := map[string]any{"SessionStart": got}
		data, err := json.Marshal(wrapper)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), cmd) {
			t.Errorf("command missing from marshaled JSON: %s", data)
		}
	})
}
