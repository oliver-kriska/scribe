//go:build integration

// Integration test for Phase 4A: drives runFactsPass against a live
// Ollama server (gemma3:4b) and verifies the merged facts file is
// produced with valid JSON facts.
//
// Skipped from `go test ./...`. Run explicitly:
//
//	go test ./cmd/scribe/ -tags integration -run TestRunFactsPass_Ollama -v
//
// Requires Ollama at localhost:11434 with gemma3:4b pulled. The test
// fails fast with a hint if either is missing.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const ragChunk = `Retrieval-augmented generation (RAG) combines a language model with an
external document store. At inference time the system retrieves relevant
passages and conditions the model on them. This reduces hallucination and
lets the model answer questions about documents that postdate its training.

The architecture has three stages. First, an embedding model converts
documents into 1024-dimensional vectors. Second, a vector database indexes
those embeddings using HNSW for fast approximate nearest-neighbor search.
Third, a generator model — typically GPT-4 or Claude Sonnet — receives the
top 5 retrieved passages alongside the user query.

Lewis et al. (2020) introduced the original RAG framework. Their system
achieved 56.8% accuracy on Natural Questions, compared to 44.5% for the
closed-book baseline.
`

func TestRunFactsPass_Ollama(t *testing.T) {
	requireOllama(t, "gemma3:4b")

	root := t.TempDir()
	rawFile := filepath.Join(root, "raw", "articles", "rag-basics.md")
	if err := os.MkdirAll(filepath.Dir(rawFile), 0o755); err != nil {
		t.Fatal(err)
	}
	rawBody := "---\ntitle: \"RAG Basics\"\n---\n\n# Retrieval-Augmented Generation Basics\n\n" + ragChunk
	if err := os.WriteFile(rawFile, []byte(rawBody), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pre-create the chunk file the way runPass1Chaptered would. Two
	// chapters so the parallelism path exercises real concurrency.
	chunksDir := filepath.Join(root, "output", "absorb-chunks", "rag-basics")
	if err := os.MkdirAll(chunksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runs := []chapterRun{
		{
			index:   0,
			chunk:   Chunk{Title: "RAG Basics", Body: ragChunk},
			chunkMD: filepath.Join(chunksDir, "00-rag-basics.md"),
		},
		{
			index:   1,
			chunk:   Chunk{Title: "RAG Basics Part 2", Body: ragChunk},
			chunkMD: filepath.Join(chunksDir, "01-rag-basics-part-2.md"),
		},
	}
	for _, r := range runs {
		if err := os.WriteFile(r.chunkMD, []byte("# "+r.chunk.Title+"\n\n"+r.chunk.Body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	trueV := true
	cfg := AbsorbConfig{
		AtomicFacts:     &trueV,
		ChapterAware:    &trueV,
		FactsProvider:   "ollama",
		FactsModel:      "gemma3:4b",
		FactsTimeoutMin: 4,
		Pass1Model:      "haiku",
		ChapterParallel: 2,
		Contextualize: ContextualizeConfig{
			OllamaURL: "http://localhost:11434",
		},
	}

	s := &SyncCmd{}
	ctx := context.Background()
	merged, err := s.runFactsPass(ctx, root, rawFile, "rag-basics.md", runs, cfg)
	if err != nil {
		t.Fatalf("runFactsPass failed: %v", err)
	}
	if merged == nil {
		t.Fatal("expected merged facts, got nil")
	}
	if len(merged.Chapters) != 2 {
		t.Errorf("expected 2 chapter entries, got %d", len(merged.Chapters))
	}
	if len(merged.Facts) < 5 {
		t.Errorf("expected at least 5 facts, got %d", len(merged.Facts))
	}

	// Per-chunk file written.
	for i := range runs {
		stem := filepath.Join(root, "output", "absorb-facts", "rag-basics")
		entries, err := os.ReadDir(stem)
		if err != nil {
			t.Fatalf("read facts dir: %v", err)
		}
		if len(entries) < i+1 {
			t.Errorf("expected at least %d per-chunk files, got %d", i+1, len(entries))
		}
	}

	// Merged file written.
	mergedPath := filepath.Join(root, "output", "facts", "rag-basics.json")
	if _, err := os.Stat(mergedPath); err != nil {
		t.Errorf("merged facts file missing: %v", err)
	}

	// All facts have one of the allowed types.
	allowed := map[string]bool{
		"definition": true, "claim": true, "numeric": true,
		"decision": true, "citation": true,
	}
	bad := 0
	for _, f := range merged.Facts {
		if !allowed[f.Type] {
			bad++
			t.Logf("unrecognized type: %q on fact %s (%s)", f.Type, f.ID, f.Claim)
		}
	}
	if bad > 0 {
		t.Errorf("%d/%d facts had unrecognized type values", bad, len(merged.Facts))
	}

	// IDs are chapter-prefixed.
	for _, f := range merged.Facts {
		if len(f.ID) < 4 || f.ID[0] != 'c' {
			t.Errorf("expected chapter-prefixed ID, got %q", f.ID)
		}
	}

	t.Logf("✓ ollama facts pass: %d chapters, %d facts merged", len(merged.Chapters), len(merged.Facts))
	for i, f := range merged.Facts {
		if i >= 3 {
			break
		}
		t.Logf("  [%s, %s] %s", f.ID, f.Type, f.Claim)
	}
}

// requireOllama skips the test when Ollama isn't reachable or the
// requested model isn't pulled. This keeps the integration test
// runnable on a fresh machine without hangs.
func requireOllama(t *testing.T, model string) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://localhost:11434/api/tags")
	if err != nil {
		t.Skipf("ollama unreachable: %v (start with `ollama serve`)", err)
	}
	defer resp.Body.Close()
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		t.Skipf("decode /api/tags: %v", err)
	}
	for _, m := range tags.Models {
		if m.Name == model || m.Name == model+":latest" {
			return
		}
	}
	t.Skipf("model %s not pulled; run `ollama pull %s`", model, model)
}
