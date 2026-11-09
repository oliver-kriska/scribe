package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// doctor --section telemetry reads the dream rows of the cost ledger
// (output/costs/<day>.jsonl) and flags two kinds of silent drift in the
// claude -p envelope telemetry that 0.6.0 started recording:
//
//   - Model drift: the latest dream run's resolved model ids differ from
//     those of the previous run of the same op under the same alias. An
//     alias like `sonnet` can move to a new model without any scribe change.
//   - Denial spike: the latest run's refused tool calls exceed
//     max(telemetryDenialFloor, 2 × median of the previous
//     telemetryDenialHistory runs of the same op). A CLI update that
//     narrows the dream's tool grant shows up here first.
//
// Rows written before 0.6.0 carry neither resolved_models nor num_turns,
// and neither do rows from non-claude providers (orchestrator dreams on
// Together or Ollama). Both are left out of the comparisons; when no row
// carries telemetry, one OK line names the newest dream row and why it has
// none, so "no telemetry" never reads as "no dream runs".
//
// Budget: doctor must stay under ~1s. Only day files from the last
// telemetryLookbackDays are opened, newest first; each is read from at
// most its last telemetryFileTailBytes; a byte prefilter skips non-dream
// rows before any JSON decode; and the scan stops once every dream op has
// enough history.
const (
	telemetryLookbackDays  = 30
	telemetryFileTailBytes = 2 << 20
	telemetryDenialHistory = 5
	telemetryDenialFloor   = 3
)

// dreamLedgerOps are the op labels dream runs write to the ledger
// (dream.go and dream_orchestrator.go use "dream", dream_hot.go
// "dream-hot"). Ops are compared only with themselves: the hot dream has
// no tool grant and a different model, so mixing them would flag noise.
var dreamLedgerOps = []string{"dream", "dream-hot"}

var dreamOpPrefilter = []byte(`"op":"dream`)

// hasEnvelopeTelemetry reports whether a ledger row was written with the
// 0.6.0 envelope fields. Denials is omitempty, so zero denials alone
// can't tell a new row from an old one.
func hasEnvelopeTelemetry(e CostEntry) bool {
	return len(e.ResolvedModels) > 0 || e.NumTurns != nil
}

func checkTelemetry(root string, now time.Time) []check {
	rows, untracked, err := loadDreamTelemetry(root, now)
	if err != nil {
		return []check{{
			Section: "telemetry", Name: "cost ledger", Status: statusWarn,
			Detail: err.Error(),
			Fix:    "check filesystem permissions on output/costs/",
		}}
	}
	var out []check
	for _, op := range dreamLedgerOps {
		out = append(out, telemetryChecksForOp(op, rows[op])...)
	}
	if len(out) == 0 {
		return []check{{
			Section: "telemetry", Name: "dream telemetry", Status: statusOK,
			Detail: noTelemetryDetail(untracked),
		}}
	}
	return out
}

// noTelemetryDetail explains an empty telemetry section: no dream rows at
// all, or the newest dream row and why it carries no envelope fields.
func noTelemetryDetail(latest *CostEntry) string {
	if latest == nil {
		return fmt.Sprintf("no dream runs in the cost ledger in the last %dd", telemetryLookbackDays)
	}
	model := latest.Model
	if latest.Provider != "" && !strings.HasPrefix(model, latest.Provider+"/") {
		model = latest.Provider + "/" + model
	}
	outcome := "ok"
	if !latest.OK {
		outcome = "failed"
	}
	why := "envelope telemetry is recorded for claude -p dreams only"
	if latest.Provider == "" || latest.Provider == "anthropic" {
		why = "written before envelope telemetry was recorded (0.6.0)"
	}
	return fmt.Sprintf("latest %s run %s on %s (%s) has no telemetry: %s", latest.Op, ledgerDay(latest.Timestamp), model, outcome, why)
}

// telemetryChecksForOp evaluates one op's rows (oldest first). No rows,
// no checks.
func telemetryChecksForOp(op string, rows []CostEntry) []check {
	if len(rows) == 0 {
		return nil
	}
	latest := rows[len(rows)-1]
	prev := rows[:len(rows)-1]
	day := ledgerDay(latest.Timestamp)
	var out []check

	if c, ok := modelDriftCheck(op, latest, prev); ok {
		out = append(out, c)
	}
	if c, ok := denialSpikeCheck(op, latest, prev); ok {
		out = append(out, c)
	}
	if len(out) > 0 {
		return out
	}
	detail := fmt.Sprintf("latest run %s: %s, %d refused tool call(s)", day, modelSetLabel(latest), latest.Denials)
	if len(prev) == 0 {
		detail += "; no earlier run to compare yet"
	}
	return []check{{Section: "telemetry", Name: op, Status: statusOK, Detail: detail}}
}

// modelDriftCheck compares the latest run's resolved models with the most
// recent earlier run of the same op under the same alias.
func modelDriftCheck(op string, latest CostEntry, prev []CostEntry) (check, bool) {
	if len(latest.ResolvedModels) == 0 {
		return check{}, false
	}
	for i := len(prev) - 1; i >= 0; i-- {
		p := prev[i]
		if p.Model != latest.Model || len(p.ResolvedModels) == 0 {
			continue
		}
		if slices.Equal(sortedCopy(p.ResolvedModels), sortedCopy(latest.ResolvedModels)) {
			return check{}, false
		}
		return check{
			Section: "telemetry", Name: op + " model", Status: statusWarn,
			Detail: fmt.Sprintf("%s: %s → %s (since %s)", latest.Model,
				strings.Join(sortedCopy(p.ResolvedModels), "+"),
				strings.Join(sortedCopy(latest.ResolvedModels), "+"),
				ledgerDay(latest.Timestamp)),
			Fix: "the alias now resolves to a different model; review the last " + op + " run's changes, and set a full model id instead of the alias if the new model misbehaves",
		}, true
	}
	return check{}, false
}

// denialSpikeCheck applies the documented rule: WARN when the latest
// run's denials exceed max(telemetryDenialFloor, 2 × median) of the
// previous telemetryDenialHistory runs. Needs at least one earlier run.
func denialSpikeCheck(op string, latest CostEntry, prev []CostEntry) (check, bool) {
	if len(prev) == 0 {
		return check{}, false
	}
	if len(prev) > telemetryDenialHistory {
		prev = prev[len(prev)-telemetryDenialHistory:]
	}
	counts := make([]int, len(prev))
	for i, p := range prev {
		counts[i] = p.Denials
	}
	med := medianOf(counts)
	limit := max(float64(telemetryDenialFloor), 2*med)
	if float64(latest.Denials) <= limit {
		return check{}, false
	}
	return check{
		Section: "telemetry", Name: op + " denials", Status: statusWarn,
		Detail: fmt.Sprintf("latest run %s had %d refused tool call(s)%s vs median %s over the previous %d run(s)",
			ledgerDay(latest.Timestamp), latest.Denials, deniedToolsLabel(latest.DeniedTools), formatMedian(med), len(prev)),
		Fix: "a Claude Code update may have narrowed the tool grant: read denied_samples for that run in output/costs/" + ledgerDay(latest.Timestamp) + ".jsonl, and run `make live-probe` from a scribe checkout",
	}, true
}

// loadDreamTelemetry returns, per dream op, the telemetry-bearing ledger
// rows of the lookback window, oldest first, and the newest dream row of
// the files it read that carries no telemetry (nil if none).
func loadDreamTelemetry(root string, now time.Time) (map[string][]CostEntry, *CostEntry, error) {
	dir := filepath.Join(root, "output", "costs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	// One extra day for rows whose UTC date trails local time.
	cutoff := now.UTC().AddDate(0, 0, -telemetryLookbackDays-1).Format("2006-01-02") + ".jsonl"
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".jsonl") || n < cutoff {
			continue
		}
		names = append(names, n)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	out := map[string][]CostEntry{}
	var untracked *CostEntry
	enough := func() bool {
		for _, op := range dreamLedgerOps {
			if len(out[op]) <= telemetryDenialHistory {
				return false
			}
		}
		return true
	}
	for _, n := range names {
		rows, err := readDreamRows(filepath.Join(dir, n))
		if err != nil {
			return nil, nil, err
		}
		for op, r := range rows {
			var tracked []CostEntry
			for i := range r {
				switch {
				case hasEnvelopeTelemetry(r[i]):
					tracked = append(tracked, r[i])
				case untracked == nil || r[i].Timestamp > untracked.Timestamp:
					untracked = &r[i]
				}
			}
			out[op] = append(tracked, out[op]...) // older file goes in front
		}
		if enough() {
			break
		}
	}
	for op := range out {
		sort.SliceStable(out[op], func(i, j int) bool { return out[op][i].Timestamp < out[op][j].Timestamp })
	}
	return out, untracked, nil
}

// readDreamRows reads at most the last telemetryFileTailBytes of one day
// file and returns its dream rows by op, in file order.
func readDreamRows(path string) (map[string][]CostEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	partialFirst := false
	if fi, err := f.Stat(); err == nil && fi.Size() > telemetryFileTailBytes {
		if _, err := f.Seek(fi.Size()-telemetryFileTailBytes, io.SeekStart); err != nil {
			return nil, err
		}
		partialFirst = true
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	out := map[string][]CostEntry{}
	for scanner.Scan() {
		line := scanner.Bytes()
		if partialFirst {
			partialFirst = false // the seek landed mid-row
			continue
		}
		if !bytes.Contains(line, dreamOpPrefilter) {
			continue
		}
		var e CostEntry
		if json.Unmarshal(line, &e) != nil || !slices.Contains(dreamLedgerOps, e.Op) {
			continue
		}
		out[e.Op] = append(out[e.Op], e)
	}
	// A corrupt or oversized line ends the scan; keep what was read, as
	// the cost report does. The ledger is not load-bearing.
	return out, nil
}

func ledgerDay(ts string) string {
	if len(ts) >= len("2006-01-02") {
		return ts[:len("2006-01-02")]
	}
	return ts
}

func modelSetLabel(e CostEntry) string {
	if len(e.ResolvedModels) == 0 {
		return e.Model
	}
	return e.Model + " → " + strings.Join(sortedCopy(e.ResolvedModels), "+")
}

func deniedToolsLabel(tally map[string]int) string {
	if len(tally) == 0 {
		return ""
	}
	names := make([]string, 0, len(tally))
	for n := range tally {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = fmt.Sprintf("%s: %d", n, tally[n])
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	sort.Strings(c)
	return c
}

func medianOf(v []int) float64 {
	s := slices.Clone(v)
	sort.Ints(s)
	n := len(s)
	if n%2 == 1 {
		return float64(s[n/2])
	}
	return float64(s[n/2-1]+s[n/2]) / 2
}

func formatMedian(m float64) string {
	if m == float64(int(m)) {
		return strconv.Itoa(int(m))
	}
	return fmt.Sprintf("%.1f", m)
}
