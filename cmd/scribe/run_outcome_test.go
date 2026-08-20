package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestRecordDegraded_CollectsAndSorts(t *testing.T) {
	resetRunOutcome()
	t.Cleanup(resetRunOutcome)

	recordDegraded("session mining", errors.New("ccrider db locked"))
	recordDegraded("absorb", errors.New("ollama refused"))

	names, msgs := degradedPhases()
	if !slices.Equal(names, []string{"absorb", "session mining"}) {
		t.Fatalf("phases = %v, want sorted [absorb, session mining]", names)
	}
	if msgs["absorb"] != "ollama refused" {
		t.Fatalf("absorb msg = %q", msgs["absorb"])
	}
}

func TestRecordDegraded_KeepsFirstErrorPerPhase(t *testing.T) {
	resetRunOutcome()
	t.Cleanup(resetRunOutcome)

	recordDegraded("absorb", errors.New("root cause"))
	recordDegraded("absorb", errors.New("cascade"))

	names, msgs := degradedPhases()
	if len(names) != 1 {
		t.Fatalf("one phase should collapse to one entry, got %v", names)
	}
	if msgs["absorb"] != "root cause" {
		t.Fatalf("expected the first error to win, got %q", msgs["absorb"])
	}
}

func TestRecordDegraded_IgnoresNilAndEmpty(t *testing.T) {
	resetRunOutcome()
	t.Cleanup(resetRunOutcome)

	recordDegraded("absorb", nil)
	recordDegraded("", errors.New("boom"))

	if names, _ := degradedPhases(); len(names) != 0 {
		t.Fatalf("nil error / empty phase should record nothing, got %v", names)
	}
}

func TestRecordDegraded_TrimsIndentation(t *testing.T) {
	resetRunOutcome()
	t.Cleanup(resetRunOutcome)

	// Per-project log lines are indented (" [proj] …"); doctor renders the
	// stored message inline after "partial failure in <phase>: ", where a
	// leading space would show up as a double space mid-sentence.
	logPhaseDegraded("sync", "project extraction", " [proj] extraction failed: %v", errors.New("boom"))
	recordDegradedMsg("whitespace only", "   \n\t  ")

	_, msgs := degradedPhases()
	if got := msgs["project extraction"]; got != "[proj] extraction failed: boom" {
		t.Fatalf("msg = %q, want it trimmed", got)
	}
	if _, ok := msgs["whitespace only"]; ok {
		t.Errorf("an all-whitespace message should record nothing, got %v", msgs)
	}
}

func TestRecordDegraded_RedactsTokens(t *testing.T) {
	resetRunOutcome()
	t.Cleanup(resetRunOutcome)

	// Run records are printed back by `scribe doctor --section errors`,
	// so a token-bearing URL in a phase error must not survive.
	recordDegraded("pull", errors.New("GET https://api.example.com/v1?auth_token=sk-secret-value failed"))

	_, msgs := degradedPhases()
	if got := msgs["pull"]; got == "" {
		t.Fatal("expected a recorded message")
	} else if strings.Contains(got, "sk-secret-value") {
		t.Fatalf("token survived redaction: %q", got)
	}
}

func TestLogPhaseFailure_NilIsNoOp(t *testing.T) {
	resetRunOutcome()
	t.Cleanup(resetRunOutcome)

	logPhaseFailure("sync", "absorb", nil)
	if names, _ := degradedPhases(); len(names) != 0 {
		t.Fatalf("nil error must not degrade the run, got %v", names)
	}
}

func TestDegradedPhases_EmptyByDefault(t *testing.T) {
	resetRunOutcome()
	t.Cleanup(resetRunOutcome)

	names, msgs := degradedPhases()
	if names != nil || msgs != nil {
		t.Fatalf("clean run should report nothing, got %v / %v", names, msgs)
	}
}
