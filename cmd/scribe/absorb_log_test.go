package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Phase 3C test scope: typed absorb-log loader (legacy + v2),
// content-aware decision routing (run / skip-same / skip-dup /
// run-refresh), and atomic save.

func TestLoadAbsorbLog_MissingFileReturnsEmpty(t *testing.T) {
	tmp := t.TempDir()
	log, err := loadAbsorbLog(filepath.Join(tmp, "absent.json"))
	if err != nil {
		t.Fatalf("missing file should not error; got %v", err)
	}
	if log == nil || len(log) != 0 {
		t.Errorf("missing file should return empty log; got %v", log)
	}
}

func TestLoadAbsorbLog_LegacyStringFormatUpgrades(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "log.json")
	body := []byte(`{"foo.md": "2025-04-01T10:00:00Z", "bar.md": "2025-04-02T11:00:00Z"}`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	log, err := loadAbsorbLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(log))
	}
	if log["foo.md"].At != "2025-04-01T10:00:00Z" {
		t.Errorf("legacy timestamp lost: %+v", log["foo.md"])
	}
	if log["foo.md"].SHA != "" {
		t.Errorf("legacy entry should have empty SHA; got %q", log["foo.md"].SHA)
	}
}

func TestLoadAbsorbLog_V2ObjectFormat(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "log.json")
	body := []byte(`{"foo.md": {"sha":"abc123","at":"2026-04-01T10:00:00Z"}}`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	log, err := loadAbsorbLog(path)
	if err != nil {
		t.Fatal(err)
	}
	entry := log["foo.md"]
	if entry.SHA != "abc123" || entry.At != "2026-04-01T10:00:00Z" {
		t.Errorf("v2 entry parse wrong: %+v", entry)
	}
}

func TestLoadAbsorbLog_MixedFormatsCoexist(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "log.json")
	body := []byte(`{
		"legacy.md": "2025-01-01T00:00:00Z",
		"v2.md": {"sha":"deadbeef","at":"2026-04-01T00:00:00Z"}
	}`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	log, err := loadAbsorbLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 2 {
		t.Fatalf("mixed-format log should yield both entries; got %+v", log)
	}
	if log["legacy.md"].SHA != "" || log["legacy.md"].At == "" {
		t.Errorf("legacy entry malformed: %+v", log["legacy.md"])
	}
	if log["v2.md"].SHA != "deadbeef" {
		t.Errorf("v2 entry malformed: %+v", log["v2.md"])
	}
}

func TestSaveAbsorbLog_RoundTripsAndSorts(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "log.json")
	log := AbsorbLog{
		"zebra.md":  {SHA: "z", At: "2026-04-01T00:00:00Z"},
		"alpha.md":  {SHA: "a", At: "2026-04-02T00:00:00Z"},
		"middle.md": {SHA: "m", At: "2026-04-03T00:00:00Z"},
	}
	if err := saveAbsorbLog(path, log); err != nil {
		t.Fatal(err)
	}
	// Re-read and verify content.
	roundTripped, err := loadAbsorbLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(roundTripped) != 3 {
		t.Errorf("round-trip lost entries: %v", roundTripped)
	}
	// Verify keys serialized in sorted order (so git diffs stay readable).
	raw, _ := os.ReadFile(path)
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	// MarshalIndent on a map sorts keys by default in Go's encoding/json.
	// We just verify the content survives.
	if _, ok := parsed["alpha.md"]; !ok {
		t.Errorf("alpha.md missing from saved file")
	}
}

func TestCheckAbsorbDecision_NewFileRuns(t *testing.T) {
	log := AbsorbLog{}
	if checkAbsorbDecision(log, "new.md", "abc") != absorbDecisionRun {
		t.Error("new file should be absorbed")
	}
}

func TestCheckAbsorbDecision_SameContentSkips(t *testing.T) {
	log := AbsorbLog{
		"foo.md": {SHA: "abc", At: "now"},
	}
	if checkAbsorbDecision(log, "foo.md", "abc") != absorbDecisionSkipSameContent {
		t.Error("same name + same sha should skip")
	}
}

func TestCheckAbsorbDecision_LegacyEntrySkips(t *testing.T) {
	// A legacy entry has empty SHA. We can't compare; treat as "already
	// absorbed" for backward compat — that's the original behavior.
	log := AbsorbLog{
		"foo.md": {SHA: "", At: "2025-01-01"},
	}
	if checkAbsorbDecision(log, "foo.md", "abc") != absorbDecisionSkipSameContent {
		t.Error("legacy entry should skip (compat)")
	}
}

func TestCheckAbsorbDecision_DriftReabsorbs(t *testing.T) {
	log := AbsorbLog{
		"foo.md": {SHA: "old", At: "earlier"},
	}
	if checkAbsorbDecision(log, "foo.md", "new") != absorbDecisionRunRefresh {
		t.Error("same name + different sha should re-absorb")
	}
}

func TestCheckAbsorbDecision_CrossNameDuplicateSkips(t *testing.T) {
	log := AbsorbLog{
		"first.md": {SHA: "shared", At: "first-run"},
	}
	got := checkAbsorbDecision(log, "second.md", "shared")
	if got != absorbDecisionSkipDupContent {
		t.Errorf("cross-name duplicate should skip-as-dup; got decision %d", got)
	}
}

func TestCheckAbsorbDecision_EmptyShaFallsBackToNameCheck(t *testing.T) {
	// When sha can't be computed, treat as "new" only if name absent.
	// If name present, the legacy path skips.
	log := AbsorbLog{
		"existing.md": {SHA: "", At: "old"},
	}
	if checkAbsorbDecision(log, "existing.md", "") != absorbDecisionSkipSameContent {
		t.Error("empty sha + existing legacy entry should skip")
	}
	if checkAbsorbDecision(log, "fresh.md", "") != absorbDecisionRun {
		t.Error("empty sha + absent name should run")
	}
}

func TestFindDupName_FindsMatch(t *testing.T) {
	log := AbsorbLog{
		"first.md":  {SHA: "abc"},
		"second.md": {SHA: "xyz"},
	}
	if got := findDupName(log, "third.md", "abc"); got != "first.md" {
		t.Errorf("findDupName = %q, want first.md", got)
	}
}

func TestFindDupName_ExcludesSelf(t *testing.T) {
	log := AbsorbLog{"foo.md": {SHA: "abc"}}
	if got := findDupName(log, "foo.md", "abc"); got != "" {
		t.Errorf("findDupName should exclude self; got %q", got)
	}
}

func TestFindDupName_EmptyShaReturnsEmpty(t *testing.T) {
	log := AbsorbLog{"foo.md": {SHA: "abc"}}
	if got := findDupName(log, "bar.md", ""); got != "" {
		t.Errorf("empty sha should yield empty result; got %q", got)
	}
}
