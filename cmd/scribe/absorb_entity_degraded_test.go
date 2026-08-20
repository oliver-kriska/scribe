package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A pass-2 entity that exhausts its corrective retries is LOST, not
// deferred: runPass2 only returns an error when every entity produced
// nothing, so a partial failure still stamps _absorb_log.json and the
// article is never re-absorbed. The run must therefore be degraded —
// otherwise the only trace of the dropped knowledge is a log line no
// `scribe doctor` surface reads.
func TestRunPass2JSONEntity_DroppedEntityDegradesTheRun(t *testing.T) {
	resetRunOutcome()
	t.Cleanup(resetRunOutcome)

	stub := &stubLLM{DefaultErr: errors.New("model refused")}
	s := &SyncCmd{}
	run := pass2Run{
		root:     t.TempDir(),
		provider: stub,
		timeout:  2 * time.Second,
		jsonMode: true,
	}

	// The contract the caller depends on is unchanged: a partial absorb
	// beats losing the whole source, so this still reports success.
	applied, err := s.runPass2JSONEntity(context.Background(), run,
		absorbEntity{Label: "Widget Registry"}, "prompt")
	if err != nil {
		t.Fatalf("entity failure must not propagate (partial absorb): %v", err)
	}
	if applied != 0 {
		t.Fatalf("applied = %d, want 0", applied)
	}

	names, msgs := degradedPhases()
	if len(names) != 1 || names[0] != "absorb entity" {
		t.Fatalf("degraded phases = %v, want [absorb entity]", names)
	}
	msg := msgs["absorb entity"]
	if !strings.Contains(msg, "Widget Registry") {
		t.Errorf("message must name the entity that was dropped, got %q", msg)
	}
	if !strings.Contains(msg, "dropped") {
		t.Errorf("message must say the entity was dropped, got %q", msg)
	}
}

// The rate-limit sentinel is stop-the-world, not a dropped entity: it
// propagates so the caller stops cleanly and the article is retried next
// run. It must NOT mark the run degraded — nothing was lost.
func TestRunPass2JSONEntity_RateLimitDoesNotDegrade(t *testing.T) {
	resetRunOutcome()
	t.Cleanup(resetRunOutcome)

	stub := &stubLLM{DefaultErr: ErrRateLimit}
	s := &SyncCmd{}
	run := pass2Run{
		root:     t.TempDir(),
		provider: stub,
		timeout:  2 * time.Second,
		jsonMode: true,
	}

	if _, err := s.runPass2JSONEntity(context.Background(), run,
		absorbEntity{Label: "Widget Registry"}, "prompt"); !errors.Is(err, ErrRateLimit) {
		t.Fatalf("rate limit must propagate, got %v", err)
	}
	if names, _ := degradedPhases(); len(names) != 0 {
		t.Fatalf("a rate limit loses nothing and must not degrade, got %v", names)
	}
}
