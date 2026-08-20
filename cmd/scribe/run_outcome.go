package main

import (
	"fmt"
	"sort"
	"sync"
)

// Sync deliberately logs and continues when a single phase fails: one
// broken source must not starve the others, and a cron job that exits
// non-zero on a flaky network call is worse than one that carries on.
// The cost is that the run record then says "ok" — a full success and a
// run where session mining died are indistinguishable afterwards, and
// `scribe doctor` reads those records.
//
// recordDegraded closes that gap without touching exit codes: the
// process still exits 0, the run record just says "degraded" and names
// the phases that failed.
//
// Package-level because writeRunRecord runs from main's deferred exit
// path, which holds no reference to the command struct. Tests must call
// resetRunOutcome to avoid leaking state between cases.
var (
	runOutcomeMu    sync.Mutex
	runDegradations map[string]string
)

// degradedMsgMax bounds a stored message. writeRunRecord caps its own
// `error` field at the same length, and for the same reason: run records
// are committed into the KB and read back line-by-line by `scribe
// doctor`, whose scanner gives up on the whole day-file past 1 MB in one
// line. An LLM error wrapping a response body reaches that easily.
const degradedMsgMax = 500

// recordDegradedMsg marks the current run as degraded, attributing it to
// a phase. Repeated failures in one phase keep the first message: it is
// usually the root cause, later ones the cascade.
func recordDegradedMsg(phase, msg string) {
	if phase == "" || msg == "" {
		return
	}
	msg = redactURLToken(msg)
	if len(msg) > degradedMsgMax {
		msg = msg[:degradedMsgMax]
	}
	runOutcomeMu.Lock()
	defer runOutcomeMu.Unlock()
	if runDegradations == nil {
		runDegradations = make(map[string]string)
	}
	if _, seen := runDegradations[phase]; !seen {
		runDegradations[phase] = msg
	}
}

// recordDegraded is the error-shaped form of recordDegradedMsg.
func recordDegraded(phase string, err error) {
	if err == nil {
		return
	}
	recordDegradedMsg(phase, err.Error())
}

// logPhaseFailure logs a non-fatal phase failure and marks the run
// degraded in one call, so the log line and the run record cannot drift
// apart. Use this at every seam where a phase deliberately continues
// past an error — a bare logMsg there is invisible to `scribe doctor`.
func logPhaseFailure(script, phase string, err error) {
	if err == nil {
		return
	}
	logMsg(script, "%s error: %v", phase, err)
	recordDegraded(phase, err)
}

// logPhaseDegraded is logPhaseFailure for the seams that continue past a
// failure they describe in prose rather than an error value — a push
// that reported "offline", a commit the secret gate held back.
func logPhaseDegraded(script, phase, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	logMsg(script, "%s", msg)
	recordDegradedMsg(phase, msg)
}

// degradedPhases returns the failed phase names, sorted for a stable
// run record, plus their first error message keyed by phase.
func degradedPhases() ([]string, map[string]string) {
	runOutcomeMu.Lock()
	defer runOutcomeMu.Unlock()
	if len(runDegradations) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(runDegradations))
	msgs := make(map[string]string, len(runDegradations))
	for phase, msg := range runDegradations {
		names = append(names, phase)
		msgs[phase] = msg
	}
	sort.Strings(names)
	return names, msgs
}

// resetRunOutcome clears accumulated state. Test-only.
func resetRunOutcome() {
	runOutcomeMu.Lock()
	defer runOutcomeMu.Unlock()
	runDegradations = nil
}
