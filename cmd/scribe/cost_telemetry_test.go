package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A synthetic composite envelope. The field names and shapes match real
// `claude -p --output-format json` results (one JSON line per run;
// permission_denials[].tool_input is an object; modelUsage is keyed by model
// id, and a run that used subagents lists their model too), but the values
// are made up and no real KB paths appear.
const telemetryEnvelope = `{"type":"result","subtype":"success","is_error":false,` +
	`"num_turns":45,"terminal_reason":"completed","result":"done",` +
	`"usage":{"input_tokens":12,"output_tokens":3400,"cache_read_input_tokens":900},` +
	`"total_cost_usd":0.46,` +
	`"modelUsage":{"claude-sonnet-5-5":{"inputTokens":12,"outputTokens":3400,"canonicalModel":"claude-sonnet-5-5"},` +
	`"claude-haiku-4-5":{"inputTokens":3,"outputTokens":40}},` +
	`"permission_denials":[` +
	`{"tool_name":"Bash","tool_use_id":"t1","tool_input":{"command":"cd /kb && ls; for f in wiki/*.md; do wc -l \"$f\"; done"}},` +
	`{"tool_name":"Bash","tool_use_id":"t2","tool_input":{"command":"python3 -c 'print(1)'"}},` +
	`{"tool_name":"Task","tool_use_id":"t3","tool_input":{"prompt":"sweep the KB"}},` +
	`{"tool_name":"Bash","tool_use_id":"t4","tool_input":{"command":"scribe lint"}}],` +
	`"subagent_stats":{"spawned":4,"completed":1,"failed":3}}`

func TestWithEnvelope_FillsTokensAndTelemetry(t *testing.T) {
	env, ok := parseClaudeResult(telemetryEnvelope)
	if !ok {
		t.Fatal("fixture envelope should parse")
	}
	base := CostEntry{Model: "sonnet", Op: "dream"}

	got := base.withEnvelope(env)

	if got.Model != "sonnet" || got.Op != "dream" {
		t.Errorf("identity fields changed: %+v", got)
	}
	if got.InputTokens == nil || *got.InputTokens != 12 || got.OutputTokens == nil || *got.OutputTokens != 3400 {
		t.Errorf("tokens not filled: in=%v out=%v", got.InputTokens, got.OutputTokens)
	}
	if got.CacheReadTokens == nil || *got.CacheReadTokens != 900 || got.CostUSD == nil || *got.CostUSD != 0.46 {
		t.Errorf("cache/cost not filled: cache=%v cost=%v", got.CacheReadTokens, got.CostUSD)
	}
	if want := []string{"claude-haiku-4-5", "claude-sonnet-5-5"}; !slices.Equal(got.ResolvedModels, want) {
		t.Errorf("ResolvedModels = %v, want %v (sorted)", got.ResolvedModels, want)
	}
	if got.NumTurns == nil || *got.NumTurns != 45 {
		t.Errorf("NumTurns = %v, want 45", got.NumTurns)
	}
	if got.TerminalReason != "completed" {
		t.Errorf("TerminalReason = %q", got.TerminalReason)
	}
	if got.Denials != 4 {
		t.Errorf("Denials = %d, want 4", got.Denials)
	}
	if got.DeniedTools["Bash"] != 3 || got.DeniedTools["Task"] != 1 {
		t.Errorf("DeniedTools = %v, want Bash:3 Task:1", got.DeniedTools)
	}
	if len(got.DeniedSamples) != maxDeniedSamples {
		t.Fatalf("DeniedSamples has %d entries, want cap %d: %v", len(got.DeniedSamples), maxDeniedSamples, got.DeniedSamples)
	}
	// The leading `cd <path> && ` is stripped: it carries no signal about
	// what was refused and is where most of the absolute path lives.
	if !strings.HasPrefix(got.DeniedSamples[0], "Bash: ls; for f in") {
		t.Errorf("first sample = %q, want the Bash command without its cd prefix", got.DeniedSamples[0])
	}
	if got.DeniedSamples[2] != "Task" {
		t.Errorf("non-Bash sample = %q, want the bare tool name", got.DeniedSamples[2])
	}
	if got.Subagents == nil || *got.Subagents != (SubagentCounts{Spawned: 4, Completed: 1, Failed: 3}) {
		t.Errorf("Subagents = %+v", got.Subagents)
	}
}

func TestWithEnvelope_DoesNotMutateReceiver(t *testing.T) {
	env, _ := parseClaudeResult(telemetryEnvelope)
	base := CostEntry{Model: "sonnet"}
	_ = base.withEnvelope(env)
	if base.InputTokens != nil || base.ResolvedModels != nil || base.Denials != 0 {
		t.Errorf("receiver was mutated: %+v", base)
	}
}

// A legacy envelope (no telemetry fields) must leave every new field at its
// zero value, so the ledger row serializes exactly as before.
func TestWithEnvelope_LegacyEnvelopeAddsNoKeys(t *testing.T) {
	env, ok := parseClaudeResult(`{"type":"result","subtype":"success","is_error":false,"result":"ok","usage":{"input_tokens":10,"output_tokens":20}}`)
	if !ok {
		t.Fatal("legacy envelope should parse")
	}
	got := CostEntry{Model: "haiku"}.withEnvelope(env)
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"resolved_models", "num_turns", "terminal_reason", "denials", "denied_tools", "denied_samples", "subagents"} {
		if strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("legacy row gained key %q: %s", key, raw)
		}
	}
}

func TestDenialSample_TruncatesLongCommands(t *testing.T) {
	long := strings.Repeat("x", 500)
	d := claudeDenial{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"` + long + `"}`)}
	got := denialSample(d)
	if !strings.HasPrefix(got, "Bash: ") {
		t.Fatalf("sample = %q", got)
	}
	if body := strings.TrimPrefix(got, "Bash: "); len(body) > deniedSampleMaxBytes {
		t.Errorf("command part is %d bytes, want <= %d", len(body), deniedSampleMaxBytes)
	}
}

func TestDenialSample_UnparseableInputFallsBackToToolName(t *testing.T) {
	d := claudeDenial{ToolName: "Bash", ToolInput: json.RawMessage(`"not an object"`)}
	if got := denialSample(d); got != "Bash" {
		t.Errorf("sample = %q, want bare tool name", got)
	}
}

// Only Bash commands are previewed. A non-Bash tool (including an MCP tool)
// whose input happens to have a "command" key stores its name only.
func TestDenialSample_NonBashNeverStoresInput(t *testing.T) {
	d := claudeDenial{ToolName: "mcp__shell__run", ToolInput: json.RawMessage(`{"command":"cat ~/.secrets"}`)}
	if got := denialSample(d); got != "mcp__shell__run" {
		t.Errorf("sample = %q, want bare tool name", got)
	}
}

func TestDenialSample_RedactsURLTokens(t *testing.T) {
	d := claudeDenial{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"curl https://api.example.com/x?token=s3cr3t"}`)}
	if got := denialSample(d); strings.Contains(got, "s3cr3t") {
		t.Errorf("sample leaked a URL token: %q", got)
	}
}

// installEnvelopeClaude puts a `claude` shim first on PATH that drains stdin,
// prints envelope on stdout, and exits with exitCode — the shape of a real
// claude -p run, including the failure case where the CLI exits 1 but still
// prints a full result envelope (e.g. an api_error mid-run).
func installEnvelopeClaude(t *testing.T, envelope string, exitCode int) {
	t.Helper()
	installEnvelopeClaudeWithStderr(t, envelope, exitCode, "")
}

// installEnvelopeClaudeWithStderr is installEnvelopeClaude plus a line the
// shim writes to stderr (skipped when empty).
func installEnvelopeClaudeWithStderr(t *testing.T, envelope string, exitCode int, stderr string) {
	t.Helper()
	dir := t.TempDir()
	envFile := filepath.Join(dir, "envelope.json")
	if err := os.WriteFile(envFile, []byte(envelope), 0o644); err != nil {
		t.Fatal(err)
	}
	stderrLine := ""
	if stderr != "" {
		stderrLine = "echo '" + stderr + "' >&2\n"
	}
	script := "#!/bin/sh\ncat >/dev/null\n" + stderrLine + "cat \"" + envFile + "\"\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// checkBudget also reads the real ~/.config/scribe/config.yaml and every
	// registered KB's ledger; bypass it so a real machine ceiling can't fail
	// these tests.
	t.Setenv(budgetBypassEnv, "1")
}

// lastCostEntry reads the newest row of today's cost ledger under root.
func lastCostEntry(t *testing.T, root string) CostEntry {
	t.Helper()
	day := time.Now().UTC().Format("2006-01-02")
	data, err := os.ReadFile(filepath.Join(root, "output", "costs", day+".jsonl"))
	if err != nil {
		t.Fatalf("no cost ledger written: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var got CostEntry
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
		t.Fatalf("last ledger line is not JSON: %v", err)
	}
	return got
}

// failingEnvelope is a non-zero-exit run that still printed a full
// envelope — the shape of the dream failures that hid a model change.
const failingEnvelope = `{"type":"result","subtype":"success","is_error":true,` +
	`"num_turns":4,"terminal_reason":"api_error","result":"API Error: Connection lost",` +
	`"usage":{"input_tokens":0,"output_tokens":0},` +
	`"modelUsage":{"claude-sonnet-5-5":{}},` +
	`"permission_denials":[{"tool_name":"Bash","tool_input":{"command":"python3 x.py"}}]}`

func TestRealRunClaude_RecordsTelemetryOnSuccess(t *testing.T) {
	root := testKB(t, "")
	installEnvelopeClaude(t, telemetryEnvelope, 0)
	if _, err := realRunClaude(context.Background(), root, "prompt", "sonnet", []string{"Read"}, 30*time.Second); err != nil {
		t.Fatalf("realRunClaude: %v", err)
	}
	got := lastCostEntry(t, root)
	if !got.OK || got.Denials != 4 || got.NumTurns == nil || *got.NumTurns != 45 {
		t.Errorf("success row missing telemetry: %+v", got)
	}
	if !slices.Contains(got.ResolvedModels, "claude-sonnet-5-5") {
		t.Errorf("ResolvedModels = %v", got.ResolvedModels)
	}
}

func TestRealRunClaude_RecordsTelemetryOnNonZeroExit(t *testing.T) {
	root := testKB(t, "")
	installEnvelopeClaude(t, failingEnvelope, 1)
	if _, err := realRunClaude(context.Background(), root, "prompt", "sonnet", []string{"Read"}, 30*time.Second); err == nil {
		t.Fatal("expected an error from a non-zero exit")
	}
	got := lastCostEntry(t, root)
	if got.OK || got.ErrKind != "other" {
		t.Errorf("error classification changed: ok=%v kind=%q", got.OK, got.ErrKind)
	}
	if !slices.Equal(got.ResolvedModels, []string{"claude-sonnet-5-5"}) {
		t.Errorf("ResolvedModels on failure = %v, want [claude-sonnet-5-5]", got.ResolvedModels)
	}
	if got.TerminalReason != "api_error" || got.Denials != 1 {
		t.Errorf("failure telemetry missing: reason=%q denials=%d", got.TerminalReason, got.Denials)
	}
}

func TestRealRunClaude_RecordsTelemetryOnStderrRateLimit(t *testing.T) {
	root := testKB(t, "")
	installEnvelopeClaudeWithStderr(t, telemetryEnvelope, 0, "Error: 429 rate limit reached")
	_, err := realRunClaude(context.Background(), root, "prompt", "sonnet", []string{"Read"}, 30*time.Second)
	if !errors.Is(err, ErrRateLimit) {
		t.Fatalf("err = %v, want ErrRateLimit", err)
	}
	got := lastCostEntry(t, root)
	if got.OK || got.ErrKind != "rate_limit" {
		t.Errorf("classification: ok=%v kind=%q, want rate_limit", got.OK, got.ErrKind)
	}
	if got.Denials != 4 || len(got.ResolvedModels) == 0 {
		t.Errorf("rate-limited row lost telemetry: %+v", got)
	}
}

func TestAnthropicGenerate_RecordsTelemetryOnSuccessAndFailure(t *testing.T) {
	root := testKB(t, "")
	p := &anthropicProvider{model: "sonnet", root: root}

	installEnvelopeClaude(t, telemetryEnvelope, 0)
	if _, err := p.Generate(context.Background(), "prompt"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got := lastCostEntry(t, root); got.Denials != 4 || len(got.ResolvedModels) != 2 {
		t.Errorf("Generate success row missing telemetry: %+v", got)
	}

	installEnvelopeClaude(t, failingEnvelope, 1)
	if _, err := p.Generate(context.Background(), "prompt"); err == nil {
		t.Fatal("expected an error from a non-zero exit")
	}
	if got := lastCostEntry(t, root); got.TerminalReason != "api_error" || got.OK {
		t.Errorf("Generate failure row missing telemetry: %+v", got)
	} else if got.ErrKind != "other" {
		t.Errorf("Generate failure ErrKind = %q, want other", got.ErrKind)
	}
}

// Deliberate behavior change: a failed run's usage used to be thrown away,
// so its output tokens never counted toward the daily output-token ceiling
// (one failed dream billed ~49k output tokens and ~$5 that the ledger never
// saw). Recording the envelope on the failure path makes them count.
// readDailyMeteredOutputTokens sums every row regardless of OK.
func TestBudget_CountsFailedRunOutputTokens(t *testing.T) {
	root := testKB(t, "")
	failingWithUsage := strings.Replace(failingEnvelope,
		`"usage":{"input_tokens":0,"output_tokens":0}`,
		`"usage":{"input_tokens":5,"output_tokens":4800}`, 1)
	installEnvelopeClaude(t, failingWithUsage, 1)
	if _, err := realRunClaude(context.Background(), root, "prompt", "sonnet", []string{"Read"}, 30*time.Second); err == nil {
		t.Fatal("expected an error from a non-zero exit")
	}
	day := time.Now().UTC().Format("2006-01-02")
	if got := readDailyMeteredOutputTokens(root, day); got != 4800 {
		t.Errorf("daily metered output tokens = %d, want 4800 from the failed run", got)
	}
}

func writeCostLines(t *testing.T, root string, lines ...string) {
	t.Helper()
	dir := filepath.Join(root, "output", "costs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Format("2006-01-02")
	if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSummarizeCosts_AggregatesResolvedModelsAndDenials(t *testing.T) {
	root := t.TempDir()
	writeCostLines(t, root,
		`{"timestamp":"2026-10-23T10:00:00Z","model":"sonnet","ok":true,"resolved_models":["claude-sonnet-5"]}`,
		`{"timestamp":"2026-10-23T11:00:00Z","model":"sonnet","ok":true,"resolved_models":["claude-sonnet-5-5"],"denials":3}`,
		`{"timestamp":"2026-10-23T12:00:00Z","model":"sonnet","ok":false,"err_kind":"other","resolved_models":["claude-sonnet-5-5"],"denials":1}`,
		`{"timestamp":"2026-10-23T13:00:00Z","model":"haiku","ok":true}`,
	)
	rows, err := summarizeCosts(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	var sonnet, haiku CostSummary
	for _, r := range rows {
		switch r.Model {
		case "sonnet":
			sonnet = r
		case "haiku":
			haiku = r
		}
	}
	if sonnet.ResolvedModels["claude-sonnet-5"] != 1 || sonnet.ResolvedModels["claude-sonnet-5-5"] != 2 {
		t.Errorf("sonnet ResolvedModels = %v", sonnet.ResolvedModels)
	}
	if sonnet.Denials != 4 {
		t.Errorf("sonnet Denials = %d, want 4", sonnet.Denials)
	}
	if len(haiku.ResolvedModels) != 0 || haiku.Denials != 0 {
		t.Errorf("legacy haiku row gained telemetry: %+v", haiku)
	}
}

func TestCostModelLabel(t *testing.T) {
	cases := []struct {
		name string
		row  CostSummary
		want string
	}{
		{"legacy row", CostSummary{Model: "haiku"}, "haiku"},
		{"resolved equals alias", CostSummary{Model: "ollama/qwen3.8:27b", ResolvedModels: map[string]int{"ollama/qwen3.8:27b": 2}}, "ollama/qwen3.8:27b"},
		{"one resolved", CostSummary{Model: "sonnet", ResolvedModels: map[string]int{"claude-sonnet-5-5": 3}}, "sonnet → claude-sonnet-5-5"},
		{"model changed in window", CostSummary{Model: "sonnet", ResolvedModels: map[string]int{"claude-sonnet-5-5": 2, "claude-sonnet-5": 1}}, "sonnet → claude-sonnet-5, claude-sonnet-5-5"},
		// A dream that spawned Haiku subagents lists both models in
		// modelUsage. Only ids matching the alias are shown, so the label
		// doesn't read as if "sonnet" moved to Haiku.
		{"subagent model ignored", CostSummary{Model: "sonnet", ResolvedModels: map[string]int{"claude-sonnet-5": 1, "claude-haiku-4-5-20251001": 1}}, "sonnet → claude-sonnet-5"},
		// No id matches the alias (an alias name nobody embeds): show all.
		// No alias at all: nothing may match (Contains("", x) is always
		// true), so the fallback shows every id.
		{"empty alias", CostSummary{Model: "", ResolvedModels: map[string]int{"claude-x": 1}}, " → claude-x"},
		{"no id matches alias", CostSummary{Model: "best", ResolvedModels: map[string]int{"claude-opus-5-5": 1}}, "best → claude-opus-5-5"},
	}
	for _, c := range cases {
		if got := costModelLabel(c.row); got != c.want {
			t.Errorf("%s: costModelLabel = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestMergeByModel_MergesTelemetryWithoutAliasing(t *testing.T) {
	a := CostSummary{Model: "sonnet", Calls: 1, ResolvedModels: map[string]int{"claude-sonnet-5": 1}, Denials: 2}
	b := CostSummary{Model: "sonnet", Calls: 1, ResolvedModels: map[string]int{"claude-sonnet-5-5": 1}, Denials: 1}
	dst := map[string]*CostSummary{}
	mergeByModel(dst, []CostSummary{a})
	mergeByModel(dst, []CostSummary{b})
	got := dst["sonnet"]
	if got.Denials != 3 || got.ResolvedModels["claude-sonnet-5"] != 1 || got.ResolvedModels["claude-sonnet-5-5"] != 1 {
		t.Errorf("merged = %+v", got)
	}
	if len(a.ResolvedModels) != 1 {
		t.Errorf("source row's map was mutated through the merge: %v", a.ResolvedModels)
	}
}
