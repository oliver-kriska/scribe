package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureSyncLog swaps the default slog logger for one writing to a buffer,
// so tests can assert on what logMsg emits. Restored on cleanup.
func captureSyncLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestProjectsNeedingExtractionUnchangedSummary locks the collapsed
// unchanged-skip output (issue #20): no per-project "unchanged, skipping"
// lines, one "N project(s) unchanged" summary when anything was skipped —
// including when ALL projects are unchanged, so cron logs still show the
// run scanned them.
func TestProjectsNeedingExtractionUnchangedSummary(t *testing.T) {
	cases := []struct {
		name        string
		unchanged   int
		changed     int
		wantSummary string // "" = summary line must be absent
	}{
		{name: "no unchanged projects", unchanged: 0, changed: 2, wantSummary: ""},
		{name: "some unchanged", unchanged: 2, changed: 1, wantSummary: "2 project(s) unchanged"},
		{name: "all unchanged", unchanged: 3, changed: 0, wantSummary: "3 project(s) unchanged"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manifest{Projects: map[string]*ProjectEntry{}}
			for i := range tc.unchanged {
				// Non-git dir, previously extracted, no .md files newer
				// than the marker → counted as unchanged.
				m.Projects[fmt.Sprintf("idle-%d", i)] = &ProjectEntry{
					Path:          t.TempDir(),
					LastSHA:       "deadbeef",
					LastExtracted: time.Now().UTC().Format(time.RFC3339),
				}
			}
			for i := range tc.changed {
				// Never extracted → needs extraction.
				m.Projects[fmt.Sprintf("busy-%d", i)] = &ProjectEntry{
					Path: t.TempDir(),
				}
			}

			buf := captureSyncLog(t)
			s := &SyncCmd{}
			got := s.projectsNeedingExtraction(t.TempDir(), m)

			if len(got) != tc.changed {
				t.Errorf("projectsNeedingExtraction returned %v, want %d project(s)", got, tc.changed)
			}

			out := buf.String()
			if strings.Contains(out, "unchanged, skipping") {
				t.Errorf("per-project unchanged-skip line should be collapsed, got:\n%s", out)
			}
			if tc.wantSummary == "" {
				if strings.Contains(out, "unchanged") {
					t.Errorf("no summary expected with 0 unchanged projects, got:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, tc.wantSummary) {
				t.Errorf("summary %q missing from output:\n%s", tc.wantSummary, out)
			}
		})
	}
}

// TestProjectsNeedingExtractionPendingDrops locks the drop-file gap: a
// project whose repo has not moved is normally "unchanged", but a drop file
// staged into output/drops-<name>/ is pending work that ONLY extractProject
// consumes. Before this, a low-churn repo stranded its drops forever —
// collectDropFiles re-copied and re-logged the same files every run while
// the summary reported them collected and absorbed 0.
func TestProjectsNeedingExtractionPendingDrops(t *testing.T) {
	root := t.TempDir()
	idle := &ProjectEntry{
		Name:          "idle-project",
		Path:          t.TempDir(),
		LastSHA:       "deadbeef",
		LastExtracted: time.Now().UTC().Format(time.RFC3339),
	}
	m := &Manifest{Projects: map[string]*ProjectEntry{idle.Path: idle}}
	s := &SyncCmd{}
	captureSyncLog(t)

	// Baseline: unchanged and no drops staged → not a candidate.
	if got := s.projectsNeedingExtraction(root, m); len(got) != 0 {
		t.Fatalf("unchanged project with no drops should not need extraction, got %v", got)
	}

	// Stage a drop exactly as collectDropFiles does (entry.Name, not the key).
	staging := filepath.Join(root, "output", "drops-"+idle.Name)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "2026-01-01-note.md"), []byte("# note\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := s.projectsNeedingExtraction(root, m)
	if len(got) != 1 {
		t.Fatalf("project with a staged drop file should need extraction, got %v", got)
	}
	if got[0] != idle.Path {
		t.Errorf("returned key %q, want the manifest key %q", got[0], idle.Path)
	}
}

// TestHasPendingDropsIgnoresNonMarkdownAndMissingDir guards the predicate
// itself: only *.md counts (collectDropFiles writes markdown), and a
// missing staging dir is not an error.
func TestHasPendingDropsIgnoresNonMarkdownAndMissingDir(t *testing.T) {
	root := t.TempDir()
	if hasPendingDrops(root, "nope") {
		t.Error("missing staging dir should report no pending drops")
	}

	staging := filepath.Join(root, "output", "drops-proj")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	if hasPendingDrops(root, "proj") {
		t.Error("empty staging dir should report no pending drops")
	}

	if err := os.WriteFile(filepath.Join(staging, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasPendingDrops(root, "proj") {
		t.Error("non-markdown file should not count as a pending drop")
	}

	if err := os.WriteFile(filepath.Join(staging, "real.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasPendingDrops(root, "proj") {
		t.Error("markdown drop file should count as pending")
	}
}
