package main

import (
	"bytes"
	"fmt"
	"log/slog"
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
