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

// newLLMProvider picks the provider backend based on scribe.yaml. Unknown
// provider names fall back to anthropic with a log line so misconfiguration
// never silently no-ops.
//
// kbRoot is forwarded to the anthropic provider so its claude calls can
// land in output/costs/<day>.jsonl alongside calls from runClaude. Empty
// kbRoot is tolerated — appendCostEntry no-ops on empty root, so callers
// without a KB context (e.g. unit tests) keep working.
func newLLMProvider(provider, model, ollamaURL, kbRoot string) llmProviderGenerator {
	switch strings.ToLower(provider) {
	case "ollama":
		return &ollamaProvider{baseURL: ollamaURL, model: model}
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
	args := []string{
		"-p", prompt,
		"--no-session-persistence",
		"--model", a.model,
		// No tools — this path is for pure text generation.
		"--settings", `{"hooks":{}}`,
	}
	started := time.Now()
	op := opLabelFromContext(ctx)
	entry := CostEntry{
		Timestamp:   started.UTC().Format(time.RFC3339),
		Model:       a.model,
		Op:          op,
		PromptChars: len(prompt),
	}
	defer func() {
		entry.DurationMS = time.Since(started).Milliseconds()
		appendCostEntry(a.root, entry)
	}()

	cmd := exec.CommandContext(ctx, "claude", args...)
	out, err := cmd.CombinedOutput()
	outStr := string(out)
	if isRateLimited(outStr) {
		entry.OK = false
		entry.ErrKind = "rate_limit"
		return tailLines(outStr, 5), ErrRateLimit
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
		return tailLines(outStr, 15), fmt.Errorf("claude -p: %w", err)
	}
	entry.OK = true
	return strings.TrimSpace(outStr), nil
}

// --- ollama ---

// ollamaProvider hits the local Ollama HTTP API. Free, fully offline, and
// reuses whatever model the user already pulled (`ollama pull llama3.2:3b`).
// Llama.cpp's server exposes the same shape at /api/generate, so this also
// works against a plain llama-server if you run one on localhost.
type ollamaProvider struct {
	baseURL string
	model   string
}

func (o *ollamaProvider) Name() string { return "ollama/" + o.model }

// ollamaRequest mirrors the shape Ollama's /api/generate expects.
type ollamaRequest struct {
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt"`
	Stream  bool           `json:"stream"`
	Options map[string]any `json:"options,omitempty"`
}

type ollamaResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
	// Ollama returns error in a top-level "error" field on 4xx/5xx.
	Error string `json:"error,omitempty"`
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
	if err := o.ensureReady(ctx); err != nil {
		return "", err
	}
	url := strings.TrimRight(o.baseURL, "/") + "/api/generate"
	reqBody := ollamaRequest{
		Model:  o.model,
		Prompt: prompt,
		Stream: false,
		// num_ctx 8192 accommodates most raw articles; the caller trims if
		// a source is pathologically long.
		Options: map[string]any{"num_ctx": 8192, "temperature": 0.3},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama http: %w — is Ollama running at %s?", err, o.baseURL)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("ollama %d: %s", resp.StatusCode, string(data))
	}

	var or ollamaResponse
	if err := json.Unmarshal(data, &or); err != nil {
		return "", fmt.Errorf("decode response: %w (body=%s)", err, string(data))
	}
	if or.Error != "" {
		return "", fmt.Errorf("ollama: %s", or.Error)
	}
	return strings.TrimSpace(or.Response), nil
}
