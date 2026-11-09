package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	envEmptyActions = `{"version":2,"entity":"s","actions":[],"meta":[{"op":"sessions_log_append","session_id":"sid"}]}`
	envOneAction    = `{"version":2,"entity":"s","actions":[{"op":"create","path":"patterns/x.md","content":"---\ntitle: X\n---\nbody"}],"meta":[{"op":"sessions_log_append","session_id":"sid"}]}`
)

func TestParseEnvelopeV2RejectsBareEmptyObject(t *testing.T) {
	for _, js := range []string{`{}`, `{"actions":[]}`, `{"actions":null,"meta":[]}`} {
		if _, err := parseEnvelopeV2(js, "test"); !errors.Is(err, errBareEmptyEnvelope) {
			t.Errorf("%s: want errBareEmptyEnvelope, got %v", js, err)
		}
	}
	// A real "nothing to extract" answer still parses.
	env, err := parseEnvelopeV2(envEmptyActions, "test")
	if err != nil || len(env.Actions) != 0 || len(env.Meta) != 1 {
		t.Fatalf("empty-actions envelope with meta must parse: env=%+v err=%v", env, err)
	}
}

func runRetryForTest(t *testing.T, stub *stubLLM) (WikiActionEnvelope, error) {
	t.Helper()
	t.Setenv("SCRIBE_KB", t.TempDir())
	return runSessionEnvelopeWithRetry(t.Context(), stub, "session-mine", "session-mine", "/p", "sid", "", "user: hi", nil, time.Minute)
}

// The bare "{}" goes through the corrective retry, not into the KB as
// "nothing to extract".
func TestSessionEnvelopeRetry_BareEmptyObjectIsRetried(t *testing.T) {
	stub := &stubLLM{Rules: []*stubRule{{Reply: `{}`, Once: true}}, DefaultReply: envOneAction}
	env, err := runRetryForTest(t, stub)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Actions) != 1 {
		t.Fatalf("want the retried envelope, got %+v", env)
	}
	calls := stub.Calls()
	if len(calls) != 2 || !strings.Contains(calls[1].Prompt, "## CORRECTION") {
		t.Fatalf("want 2 calls, the second corrective; got %d", len(calls))
	}
}

// An envelope with no actions gets one check retry; if that one finds
// knowledge it wins.
func TestSessionEnvelopeRetry_EmptyActionsGetCheckRetry(t *testing.T) {
	stub := &stubLLM{Rules: []*stubRule{{Reply: envEmptyActions, Once: true}}, DefaultReply: envOneAction}
	env, err := runRetryForTest(t, stub)
	if err != nil {
		t.Fatal(err)
	}
	calls := stub.Calls()
	if len(env.Actions) != 1 || len(calls) != 2 || !strings.Contains(calls[1].Prompt, "## CHECK") {
		t.Fatalf("want check retry to recover 1 action; actions=%d calls=%d", len(env.Actions), len(calls))
	}
}

// A session that truly holds nothing costs exactly one extra call and
// comes back empty, so the caller can record it as processed.
func TestSessionEnvelopeRetry_TrulyEmptySessionStopsAfterOneCheck(t *testing.T) {
	stub := &stubLLM{DefaultReply: envEmptyActions}
	env, err := runRetryForTest(t, stub)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Actions) != 0 || len(stub.Calls()) != 2 {
		t.Fatalf("want empty envelope after exactly 2 calls; actions=%d calls=%d", len(env.Actions), len(stub.Calls()))
	}
}

// A failed check retry keeps the first (valid, empty) answer.
func TestSessionEnvelopeRetry_FailedCheckKeepsEmptyAnswer(t *testing.T) {
	stub := &stubLLM{Rules: []*stubRule{{Reply: envEmptyActions, Once: true}}, DefaultReply: "not json"}
	env, err := runRetryForTest(t, stub)
	if err != nil {
		t.Fatalf("a failed check retry must not fail the session: %v", err)
	}
	if len(env.Meta) != 1 {
		t.Fatalf("want the first envelope back, got %+v", env)
	}
}

// Rate limits stop at once: no retry, error surfaced so the session
// stays queued.
func TestSessionEnvelopeRetry_RateLimitStopsImmediately(t *testing.T) {
	stub := &stubLLM{DefaultErr: ErrRateLimit}
	if _, err := runRetryForTest(t, stub); !errors.Is(err, ErrRateLimit) {
		t.Fatalf("want ErrRateLimit, got %v", err)
	}
	if n := len(stub.Calls()); n != 1 {
		t.Fatalf("rate limit must not be retried; got %d calls", n)
	}
}

// Two bare "{}" replies leave the session failed (not processed), so the
// next run retries it instead of losing it.
func TestSessionEnvelopeRetry_TwoBareEmptyRepliesFail(t *testing.T) {
	stub := &stubLLM{DefaultReply: `{}`}
	if _, err := runRetryForTest(t, stub); !errors.Is(err, errBareEmptyEnvelope) {
		t.Fatalf("want errBareEmptyEnvelope, got %v", err)
	}
}

// The model is told which domains rolling_memory_append may use, and on
// schema-capable providers the domain is an enum. Before this, the prompt
// never named them and every live session lost its rolling-memory op to
// "domain required" or an invented domain.
func TestSessionEnvelopeNamesAllowedDomains(t *testing.T) {
	stub := &stubLLM{DefaultReply: envOneAction}
	if _, err := runRetryForTest(t, stub); err != nil {
		t.Fatal(err)
	}
	if p := stub.Calls()[0].Prompt; !strings.Contains(p, "personal, general") || strings.Contains(p, "{{DOMAINS}}") {
		t.Fatalf("prompt must list the allowed domains")
	}
	meta := sessionEnvelopeSchema([]string{"general"}).Schema["properties"].(map[string]any)["meta"].(map[string]any)
	dom := meta["items"].(map[string]any)["properties"].(map[string]any)["domain"].(map[string]any)
	if enum, ok := dom["enum"].([]any); !ok || len(enum) != 1 || enum[0] != "general" {
		t.Fatalf("want domain enum [general], got %v", dom)
	}
	meta = sessionEnvelopeSchema(nil).Schema["properties"].(map[string]any)["meta"].(map[string]any)
	if _, ok := meta["items"].(map[string]any)["properties"].(map[string]any)["domain"].(map[string]any)["enum"]; ok {
		t.Fatal("no domains must mean no enum")
	}
}

// A re-mine delta that holds nothing new is a normal answer: no check
// retry, whose "extract it now" nudge would re-extract what earlier
// mines already wrote. The continuation block reaches the prompt, before
// the transcript markers.
func TestSessionEnvelopeRetry_RemineSkipsCheckRetry(t *testing.T) {
	t.Setenv("SCRIBE_KB", t.TempDir())
	stub := &stubLLM{DefaultReply: envEmptyActions}
	cont := "## Continuation of an earlier mine\n\nMARKER-CONT\n"
	env, err := runSessionEnvelopeWithRetry(t.Context(), stub, remineLabel, "session-mine", "/p", "sid", "", "user: hi",
		map[string]string{"CONTINUATION": cont}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	calls := stub.Calls()
	if len(env.Actions) != 0 || len(calls) != 1 {
		t.Fatalf("want one call and the empty answer; actions=%d calls=%d", len(env.Actions), len(calls))
	}
	p := calls[0].Prompt
	if i, j := strings.Index(p, "MARKER-CONT"), strings.Index(p, "<<<TRANSCRIPT_BEGIN>>>"); i < 0 || j < 0 || i > j {
		t.Fatalf("continuation block missing or inside the transcript (at %d, markers at %d)", i, j)
	}
	if strings.Contains(p, "{{CONTINUATION}}") {
		t.Fatal("placeholder left unfilled")
	}
}
