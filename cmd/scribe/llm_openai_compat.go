// llm_openai_compat.go — hosted OpenAI-compatible inference providers
// (Together AI, Groq, Fireworks, Hugging Face Inference, or any endpoint
// that speaks /v1/chat/completions). One implementation covers all of
// them: they differ only in base URL and which env var carries the API
// key. See issue #43 and research/2026-06-25-cloud-inference-cost-vs-local.
//
// Why this exists: the local Ollama path assumes a machine that can hold
// a quantized 12B–30B model in memory. A hosted provider removes that
// hardware floor while keeping scribe's "bring your own model" story —
// you still pin `qwen3-30b-a3b`, it just runs on someone else's GPU for
// a few dollars a month. The workload is ~89% input tokens (absorb
// re-reads large article context), which is the cheap side everywhere.
//
// Three things make this more than an HTTP client (all handled here +
// budget.go + cost_ledger.go):
//   - it's a METERED provider, so it honors the daily output-token
//     ceiling (budget.go) exactly like anthropic does — a looping pass
//     can otherwise emit millions of output tokens before anyone notices;
//   - it ships KB content to a third party, so it's strictly opt-in via
//     explicit scribe.yaml config (never the default);
//   - the cost ledger can report dollars when a per-model price is
//     configured (rates drift and vary by provider, so they live in
//     config, not a hardcoded table).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// hostedProviderDefaults is the built-in base URL + conventional API-key
// env var for a named hosted provider. openai-compat carries no defaults
// — the user supplies base_url and api_key_env (or SCRIBE_LLM_API_KEY).
type hostedProviderDefaults struct {
	baseURL string // OpenAI-compatible base, includes the /v1 segment
	keyEnv  string // env var the API key is read from by convention
}

// hostedProviderDefaultsByName registers the provider names accepted in
// scribe.yaml. Base URLs are the documented OpenAI-compatible endpoints
// as of June 2026; a user can always override with llm.base_url when a
// provider moves its endpoint. Membership in this map is also what makes
// newLLMProvider route to the hosted path instead of falling back to
// anthropic.
var hostedProviderDefaultsByName = map[string]hostedProviderDefaults{
	"together":      {baseURL: "https://api.together.xyz/v1", keyEnv: "TOGETHER_API_KEY"},
	"groq":          {baseURL: "https://api.groq.com/openai/v1", keyEnv: "GROQ_API_KEY"},
	"fireworks":     {baseURL: "https://api.fireworks.ai/inference/v1", keyEnv: "FIREWORKS_API_KEY"},
	"huggingface":   {baseURL: "https://router.huggingface.co/v1", keyEnv: "HF_TOKEN"},
	"openai-compat": {baseURL: "", keyEnv: ""},
}

// genericAPIKeyEnv is the catch-all env var read when a provider's
// specific key env var is unset. Lets a single key serve whichever
// hosted provider is configured without per-provider env juggling.
const genericAPIKeyEnv = "SCRIBE_LLM_API_KEY" // #nosec G101 -- env var name, not a credential

// isHostedProvider reports whether name is a registered OpenAI-compatible
// hosted provider. Case-insensitive; trims whitespace.
func isHostedProvider(name string) bool {
	_, ok := hostedProviderDefaultsByName[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// openaiCompatProvider implements llmProviderGenerator + jsonModeProvider
// against an OpenAI-compatible /v1/chat/completions endpoint. Safe for
// concurrent use (no mutable state after construction).
type openaiCompatProvider struct {
	providerName string // "together" | "groq" | "fireworks" | "huggingface" | "openai-compat"
	baseURL      string // resolved, no trailing slash, includes /v1
	apiKey       string // resolved from env at construction; "" → Generate errors clearly
	keyEnvName   string // env var the key should have come from (for the error message)
	model        string
	// root points at the KB so Generate can honor the budget ceiling and
	// append a cost-ledger row. Empty root means "don't track" — the
	// budget check and appendCostEntry both no-op on empty root.
	root string
	// pricing, when non-nil, is this model's USD-per-1M-token rate from
	// llm.pricing in scribe.yaml. Used to bake a real cost_usd into the
	// ledger so `scribe cost` reports dollars, not just tokens.
	pricing *ModelPrice
}

func (p *openaiCompatProvider) Name() string { return p.providerName + "/" + p.model }

// newOpenAICompatProvider resolves base URL, API key, and per-model
// pricing for a hosted provider from the registry + scribe.yaml + env.
// The API key is read from an env var — never from scribe.yaml — so a
// committed config can't leak a credential. A missing key or base URL is
// NOT a construction error: the provider is returned anyway and fails
// loudly on first Generate, so a misconfigured hosted route never
// silently falls back to a different (still-paid) vendor.
func newOpenAICompatProvider(provider, model, kbRoot string) *openaiCompatProvider {
	name := strings.ToLower(strings.TrimSpace(provider))
	def := hostedProviderDefaultsByName[name]
	baseURL := def.baseURL
	keyEnv := def.keyEnv

	var pricing *ModelPrice
	if cfg := loadConfig(kbRoot); cfg != nil {
		if cfg.LLM.BaseURL != "" {
			baseURL = cfg.LLM.BaseURL
		}
		if cfg.LLM.APIKeyEnv != "" {
			keyEnv = cfg.LLM.APIKeyEnv
		}
		// Price lookup prefers the exact ledger key (provider/model, the
		// `model` column in `scribe cost`) and falls back to the bare
		// model name so a single entry can serve the same model across
		// providers.
		if cfg.LLM.Pricing != nil {
			if mp, ok := cfg.LLM.Pricing[name+"/"+model]; ok {
				pricing = &mp
			} else if mp, ok := cfg.LLM.Pricing[model]; ok {
				pricing = &mp
			}
		}
	}

	return &openaiCompatProvider{
		providerName: name,
		baseURL:      strings.TrimRight(baseURL, "/"),
		apiKey:       resolveHostedAPIKey(name, keyEnv),
		keyEnvName:   keyEnv,
		model:        model,
		root:         kbRoot,
		pricing:      pricing,
	}
}

// resolveHostedAPIKey finds the API key for a hosted provider. Order:
//
//  1. the provider's specific env var (e.g. GROQ_API_KEY, or whatever
//     llm.api_key_env names) — a one-off export still wins;
//  2. the generic SCRIBE_LLM_API_KEY env var;
//  3. the per-machine user config (~/.config/scribe/config.yaml):
//     llm_api_keys[provider], then the single llm_api_key.
//
// The user-config fallback is what makes a paste-once key work for cron
// (no shell exports needed) and keeps the secret out of any KB scribe.yaml
// that could be committed to a shared repo. Returns "" when nothing is
// set; the provider's Generate then errors clearly.
func resolveHostedAPIKey(providerName, keyEnvName string) string {
	if keyEnvName != "" {
		if v := os.Getenv(keyEnvName); v != "" {
			return v
		}
	}
	if v := os.Getenv(genericAPIKeyEnv); v != "" {
		return v
	}
	uc := loadUserConfig()
	if uc.LLMAPIKeys != nil {
		if v := strings.TrimSpace(uc.LLMAPIKeys[providerName]); v != "" {
			return v
		}
	}
	return strings.TrimSpace(uc.LLMAPIKey)
}

func (p *openaiCompatProvider) Generate(ctx context.Context, prompt string) (string, error) {
	return p.generate(ctx, prompt, false)
}

// GenerateJSON requests structured JSON output via response_format. The
// major hosted providers (Together/Groq/Fireworks) honor
// {"type":"json_object"}; the generate path retries once without it if
// the endpoint rejects the field, so a provider that lacks json mode
// still works (the envelope prompts enforce shape on their own).
func (p *openaiCompatProvider) GenerateJSON(ctx context.Context, prompt string) (string, error) {
	return p.generate(ctx, prompt, true)
}

// oaiChatRequest is the subset of the OpenAI /v1/chat/completions request
// body scribe sends. ResponseFormat is omitted unless JSON mode is on.
type oaiChatRequest struct {
	Model          string           `json:"model"`
	Messages       []oaiChatMessage `json:"messages"`
	Temperature    float64          `json:"temperature"`
	ResponseFormat *oaiResponseFmt  `json:"response_format,omitempty"`
}

type oaiChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaiResponseFmt struct {
	Type string `json:"type"`
}

// oaiChatResponse covers the success and error shapes. Providers add
// fields over time; unknown keys are ignored. usage is the OpenAI-
// standard token accounting every one of these providers returns.
type oaiChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// generate is the shared implementation behind Generate / GenerateJSON.
// It honors the daily metered-output ceiling, writes one cost-ledger row
// (with dollars when a price is configured), and maps a 429 to
// ErrRateLimit so sync's outer loop drains cleanly — same contract as
// the anthropic provider.
func (p *openaiCompatProvider) generate(ctx context.Context, prompt string, jsonMode bool) (string, error) {
	// Metered provider → honor the daily output-token ceiling. Reading
	// config per call mirrors anthropicProvider.Generate; the budget
	// check no-ops on empty root or a zero ceiling.
	if cfg := loadConfig(p.root); cfg != nil {
		if err := checkBudget(p.root, effectiveOutputTokenCeiling(cfg.Sync)); err != nil {
			return "", err
		}
	}

	// Fail loudly before spending a network round-trip on a request that
	// can't succeed. Falling back to anthropic here would be a privacy
	// surprise (different vendor) and could spend on the wrong account.
	if p.apiKey == "" {
		envHint := p.keyEnvName
		if envHint == "" {
			envHint = genericAPIKeyEnv
		}
		return "", fmt.Errorf("%s: no API key — set %s (or %s) in the environment", p.providerName, envHint, genericAPIKeyEnv)
	}
	if p.baseURL == "" {
		return "", fmt.Errorf("%s: no base URL — set llm.base_url in scribe.yaml", p.providerName)
	}
	// A Claude alias (or empty model) routed to a hosted endpoint is
	// always a misconfiguration — the per-op default model leaked through
	// without the user pinning a real open model. A clear error here
	// beats an opaque upstream 400.
	if p.model == "" || isClaudeModelAlias(p.model) {
		return "", fmt.Errorf("%s: model %q is not a hosted model — pin an explicit open model (e.g. qwen3-30b-a3b, gemma3-12b) for this provider", p.providerName, p.model)
	}

	started := time.Now()
	op := opLabelFromContext(ctx)
	entry := CostEntry{
		Timestamp: started.UTC().Format(time.RFC3339),
		Provider:  p.providerName,
		// Prefix with the provider so `scribe cost` separates, say,
		// groq/qwen3-32b from together/qwen3-32b, and so the row key
		// matches the llm.pricing lookup.
		Model:       p.providerName + "/" + p.model,
		Op:          op,
		PromptChars: len(prompt),
	}
	defer func() {
		entry.DurationMS = time.Since(started).Milliseconds()
		appendCostEntry(p.root, entry)
	}()

	out, resp, err := p.doRequest(ctx, prompt, jsonMode)
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
		return "", err
	}

	// Token accounting + dollars. Pointers so "absent" stays
	// distinguishable from zero in the ledger.
	if resp.Usage.PromptTokens > 0 {
		in := resp.Usage.PromptTokens
		entry.InputTokens = &in
	}
	if resp.Usage.CompletionTokens > 0 {
		o := resp.Usage.CompletionTokens
		entry.OutputTokens = &o
	}
	if p.pricing != nil {
		cost := (float64(resp.Usage.PromptTokens)*p.pricing.Input + float64(resp.Usage.CompletionTokens)*p.pricing.Output) / 1_000_000.0
		if cost > 0 {
			entry.CostUSD = &cost
		}
	}
	entry.OK = true
	return strings.TrimSpace(out), nil
}

// doRequest performs the HTTP call and returns the assistant message
// content plus the parsed response (for usage accounting). A 429 maps to
// ErrRateLimit. JSON mode that trips a response_format rejection retries
// once without the field so providers lacking json_object still work.
func (p *openaiCompatProvider) doRequest(ctx context.Context, prompt string, jsonMode bool) (string, oaiChatResponse, error) {
	reqBody := oaiChatRequest{
		Model:       p.model,
		Messages:    []oaiChatMessage{{Role: "user", Content: prompt}},
		Temperature: 0.3,
	}
	if jsonMode {
		reqBody.ResponseFormat = &oaiResponseFmt{Type: "json_object"}
	}

	content, resp, status, body, err := p.postChat(ctx, reqBody)
	if err != nil {
		return "", oaiChatResponse{}, err
	}

	// Defensive seam: a provider that doesn't support json_object 400s
	// complaining about response_format. Retry once without it rather
	// than stranding the whole pass — the envelope prompts already
	// constrain shape, so plain text is an acceptable fallback.
	if status == http.StatusBadRequest && jsonMode && mentionsResponseFormat(resp, body) {
		logMsg("llm", "%s: endpoint rejected response_format=json_object — retrying without structured-output flag", p.providerName)
		reqBody.ResponseFormat = nil
		content, resp, status, body, err = p.postChat(ctx, reqBody)
		if err != nil {
			return "", oaiChatResponse{}, err
		}
	}

	if status == http.StatusTooManyRequests {
		return "", oaiChatResponse{}, ErrRateLimit
	}
	if status >= 400 {
		msg := strings.TrimSpace(body)
		if resp.Error != nil && resp.Error.Message != "" {
			msg = resp.Error.Message
		}
		return "", oaiChatResponse{}, fmt.Errorf("%s http %d: %s", p.providerName, status, tailLines(msg, 5))
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return "", oaiChatResponse{}, fmt.Errorf("%s: %s", p.providerName, resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return "", oaiChatResponse{}, fmt.Errorf("%s: response had no choices (body=%s)", p.providerName, tailLines(body, 3))
	}
	return content, resp, nil
}

// postChat marshals and sends one request, returning the decoded content,
// the parsed response, the HTTP status, the raw body (for error tails),
// and any transport/decoding error. No client-level timeout: every
// caller bounds the request via context (absorb-pass2 25m, dream 20m,
// …), matching the ollama provider's contract.
func (p *openaiCompatProvider) postChat(ctx context.Context, reqBody oaiChatRequest) (string, oaiChatResponse, int, string, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", oaiChatResponse{}, 0, "", fmt.Errorf("marshal request: %w", err)
	}
	url := p.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", oaiChatResponse{}, 0, "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	httpResp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", oaiChatResponse{}, 0, "", fmt.Errorf("%s http: %w", p.providerName, err)
	}
	defer httpResp.Body.Close()

	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return "", oaiChatResponse{}, httpResp.StatusCode, "", fmt.Errorf("read response: %w", err)
	}

	var cr oaiChatResponse
	// A non-JSON body is possible on gateway errors (HTML 502, etc.); the
	// caller surfaces the raw body tail in that case, so a decode failure
	// is not fatal here.
	_ = json.Unmarshal(data, &cr)
	content := ""
	if len(cr.Choices) > 0 {
		content = cr.Choices[0].Message.Content
	}
	return content, cr, httpResp.StatusCode, string(data), nil
}

// mentionsResponseFormat reports whether an error body is complaining
// about the response_format field, so generate can retry without it.
func mentionsResponseFormat(resp oaiChatResponse, body string) bool {
	hay := strings.ToLower(body)
	if resp.Error != nil {
		hay += " " + strings.ToLower(resp.Error.Message) + " " + strings.ToLower(resp.Error.Code)
	}
	return strings.Contains(hay, "response_format") || strings.Contains(hay, "json_object") || strings.Contains(hay, "json mode")
}
