package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeArticleForStale builds a minimal article with a specific updated:
// date so we can drive halfLifeDays threshold tests deterministically.
func writeArticleForStale(t *testing.T, dir, sub, slug, title, ctype, status, updated string, sourceURL string) string {
	t.Helper()
	full := filepath.Join(dir, sub, slug+".md")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString("title: \"" + title + "\"\n")
	sb.WriteString("type: " + ctype + "\n")
	if status != "" {
		sb.WriteString("status: " + status + "\n")
	}
	if updated != "" {
		sb.WriteString("updated: " + updated + "\n")
	}
	if sourceURL != "" {
		sb.WriteString("source_url: \"" + sourceURL + "\"\n")
	}
	sb.WriteString("---\n\nbody\n")
	if err := os.WriteFile(full, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return full
}

func TestHalfLifeDays_PerType(t *testing.T) {
	cases := []struct {
		typ, status string
		want        int
	}{
		{"decision", "", 180},
		{"decision", "superseded", 0},
		{"pattern", "", 365},
		{"solution", "", 365},
		{"research", "active", 90},
		{"research", "superseded", 0},
		{"tool", "", 365},
		{"idea", "", 90},
		{"project", "", 60},
		{"unknown", "", 365},
	}
	for _, c := range cases {
		got := halfLifeDays(c.typ, c.status)
		if got != c.want {
			t.Errorf("halfLifeDays(%q, %q) = %d, want %d", c.typ, c.status, got, c.want)
		}
	}
}

func TestBuildStalenessLedger_DateSignalAboveThreshold(t *testing.T) {
	dir := t.TempDir()
	// Decision with updated: 200d ago, half-life is 180d → date stale.
	now := time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -200).Format("2006-01-02")
	writeArticleForStale(t, dir, "decisions", "old", "Old Decision", "decision", "", old, "")

	counts, err := buildStalenessLedger(dir, BuildStaleOpts{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Total != 1 || counts.Date != 1 {
		t.Errorf("want 1 date-stale, got total=%d date=%d", counts.Total, counts.Date)
	}
	entries, _ := readStalenessLedger(dir)
	if len(entries) != 1 {
		t.Fatalf("ledger length: %d", len(entries))
	}
	if entries[0].AgeDays < 200 || entries[0].HalfLifeDays != 180 {
		t.Errorf("expected age≥200, half-life=180; got %+v", entries[0])
	}
}

func TestBuildStalenessLedger_DateSignalBelowThreshold(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
	recent := now.AddDate(0, 0, -30).Format("2006-01-02") // 30d < 180d half-life
	writeArticleForStale(t, dir, "decisions", "fresh", "Fresh Decision", "decision", "", recent, "")

	counts, err := buildStalenessLedger(dir, BuildStaleOpts{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Total != 0 {
		t.Errorf("fresh article should not be stale; got %+v", counts)
	}
	if _, err := os.Stat(stalenessLedgerPath(dir)); !os.IsNotExist(err) {
		t.Errorf("expected no ledger file when nothing is stale; stat err=%v", err)
	}
}

func TestBuildStalenessLedger_SupersededResearchNeverStale(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
	ancient := now.AddDate(-3, 0, 0).Format("2006-01-02") // 3 years
	writeArticleForStale(t, dir, "research", "old", "Old Research", "research", "superseded", ancient, "")

	counts, err := buildStalenessLedger(dir, BuildStaleOpts{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Total != 0 {
		t.Errorf("superseded research should never be date-stale; got %+v", counts)
	}
}

func TestBuildStalenessLedger_PreservesFirstObservedAt(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -200).Format("2006-01-02")
	writeArticleForStale(t, dir, "decisions", "old", "Old Decision", "decision", "", old, "")

	if _, err := buildStalenessLedger(dir, BuildStaleOpts{}, now); err != nil {
		t.Fatal(err)
	}
	first, _ := readStalenessLedger(dir)
	firstTS := first[0].FirstObservedAt

	later := now.Add(72 * time.Hour)
	if _, err := buildStalenessLedger(dir, BuildStaleOpts{}, later); err != nil {
		t.Fatal(err)
	}
	second, _ := readStalenessLedger(dir)
	if second[0].FirstObservedAt != firstTS {
		t.Errorf("first_observed_at must persist; got %s -> %s", firstTS, second[0].FirstObservedAt)
	}
	if second[0].LastSeenAt == firstTS {
		t.Errorf("last_seen_at should advance on rebuild")
	}
}

func TestBuildStalenessLedger_RemovesFileWhenAllResolved(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -200).Format("2006-01-02")
	path := writeArticleForStale(t, dir, "decisions", "old", "Old Decision", "decision", "", old, "")

	if _, err := buildStalenessLedger(dir, BuildStaleOpts{}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stalenessLedgerPath(dir)); err != nil {
		t.Fatalf("expected ledger to exist after first build; got %v", err)
	}

	// Touch updated: to a recent date — article becomes fresh.
	fresh := now.Format("2006-01-02")
	writeArticleForStale(t, dir, "decisions", "old", "Old Decision", "decision", "", fresh, "")
	_ = path
	if _, err := buildStalenessLedger(dir, BuildStaleOpts{}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stalenessLedgerPath(dir)); !os.IsNotExist(err) {
		t.Errorf("expected ledger removed when all entries resolved; stat err=%v", err)
	}
}

func TestUpdatedToTime_AcceptsBothShapes(t *testing.T) {
	// String form.
	if got, ok := updatedToTime("2026-01-02"); !ok || got.Year() != 2026 {
		t.Errorf("string YYYY-MM-DD: got=%v ok=%v", got, ok)
	}
	// time.Time form (yaml.v3 returns this for bare YYYY-MM-DD).
	tt := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if got, ok := updatedToTime(tt); !ok || !got.Equal(tt) {
		t.Errorf("time.Time: got=%v ok=%v", got, ok)
	}
	// Nil + nonsense.
	if _, ok := updatedToTime(nil); ok {
		t.Errorf("nil should fail")
	}
	if _, ok := updatedToTime("not a date"); ok {
		t.Errorf("nonsense string should fail")
	}
}

func TestCheckStale_OkWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	out := checkStale(dir)
	if len(out) != 1 || out[0].Status != statusOK {
		t.Errorf("empty ledger should be ok; got %+v", out)
	}
}

func TestCheckStale_WarnPerSignal(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -200).Format("2006-01-02")
	writeArticleForStale(t, dir, "decisions", "a", "A", "decision", "", old, "")
	writeArticleForStale(t, dir, "decisions", "b", "B", "decision", "", old, "")

	if _, err := buildStalenessLedger(dir, BuildStaleOpts{}, now); err != nil {
		t.Fatal(err)
	}
	out := checkStale(dir)
	warns := 0
	for _, c := range out {
		if c.Status == statusWarn {
			warns++
		}
	}
	if warns != 1 {
		t.Errorf("expected 1 warn (date), got %d (full: %+v)", warns, out)
	}
}
