package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeProject makes a non-git project dir holding n markdown files, so
// gitChangedFiles (never-extracted path) counts exactly n.
func writeProject(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	for i := range n {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("doc%02d.md", i)), []byte("# doc\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Two held projects with staged drops sorted first and took both
// `sync --max 2` slots on every run; the size gate then skipped them, so
// no project extracted at all. The gate now runs before the cap: a held
// project without drops takes no slot, and one with drops gets a
// drops-only job.
func TestPlanExtractions_HeldProjectsDoNotTakeSlots(t *testing.T) {
	root := t.TempDir()
	cfg := &ScribeConfig{Sync: SyncConfig{MaxExtractFiles: 3}}
	m := &Manifest{Projects: map[string]*ProjectEntry{
		"a": {Name: "a-held", Path: writeProject(t, 5)},
		"b": {Name: "b-held-drops", Path: writeProject(t, 5)},
		"c": {Name: "c-small", Path: writeProject(t, 2)},
	}}
	staging := dropStagingDir(root, "b-held-drops")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "handoff.md"), []byte("---\nscriptorium: true\n---\nnote\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	jobs := planExtractions(root, cfg, m, []string{"a", "b", "c"})
	if len(jobs) != 2 {
		t.Fatalf("want 2 jobs (held-with-drops + small), got %d: %+v", len(jobs), jobs)
	}
	if j := jobs[0]; j.entry.Name != "b-held-drops" || !j.dropsOnly || j.held != 5 || len(j.changed) != 0 {
		t.Fatalf("held project with drops: want drops-only job holding 5 files, got %+v", j)
	}
	if j := jobs[1]; j.entry.Name != "c-small" || j.dropsOnly || len(j.changed) != 2 {
		t.Fatalf("small project: want a normal job with 2 changed files, got %+v", j)
	}
}

// A drops-only extraction must not advance last_sha: the held commits
// still need `scribe deep`, and a moved marker would hide them.
func TestMarkExtracted_DropsOnlyKeepsLastSHA(t *testing.T) {
	e := &ProjectEntry{LastSHA: "aaaa", LastExtracted: "2026-01-01T00:00:00Z"}
	if markExtracted(e, "bbbb", "2026-11-13T00:00:00Z", true) {
		t.Fatal("drops-only run must not count as an extracted revision")
	}
	if e.LastSHA != "aaaa" || e.LastExtracted != "2026-01-01T00:00:00Z" {
		t.Fatalf("drops-only run moved the markers: %+v", e)
	}
	if !markExtracted(e, "bbbb", "2026-11-13T00:00:00Z", false) || e.LastSHA != "bbbb" {
		t.Fatalf("full run must advance last_sha, got %+v", e)
	}
}
