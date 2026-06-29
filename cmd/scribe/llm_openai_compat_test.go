package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readTodaysCostEntries reads <root>/output/costs/<today>.jsonl and
// returns the parsed rows. Used to assert ledger side effects.
func readTodaysCostEntries(t *testing.T, root string) []CostEntry {
	t.Helper()
	day := time.Now().UTC().Format("2006-01-02")
	path := filepath.Join(root, "output", "costs", day+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []CostEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ce CostEntry
		if err := json.Unmarshal([]byte(line), &ce); err != nil {
			t.Fatalf("bad ledger line %q: %v", line, err)
		}
		out = append(out, ce)
	}
	return out
}

func TestOpenAICompatGenerate(t *testing.T) {
	var gotReq oaiChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "wrong path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer sk-test" {
			http.Error(w, "bad auth: "+auth, http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": "  hello world  "}, "finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": 120, "completion_tokens": 40},
		})
	}))
	defer srv.Close()

	root := t.TempDir()
	p := &openaiCompatProvider{
		providerName: "together",
		baseURL:      srv.URL + "/v1",
		apiKey:       "sk-test",
		keyEnvName:   "TOGETHER_API_KEY",
		model:        "qwen3-30b-a3b",
		root:         root,
	}
	out, err := p.Generate(context.Background(), "say hi")
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello world" {
		t.Errorf("output not trimmed: %q", out)
	}
	if gotReq.Model != "qwen3-30b-a3b" {
		t.Errorf("model = %q", gotReq.Model)
	}
	if len(gotReq.Messages) != 1 || gotReq.Messages[0].Role != "user" || gotReq.Messages[0].Content != "say hi" {
		t.Errorf("messages = %+v", gotReq.Messages)
	}
	if gotReq.ResponseFormat != nil {
		t.Errorf("plain Generate must not set response_format, got %+v", gotReq.ResponseFormat)
	}

	// Ledger row: provider/model prefixed, real token counts, no USD
	// (no pricing configured).
	rows := readTodaysCostEntries(t, root)
	if len(rows) != 1 {
		t.Fatalf("want 1 ledger row, got %d", len(rows))
	}
	ce := rows[0]
	if ce.Provider != "together" || ce.Model != "together/qwen3-30b-a3b" {
		t.Errorf("ledger provider/model = %q/%q", ce.Provider, ce.Model)
	}
	if ce.InputTokens == nil || *ce.InputTokens != 120 || ce.OutputTokens == nil || *ce.OutputTokens != 40 {
		t.Errorf("token counts wrong: in=%v out=%v", ce.InputTokens, ce.OutputTokens)
	}
	if !ce.OK {
		t.Error("expected OK row")
	}
	if ce.CostUSD != nil {
		t.Errorf("no pricing → no CostUSD, got %v", *ce.CostUSD)
	}
}

func TestOpenAICompatGenerateJSONSetsResponseFormat(t *testing.T) {
	var gotReq oaiChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": `{"ok":true}`}}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	defer srv.Close()

	p := &openaiCompatProvider{providerName: "groq", baseURL: srv.URL + "/v1", apiKey: "k", model: "qwen3-32b"}
	out, err := p.GenerateJSON(context.Background(), "give json")
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"ok":true}` {
		t.Errorf("out = %q", out)
	}
	if gotReq.ResponseFormat == nil || gotReq.ResponseFormat.Type != "json_object" {
		t.Errorf("GenerateJSON must request json_object, got %+v", gotReq.ResponseFormat)
	}
}

func TestOpenAICompatResponseFormatRetry(t *testing.T) {
	var calls int
	var sawResponseFormat []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req oaiChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		sawResponseFormat = append(sawResponseFormat, req.ResponseFormat != nil)
		if req.ResponseFormat != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"message": "response_format is not supported by this model"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "plain ok"}}},
			"usage":   map[string]int{"prompt_tokens": 8, "completion_tokens": 3},
		})
	}))
	defer srv.Close()

	p := &openaiCompatProvider{providerName: "huggingface", baseURL: srv.URL + "/v1", apiKey: "k", model: "some-model"}
	out, err := p.GenerateJSON(context.Background(), "json please")
	if err != nil {
		t.Fatalf("retry path should succeed, got %v", err)
	}
	if out != "plain ok" {
		t.Errorf("out = %q", out)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls (json then retry), got %d", calls)
	}
	if len(sawResponseFormat) != 2 || !sawResponseFormat[0] || sawResponseFormat[1] {
		t.Errorf("expected [true,false] response_format, got %v", sawResponseFormat)
	}
}

func TestOpenAICompatRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()

	root := t.TempDir()
	p := &openaiCompatProvider{providerName: "groq", baseURL: srv.URL + "/v1", apiKey: "k", model: "qwen3-32b", root: root}
	_, err := p.Generate(context.Background(), "hi")
	if !errors.Is(err, ErrRateLimit) {
		t.Fatalf("429 should map to ErrRateLimit, got %v", err)
	}
	// Failed call still records a ledger row marked not-OK.
	rows := readTodaysCostEntries(t, root)
	if len(rows) != 1 || rows[0].OK {
		t.Errorf("expected one not-OK ledger row, got %+v", rows)
	}
}

func TestOpenAICompatMissingKeyErrorsBeforeHTTP(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &openaiCompatProvider{providerName: "together", baseURL: srv.URL + "/v1", apiKey: "", keyEnvName: "TOGETHER_API_KEY", model: "qwen3-30b-a3b"}
	_, err := p.Generate(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "TOGETHER_API_KEY") {
		t.Fatalf("expected clear missing-key error naming the env var, got %v", err)
	}
	if hit {
		t.Error("must not make an HTTP call without a key")
	}
}

func TestOpenAICompatClaudeAliasModelRejected(t *testing.T) {
	p := &openaiCompatProvider{providerName: "groq", baseURL: "http://example.invalid/v1", apiKey: "k", model: "haiku"}
	_, err := p.Generate(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "hosted model") {
		t.Fatalf("Claude alias model should be rejected with a clear error, got %v", err)
	}
}

func TestOpenAICompatBakesCostFromPricing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "x"}}},
			"usage":   map[string]int{"prompt_tokens": 1_000_000, "completion_tokens": 500_000},
		})
	}))
	defer srv.Close()

	root := t.TempDir()
	p := &openaiCompatProvider{
		providerName: "fireworks", baseURL: srv.URL + "/v1", apiKey: "k",
		model: "qwen3-30b-a3b", root: root,
		pricing: &ModelPrice{Input: 0.90, Output: 0.90},
	}
	if _, err := p.Generate(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	rows := readTodaysCostEntries(t, root)
	if len(rows) != 1 || rows[0].CostUSD == nil {
		t.Fatalf("expected a baked CostUSD, got %+v", rows)
	}
	// 1M in * 0.90/1M + 0.5M out * 0.90/1M = 0.90 + 0.45 = 1.35
	if got := *rows[0].CostUSD; got < 1.349 || got > 1.351 {
		t.Errorf("CostUSD = %v, want ~1.35", got)
	}
}

func TestNewOpenAICompatProviderResolvesEnvKey(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "tok-123")
	p := newOpenAICompatProvider("together", "qwen3-30b-a3b", "")
	if p.apiKey != "tok-123" {
		t.Errorf("apiKey = %q, want tok-123", p.apiKey)
	}
	if p.baseURL != "https://api.together.xyz/v1" {
		t.Errorf("baseURL = %q", p.baseURL)
	}
	if p.Name() != "together/qwen3-30b-a3b" {
		t.Errorf("Name = %q", p.Name())
	}
}

func TestNewOpenAICompatProviderGenericKeyFallback(t *testing.T) {
	// Specific env var unset; generic SCRIBE_LLM_API_KEY supplies the key.
	t.Setenv("GROQ_API_KEY", "")
	t.Setenv(genericAPIKeyEnv, "generic-key")
	p := newOpenAICompatProvider("groq", "qwen3-32b", "")
	if p.apiKey != "generic-key" {
		t.Errorf("apiKey = %q, want generic-key", p.apiKey)
	}
}

func TestNewOpenAICompatProviderConfigOverrides(t *testing.T) {
	root := t.TempDir()
	cfgYAML := `kb_name: test
llm:
  provider: openai-compat
  base_url: https://my-endpoint.example/v1
  api_key_env: MY_CUSTOM_KEY
  pricing:
    "openai-compat/local-model":
      input: 0.10
      output: 0.30
`
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MY_CUSTOM_KEY", "custom-123")
	p := newOpenAICompatProvider("openai-compat", "local-model", root)
	if p.baseURL != "https://my-endpoint.example/v1" {
		t.Errorf("baseURL = %q", p.baseURL)
	}
	if p.apiKey != "custom-123" {
		t.Errorf("apiKey = %q", p.apiKey)
	}
	if p.pricing == nil || p.pricing.Input != 0.10 || p.pricing.Output != 0.30 {
		t.Errorf("pricing = %+v", p.pricing)
	}
}

func TestResolveHostedAPIKeyFromUserConfig(t *testing.T) {
	// Point the user-config dir at a temp dir so we don't read the real
	// ~/.config/scribe/config.yaml, and clear env so the file is consulted.
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("TOGETHER_API_KEY", "")
	t.Setenv("GROQ_API_KEY", "")
	t.Setenv(genericAPIKeyEnv, "")
	dir := filepath.Join(cfgHome, "scribe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ucYAML := `kb_dir: /tmp/whatever
llm_api_key: file-default-key
llm_api_keys:
  groq: groq-specific-key
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(ucYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	// Per-provider map wins for groq.
	if got := resolveHostedAPIKey("groq", "GROQ_API_KEY"); got != "groq-specific-key" {
		t.Errorf("groq key = %q, want groq-specific-key", got)
	}
	// A provider with no map entry falls back to the single llm_api_key.
	if got := resolveHostedAPIKey("together", "TOGETHER_API_KEY"); got != "file-default-key" {
		t.Errorf("together key = %q, want file-default-key", got)
	}
	// An env var still overrides the file.
	t.Setenv("TOGETHER_API_KEY", "env-wins")
	if got := resolveHostedAPIKey("together", "TOGETHER_API_KEY"); got != "env-wins" {
		t.Errorf("env should override file, got %q", got)
	}
	// Generic env var beats the file too.
	t.Setenv("TOGETHER_API_KEY", "")
	t.Setenv(genericAPIKeyEnv, "generic-wins")
	if got := resolveHostedAPIKey("together", "TOGETHER_API_KEY"); got != "generic-wins" {
		t.Errorf("generic env should beat file, got %q", got)
	}
}

func TestIsHostedProvider(t *testing.T) {
	for _, name := range []string{"together", "groq", "fireworks", "huggingface", "openai-compat", "TOGETHER", " groq "} {
		if !isHostedProvider(name) {
			t.Errorf("isHostedProvider(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"anthropic", "ollama", "", "openai", "bogus"} {
		if isHostedProvider(name) {
			t.Errorf("isHostedProvider(%q) = true, want false", name)
		}
	}
}

func TestNewLLMProviderRoutesHosted(t *testing.T) {
	cases := map[string]string{
		"together":      "together/qwen3-30b-a3b",
		"groq":          "groq/qwen3-30b-a3b",
		"fireworks":     "fireworks/qwen3-30b-a3b",
		"huggingface":   "huggingface/qwen3-30b-a3b",
		"openai-compat": "openai-compat/qwen3-30b-a3b",
	}
	for provider, want := range cases {
		got := newLLMProvider(provider, "qwen3-30b-a3b", "", "").Name()
		if got != want {
			t.Errorf("newLLMProvider(%q).Name() = %q, want %q", provider, got, want)
		}
	}
}
