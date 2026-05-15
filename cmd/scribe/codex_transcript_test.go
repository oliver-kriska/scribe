package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFetchCodexTranscript_Fixture pins the parser against the real
// (scrubbed) 13-event scribe rollout. The fixture deliberately
// exercises every trap the original C3 plan's guessed table fell into:
// the synthetic <environment_context> user turn, the event_msg
// duplicate stream, encrypted reasoning, and turn_context/session_meta
// scaffolding — all must be dropped, leaving only the 4 canonical
// response_item turns.
func TestFetchCodexTranscript_Fixture(t *testing.T) {
	turns, err := fetchCodexTranscript("testdata/codex/rollout-transcript.jsonl")
	if err != nil {
		t.Fatalf("fetchCodexTranscript: %v", err)
	}
	if len(turns) != 4 {
		t.Fatalf("want 4 canonical turns, got %d: %+v", len(turns), turns)
	}

	wantRoles := []string{"user", "assistant", "tool", "tool"}
	for i, w := range wantRoles {
		if turns[i].Role != w {
			t.Errorf("turn[%d].Role = %q, want %q", i, turns[i].Role, w)
		}
	}

	// 1. Real user prompt captured from the response_item stream.
	if !strings.Contains(turns[0].Text, "Analyze this project") {
		t.Errorf("turn[0] should be the real user prompt, got %q", turns[0].Text)
	}
	// 2. Synthetic <environment_context> user turn dropped entirely.
	for _, tr := range turns {
		if strings.Contains(tr.Text, "<environment_context>") {
			t.Errorf("environment_context harness turn leaked: %q", tr.Text)
		}
	}
	// 3. event_msg duplicate stream dropped — the user prompt text
	//    also appears in an event_msg/user_message line; it must NOT
	//    produce a second turn.
	userHits := 0
	for _, tr := range turns {
		if strings.Contains(tr.Text, "Analyze this project") {
			userHits++
		}
	}
	if userHits != 1 {
		t.Errorf("user prompt should appear exactly once (no event_msg double-count), got %d", userHits)
	}
	// 4. Assistant content from response_item, not the event_msg
	//    agent_message duplicate.
	if !strings.Contains(turns[1].Text, "code review of the repository") {
		t.Errorf("turn[1] assistant text wrong: %q", turns[1].Text)
	}
	// 5. Encrypted reasoning dropped (never recoverable).
	for _, tr := range turns {
		if strings.Contains(tr.Text, "UNRECOVERABLE") || strings.Contains(tr.ToolText, "UNRECOVERABLE") {
			t.Errorf("encrypted reasoning leaked into a turn: %+v", tr)
		}
	}
	// 6. function_call / function_call_output → tool turns, paired by
	//    call_id so the rendered transcript can correlate them.
	if !strings.Contains(turns[2].ToolText, "exec_command") {
		t.Errorf("turn[2] should be the function_call, got %q", turns[2].ToolText)
	}
	if !strings.Contains(turns[3].ToolText, "Chunk ID") {
		t.Errorf("turn[3] should be the function_call_output, got %q", turns[3].ToolText)
	}
	callA := codexCallID(turns[2].ToolText)
	callB := codexCallID(turns[3].ToolText)
	if callA == "" || callA != callB {
		t.Errorf("function_call/output not paired by call_id: %q vs %q", turns[2].ToolText, turns[3].ToolText)
	}
	// Sequence is monotonic in append order.
	for i := 1; i < len(turns); i++ {
		if turns[i].Sequence <= turns[i-1].Sequence {
			t.Errorf("Sequence not strictly increasing: %d then %d", turns[i-1].Sequence, turns[i].Sequence)
		}
	}
}

func codexCallID(toolText string) string {
	// ToolText is "call <id>: ..." or "result <id>: ...".
	f := strings.Fields(toolText)
	if len(f) >= 2 {
		return strings.TrimSuffix(f[1], ":")
	}
	return ""
}

func TestFetchCodexTranscript_MissingFile(t *testing.T) {
	if _, err := fetchCodexTranscript(filepath.Join(t.TempDir(), "nope.jsonl")); err == nil {
		t.Error("missing rollout should error (caller distinguishes from empty)")
	}
}

func TestFetchCodexTranscript_EmptyYieldsNoTurns(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	turns, err := fetchCodexTranscript(p)
	if err != nil {
		t.Fatalf("empty rollout must not error: %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("empty rollout should yield no turns, got %+v", turns)
	}
}

func TestFetchCodexTranscript_MalformedLinesSkippedNonFatal(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mixed.jsonl")
	body := strings.Join([]string{
		`not json at all <<<>>>`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`,
		`{"type":"event_msg","payload":{"type":"token_count"}}`,
		`{broken json`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi back"}]}}`,
		``,
	}, "\n")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	turns, err := fetchCodexTranscript(p)
	if err != nil {
		t.Fatalf("malformed lines must be skipped, not fatal: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("want 2 valid turns around the garbage, got %d: %+v", len(turns), turns)
	}
	if turns[0].Role != "user" || turns[0].Text != "hello" {
		t.Errorf("turn[0] = %+v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Text != "hi back" {
		t.Errorf("turn[1] = %+v", turns[1])
	}
}
