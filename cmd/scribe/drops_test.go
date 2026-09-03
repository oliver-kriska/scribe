package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeDrop(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestGatherExtractFilesReportsConsumedDrops: with a files budget that
// fits two of three drops, the third must be reported as NOT consumed so
// the caller keeps it staged instead of deleting it unseen.
func TestGatherExtractFilesReportsConsumedDrops(t *testing.T) {
	root := t.TempDir()
	staging := dropStagingDir(root, "proj")
	body := strings.Repeat("knowledge ", 60) // ~600 bytes
	a := writeDrop(t, staging, "2026-09-01-a.md", body)
	b := writeDrop(t, staging, "2026-09-01-b.md", body)
	c := writeDrop(t, staging, "2026-09-01-c.md", body)
	entry := &ProjectEntry{Name: "proj", Path: t.TempDir()}

	content, consumed := gatherExtractFiles(root, entry, nil, staging, 10000, 1400)
	if len(consumed) != 2 || consumed[0] != a || consumed[1] != b {
		t.Fatalf("want a,b consumed in name order, got %v", consumed)
	}
	if !strings.Contains(content, "2026-09-01-b.md") || strings.Contains(content, "2026-09-01-c.md") {
		t.Errorf("block should carry a+b and not c:\n%s", content)
	}
	_ = c

	// Everything fits → everything consumed.
	_, all := gatherExtractFiles(root, entry, nil, staging, 10000, 100000)
	if len(all) != 3 {
		t.Errorf("want all 3 consumed under a big budget, got %v", all)
	}
	// No staging dir → nothing consumed, no panic.
	if _, none := gatherExtractFiles(root, entry, nil, filepath.Join(root, "nope"), 10000, 100000); none != nil {
		t.Errorf("want nil consumed without staging, got %v", none)
	}
}

// TestFinishDropStagingKeepsDeferred pins the cleanup contract: consumed
// drops go, deferred drops stay, the stamp advances, and the dir is
// removed only once it is empty.
func TestFinishDropStagingKeepsDeferred(t *testing.T) {
	root := t.TempDir()
	staging := dropStagingDir(root, "proj")
	a := writeDrop(t, staging, "a.md", "x")
	b := writeDrop(t, staging, "b.md", "x")
	c := writeDrop(t, staging, "c.md", "x")
	entry := &ProjectEntry{Name: "proj"}

	finishDropStaging("proj", staging, []string{a, b}, entry, "2026-09-03T00:00:00Z")
	if entry.LastDropProcessed != "2026-09-03T00:00:00Z" {
		t.Errorf("stamp must advance when anything was consumed; got %q", entry.LastDropProcessed)
	}
	if left := stagedDrops(staging); len(left) != 1 || left[0] != c {
		t.Fatalf("deferred drop must stay staged; got %v", left)
	}

	// Nothing consumed (e.g. the model call failed before reading) →
	// nothing removed, stamp untouched.
	entry.LastDropProcessed = ""
	finishDropStaging("proj", staging, nil, entry, "2026-09-04T00:00:00Z")
	if entry.LastDropProcessed != "" || len(stagedDrops(staging)) != 1 {
		t.Errorf("no-consumption call must be a no-op; stamp=%q left=%v", entry.LastDropProcessed, stagedDrops(staging))
	}

	finishDropStaging("proj", staging, []string{c}, entry, "2026-09-05T00:00:00Z")
	if dirExists(staging) {
		t.Error("staging dir must be removed once empty")
	}
}

// TestProjectsNeedingExtractionSeesStagedDrops: an otherwise unchanged
// project with drops waiting in staging must be scheduled — both for a
// drop written into an idle project and for drops deferred by the budget.
func TestProjectsNeedingExtractionSeesStagedDrops(t *testing.T) {
	root := t.TempDir()
	idle := &ProjectEntry{
		Name:          "idle",
		Path:          t.TempDir(),
		LastSHA:       "deadbeef",
		LastExtracted: time.Now().UTC().Format(time.RFC3339),
	}
	m := &Manifest{Projects: map[string]*ProjectEntry{idle.Path: idle}}
	s := &SyncCmd{}
	if got := s.projectsNeedingExtraction(root, m); len(got) != 0 {
		t.Fatalf("unchanged project must not be scheduled; got %v", got)
	}
	writeDrop(t, dropStagingDir(root, "idle"), "2026-09-03-handoff.md", "---\nscribe: true\n---\nnote\n")
	if got := s.projectsNeedingExtraction(root, m); len(got) != 1 {
		t.Errorf("staged drop must schedule the project; got %v", got)
	}
}

// TestCollectDropFilesWorktreeCollision: the main checkout and a linked
// worktree each carry .claude/<kb>/note.md — both must reach staging.
func TestCollectDropFilesWorktreeCollision(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("kb_name: testkb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainPath := t.TempDir()
	wt := t.TempDir()
	writeDrop(t, filepath.Join(mainPath, ".claude", "testkb"), "note.md", "from main")
	writeDrop(t, filepath.Join(wt, ".claude", "testkb"), "note.md", "from worktree")
	entry := &ProjectEntry{Name: "proj", Path: mainPath, Worktrees: []string{wt}}
	m := &Manifest{Projects: map[string]*ProjectEntry{mainPath: entry}}

	s := &SyncCmd{}
	if n := s.collectDropFiles(root, m); n != 2 {
		t.Fatalf("want 2 drops collected, got %d", n)
	}
	staged := stagedDrops(dropStagingDir(root, "proj"))
	if len(staged) != 2 {
		t.Fatalf("both same-named drops must be staged; got %v", staged)
	}
	bodies := make([]string, 0, len(staged))
	for _, p := range staged {
		data, _ := os.ReadFile(p)
		bodies = append(bodies, string(data))
	}
	joined := strings.Join(bodies, "|")
	if !strings.Contains(joined, "from main") || !strings.Contains(joined, "from worktree") {
		t.Errorf("one copy overwrote the other: %v", bodies)
	}
}
