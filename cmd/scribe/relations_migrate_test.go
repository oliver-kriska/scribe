package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubClassifier replaces classifierFn so the test deterministically
// drives migration outcomes. Returns one ClassifierResult per target.
func stubClassifier(t *testing.T, mapping map[string]ClassifierResult) func() {
	t.Helper()
	prev := classifierFn
	classifierFn = func(c migrationCandidate, _ string) ([]ClassifierResult, error) {
		out := make([]ClassifierResult, 0, len(c.Targets))
		for _, target := range c.Targets {
			r, ok := mapping[c.SourceTitle+"|"+target]
			if !ok {
				// Fall back to a "no-typed-kind" verdict.
				r = ClassifierResult{Target: target, Kind: nil, Confidence: "low", Reasoning: "stub default"}
			} else {
				r.Target = target
			}
			out = append(out, r)
		}
		return out, nil
	}
	return func() { classifierFn = prev }
}

func writeMigrationArticle(t *testing.T, root, sub, slug, title, ctype string, related []string, locked bool) string {
	t.Helper()
	full := filepath.Join(root, sub, slug+".md")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString("title: \"" + title + "\"\n")
	sb.WriteString("type: " + ctype + "\n")
	sb.WriteString("created: 2026-01-01\n")
	sb.WriteString("updated: 2026-05-07\n")
	if locked {
		sb.WriteString("relations_locked: true\n")
	}
	if len(related) > 0 {
		sb.WriteString("related: [")
		for i, r := range related {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("\"[[" + r + "]]\"")
		}
		sb.WriteString("]\n")
	}
	sb.WriteString("---\n\nbody words go here\n")
	if err := os.WriteFile(full, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return full
}

func kindPtr(s string) *string { return &s }

func TestExtractWikilinkList_StripsBrackets(t *testing.T) {
	got := extractWikilinkList([]any{"[[A]]", "[[B|alias]]", "[[C]]"})
	want := []string{"A", "B", "C"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func TestKindAllowedForType_ClosedSet(t *testing.T) {
	if !kindAllowedForType(RelSupersedes, "decision") {
		t.Error("supersedes allowed on decision")
	}
	if kindAllowedForType(RelSupersedes, "research") {
		t.Error("supersedes must NOT be allowed on research")
	}
}

func TestConfidenceRank_Ordering(t *testing.T) {
	if confidenceRank("high") <= confidenceRank("medium") {
		t.Error("high > medium")
	}
	if confidenceRank("medium") <= confidenceRank("low") {
		t.Error("medium > low")
	}
	if confidenceRank("garbage") != 0 {
		t.Error("unknown confidences rank 0")
	}
}

func TestCollectMigrationCandidates_SkipsLockedAndEmpty(t *testing.T) {
	dir := t.TempDir()
	writeMigrationArticle(t, dir, "decisions", "a", "A", "decision", []string{"B", "C"}, false)
	writeMigrationArticle(t, dir, "decisions", "b", "B", "decision", []string{"A"}, true) // locked
	writeMigrationArticle(t, dir, "decisions", "c", "C", "decision", nil, false)          // empty related

	cands, err := collectMigrationCandidates(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].SourceTitle != "A" {
		t.Errorf("expected only A; got %d candidates: %+v", len(cands), cands)
	}
}

func TestRelationsMigrate_AppliesTypedAndLogs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SCRIBE_KB", dir)
	writeMigrationArticle(t, dir, "decisions", "a", "A", "decision", []string{"B"}, false)
	writeMigrationArticle(t, dir, "decisions", "b", "B", "decision", nil, false)

	restore := stubClassifier(t, map[string]ClassifierResult{
		"A|B": {Kind: kindPtr("supersedes"), Confidence: "high", Reasoning: "A explicitly says it replaces B"},
	})
	defer restore()

	cmd := &RelationsMigrateCmd{Model: "stub", MinConf: "medium"}
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// A should now have supersedes: [[B]] and no related: line
	aBytes, _ := os.ReadFile(filepath.Join(dir, "decisions", "a.md"))
	a := string(aBytes)
	if !strings.Contains(a, "supersedes: ") || !strings.Contains(a, "[[B]]") {
		t.Errorf("expected supersedes:[[B]] in A; got:\n%s", a)
	}
	if strings.Contains(a, "related: ") && strings.Contains(a, "[[B]]") {
		// related: may still exist but must not contain B
		// Phase 6A v2's removeFromRelated drops the entry; the line itself
		// disappears when the list becomes empty.
		t.Errorf("[[B]] should be removed from related: in A; got:\n%s", a)
	}

	// B should have superseded_by: [[A]] (auto-reverse)
	bBytes, _ := os.ReadFile(filepath.Join(dir, "decisions", "b.md"))
	b := string(bBytes)
	if !strings.Contains(b, "superseded_by: ") || !strings.Contains(b, "[[A]]") {
		t.Errorf("expected superseded_by:[[A]] auto-reverse on B; got:\n%s", b)
	}

	// Migration log file must exist with one entry.
	matches, _ := filepath.Glob(filepath.Join(dir, "wiki", "_relations_migration_*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("expected 1 migration log; got %d", len(matches))
	}
	entries, _ := readMigrationLog(matches[0])
	if len(entries) != 1 || entries[0].ToField != "supersedes" || entries[0].Target != "B" {
		t.Errorf("log entries: %+v", entries)
	}
}

func TestRelationsMigrate_DryRunMakesNoChanges(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SCRIBE_KB", dir)
	writeMigrationArticle(t, dir, "decisions", "a", "A", "decision", []string{"B"}, false)
	writeMigrationArticle(t, dir, "decisions", "b", "B", "decision", nil, false)

	restore := stubClassifier(t, map[string]ClassifierResult{
		"A|B": {Kind: kindPtr("supersedes"), Confidence: "high", Reasoning: "..."},
	})
	defer restore()

	before, _ := os.ReadFile(filepath.Join(dir, "decisions", "a.md"))
	cmd := &RelationsMigrateCmd{Model: "stub", MinConf: "medium", DryRun: true}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "decisions", "a.md"))
	if !bytes.Equal(before, after) {
		t.Errorf("dry-run must not modify file")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "wiki", "_relations_migration_*.jsonl"))
	if len(matches) != 0 {
		t.Errorf("dry-run must not write a migration log; got %d", len(matches))
	}
	// But sidecar IS written so re-runs short-circuit.
	scPath := classifierSidecarPath(dir, "decisions/a.md")
	if _, err := os.Stat(scPath); err != nil {
		t.Errorf("sidecar should exist after dry-run; got %v", err)
	}
}

func TestRelationsMigrate_BelowConfidenceThresholdSkips(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SCRIBE_KB", dir)
	writeMigrationArticle(t, dir, "decisions", "a", "A", "decision", []string{"B"}, false)
	writeMigrationArticle(t, dir, "decisions", "b", "B", "decision", nil, false)

	restore := stubClassifier(t, map[string]ClassifierResult{
		"A|B": {Kind: kindPtr("supersedes"), Confidence: "low", Reasoning: "weak signal"},
	})
	defer restore()

	cmd := &RelationsMigrateCmd{Model: "stub", MinConf: "medium"}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(dir, "decisions", "a.md"))
	if strings.Contains(string(a), "supersedes:") {
		t.Errorf("low-confidence verdict must not write the typed edge; got:\n%s", a)
	}
}

func TestRelationsMigrate_RejectsKindOutsideAllowedSet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SCRIBE_KB", dir)
	writeMigrationArticle(t, dir, "research", "r", "R", "research", []string{"S"}, false)
	writeMigrationArticle(t, dir, "research", "s", "S", "research", nil, false)

	restore := stubClassifier(t, map[string]ClassifierResult{
		// supersedes is decision-only — must be rejected for research source.
		"R|S": {Kind: kindPtr("supersedes"), Confidence: "high", Reasoning: "wrong kind for type"},
	})
	defer restore()

	cmd := &RelationsMigrateCmd{Model: "stub", MinConf: "medium"}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	r, _ := os.ReadFile(filepath.Join(dir, "research", "r.md"))
	if strings.Contains(string(r), "supersedes:") {
		t.Errorf("supersedes is not allowed on research; must not be written. got:\n%s", r)
	}
}

func TestRelationsMigrate_NullKindLeavesRelated(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SCRIBE_KB", dir)
	writeMigrationArticle(t, dir, "decisions", "a", "A", "decision", []string{"B"}, false)

	restore := stubClassifier(t, map[string]ClassifierResult{
		"A|B": {Kind: nil, Confidence: "low", Reasoning: "no clear typed link"},
	})
	defer restore()

	cmd := &RelationsMigrateCmd{Model: "stub", MinConf: "medium"}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(dir, "decisions", "a.md"))
	if !strings.Contains(string(a), "related:") || !strings.Contains(string(a), "[[B]]") {
		t.Errorf("null verdict must keep [[B]] in related:; got:\n%s", a)
	}
}

func TestMigrationLog_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log.jsonl")
	for _, e := range []MigrationLogEntry{
		{TS: "2026-01-01T00:00:00Z", Source: "decisions/a.md", Target: "B", ToField: "supersedes"},
		{TS: "2026-01-01T00:00:01Z", Source: "decisions/c.md", Target: "D", ToField: "contradicts"},
	} {
		if err := appendMigrationLog(logPath, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readMigrationLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Target != "B" || got[1].ToField != "contradicts" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestRelationsMigrate_RevertRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SCRIBE_KB", dir)
	writeMigrationArticle(t, dir, "decisions", "a", "A", "decision", []string{"B"}, false)
	writeMigrationArticle(t, dir, "decisions", "b", "B", "decision", nil, false)

	restore := stubClassifier(t, map[string]ClassifierResult{
		"A|B": {Kind: kindPtr("supersedes"), Confidence: "high", Reasoning: "..."},
	})
	defer restore()

	if err := (&RelationsMigrateCmd{Model: "stub", MinConf: "medium"}).Run(); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "wiki", "_relations_migration_*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("expected 1 log; got %d", len(matches))
	}

	// Revert.
	if err := (&RelationsMigrateRevertCmd{Log: matches[0]}).Run(); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(dir, "decisions", "a.md"))
	if strings.Contains(string(a), "supersedes:") {
		t.Errorf("revert must remove the typed edge; got:\n%s", a)
	}
	if !strings.Contains(string(a), "related:") || !strings.Contains(string(a), "[[B]]") {
		t.Errorf("revert must restore [[B]] to related:; got:\n%s", a)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "decisions", "b.md"))
	if strings.Contains(string(b), "superseded_by:") {
		t.Errorf("revert must remove the auto-reverse edge; got:\n%s", b)
	}
}
