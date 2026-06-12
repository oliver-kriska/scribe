package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// llmProviderGenerator is a minimal text-in → text-out interface. No tool
// use, no streaming — just "here's a prompt, give me a paragraph." All
// scribe passes that don't need filesystem or grep access should use this
// instead of runClaude. Current callers: contextualize.
//
// Implementations must be safe to call from multiple goroutines.
type llmProviderGenerator interface {
	Generate(ctx context.Context, prompt string) (string, error)
	// Name returns a short identifier for logs and run records.
	Name() string
}

// jsonModeProvider is the optional interface a provider implements when it
// supports a structured-output mode for prompts that must return one JSON
// document. Callers that know they want JSON (e.g. pass-2 envelope mode)
// type-assert and prefer GenerateJSON when available; everything else stays
// on Generate. Anthropic intentionally doesn't implement this — `claude -p`
// already returns clean text and the prompt itself enforces shape.
type jsonModeProvider interface {
	GenerateJSON(ctx context.Context, prompt string) (string, error)
}

// generateMaybeJSON dispatches to GenerateJSON when the provider supports
// it, falling back to Generate otherwise. Centralized so the call sites
// (absorbDenseTwoPass, future envelope callers) don't repeat the type
// assertion.
func generateMaybeJSON(ctx context.Context, p llmProviderGenerator, prompt string) (string, error) {
	if jp, ok := p.(jsonModeProvider); ok {
		return jp.GenerateJSON(ctx, prompt)
	}
	return p.Generate(ctx, prompt)
}

// newLLMProvider picks the provider backend based on scribe.yaml. It is a
// package variable (pointing at realNewLLMProvider) purely so driver tests
// can swap in a scripted stub provider (see llm_stub_test.go) without a
// network or a real LLM; production code never reassigns it.
var newLLMProvider = realNewLLMProvider

// realNewLLMProvider is the production implementation behind the
// newLLMProvider seam. Unknown provider names fall back to anthropic with
// a log line so misconfiguration never silently no-ops.
//
// kbRoot is forwarded to the anthropic provider so its claude calls can
// land in output/costs/<day>.jsonl alongside calls from runClaude. Empty
// kbRoot is tolerated — appendCostEntry no-ops on empty root, so callers
// without a KB context (e.g. unit tests) keep working.
func realNewLLMProvider(provider, model, ollamaURL, kbRoot string) llmProviderGenerator {
	switch strings.ToLower(provider) {
	case "ollama":
		return &ollamaProvider{baseURL: ollamaURL, model: model, root: kbRoot}
	case "anthropic", "":
		return &anthropicProvider{model: model, root: kbRoot}
	default:
		logMsg("llm", "unknown provider %q — falling back to anthropic", provider)
		return &anthropicProvider{model: model, root: kbRoot}
	}
}

// --- anthropic (claude -p) ---

// anthropicProvider shells out to `claude -p` with the prompt as the only
// input, no allowed tools. Reuses whatever auth is already configured for
// the Claude CLI. Suitable for cheap short-context generation (Haiku) and
// longer tasks (Sonnet) alike.
type anthropicProvider struct {
	model string
	// root points at the KB so Generate can append a row to the cost
	// ledger. Empty root means "don't track" — appendCostEntry no-ops
	// on empty root and we don't drag a logger error through every
	// pure-text generation. Tests construct providers without a KB.
	root string
}

func (a *anthropicProvider) Name() string { return "anthropic/" + a.model }

// Generate shells out to `claude -p` and writes one CostEntry to the
// daily cost ledger so contextualize / contradictions / identity
// passes show up alongside runClaude calls. The ledger write is a
// deferred best-effort: errors there don't bubble to the caller.
//
// Op label is read from ctx via opLabelFromContext (same plumbing as
// runClaude), so callers tagging their context propagate the label
// here too. Untagged calls record an empty op field — still tracked.
func (a *anthropicProvider) Generate(ctx context.Context, prompt string) (string, error) {
	// Daily anthropic output-token ceiling: same gate runClaude uses.
	// Local-provider Generate paths (ollamaProvider) skip this — the
	// ceiling tracks Anthropic quota only.
	if cfg := loadConfig(a.root); cfg != nil {
		if err := checkBudget(a.root, cfg.Sync.DailyAnthropicOutputTokenCeiling); err != nil {
			return "", err
		}
	}

	args := []string{
		"-p", prompt,
		"--no-session-persistence",
		"--model", a.model,
		// No tools — this path is for pure text generation.
		"--settings", `{"hooks":{}}`,
		// Phase 3D.5: structured output → real token counts in the
		// ledger. Same fallback strategy as runClaude.
		"--output-format", "json",
	}
	started := time.Now()
	op := opLabelFromContext(ctx)
	entry := CostEntry{
		Timestamp:   started.UTC().Format(time.RFC3339),
		Provider:    "anthropic",
		Model:       a.model,
		Op:          op,
		PromptChars: len(prompt),
	}
	defer func() {
		entry.DurationMS = time.Since(started).Milliseconds()
		appendCostEntry(a.root, entry)
	}()

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	stdoutStr := stdoutBuf.String()
	stderrStr := stderrBuf.String()
	combined := stdoutStr + "\n" + stderrStr

	// Rate-limit text-matching scans stderr only — see claude.go for
	// the full rationale. Short version: ~10% of articles in the
	// corpus discuss rate-limiting as a topic, and matching against
	// the model's response content (stdout) produces catastrophic
	// false positives that strand whole absorb runs.
	if isRateLimited(stderrStr) {
		entry.OK = false
		entry.ErrKind = "rate_limit"
		return tailLines(combined, 5), ErrRateLimit
	}
	if err != nil {
		entry.OK = false
		switch ctx.Err() {
		case context.DeadlineExceeded:
			entry.ErrKind = "timeout"
		case context.Canceled:
			entry.ErrKind = "canceled"
		default:
			entry.ErrKind = "other"
		}
		return tailLines(combined, 15), fmt.Errorf("claude -p: %w", err)
	}

	env, ok := parseClaudeResult(stdoutStr)
	if ok {
		if env.Usage.InputTokens > 0 {
			in := env.Usage.InputTokens
			entry.InputTokens = &in
		}
		if env.Usage.OutputTokens > 0 {
			out := env.Usage.OutputTokens
			entry.OutputTokens = &out
		}
		if env.Usage.CacheReadInputTokens > 0 {
			c := env.Usage.CacheReadInputTokens
			entry.CacheReadTokens = &c
		}
		if env.TotalCostUSD > 0 {
			cost := env.TotalCostUSD
			entry.CostUSD = &cost
		}
		if env.IsError {
			entry.OK = false
			if isRateLimitSubtype(env.Subtype) {
				entry.ErrKind = "rate_limit"
				return env.Result, ErrRateLimit
			}
			entry.ErrKind = "other"
			return env.Result, fmt.Errorf("claude -p: %s", env.Subtype)
		}
		entry.OK = true
		return strings.TrimSpace(env.Result), nil
	}

	// Fall back to text mode if JSON parse fails.
	entry.OK = true
	return strings.TrimSpace(stdoutStr), nil
}

// --- ollama ---

// ollamaProvider hits the local Ollama HTTP API. Free, fully offline, and
// reuses whatever model the user already pulled (`ollama pull llama3.2:3b`).
// Llama.cpp's server exposes the same shape at /api/generate, so this also
// works against a plain llama-server if you run one on localhost.
type ollamaProvider struct {
	baseURL string
	model   string
	// root is the KB so Generate / GenerateJSON can append a cost-ledger
	// row alongside anthropic-provider calls. Empty root means "don't
	// track" — appendCostEntry no-ops on empty root, which is what test
	// constructors without a KB context want.
	root string
}

func (o *ollamaProvider) Name() string { return "ollama/" + o.model }

// ollamaRequest mirrors the shape Ollama's /api/generate expects.
// Format is the structured-output flag — "json" forces the model to
// emit exactly one JSON document. Models without explicit grammar
// support still benefit because Ollama's runtime falls back to
// token-level constraint when format=json is set.
type ollamaRequest struct {
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt"`
	Stream  bool           `json:"stream"`
	Format  string         `json:"format,omitempty"`
	Options map[string]any `json:"options,omitempty"`
}

type ollamaResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
	// Ollama returns error in a top-level "error" field on 4xx/5xx.
	Error string `json:"error,omitempty"`
	// Token counts used for the cost ledger. Ollama emits these on
	// every successful /api/generate response.
	PromptEvalCount int `json:"prompt_eval_count,omitempty"`
	EvalCount       int `json:"eval_count,omitempty"`
}

// ensureReady checks that Ollama is reachable and the requested model is
// pulled locally. First call per (baseURL, model) pair does real work; later
// calls are O(1) via ollamaReadyCache. Mirrors the qmd UX where the user
// only has to install ollama — scribe handles the rest.
func (o *ollamaProvider) ensureReady(ctx context.Context) error {
	key := o.baseURL + "|" + o.model
	ollamaReadyMu.Lock()
	done := ollamaReadyCache[key]
	ollamaReadyMu.Unlock()
	if done {
		return nil
	}

	present, err := o.listedModels(ctx)
	if err != nil {
		return fmt.Errorf("ollama unreachable at %s: %w — install with `brew install ollama` and run `ollama serve`", o.baseURL, err)
	}

	if !o.modelListContains(present, o.model) {
		logMsg("llm", "ollama: %s not found locally — pulling (first-run, can take minutes)", o.model)
		if err := o.pull(ctx); err != nil {
			return fmt.Errorf("ollama pull %s: %w", o.model, err)
		}
		logMsg("llm", "ollama: %s ready", o.model)
	}

	ollamaReadyMu.Lock()
	ollamaReadyCache[key] = true
	ollamaReadyMu.Unlock()
	return nil
}

// listedModels hits GET /api/tags and returns the names reported by the
// server. Connection refused / HTTP errors bubble up so the caller can
// present a friendly install hint.
func (o *ollamaProvider) listedModels(ctx context.Context) ([]string, error) {
	url := strings.TrimRight(o.baseURL, "/") + "/api/tags"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	// Short timeout for health check — we don't want to hang minutes on a
	// stopped server.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama %d: %s", resp.StatusCode, string(body))
	}
	var tags struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, fmt.Errorf("decode /api/tags: %w", err)
	}
	names := make([]string, 0, len(tags.Models))
	for _, m := range tags.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

// modelListContains handles Ollama's tag-optional naming: "llama3.2" matches
// "llama3.2:latest", "llama3.2:3b" matches itself, etc.
func (o *ollamaProvider) modelListContains(list []string, want string) bool {
	wantBase, wantTag := splitModelTag(want)
	for _, name := range list {
		nameBase, nameTag := splitModelTag(name)
		if nameBase != wantBase {
			continue
		}
		// Exact match on tag, or caller omitted tag (matches any), or caller
		// asked for "latest" and the server returned bare name.
		if wantTag == "" || nameTag == wantTag || wantTag == "latest" && nameTag == "latest" {
			return true
		}
	}
	return false
}

// splitModelTag breaks "family:tag" into (family, tag). Missing tag → "".
func splitModelTag(s string) (string, string) {
	if i := strings.Index(s, ":"); i > 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// pull streams the /api/pull progress and logs completion milestones. Does
// not render per-byte progress — too noisy for a sync log. Returns when the
// server emits {"status":"success"}.
func (o *ollamaProvider) pull(ctx context.Context) error {
	url := strings.TrimRight(o.baseURL, "/") + "/api/pull"
	body, _ := json.Marshal(map[string]any{"model": o.model, "stream": true})
	// Generous timeout — pulling a 3B model on a slow connection can take
	// several minutes.
	pullCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(pullCtx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ollama pull http %d: %s", resp.StatusCode, string(b))
	}
	scanner := bufio.NewScanner(resp.Body)
	// Pull responses can be large — bump scanner buffer.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lastStatus string
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Error != "" {
			return fmt.Errorf("ollama pull error: %s", ev.Error)
		}
		if ev.Status != lastStatus && ev.Status != "" {
			// Log only coarse status changes (not per-chunk download progress).
			if ev.Status != "pulling" && !strings.HasPrefix(ev.Status, "pulling ") {
				logMsg("llm", "ollama pull: %s", ev.Status)
			}
			lastStatus = ev.Status
		}
		if ev.Status == "success" {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read pull stream: %w", err)
	}
	return fmt.Errorf("ollama pull ended without success marker")
}

// ollamaReadyCache remembers which (baseURL|model) pairs have already been
// verified + pulled during this process lifetime. Prevents redundant /api/tags
// hits from the contextualize loop.
var (
	ollamaReadyMu    sync.Mutex
	ollamaReadyCache = map[string]bool{}
)

func (o *ollamaProvider) Generate(ctx context.Context, prompt string) (string, error) {
	return o.generate(ctx, prompt, false)
}

// GenerateJSON forces Ollama into structured-output mode by setting
// `format: "json"` on the /api/generate request. Models that support it
// (all current Ollama-served Llama/Qwen/Gemma/Mistral variants) will
// produce exactly one JSON document with no preamble, no markdown
// fences, no trailing prose. Smaller models in particular benefit —
// gemma3:4b emits malformed envelopes ~30% of the time under plain
// text, near zero under format:"json".
func (o *ollamaProvider) GenerateJSON(ctx context.Context, prompt string) (string, error) {
	return o.generate(ctx, prompt, true)
}

// generate is the shared implementation behind Generate and GenerateJSON.
// jsonMode toggles Ollama's structured-output flag; everything else
// (cost-ledger row, error handling, ensureReady) is identical.
//
// num_ctx defaults to 8192 (enough for contextualize / facts / pass-1
// chapter prompts). Callers handling bigger packets (session-mine
// transcripts, dream orient packets, assess/deep file batches) MUST
// pre-tag their context with withOllamaNumCtx — without that, Ollama
// silently truncates anything past 8192 tokens. The default isn't
// "fits everything" because a 32K context costs RAM and slows the
// model on every call, not just the big ones.
func (o *ollamaProvider) generate(ctx context.Context, prompt string, jsonMode bool) (string, error) {
	if err := o.ensureReady(ctx); err != nil {
		return "", err
	}
	url := strings.TrimRight(o.baseURL, "/") + "/api/generate"
	numCtx := ollamaNumCtxFromContext(ctx)
	if numCtx <= 0 {
		numCtx = 8192
	}
	reqBody := ollamaRequest{
		Model:   o.model,
		Prompt:  prompt,
		Stream:  false,
		Options: map[string]any{"num_ctx": numCtx, "temperature": 0.3},
	}
	if jsonMode {
		reqBody.Format = "json"
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	// Cost-ledger plumbing mirrors anthropicProvider. Ollama calls have
	// no USD cost, but recording duration + token counts makes the local
	// path show up in `scribe cost` alongside anthropic calls — without
	// this the daily ledger silently underreports work that's moved
	// off Anthropic quota.
	started := time.Now()
	op := opLabelFromContext(ctx)
	entry := CostEntry{
		Timestamp:   started.UTC().Format(time.RFC3339),
		Provider:    "ollama",
		Model:       "ollama/" + o.model,
		Op:          op,
		PromptChars: len(prompt),
	}
	defer func() {
		entry.DurationMS = time.Since(started).Milliseconds()
		appendCostEntry(o.root, entry)
	}()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		entry.OK = false
		entry.ErrKind = "other"
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// No client-level timeout: every Ollama caller already bounds the
	// request via context.WithTimeout (dream 20m, session-mine 8m,
	// deep 10m, assess 10m, absorb-pass2 25m, …). A separate 10-min
	// client cap silently overrode those — with Stream: false the
	// /api/generate call buffers the whole response, so the client
	// timeout has to cover cold-load + full generation. Trusting the
	// per-op context is both correct and more honest about which
	// knob the user should tune.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		entry.OK = false
		entry.ErrKind = "other"
		return "", fmt.Errorf("ollama http: %w — is Ollama running at %s?", err, o.baseURL)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		entry.OK = false
		entry.ErrKind = "other"
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		entry.OK = false
		entry.ErrKind = "other"
		return "", fmt.Errorf("ollama %d: %s", resp.StatusCode, string(data))
	}

	var or ollamaResponse
	if err := json.Unmarshal(data, &or); err != nil {
		entry.OK = false
		entry.ErrKind = "other"
		return "", fmt.Errorf("decode response: %w (body=%s)", err, string(data))
	}
	if or.Error != "" {
		entry.OK = false
		entry.ErrKind = "other"
		return "", fmt.Errorf("ollama: %s", or.Error)
	}
	entry.OK = true
	// Ollama returns token counts in eval_count + prompt_eval_count
	// when set on the response struct. Wire those into the ledger so
	// `scribe cost` rolls them up like the anthropic counterparts.
	if or.PromptEvalCount > 0 {
		in := int64(or.PromptEvalCount)
		entry.InputTokens = &in
	}
	if or.EvalCount > 0 {
		out := int64(or.EvalCount)
		entry.OutputTokens = &out
	}
	return strings.TrimSpace(or.Response), nil
}
