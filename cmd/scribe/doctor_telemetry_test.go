package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var telemetryNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// ledgerRow builds a cost-ledger row. models nil + turns 0 = a pre-0.6.0
// row without envelope telemetry.
func ledgerRow(day, op, alias string, models []string, turns, denials int) CostEntry {
	e := CostEntry{Timestamp: day + "T03:00:00Z", Provider: "anthropic", Model: alias, Op: op, OK: true, ResolvedModels: models, Denials: denials}
	if turns > 0 {
		e.NumTurns = &turns
	}
	if denials > 0 {
		e.DeniedTools = map[string]int{"Bash": denials}
	}
	return e
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeLedger(t *testing.T, root string, rows []CostEntry) {
	t.Helper()
	dir := filepath.Join(root, "output", "costs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		f, err := os.OpenFile(filepath.Join(dir, ledgerDay(r.Timestamp)+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		b := mustJSON(t, r)
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
}

func TestCheckTelemetry(t *testing.T) {
	s5 := []string{"claude-sonnet-5"}
	s55 := []string{"claude-sonnet-5-5"}
	h := []string{"claude-haiku-5"}
	cases := []struct {
		name string
		rows []CostEntry
		// want maps check name → status; every check must be listed.
		want       map[string]checkStatus
		wantDetail string
	}{
		{
			name:       "no ledger",
			want:       map[string]checkStatus{"dream telemetry": statusOK},
			wantDetail: "no dream runs in the cost ledger",
		},
		{
			name: "pre-0.6.0 rows only",
			rows: []CostEntry{
				ledgerRow("2026-11-01", "dream", "sonnet", nil, 0, 0),
				ledgerRow("2026-11-05", "dream", "sonnet", nil, 0, 0),
			},
			want:       map[string]checkStatus{"dream telemetry": statusOK},
			wantDetail: "latest dream run 2026-11-05 on anthropic/sonnet (ok) has no telemetry: written before envelope telemetry was recorded",
		},
		{
			// Orchestrator dreams on a hosted provider never carry claude -p
			// telemetry; the check must not read as "no dream runs".
			name: "non-claude provider rows only",
			rows: []CostEntry{
				{Timestamp: "2026-11-04T01:00:00Z", Provider: "together", Model: "together/M3", Op: "dream", OK: true},
				{Timestamp: "2026-11-05T02:10:00Z", Provider: "together", Model: "together/M3", Op: "dream-hot", ErrKind: "other"},
			},
			want:       map[string]checkStatus{"dream telemetry": statusOK},
			wantDetail: "latest dream-hot run 2026-11-05 on together/M3 (failed) has no telemetry: envelope telemetry is recorded for claude -p dreams only",
		},
		{
			name: "single telemetry run",
			rows: []CostEntry{ledgerRow("2026-11-05", "dream", "sonnet", s5, 30, 2)},
			want: map[string]checkStatus{"dream": statusOK},
		},
		{
			name: "steady model and denials",
			rows: []CostEntry{
				ledgerRow("2026-10-29", "dream", "sonnet", s5, 30, 2),
				ledgerRow("2026-11-05", "dream", "sonnet", s5, 30, 3),
			},
			want: map[string]checkStatus{"dream": statusOK},
		},
		{
			name: "alias moved to a new model",
			rows: []CostEntry{
				ledgerRow("2026-10-11", "dream", "sonnet", s5, 30, 1),
				ledgerRow("2026-10-18", "dream", "sonnet", s55, 30, 1),
			},
			want:       map[string]checkStatus{"dream model": statusWarn},
			wantDetail: "sonnet: claude-sonnet-5 → claude-sonnet-5-5 (since 2026-10-18)",
		},
		{
			name: "alias switch in config is not drift",
			rows: []CostEntry{
				ledgerRow("2026-10-11", "dream", "sonnet", s5, 30, 1),
				ledgerRow("2026-10-18", "dream", "haiku", h, 30, 1),
			},
			want: map[string]checkStatus{"dream": statusOK},
		},
		{
			name: "drift compares with the same alias across a gap",
			rows: []CostEntry{
				ledgerRow("2026-10-10", "dream", "sonnet", s5, 30, 1),
				ledgerRow("2026-10-14", "dream", "haiku", h, 30, 1),
				ledgerRow("2026-10-18", "dream", "sonnet", s55, 30, 1),
			},
			want: map[string]checkStatus{"dream model": statusWarn},
		},
		{
			name: "denial spike above 2x median",
			rows: []CostEntry{
				ledgerRow("2026-10-10", "dream", "sonnet", s5, 30, 2),
				ledgerRow("2026-10-15", "dream", "sonnet", s5, 30, 1),
				ledgerRow("2026-10-20", "dream", "sonnet", s5, 30, 3),
				ledgerRow("2026-10-25", "dream", "sonnet", s5, 30, 2),
				ledgerRow("2026-10-29", "dream", "sonnet", s5, 30, 2),
				ledgerRow("2026-11-05", "dream", "sonnet", s5, 30, 9),
			},
			want:       map[string]checkStatus{"dream denials": statusWarn},
			wantDetail: "9 refused tool call(s) (Bash: 9) vs median 2 over the previous 5 run(s)",
		},
		{
			name: "at exactly 2x median is fine",
			rows: []CostEntry{
				ledgerRow("2026-10-29", "dream", "sonnet", s5, 30, 4),
				ledgerRow("2026-11-05", "dream", "sonnet", s5, 30, 8),
			},
			want: map[string]checkStatus{"dream": statusOK},
		},
		{
			name: "floor of 3 when history is clean",
			rows: []CostEntry{
				ledgerRow("2026-10-29", "dream", "sonnet", s5, 30, 0),
				ledgerRow("2026-11-05", "dream", "sonnet", s5, 30, 3),
			},
			want: map[string]checkStatus{"dream": statusOK},
		},
		{
			name: "above the floor when history is clean",
			rows: []CostEntry{
				ledgerRow("2026-10-29", "dream", "sonnet", s5, 30, 0),
				ledgerRow("2026-11-05", "dream", "sonnet", s5, 30, 4),
			},
			want: map[string]checkStatus{"dream denials": statusWarn},
		},
		{
			name: "only the last 5 runs count toward the median",
			rows: []CostEntry{
				ledgerRow("2026-10-07", "dream", "sonnet", s5, 30, 50),
				ledgerRow("2026-10-08", "dream", "sonnet", s5, 30, 50),
				ledgerRow("2026-10-09", "dream", "sonnet", s5, 30, 50),
				ledgerRow("2026-10-10", "dream", "sonnet", s5, 30, 50),
				ledgerRow("2026-10-11", "dream", "sonnet", s5, 30, 50),
				ledgerRow("2026-10-12", "dream", "sonnet", s5, 30, 50),
				ledgerRow("2026-10-15", "dream", "sonnet", s5, 30, 1),
				ledgerRow("2026-10-20", "dream", "sonnet", s5, 30, 1),
				ledgerRow("2026-10-25", "dream", "sonnet", s5, 30, 1),
				ledgerRow("2026-10-29", "dream", "sonnet", s5, 30, 1),
				ledgerRow("2026-11-02", "dream", "sonnet", s5, 30, 1),
				ledgerRow("2026-11-05", "dream", "sonnet", s5, 30, 6),
			},
			want: map[string]checkStatus{"dream denials": statusWarn},
		},
		{
			name: "ops are compared only with themselves",
			rows: []CostEntry{
				ledgerRow("2026-11-04", "dream", "sonnet", s5, 30, 10),
				ledgerRow("2026-11-05", "dream-hot", "haiku", h, 5, 0),
				ledgerRow("2026-11-06", "dream", "sonnet", s5, 30, 11),
			},
			want: map[string]checkStatus{"dream": statusOK, "dream-hot": statusOK},
		},
		{
			name: "rows older than the lookback are ignored",
			rows: []CostEntry{
				ledgerRow("2026-08-01", "dream", "sonnet", s5, 30, 1),
				ledgerRow("2026-11-05", "dream", "sonnet", s55, 30, 1),
			},
			want: map[string]checkStatus{"dream": statusOK},
		},
		{
			name: "non-dream ops are ignored",
			rows: []CostEntry{
				ledgerRow("2026-11-05", "absorb-pass2", "sonnet", s5, 30, 40),
			},
			want: map[string]checkStatus{"dream telemetry": statusOK},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeLedger(t, root, tc.rows)
			got := checkTelemetry(root, telemetryNow)
			names := map[string]checkStatus{}
			detailFound := tc.wantDetail == ""
			for _, c := range got {
				if c.Section != "telemetry" {
					t.Errorf("check %q has section %q", c.Name, c.Section)
				}
				names[c.Name] = c.Status
				if tc.wantDetail != "" && strings.Contains(c.Detail, tc.wantDetail) {
					detailFound = true
				}
			}
			if !detailFound {
				t.Errorf("no check detail contains %q: %+v", tc.wantDetail, got)
			}
			if len(names) != len(tc.want) {
				t.Errorf("checks = %v, want %v", got, tc.want)
			}
			for n, st := range tc.want {
				if names[n] != st {
					t.Errorf("check %q = %q, want %q (all: %+v)", n, names[n], st, got)
				}
			}
		})
	}
}

// A large day file is read from its tail only; the cut first line must not
// be decoded, and rows in the tail are still found.
func TestReadDreamRows_TailReadsLargeFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "output", "costs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	early := mustJSON(t, ledgerRow("2026-11-05", "dream", "sonnet", []string{"early-model"}, 30, 1))
	b.Write(append(early, '\n'))
	filler := mustJSON(t, CostEntry{Timestamp: "2026-11-05T01:00:00Z", Model: "haiku", Op: "absorb-pass1-whole", OK: true})
	for b.Len() < telemetryFileTailBytes+1024 {
		b.Write(append(filler, '\n'))
	}
	late := mustJSON(t, ledgerRow("2026-11-05", "dream", "sonnet", []string{"late-model"}, 30, 1))
	b.Write(append(late, '\n'))
	path := filepath.Join(dir, "2026-11-05.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := readDreamRows(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows["dream"]) != 1 || rows["dream"][0].ResolvedModels[0] != "late-model" {
		t.Errorf("rows = %+v, want only the tail's late-model row", rows["dream"])
	}
}

// The section must stay well inside doctor's 1s budget on a busy ledger.
func TestCheckTelemetry_FastOnLargeLedger(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "output", "costs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	filler := mustJSON(t, ledgerRow("2026-11-01", "absorb-pass2", "sonnet", []string{"claude-sonnet-5"}, 30, 0))
	line := string(filler) + "\n"
	day := strings.Repeat(line, (3<<20)/len(line)) // ~3 MB per day
	for d := range 31 {
		name := telemetryNow.AddDate(0, 0, -d).Format("2006-01-02") + ".jsonl"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(day), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	checkTelemetry(root, telemetryNow)
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Errorf("checkTelemetry took %s on a ~90 MB ledger, budget is well under 1s", el)
	}
}
