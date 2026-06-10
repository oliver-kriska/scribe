package main

import (
	"context"
	"fmt"
	"time"
)

// ollama_check.go is the Phase 5 onboarding helper that pokes the
// local Ollama server during `scribe init --provider ollama` and
// `scribe doctor`. The actual /api/tags and /api/pull plumbing lives
// in ollamaProvider; this file just wraps it in a friendly UX.

// ensureOllamaReadyOrHint probes the local Ollama server and, when
// the configured model is missing, pulls it. Failures are logged as
// hints — they don't fail init.
func ensureOllamaReadyOrHint(model string) {
	if model == "" {
		model = ollamaRecommendedModel
	}
	fmt.Printf("\nProbing Ollama (model=%s)...\n", model)
	p := &ollamaProvider{baseURL: defaultOllamaURL, model: model}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	present, err := p.listedModels(ctx)
	if err != nil {
		fmt.Printf("  (could not reach Ollama at http://localhost:11434 — install with `brew install ollama` and run `ollama serve`)\n")
		fmt.Printf("  details: %v\n", err)
		return
	}
	if p.modelListContains(present, model) {
		fmt.Printf("  Ollama OK — %s is already pulled.\n", model)
		return
	}
	fmt.Printf("  %s is not pulled yet. Pulling now (this can take several minutes)...\n", model)
	pullCtx, pullCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer pullCancel()
	if err := p.pull(pullCtx); err != nil {
		fmt.Printf("  pull failed: %v\n", err)
		fmt.Printf("  hint: run `ollama pull %s` manually once Ollama is up\n", model)
		return
	}
	fmt.Printf("  pulled %s.\n", model)
}
