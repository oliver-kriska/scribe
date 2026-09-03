package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
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
	// Trim first: several call sites indent their log line (" [proj] …")
	// and that leading space would otherwise land mid-sentence in
	// doctor's "partial failure in <phase>: <msg>" rendering.
	msg = strings.TrimSpace(redactURLToken(msg))
	if msg == "" {
		return
	}
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

// A skipped run fired but could not do its job: the advisory lock was
// busy, the team dream lease belonged to another machine, or the LLM
// was rate-limited before the first unit of work. It exits 0 — cron
// must not page on it — but the run record says "skipped" so doctor's
// freshness check and dream's hot/full history do not count it as a
// completed run. Before this, a lock-busy dream was recorded as "ok",
// suppressed the daily --hot pass for a day, and kept the weekly
// freshness check green while no dream had actually run.
type runSkip struct{ reason string }

func (e *runSkip) Error() string { return "skipped: " + e.reason }

// skipRun is what a command returns to report a skipped run. The seam
// that skips logs the reason itself; main exits 0 without printing it
// again.
func skipRun(reason string) error { return &runSkip{reason: reason} }

// runSkipReason reports whether err (or anything it wraps) is a skip.
func runSkipReason(err error) (string, bool) {
	var s *runSkip
	if errors.As(err, &s) {
		return s.reason, true
	}
	return "", false
}

// logPhaseOutcome records one phase's result inside a larger run. A
// skipped phase degrades the run under the phase's name — the sync
// itself did work, this phase did none, and doctor should say so —
// while any other error goes through logPhaseFailure. nil is a no-op.
func logPhaseOutcome(script, phase string, err error) {
	if reason, ok := runSkipReason(err); ok {
		logPhaseDegraded(script, phase, "%s skipped: %s", phase, reason)
		return
	}
	logPhaseFailure(script, phase, err)
}
