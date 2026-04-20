package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewLLMProviderRouting(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"anthropic", "anthropic/haiku"},
		{"", "anthropic/haiku"}, // empty defaults to anthropic
		{"ANTHROPIC", "anthropic/haiku"},
		{"ollama", "ollama/haiku"},
		{"Ollama", "ollama/haiku"},
		{"unknown-backend", "anthropic/haiku"}, // graceful fallback
	}
	for _, tc := range cases {
		got := newLLMProvider(tc.in, "haiku", "http://localhost:11434").Name()
		if got != tc.want {
			t.Errorf("newLLMProvider(%q).Name() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOllamaProviderGenerate(t *testing.T) {
	var gotRequest ollamaRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			http.Error(w, "wrong path", 404)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotRequest)
		resp := ollamaResponse{Response: "  generated paragraph  \n", Done: true}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := &ollamaProvider{baseURL: srv.URL, model: "llama3.2:3b"}
	// Skip the lifecycle check for this test — we're exercising the generate
	// path in isolation. The ensureReady path has dedicated tests below.
	ollamaReadyMu.Lock()
	ollamaReadyCache[p.baseURL+"|"+p.model] = true
	ollamaReadyMu.Unlock()
	out, err := p.Generate(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if out != "generated paragraph" {
		t.Errorf("response not trimmed: %q", out)
	}
	if gotRequest.Model != "llama3.2:3b" {
		t.Errorf("model = %q, want llama3.2:3b", gotRequest.Model)
	}
	if gotRequest.Stream {
		t.Error("stream should be false")
	}
}

func TestOllamaProviderErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"model not found"}`))
	}))
	defer srv.Close()

	p := &ollamaProvider{baseURL: srv.URL, model: "nope"}
	_, err := p.Generate(context.Background(), "hi")
	if err == nil {
		t.Fatal("expected error on 5xx")
	}
	if !strings.Contains(err.Error(), "model not found") && !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error to include upstream message, got %v", err)
	}
}

func TestOllamaEnsureReadyPullsMissingModel(t *testing.T) {
	// Reset cache so the test's server is consulted.
	ollamaReadyMu.Lock()
	ollamaReadyCache = map[string]bool{}
	ollamaReadyMu.Unlock()

	var tagsHits, pullHits, genHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			tagsHits++
			// First hit: empty list. Second hit (shouldn't happen given cache) would also be empty.
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{}})
		case "/api/pull":
			pullHits++
			// Stream two status lines.
			w.(http.Flusher).Flush()
			_, _ = w.Write([]byte(`{"status":"pulling manifest"}` + "\n"))
			_, _ = w.Write([]byte(`{"status":"success"}` + "\n"))
		case "/api/generate":
			genHits++
			_ = json.NewEncoder(w).Encode(ollamaResponse{Response: "ok", Done: true})
		default:
			http.Error(w, "unknown path", 404)
		}
	}))
	defer srv.Close()

	p := &ollamaProvider{baseURL: srv.URL, model: "gemma3:4b"}
	if _, err := p.Generate(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if tagsHits < 1 {
		t.Error("expected /api/tags to be called")
	}
	if pullHits != 1 {
		t.Errorf("expected exactly one /api/pull, got %d", pullHits)
	}
	if genHits != 1 {
		t.Errorf("expected exactly one /api/generate, got %d", genHits)
	}

	// Second Generate on same provider must not re-pull (cache hit).
	if _, err := p.Generate(context.Background(), "hi again"); err != nil {
		t.Fatal(err)
	}
	if pullHits != 1 {
		t.Errorf("cache did not prevent re-pull: got %d", pullHits)
	}
}

func TestOllamaSkipsPullWhenModelPresent(t *testing.T) {
	ollamaReadyMu.Lock()
	ollamaReadyCache = map[string]bool{}
	ollamaReadyMu.Unlock()

	var pullHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]string{
					{"name": "llama3.2:3b", "model": "llama3.2:3b"},
				},
			})
		case "/api/pull":
			pullHits++
			_, _ = w.Write([]byte(`{"status":"success"}`))
		case "/api/generate":
			_ = json.NewEncoder(w).Encode(ollamaResponse{Response: "ok", Done: true})
		}
	}))
	defer srv.Close()

	p := &ollamaProvider{baseURL: srv.URL, model: "llama3.2:3b"}
	if _, err := p.Generate(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if pullHits != 0 {
		t.Errorf("should not pull when model present, got %d hits", pullHits)
	}
}

func TestOllamaReportsUnreachableServer(t *testing.T) {
	ollamaReadyMu.Lock()
	ollamaReadyCache = map[string]bool{}
	ollamaReadyMu.Unlock()

	// Port 1 is reserved by IANA and nothing listens on it.
	p := &ollamaProvider{baseURL: "http://127.0.0.1:1", model: "gemma3:4b"}
	_, err := p.Generate(context.Background(), "hi")
	if err == nil {
		t.Fatal("expected error when server unreachable")
	}
	if !strings.Contains(err.Error(), "brew install ollama") {
		t.Errorf("error should suggest ollama install: %v", err)
	}
}

func TestModelListContains(t *testing.T) {
	o := &ollamaProvider{}
	cases := []struct {
		list []string
		want string
		ok   bool
	}{
		{[]string{"llama3.2:3b"}, "llama3.2:3b", true},
		{[]string{"llama3.2:latest"}, "llama3.2", true}, // tagless match → any tag
		{[]string{"llama3.2:3b"}, "llama3.2:7b", false}, // different tag
		{[]string{"gemma3:4b"}, "llama3.2:3b", false},   // different family
		{[]string{"gemma3:4b", "qwen3:4b"}, "qwen3:4b", true},
	}
	for _, tc := range cases {
		if got := o.modelListContains(tc.list, tc.want); got != tc.ok {
			t.Errorf("modelListContains(%v, %q) = %v, want %v", tc.list, tc.want, got, tc.ok)
		}
	}
}

func TestSanitizeContextParagraph(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Here is the paragraph: Thread on RAG.", "Thread on RAG."},
		{"```markdown\nthread on X\n```", "thread on X"},
		{"Context: body\n", "body"},
		{"\n\nline one\nline two\n", "line one line two"},
		{"para one\n\npara two", "para one\n\npara two"},
	}
	for _, tc := range cases {
		got := sanitizeContextParagraph(tc.in)
		if got != tc.want {
			t.Errorf("sanitize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
