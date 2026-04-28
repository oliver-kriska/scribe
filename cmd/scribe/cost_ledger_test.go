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

func TestIsRateLimited_NewPatterns(t *testing.T) {
	for _, line := range []string{
		"Error: rate limit exceeded",
		"HTTP 429 Too Many Requests",
		"the model is overloaded",
		"You have hit your usage limit",
		"5-hour limit reached, try again in 23 minutes",
		"5 hour limit reached",
		"weekly limit reached",
		"Quota exceeded for this resource",
		"resource_exhausted: please retry",
	} {
		if !isRateLimited(line) {
			t.Errorf("expected isRateLimited to match %q", line)
		}
	}
}

func TestIsRateLimited_NegativeCases(t *testing.T) {
	for _, line := range []string{
		"",
		"all good",
		"connection refused",
		"file not found",
	} {
		if isRateLimited(line) {
			t.Errorf("did not expect isRateLimited to match %q", line)
		}
	}
}

func TestSummarizeCosts_PrefersActualUSDWhenPresent(t *testing.T) {
	tmp := t.TempDir()
	in1, out1 := int64(1000), int64(500)
	cost1 := 0.015
	for _, e := range []CostEntry{
		// Two real-usage rows.
		{Model: "haiku", DurationMS: 1000, OK: true, PromptChars: 4000, InputTokens: &in1, OutputTokens: &out1, CostUSD: &cost1},
		{Model: "haiku", DurationMS: 1000, OK: true, PromptChars: 4000, InputTokens: &in1, OutputTokens: &out1, CostUSD: &cost1},
		// One legacy row (no token data).
		{Model: "haiku", DurationMS: 500, OK: true, PromptChars: 4000},
	} {
		appendCostEntry(tmp, e)
	}
	rows, err := summarizeCosts(tmp, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row; got %+v", rows)
	}
	r := rows[0]
	if r.CallsWithUsage != 2 || r.Calls != 3 {
		t.Errorf("usage tally wrong: CallsWithUsage=%d Calls=%d", r.CallsWithUsage, r.Calls)
	}
	if r.InputTokens != 2000 || r.OutputTokens != 1000 {
		t.Errorf("token rollup wrong: in=%d out=%d", r.InputTokens, r.OutputTokens)
	}
	if r.ActualUSD < 0.029 || r.ActualUSD > 0.031 {
		t.Errorf("actual USD should be ~0.030; got %f", r.ActualUSD)
	}
	// Estimate covers only the unmeasured share (1 of 3 calls = 1/3
	// of PromptChars). Should be small but non-zero.
	if r.EstUSDHigh == 0 {
		t.Errorf("expected non-zero estimate for unmeasured share; got %+v", r)
	}
}

func TestSummarizeCosts_AllLegacyRowsKeepEstimates(t *testing.T) {
	tmp := t.TempDir()
	for i := 0; i < 5; i++ {
		appendCostEntry(tmp, CostEntry{Model: "haiku", PromptChars: 8000, OK: true, DurationMS: 1000})
	}
	rows, err := summarizeCosts(tmp, 0)
	if err != nil {
		t.Fatal(err)
	}
	r := rows[0]
	if r.CallsWithUsage != 0 {
		t.Errorf("legacy-only data should not have usage entries; got %d", r.CallsWithUsage)
	}
	if r.ActualUSD != 0 {
		t.Errorf("legacy-only data should not have ActualUSD; got %f", r.ActualUSD)
	}
	if r.EstUSDHigh == 0 {
		t.Errorf("legacy-only data must have estimate; got %+v", r)
	}
}

func TestFormatRowUSD_BranchSelection(t *testing.T) {
	cases := []struct {
		name   string
		row    CostSummary
		expect string
	}{
		{"real only", CostSummary{ActualUSD: 1.234}, "$1.2340"},
		{"est only", CostSummary{EstUSDLow: 0.1, EstUSDHigh: 0.5}, "$0.1000-0.5000"},
		{"mixed", CostSummary{ActualUSD: 1.0, EstUSDHigh: 0.5}, "$1.0000+~$0.50"},
		{"none", CostSummary{}, "—"},
	}
	for _, tc := range cases {
		got := formatRowUSD(tc.row)
		if got != tc.expect {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.expect)
		}
	}
}

func TestParseClaudeResult_HappyPath(t *testing.T) {
	stdout := `{"type":"result","subtype":"success","is_error":false,"result":"ok","usage":{"input_tokens":10,"output_tokens":20},"total_cost_usd":0.001}`
	env, ok := parseClaudeResult(stdout)
	if !ok {
		t.Fatal("expected parse to succeed")
	}
	if env.Result != "ok" || env.Usage.InputTokens != 10 || env.Usage.OutputTokens != 20 {
		t.Errorf("envelope wrong: %+v", env)
	}
}

func TestParseClaudeResult_TrailingHookNoise(t *testing.T) {
	// CMUX / SessionEnd hooks can leak text after the JSON.
	stdout := `{"type":"result","subtype":"success","is_error":false,"result":"ok","usage":{"input_tokens":5,"output_tokens":3}}` + "\n" +
		"SessionEnd hook [cmux] failed: Hook canceled\n" //nolint:misspell // fixture echoes US spelling; CMUX itself emits both spellings
	env, ok := parseClaudeResult(stdout)
	if !ok {
		t.Fatal("trailing hook noise should not break parse")
	}
	if env.Usage.InputTokens != 5 {
		t.Errorf("usage lost: %+v", env)
	}
}

func TestParseClaudeResult_LeadingBannerNoise(t *testing.T) {
	stdout := "Loading CLAUDE.md from /path...\n" +
		"Plugin sync: 0 changes\n" +
		`{"type":"result","subtype":"success","is_error":false,"result":"ok"}` + "\n"
	env, ok := parseClaudeResult(stdout)
	if !ok {
		t.Fatal("leading banner should not break parse")
	}
	if env.Result != "ok" {
		t.Errorf("envelope wrong: %+v", env)
	}
}

func TestParseClaudeResult_NoEnvelopeReturnsFalse(t *testing.T) {
	for _, stdout := range []string{
		"",
		"plain text response",
		`{"type":"unknown","other":"value"}`,
	} {
		_, ok := parseClaudeResult(stdout)
		if ok {
			t.Errorf("did not expect parse to succeed on %q", stdout)
		}
	}
}

func TestIsRateLimitSubtype(t *testing.T) {
	for _, sub := range []string{"rate_limit_exceeded", "QUOTA_EXCEEDED", "user_limit_reached"} {
		if !isRateLimitSubtype(sub) {
			t.Errorf("expected %q to match", sub)
		}
	}
	for _, sub := range []string{"", "success", "error_max_turns"} {
		if isRateLimitSubtype(sub) {
			t.Errorf("did not expect %q to match", sub)
		}
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
