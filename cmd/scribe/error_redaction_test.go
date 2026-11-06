package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failedEnvelopeLine is a one-line claude -p result envelope of the shape a
// failed call prints on stdout, with secrets in a refused Bash command.
const failedEnvelopeLine = `{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":4,"total_cost_usd":0.0123,"result":"a < b","permission_denials":[{"tool_name":"Bash","tool_use_id":"t1","tool_input":{"command":"GITHUB_TOKEN=ghsecret123 git push && curl -H \"Authorization: Bearer bearersecret456\" https://api.example/x?token=urlsecret789"}}]}`

func readOnlyErrorRecord(t *testing.T, root string) ErrorRecord {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(root, "output", "errors", "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("expected one error log, got %v", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var rec ErrorRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("decode error record: %v", err)
	}
	return rec
}

func TestErrorRecord_RedactsDeniedCommandsInEnvelopeTail(t *testing.T) {
	root := t.TempDir()
	stdout := "hook noise: export API_KEY=plainsecret000\n" + failedEnvelopeLine
	appendErrorRecord(root, ErrorRecord{
		Op:         "dream",
		ErrKind:    "other",
		Err:        "exit status 1",
		StdoutTail: stdout,
		StderrTail: "curl -H 'Authorization: Bearer stderrsecret111'",
	})
	rec := readOnlyErrorRecord(t, root)

	all := rec.StdoutTail + rec.StderrTail
	for _, secret := range []string{"ghsecret123", "bearersecret456", "urlsecret789", "plainsecret000", "stderrsecret111"} {
		if strings.Contains(all, secret) {
			t.Errorf("secret %q leaked into the error record:\n%s", secret, all)
		}
	}

	lines := strings.Split(rec.StdoutTail, "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout tail has %d lines, want 2 (redaction must not change the line bound)", len(lines))
	}
	// The envelope line stays parseable, with the command redacted in place.
	var env struct {
		TotalCostUSD      json.Number `json:"total_cost_usd"`
		Result            string      `json:"result"`
		PermissionDenials []struct {
			ToolInput struct {
				Command string `json:"command"`
			} `json:"tool_input"`
		} `json:"permission_denials"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &env); err != nil {
		t.Fatalf("redacted envelope line is not valid JSON: %v\n%s", err, lines[1])
	}
	if len(env.PermissionDenials) != 1 {
		t.Fatalf("denials = %d, want 1", len(env.PermissionDenials))
	}
	cmd := env.PermissionDenials[0].ToolInput.Command
	for _, want := range []string{"GITHUB_TOKEN=REDACTED", "Authorization: Bearer REDACTED", "token=REDACTED", "git push"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("redacted command %q lacks %q", cmd, want)
		}
	}
	if env.TotalCostUSD.String() != "0.0123" {
		t.Errorf("total_cost_usd = %s, want 0.0123 preserved exactly", env.TotalCostUSD)
	}
	if !strings.Contains(lines[1], `"a < b"`) {
		t.Errorf("re-encoding HTML-escaped the result text: %s", lines[1])
	}
}

func TestRedactSubprocessOutput_KeepsLineCount(t *testing.T) {
	in := make([]string, 0, 50)
	for range 49 {
		in = append(in, "log line with GITHUB_TOKEN=x")
	}
	in = append(in, failedEnvelopeLine)
	tail := tailLines(strings.Join(in, "\n"), 50)
	out := redactSubprocessOutput(tail)
	if got, want := strings.Count(out, "\n"), strings.Count(tail, "\n"); got != want {
		t.Errorf("redaction changed the line count: %d newlines, want %d", got, want)
	}
	// A truncated envelope (not valid JSON) still gets the text rules.
	cut := failedEnvelopeLine[:len(failedEnvelopeLine)-40]
	if strings.Contains(redactSubprocessOutput(cut), "ghsecret123") {
		t.Error("a truncated envelope line kept its secret")
	}
}

// The terse tail also rides on the returned error into run records and
// degraded-phase messages.
func TestRecordDegradedMsg_RedactsEnvelope(t *testing.T) {
	resetRunOutcome()
	t.Cleanup(resetRunOutcome)
	recordDegradedMsg("dream", "claude -p: exit status 1\n"+failedEnvelopeLine)
	_, msgs := degradedPhases()
	if strings.Contains(msgs["dream"], "ghsecret123") {
		t.Errorf("degraded message leaked a secret: %s", msgs["dream"])
	}
}
