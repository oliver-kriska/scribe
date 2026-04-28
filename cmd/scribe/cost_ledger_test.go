package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Phase 3D test scope: cost ledger persistence (append + parse),
// summarization (model rollup, sort order, day-window filter),
// op-label context plumbing, and rate-table cost estimation.

func TestAppendCostEntry_WritesJSONLine(t *testing.T) {
	tmp := t.TempDir()
	entry := CostEntry{
		Timestamp:   "2026-04-28T10:00:00Z",
		Model:       "haiku",
		Op:          "absorb-pass1",
		DurationMS:  1234,
		PromptChars: 5000,
		OK:          true,
	}
	appendCostEntry(tmp, entry)
	day := time.Now().UTC().Format("2006-01-02")
	dayFile := filepath.Join(tmp, "output", "costs", day+".jsonl")
	data, err := os.ReadFile(dayFile)
	if err != nil {
		t.Fatalf("expected day file at %s; got %v", dayFile, err)
	}
	var got CostEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &got); err != nil {
		t.Fatalf("appended line should parse as JSON; got %q (%v)", data, err)
	}
	if got.Model != "haiku" || got.Op != "absorb-pass1" || got.DurationMS != 1234 {
		t.Errorf("round-trip lost fields: %+v", got)
	}
}

func TestAppendCostEntry_AppendsMultiple(t *testing.T) {
	tmp := t.TempDir()
	for i := 0; i < 3; i++ {
		appendCostEntry(tmp, CostEntry{Model: "haiku", OK: true})
	}
	day := time.Now().UTC().Format("2006-01-02")
	data, _ := os.ReadFile(filepath.Join(tmp, "output", "costs", day+".jsonl"))
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Errorf("expected 3 appended lines; got %d", len(lines))
	}
}

func TestAppendCostEntry_EmptyRootIsNoop(_ *testing.T) {
	// Should not panic, should not write anywhere.
	appendCostEntry("", CostEntry{Model: "haiku"})
}

func TestSummarizeCosts_AggregatesByModel(t *testing.T) {
	tmp := t.TempDir()
	for _, e := range []CostEntry{
		{Model: "haiku", DurationMS: 1000, PromptChars: 4000, OK: true},
		{Model: "haiku", DurationMS: 2000, PromptChars: 8000, OK: true},
		{Model: "sonnet", DurationMS: 5000, PromptChars: 16000, OK: false, ErrKind: "timeout"},
	} {
		appendCostEntry(tmp, e)
	}
	rows, err := summarizeCosts(tmp, 0)
	if err != nil {
		t.Fatal(err)
	}
	byModel := map[string]CostSummary{}
	for _, r := range rows {
		byModel[r.Model] = r
	}
	if h := byModel["haiku"]; h.Calls != 2 || h.OK != 2 || h.WallclockSeconds != 3.0 || h.PromptChars != 12000 {
		t.Errorf("haiku rollup wrong: %+v", h)
	}
	if s := byModel["sonnet"]; s.Calls != 1 || s.OK != 0 || s.WallclockSeconds != 5.0 {
		t.Errorf("sonnet rollup wrong: %+v", s)
	}
}

func TestSummarizeCosts_SortsByEstimatedCost(t *testing.T) {
	tmp := t.TempDir()
	// haiku is cheap; opus is expensive. With same prompt size, opus
	// should sort first.
	for i := 0; i < 5; i++ {
		appendCostEntry(tmp, CostEntry{Model: "haiku", PromptChars: 100000, OK: true})
	}
	appendCostEntry(tmp, CostEntry{Model: "opus", PromptChars: 100000, OK: true})

	rows, err := summarizeCosts(tmp, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 model rows; got %d", len(rows))
	}
	if rows[0].Model != "opus" {
		t.Errorf("most expensive should sort first; got %s", rows[0].Model)
	}
}

func TestSummarizeCosts_MissingDirReturnsNil(t *testing.T) {
	tmp := t.TempDir()
	rows, err := summarizeCosts(tmp, 0)
	if err != nil {
		t.Fatalf("missing dir should not error; got %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected empty rows; got %v", rows)
	}
}

func TestSummarizeCosts_DayWindowFiltersOldFiles(t *testing.T) {
	tmp := t.TempDir()
	costsDir := filepath.Join(tmp, "output", "costs")
	if err := os.MkdirAll(costsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Old file: 30 days ago.
	old := time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02")
	oldEntry, _ := json.Marshal(CostEntry{Model: "haiku", DurationMS: 1000, OK: true})
	if err := os.WriteFile(filepath.Join(costsDir, old+".jsonl"), append(oldEntry, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	// Today's file.
	today := time.Now().UTC().Format("2006-01-02")
	newEntry, _ := json.Marshal(CostEntry{Model: "sonnet", DurationMS: 2000, OK: true})
	if err := os.WriteFile(filepath.Join(costsDir, today+".jsonl"), append(newEntry, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := summarizeCosts(tmp, 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Model == "haiku" {
			t.Errorf("7-day window should exclude 30-day-old haiku entries; got %v", rows)
		}
	}
}

func TestSummarizeCosts_SkipsCorruptLines(t *testing.T) {
	tmp := t.TempDir()
	costsDir := filepath.Join(tmp, "output", "costs")
	if err := os.MkdirAll(costsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format("2006-01-02")
	body := "{not valid json\n" +
		`{"model":"haiku","duration_ms":1000,"ok":true}` + "\n" +
		"another bad line\n"
	if err := os.WriteFile(filepath.Join(costsDir, today+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := summarizeCosts(tmp, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Calls != 1 {
		t.Errorf("corrupt lines should be skipped; got %+v", rows)
	}
}

func TestOpLabel_RoundTripsThroughContext(t *testing.T) {
	ctx := withOpLabel(context.Background(), "absorb-pass1")
	if got := opLabelFromContext(ctx); got != "absorb-pass1" {
		t.Errorf("op label round-trip: got %q want absorb-pass1", got)
	}
}

func TestOpLabel_AbsentReturnsEmpty(t *testing.T) {
	if got := opLabelFromContext(context.Background()); got != "" {
		t.Errorf("missing op label should return empty; got %q", got)
	}
}

func TestOpLabel_NilContextSafe(t *testing.T) {
	//nolint:staticcheck // intentionally exercise the nil branch
	if got := opLabelFromContext(nil); got != "" {
		t.Errorf("nil ctx should return empty; got %q", got)
	}
}

func TestSummarizeCosts_TallyByErrKind(t *testing.T) {
	tmp := t.TempDir()
	for _, e := range []CostEntry{
		{Model: "haiku", DurationMS: 1000, OK: true},
		{Model: "haiku", DurationMS: 0, OK: false, ErrKind: "canceled"},
		{Model: "haiku", DurationMS: 0, OK: false, ErrKind: "canceled"},
		{Model: "haiku", DurationMS: 600000, OK: false, ErrKind: "timeout"},
		{Model: "haiku", DurationMS: 50, OK: false, ErrKind: "rate_limit"},
		{Model: "haiku", DurationMS: 50, OK: false, ErrKind: "other"},
	} {
		appendCostEntry(tmp, e)
	}
	rows, err := summarizeCosts(tmp, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.OK != 1 || r.Canceled != 2 || r.Timeout != 1 || r.RateLimit != 1 {
		t.Errorf("err-kind tally wrong: %+v", r)
	}
	// "other" doesn't fall into any specific bucket — that's fine,
	// it shows up as Calls - (OK + Canceled + Timeout + RateLimit).
	if r.Calls != 6 {
		t.Errorf("Calls should still count every entry: %+v", r)
	}
}

func TestModelRateUSDPerMillion_KnownModelsHavePrices(t *testing.T) {
	for _, m := range []string{"haiku", "sonnet", "opus"} {
		rate, ok := modelRateUSDPerMillion[m]
		if !ok {
			t.Errorf("expected pricing for model %s", m)
			continue
		}
		if rate[0] <= 0 || rate[1] <= 0 {
			t.Errorf("model %s has non-positive rates: %v", m, rate)
		}
		if rate[1] <= rate[0] {
			t.Errorf("model %s output rate %f should exceed input rate %f", m, rate[1], rate[0])
		}
	}
}
