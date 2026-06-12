// sync_absorb_dense_test.go — driver tests for absorbDenseTwoPass (and
// the absorbRaw checkpoint loop above it) running against the stub LLM
// harness from llm_stub_test.go. These are the tests the
// `nolint:gocognit` marker on absorbDenseTwoPass named as the
// precondition for decomposing the function: they pin the concurrent
// pipeline's observable semantics — fan-out caps, the stop-the-world
// rate-limit/budget signals, the corrective retry, per-label write
// serialization, and partial-progress behavior — without a network or
// a real LLM.
package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// denseAbsorbYAML is the scribe.yaml used by the json-mode dense tests.
// chapter_aware off keeps pass-1 on the single-shot path (the chaptered
// fan-out has its own knobs); pass2_mode json routes pass-2 through the
// provider seam instead of `claude -p`.
const denseAbsorbYAML = `kb_name: stubkb
absorb:
  chapter_aware: false
  pass2_mode: json
  pass2_provider: anthropic
  pass2_parallel: 1
  pass1_timeout_min: 1
  pass2_timeout_min: 1
  single_pass_timeout_min: 1
  contextualize:
    enabled: false
`

// writeDenseRaw drops a raw article fixture and returns (path, name).
// The body is irrelevant to absorbDenseTwoPass itself (density gating
// happens in the caller) but carries a unique marker so tests can
// assert the body was inlined into prompts.
func writeDenseRaw(t *testing.T, root, name, marker string) (string, string) {
	t.Helper()
	body := `---
title: Stub Article
source_url: https://example.invalid/a
domain: general
density: dense
---

# Stub Article

` + marker + ` body text for the dense absorb driver test.
`
	path := filepath.Join(root, "raw", "articles", name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write raw article: %v", err)
	}
	return path, name
}

func entity(label, oneLine string) absorbEntity {
	return absorbEntity{Label: label, Type: "concept", OneLine: oneLine, KeyClaims: []string{"claim for " + label}}
}

// withParallel returns denseAbsorbYAML with pass2_parallel swapped.
func withParallel(t *testing.T, n string) string {
	t.Helper()
	out := strings.Replace(denseAbsorbYAML, "pass2_parallel: 1", "pass2_parallel: "+n, 1)
	if out == denseAbsorbYAML {
		t.Fatal("pass2_parallel knob not found in fixture yaml")
	}
	return out
}

// TestAbsorbDenseTwoPass_JSONMode_HappyPath: pass-1 plans two entities,
// pass-2 writes one wiki page per entity through applyWikiActions. The
// stub also proves the prompt plumbing: entity labels, the inlined raw
// body, the inlined plan JSON, and the num_ctx context tag all reach
// the provider.
func TestAbsorbDenseTwoPass_JSONMode_HappyPath(t *testing.T) {
	root := stubHarnessKB(t, denseAbsorbYAML)
	rawFile, rawName := writeDenseRaw(t, root, "2026-06-01-stub.md", "MARKER-HAPPY")

	stub := &stubJSONLLM{}
	stub.Rules = []*stubRule{
		{MatchOp: "absorb-pass1-whole", Reply: planJSON(t, rawFile, "general", entity("Alpha", "first"), entity("Beta", "second"))},
		{MatchOp: "absorb-pass2", MatchPrompt: "- Label: Alpha", Reply: envelopeJSON(t, 1, "Alpha", "wiki/alpha.md", "Alpha body.")},
		{MatchOp: "absorb-pass2", MatchPrompt: "- Label: Beta", Reply: envelopeJSON(t, 1, "Beta", "wiki/beta.md", "Beta body.")},
	}
	installStubLLM(t, stub)

	sc := &SyncCmd{Model: "sonnet"}
	if err := sc.absorbDenseTwoPass(root, rawFile, rawName); err != nil {
		t.Fatalf("absorbDenseTwoPass: %v", err)
	}

	for page, want := range map[string]string{
		"wiki/alpha.md": "Alpha body.",
		"wiki/beta.md":  "Beta body.",
	} {
		data, err := os.ReadFile(filepath.Join(root, page))
		if err != nil {
			t.Fatalf("expected %s to exist: %v", page, err)
		}
		if !strings.Contains(string(data), want) {
			t.Errorf("%s missing body %q:\n%s", page, want, data)
		}
	}

	// Plan checkpoint written to output/absorb-plans/<stem>.json.
	if _, err := os.Stat(filepath.Join(root, "output", "absorb-plans", "2026-06-01-stub.json")); err != nil {
		t.Errorf("plan file not checkpointed: %v", err)
	}

	if got := len(stub.CallsWithOp("absorb-pass1-whole")); got != 1 {
		t.Errorf("pass1 calls = %d, want 1", got)
	}
	pass2 := stub.CallsWithOp("absorb-pass2")
	if len(pass2) != 2 {
		t.Fatalf("pass2 calls = %d, want 2", len(pass2))
	}
	for _, c := range pass2 {
		if !c.JSONMode {
			t.Errorf("pass2 call did not use GenerateJSON (jsonModeProvider dispatch broken)")
		}
		if !strings.Contains(c.Prompt, "MARKER-HAPPY") {
			t.Errorf("pass2 prompt missing inlined raw body")
		}
		if !strings.Contains(c.Prompt, `"Stub Source"`) {
			t.Errorf("pass2 prompt missing inlined plan JSON")
		}
		if c.NumCtx <= 0 {
			t.Errorf("pass2 call num_ctx = %d, want >0 (withOllamaNumCtx tag lost)", c.NumCtx)
		}
	}
}

// TestAbsorbDenseTwoPass_RateLimitStopsTheWorld: one rate-limited
// pass-2 entity cancels the errgroup context, so queued entities exit
// before ever reaching the provider, and ErrRateLimit surfaces to the
// caller so the sync loop can stop cleanly and resume next run.
func TestAbsorbDenseTwoPass_RateLimitStopsTheWorld(t *testing.T) {
	root := stubHarnessKB(t, denseAbsorbYAML) // pass2_parallel: 1 → deterministic order
	rawFile, rawName := writeDenseRaw(t, root, "2026-06-01-rl.md", "MARKER-RL")

	stub := &stubLLM{}
	stub.Rules = []*stubRule{
		{MatchOp: "absorb-pass1-whole", Reply: planJSON(t, rawFile, "general", entity("Alpha", "a"), entity("Beta", "b"), entity("Gamma", "c"))},
		{MatchOp: "absorb-pass2", MatchPrompt: "- Label: Alpha", Err: ErrRateLimit},
		// Beta / Gamma rules intentionally absent: they must never be asked.
	}
	installStubLLM(t, stub)

	sc := &SyncCmd{Model: "sonnet"}
	err := sc.absorbDenseTwoPass(root, rawFile, rawName)
	if !errors.Is(err, ErrRateLimit) {
		t.Fatalf("err = %v, want ErrRateLimit", err)
	}
	if got := len(stub.CallsWithOp("absorb-pass2")); got != 1 {
		t.Errorf("pass2 calls = %d, want 1 — rate limit must stop the world before Beta/Gamma run", got)
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "alpha.md")); !os.IsNotExist(err) {
		t.Errorf("no wiki page should have been written, stat err = %v", err)
	}
}

// TestAbsorbDenseTwoPass_BudgetExhaustedStopsDistinctly: the daily
// budget ceiling uses the same stop-the-world shape as rate limit but
// must keep its own identity at the function boundary so the caller
// can log it accurately (json mode tracks it separately).
func TestAbsorbDenseTwoPass_BudgetExhaustedStopsDistinctly(t *testing.T) {
	root := stubHarnessKB(t, denseAbsorbYAML)
	rawFile, rawName := writeDenseRaw(t, root, "2026-06-01-budget.md", "MARKER-BUDGET")

	stub := &stubLLM{}
	stub.Rules = []*stubRule{
		{MatchOp: "absorb-pass1-whole", Reply: planJSON(t, rawFile, "general", entity("Alpha", "a"), entity("Beta", "b"))},
		{MatchOp: "absorb-pass2", MatchPrompt: "- Label: Alpha", Err: ErrDailyBudgetExhausted},
	}
	installStubLLM(t, stub)

	sc := &SyncCmd{Model: "sonnet"}
	err := sc.absorbDenseTwoPass(root, rawFile, rawName)
	if !errors.Is(err, ErrDailyBudgetExhausted) {
		t.Fatalf("err = %v, want ErrDailyBudgetExhausted", err)
	}
	if errors.Is(err, ErrRateLimit) {
		t.Errorf("budget exhaustion must not be reported as a rate limit in json mode")
	}
	if got := len(stub.CallsWithOp("absorb-pass2")); got != 1 {
		t.Errorf("pass2 calls = %d, want 1", got)
	}
}

// TestAbsorbDenseTwoPass_CorrectiveRetryRecoversMalformedEnvelope: a
// first pass-2 response that isn't a JSON envelope triggers exactly one
// corrective retry with the sharper instruction appended; a clean retry
// response is applied normally.
func TestAbsorbDenseTwoPass_CorrectiveRetryRecoversMalformedEnvelope(t *testing.T) {
	root := stubHarnessKB(t, denseAbsorbYAML)
	rawFile, rawName := writeDenseRaw(t, root, "2026-06-01-retry.md", "MARKER-RETRY")

	stub := &stubLLM{}
	stub.Rules = []*stubRule{
		{MatchOp: "absorb-pass1-whole", Reply: planJSON(t, rawFile, "general", entity("Alpha", "a"))},
		{MatchOp: "absorb-pass2", Once: true, Reply: "sorry, here is some prose instead of an envelope"},
		{MatchOp: "absorb-pass2", MatchPrompt: "## CORRECTION", Reply: envelopeJSON(t, 1, "Alpha", "wiki/alpha.md", "Alpha body.")},
	}
	installStubLLM(t, stub)

	sc := &SyncCmd{Model: "sonnet"}
	if err := sc.absorbDenseTwoPass(root, rawFile, rawName); err != nil {
		t.Fatalf("absorbDenseTwoPass: %v", err)
	}

	pass2 := stub.CallsWithOp("absorb-pass2")
	if len(pass2) != 2 {
		t.Fatalf("pass2 calls = %d, want 2 (original + corrective retry)", len(pass2))
	}
	if strings.Contains(pass2[0].Prompt, "## CORRECTION") {
		t.Errorf("first pass2 attempt must not carry the corrective suffix")
	}
	if !strings.Contains(pass2[1].Prompt, "## CORRECTION") {
		t.Errorf("retry prompt missing the corrective instruction")
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "alpha.md")); err != nil {
		t.Errorf("retry result not applied: %v", err)
	}
}

// TestAbsorbDenseTwoPass_RetryFailurePreservesPartialProgress: an
// entity whose retry also fails is dropped without failing the absorb —
// the other entities still land. Partial absorb beats losing the source.
func TestAbsorbDenseTwoPass_RetryFailurePreservesPartialProgress(t *testing.T) {
	root := stubHarnessKB(t, denseAbsorbYAML) // parallel: 1 → deterministic
	rawFile, rawName := writeDenseRaw(t, root, "2026-06-01-partial.md", "MARKER-PARTIAL")

	stub := &stubLLM{}
	stub.Rules = []*stubRule{
		{MatchOp: "absorb-pass1-whole", Reply: planJSON(t, rawFile, "general", entity("Alpha", "a"), entity("Beta", "b"))},
		{MatchOp: "absorb-pass2", MatchPrompt: "- Label: Alpha", Reply: "still not an envelope"}, // matches retry too
		{MatchOp: "absorb-pass2", MatchPrompt: "- Label: Beta", Reply: envelopeJSON(t, 1, "Beta", "wiki/beta.md", "Beta body.")},
	}
	installStubLLM(t, stub)

	sc := &SyncCmd{Model: "sonnet"}
	if err := sc.absorbDenseTwoPass(root, rawFile, rawName); err != nil {
		t.Fatalf("absorbDenseTwoPass: %v (per-entity failure must not abort the absorb)", err)
	}
	if got := len(stub.CallsWithOp("absorb-pass2")); got != 3 {
		t.Errorf("pass2 calls = %d, want 3 (Alpha twice, Beta once)", got)
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "alpha.md")); !os.IsNotExist(err) {
		t.Errorf("alpha page should not exist, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "beta.md")); err != nil {
		t.Errorf("beta page missing — partial progress lost: %v", err)
	}
}

// TestAbsorbDenseTwoPass_EmptyPlanFallsBackToSinglePass: a pass-1 plan
// with zero entities reroutes the article through the single-pass
// absorb instead of silently doing nothing.
func TestAbsorbDenseTwoPass_EmptyPlanFallsBackToSinglePass(t *testing.T) {
	root := stubHarnessKB(t, denseAbsorbYAML)
	rawFile, rawName := writeDenseRaw(t, root, "2026-06-01-empty.md", "MARKER-EMPTY")

	stub := &stubLLM{}
	stub.Rules = []*stubRule{
		{MatchOp: "absorb-pass1-whole", Reply: planJSON(t, rawFile, "general")},
		{MatchOp: "absorb-single", Reply: envelopeJSON(t, 1, "Single Note", "wiki/single-note.md", "Single body.")},
	}
	installStubLLM(t, stub)

	sc := &SyncCmd{Model: "sonnet"}
	if err := sc.absorbDenseTwoPass(root, rawFile, rawName); err != nil {
		t.Fatalf("absorbDenseTwoPass: %v", err)
	}
	if got := len(stub.CallsWithOp("absorb-pass2")); got != 0 {
		t.Errorf("pass2 calls = %d, want 0", got)
	}
	if got := len(stub.CallsWithOp("absorb-single")); got != 1 {
		t.Errorf("single-pass calls = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "single-note.md")); err != nil {
		t.Errorf("single-pass page missing: %v", err)
	}
}

// TestAbsorbDenseTwoPass_Pass1RateLimitPropagates: a rate limit during
// pass-1 aborts before any pass-2 work and keeps its sentinel identity.
func TestAbsorbDenseTwoPass_Pass1RateLimitPropagates(t *testing.T) {
	root := stubHarnessKB(t, denseAbsorbYAML)
	rawFile, rawName := writeDenseRaw(t, root, "2026-06-01-p1rl.md", "MARKER-P1RL")

	stub := &stubLLM{}
	stub.Rules = []*stubRule{{MatchOp: "absorb-pass1-whole", Err: ErrRateLimit}}
	installStubLLM(t, stub)

	sc := &SyncCmd{Model: "sonnet"}
	err := sc.absorbDenseTwoPass(root, rawFile, rawName)
	if !errors.Is(err, ErrRateLimit) {
		t.Fatalf("err = %v, want ErrRateLimit", err)
	}
	if got := len(stub.CallsWithOp("absorb-pass2")); got != 0 {
		t.Errorf("pass2 calls = %d, want 0", got)
	}
}

// TestAbsorbDenseTwoPass_Pass1GarbageOutputErrors: pass-1 output with
// no JSON object is a hard error (nothing to plan from), and pass-2
// never runs.
func TestAbsorbDenseTwoPass_Pass1GarbageOutputErrors(t *testing.T) {
	root := stubHarnessKB(t, denseAbsorbYAML)
	rawFile, rawName := writeDenseRaw(t, root, "2026-06-01-p1bad.md", "MARKER-P1BAD")

	stub := &stubLLM{DefaultReply: "no json object anywhere in this reply"}
	installStubLLM(t, stub)

	sc := &SyncCmd{Model: "sonnet"}
	err := sc.absorbDenseTwoPass(root, rawFile, rawName)
	if err == nil || !strings.Contains(err.Error(), "no JSON object") {
		t.Fatalf("err = %v, want pass1 no-JSON error", err)
	}
	if got := len(stub.CallsWithOp("absorb-pass2")); got != 0 {
		t.Errorf("pass2 calls = %d, want 0", got)
	}
}

// TestAbsorbDenseTwoPass_ParallelCapRespected: with pass2_parallel=2
// and four entities, exactly two pass-2 calls run concurrently — the
// rendezvous in the stub proves the fan-out actually overlaps, and the
// high-water mark proves SetLimit caps it.
func TestAbsorbDenseTwoPass_ParallelCapRespected(t *testing.T) {
	root := stubHarnessKB(t, withParallel(t, "2"))
	rawFile, rawName := writeDenseRaw(t, root, "2026-06-01-par.md", "MARKER-PAR")

	stub := &stubLLM{
		RendezvousOp: "absorb-pass2",
		Rendezvous:   2,
	}
	stub.Rules = []*stubRule{
		{MatchOp: "absorb-pass1-whole", Reply: planJSON(t, rawFile, "general",
			entity("Alpha", "a"), entity("Beta", "b"), entity("Gamma", "c"), entity("Delta", "d"))},
		{MatchOp: "absorb-pass2", MatchPrompt: "- Label: Alpha", Reply: envelopeJSON(t, 1, "Alpha", "wiki/alpha.md", "A.")},
		{MatchOp: "absorb-pass2", MatchPrompt: "- Label: Beta", Reply: envelopeJSON(t, 1, "Beta", "wiki/beta.md", "B.")},
		{MatchOp: "absorb-pass2", MatchPrompt: "- Label: Gamma", Reply: envelopeJSON(t, 1, "Gamma", "wiki/gamma.md", "G.")},
		{MatchOp: "absorb-pass2", MatchPrompt: "- Label: Delta", Reply: envelopeJSON(t, 1, "Delta", "wiki/delta.md", "D.")},
	}
	installStubLLM(t, stub)

	sc := &SyncCmd{Model: "sonnet"}
	if err := sc.absorbDenseTwoPass(root, rawFile, rawName); err != nil {
		t.Fatalf("absorbDenseTwoPass: %v", err)
	}
	if got := len(stub.CallsWithOp("absorb-pass2")); got != 4 {
		t.Errorf("pass2 calls = %d, want 4", got)
	}
	if got := stub.MaxInFlight(); got != 2 {
		t.Errorf("max in-flight = %d, want exactly 2 (SetLimit cap with real overlap)", got)
	}
	for _, page := range []string{"alpha", "beta", "gamma", "delta"} {
		if _, err := os.Stat(filepath.Join(root, "wiki", page+".md")); err != nil {
			t.Errorf("missing %s.md: %v", page, err)
		}
	}
}

// TestAbsorbDenseTwoPass_SameLabelEntitiesSerialize: two entities with
// the same label target the same wiki page, so the per-label mutex must
// serialize their provider calls even when the parallel budget would
// allow overlap. The Delay gives a broken lock a window to interleave;
// ActiveAtEntry proves no second same-label call entered while the
// first was in flight.
func TestAbsorbDenseTwoPass_SameLabelEntitiesSerialize(t *testing.T) {
	root := stubHarnessKB(t, withParallel(t, "2"))
	rawFile, rawName := writeDenseRaw(t, root, "2026-06-01-lock.md", "MARKER-LOCK")

	stub := &stubLLM{Delay: 50 * time.Millisecond}
	stub.Rules = []*stubRule{
		{MatchOp: "absorb-pass1-whole", Reply: planJSON(t, rawFile, "general",
			entity("Alpha", "first occurrence"), entity("Alpha", "variant occurrence"))},
		{MatchOp: "absorb-pass2", MatchPrompt: "- Label: Alpha", Reply: envelopeJSON(t, 1, "Alpha", "wiki/alpha.md", "Alpha body.")},
	}
	installStubLLM(t, stub)

	sc := &SyncCmd{Model: "sonnet"}
	if err := sc.absorbDenseTwoPass(root, rawFile, rawName); err != nil {
		t.Fatalf("absorbDenseTwoPass: %v", err)
	}
	pass2 := stub.CallsWithOp("absorb-pass2")
	if len(pass2) != 2 {
		t.Fatalf("pass2 calls = %d, want 2", len(pass2))
	}
	for i, c := range pass2 {
		for _, active := range c.ActiveAtEntry {
			if strings.Contains(active, "- Label: Alpha") {
				t.Errorf("pass2 call %d entered while another Alpha call was in flight — per-label lock broken", i)
			}
		}
	}
}

// TestAbsorbDenseTwoPass_ToolsModeUsesRunClaude: with pass2_mode=tools
// pass-2 goes through the runClaude seam — one call per entity carrying
// the sync model, the file-mutation tool set, and the entity-focused
// prompt. The wiki write happens inside claude in this mode, so only
// the call plumbing is asserted.
func TestAbsorbDenseTwoPass_ToolsModeUsesRunClaude(t *testing.T) {
	yaml := strings.Replace(denseAbsorbYAML, "pass2_mode: json", "pass2_mode: tools", 1)
	root := stubHarnessKB(t, yaml)
	rawFile, rawName := writeDenseRaw(t, root, "2026-06-01-tools.md", "MARKER-TOOLS")

	stub := &stubLLM{}
	stub.Rules = []*stubRule{
		{MatchOp: "absorb-pass1-whole", Reply: planJSON(t, rawFile, "general", entity("Alpha", "a"), entity("Beta", "b"))},
	}
	installStubLLM(t, stub)
	claude := &stubClaude{Reply: "done"}
	installStubClaude(t, claude)

	sc := &SyncCmd{Model: "sonnet"}
	if err := sc.absorbDenseTwoPass(root, rawFile, rawName); err != nil {
		t.Fatalf("absorbDenseTwoPass: %v", err)
	}

	calls := claude.Calls()
	if len(calls) != 2 {
		t.Fatalf("runClaude calls = %d, want 2", len(calls))
	}
	var sawAlpha, sawBeta bool
	for _, c := range calls {
		if c.Model != "sonnet" {
			t.Errorf("model = %q, want sonnet (inherited from SyncCmd.Model)", c.Model)
		}
		if c.Root != root {
			t.Errorf("root = %q, want %q", c.Root, root)
		}
		if c.Op != "absorb-pass2" {
			t.Errorf("op = %q, want absorb-pass2", c.Op)
		}
		if !strings.Contains(strings.Join(c.Tools, ","), "Write") {
			t.Errorf("tools %v missing Write", c.Tools)
		}
		if strings.Contains(c.Prompt, "Entity to write: Alpha") {
			sawAlpha = true
		}
		if strings.Contains(c.Prompt, "Entity to write: Beta") {
			sawBeta = true
		}
	}
	if !sawAlpha || !sawBeta {
		t.Errorf("entity prompts incomplete: alpha=%v beta=%v", sawAlpha, sawBeta)
	}
}

// TestAbsorbDenseTwoPass_ToolsModeRateLimitStops: rate limit in tools
// mode is the same stop-the-world signal. Note the documented quirk
// preserved here: in tools mode ErrDailyBudgetExhausted shares the
// rateLimited flag, so both sentinels surface as ErrRateLimit — the
// caller stops cleanly either way.
func TestAbsorbDenseTwoPass_ToolsModeRateLimitStops(t *testing.T) {
	yaml := strings.Replace(denseAbsorbYAML, "pass2_mode: json", "pass2_mode: tools", 1)
	root := stubHarnessKB(t, yaml)
	rawFile, rawName := writeDenseRaw(t, root, "2026-06-01-toolsrl.md", "MARKER-TOOLSRL")

	stub := &stubLLM{}
	stub.Rules = []*stubRule{
		{MatchOp: "absorb-pass1-whole", Reply: planJSON(t, rawFile, "general", entity("Alpha", "a"), entity("Beta", "b"), entity("Gamma", "c"))},
	}
	installStubLLM(t, stub)
	claude := &stubClaude{Script: func(claudeCall) (string, error) {
		return "", ErrRateLimit
	}}
	installStubClaude(t, claude)

	sc := &SyncCmd{Model: "sonnet"}
	err := sc.absorbDenseTwoPass(root, rawFile, rawName)
	if !errors.Is(err, ErrRateLimit) {
		t.Fatalf("err = %v, want ErrRateLimit", err)
	}
	if got := len(claude.Calls()); got != 1 {
		t.Errorf("runClaude calls = %d, want 1 (stop-the-world)", got)
	}
}

// chapteredYAML flips the chaptered pass-1 fan-out on with the given
// chapter parallelism.
func chapteredYAML(t *testing.T, parallel string) string {
	t.Helper()
	out := strings.Replace(denseAbsorbYAML, "chapter_aware: false",
		"chapter_aware: true\n  chapter_parallel: "+parallel, 1)
	if out == denseAbsorbYAML {
		t.Fatal("chapter_aware knob not found in fixture yaml")
	}
	return out
}

// writeChapteredRaw writes a raw article with three >6KB H2 sections so
// chunkByHeadings yields three chunks (sections under MinChunkBytes
// would coalesce). Each section carries a unique marker the stub rules
// key on.
func writeChapteredRaw(t *testing.T, root, name string) (string, string) {
	t.Helper()
	filler := strings.Repeat("filler words for the chapter body keep flowing here. ", 170) // ~9KB
	body := "---\ntitle: Chaptered Stub\ndensity: dense\n---\n\n" +
		"## Heading One\n\nCH1-MARKER " + filler + "\n\n" +
		"## Heading Two\n\nCH2-MARKER " + filler + "\n\n" +
		"## Heading Three\n\nCH3-MARKER " + filler + "\n"
	path := filepath.Join(root, "raw", "articles", name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write chaptered raw: %v", err)
	}
	return path, name
}

// chapterPlanJSON renders the per-chapter plan document the chapter
// pass-1 prompt asks for.
func chapterPlanJSON(t *testing.T, rawFile, chapter string, entities ...absorbEntity) string {
	t.Helper()
	b, err := json.Marshal(chapterPlan{RawFile: rawFile, SourceTitle: "Chaptered Stub", Chapter: chapter, Domain: "general", Entities: entities})
	if err != nil {
		t.Fatalf("marshal chapter plan: %v", err)
	}
	return string(b)
}

// TestAbsorbDenseTwoPass_ChapteredPass1FansOutAndMerges: with a TOC-less
// but heading-rich article, pass-1 fans out one call per chapter, the
// per-chapter plans merge (same-label entities dedup with their key
// claims folded together), and pass-2 runs once per merged entity.
func TestAbsorbDenseTwoPass_ChapteredPass1FansOutAndMerges(t *testing.T) {
	root := stubHarnessKB(t, chapteredYAML(t, "2"))
	rawFile, rawName := writeChapteredRaw(t, root, "2026-06-01-chapters.md")

	alphaCh1 := absorbEntity{Label: "Alpha", Type: "concept", OneLine: "from ch1", KeyClaims: []string{"claim one"}}
	alphaCh2 := absorbEntity{Label: "Alpha", Type: "concept", OneLine: "", KeyClaims: []string{"claim variant"}}
	beta := entity("Beta", "from ch2")
	gamma := entity("Gamma", "from ch3")

	stub := &stubLLM{}
	stub.Rules = []*stubRule{
		{MatchOp: "absorb-pass1-chapter", MatchPrompt: "CH1-MARKER", Reply: chapterPlanJSON(t, rawFile, "Heading One", alphaCh1)},
		{MatchOp: "absorb-pass1-chapter", MatchPrompt: "CH2-MARKER", Reply: chapterPlanJSON(t, rawFile, "Heading Two", alphaCh2, beta)},
		{MatchOp: "absorb-pass1-chapter", MatchPrompt: "CH3-MARKER", Reply: chapterPlanJSON(t, rawFile, "Heading Three", gamma)},
		{MatchOp: "absorb-pass2", MatchPrompt: "- Label: Alpha", Reply: envelopeJSON(t, 1, "Alpha", "wiki/alpha.md", "A.")},
		{MatchOp: "absorb-pass2", MatchPrompt: "- Label: Beta", Reply: envelopeJSON(t, 1, "Beta", "wiki/beta.md", "B.")},
		{MatchOp: "absorb-pass2", MatchPrompt: "- Label: Gamma", Reply: envelopeJSON(t, 1, "Gamma", "wiki/gamma.md", "G.")},
	}
	installStubLLM(t, stub)

	sc := &SyncCmd{Model: "sonnet"}
	if err := sc.absorbDenseTwoPass(root, rawFile, rawName); err != nil {
		t.Fatalf("absorbDenseTwoPass: %v", err)
	}

	if got := len(stub.CallsWithOp("absorb-pass1-whole")); got != 0 {
		t.Errorf("whole-article pass1 calls = %d, want 0 (chaptered path active)", got)
	}
	if got := len(stub.CallsWithOp("absorb-pass1-chapter")); got != 3 {
		t.Errorf("chapter pass1 calls = %d, want 3", got)
	}
	pass2 := stub.CallsWithOp("absorb-pass2")
	if len(pass2) != 3 {
		t.Fatalf("pass2 calls = %d, want 3 (Alpha deduped across chapters)", len(pass2))
	}
	var alphaPrompt string
	for _, c := range pass2 {
		if strings.Contains(c.Prompt, "- Label: Alpha") {
			alphaPrompt = c.Prompt
		}
	}
	if !strings.Contains(alphaPrompt, "claim one | claim variant") {
		t.Errorf("merged Alpha key claims missing from pass2 prompt — chapter dedup didn't fold claims")
	}

	for _, page := range []string{"alpha", "beta", "gamma"} {
		if _, err := os.Stat(filepath.Join(root, "wiki", page+".md")); err != nil {
			t.Errorf("missing %s.md: %v", page, err)
		}
	}
	// Chunk + per-chapter plan checkpoints written for post-mortems.
	chunks, _ := filepath.Glob(filepath.Join(root, "output", "absorb-chunks", "2026-06-01-chapters", "*.md"))
	if len(chunks) != 3 {
		t.Errorf("chunk files = %d, want 3", len(chunks))
	}
	plans, _ := filepath.Glob(filepath.Join(root, "output", "absorb-plans", "2026-06-01-chapters-chapters", "*.json"))
	if len(plans) != 3 {
		t.Errorf("per-chapter plan files = %d, want 3", len(plans))
	}
}

// TestAbsorbDenseTwoPass_ChapteredRateLimitPropagates: a rate limit in
// the chapter fan-out is stop-the-world too — remaining chapters are
// never asked and the sentinel reaches the caller unchanged (no
// whole-article fallback for rate limits).
func TestAbsorbDenseTwoPass_ChapteredRateLimitPropagates(t *testing.T) {
	root := stubHarnessKB(t, chapteredYAML(t, "1"))
	rawFile, rawName := writeChapteredRaw(t, root, "2026-06-01-chrl.md")

	stub := &stubLLM{}
	stub.Rules = []*stubRule{
		{MatchOp: "absorb-pass1-chapter", Err: ErrRateLimit},
	}
	installStubLLM(t, stub)

	sc := &SyncCmd{Model: "sonnet"}
	err := sc.absorbDenseTwoPass(root, rawFile, rawName)
	if !errors.Is(err, ErrRateLimit) {
		t.Fatalf("err = %v, want ErrRateLimit", err)
	}
	if got := len(stub.CallsWithOp("absorb-pass1-chapter")); got != 1 {
		t.Errorf("chapter pass1 calls = %d, want 1 (stop-the-world)", got)
	}
	if got := len(stub.CallsWithOp("absorb-pass1-whole")); got != 0 {
		t.Errorf("whole-article fallback ran %d times after a rate limit — must not", got)
	}
	if got := len(stub.CallsWithOp("absorb-pass2")); got != 0 {
		t.Errorf("pass2 calls = %d, want 0", got)
	}
}

// TestAbsorbRaw_CheckpointsProgressAndStopsOnRateLimit: the absorb loop
// above absorbDenseTwoPass marks each article in wiki/_absorb_log.json
// as it lands, so a rate limit mid-run keeps the finished work and
// resumes with the unfinished article next run.
func TestAbsorbRaw_CheckpointsProgressAndStopsOnRateLimit(t *testing.T) {
	root := stubHarnessKB(t, denseAbsorbYAML)

	// Two standard-density articles (60+ words, no headings → not dense,
	// not a stub) absorbed via the single-pass path, in ReadDir order.
	longBody := strings.Repeat("knowledge worth keeping flows through this paragraph again and again. ", 12)
	for name, marker := range map[string]string{
		"2026-06-01-one.md": "UNIQUE-TOKEN-ONE",
		"2026-06-02-two.md": "UNIQUE-TOKEN-TWO",
	} {
		content := "---\ntitle: " + name + "\nsource_url: https://example.invalid/" + name + "\n---\n\n" + marker + " " + longBody
		if err := os.WriteFile(filepath.Join(root, "raw", "articles", name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	stub := &stubLLM{}
	stub.Rules = []*stubRule{
		{MatchOp: "absorb-single", MatchPrompt: "UNIQUE-TOKEN-ONE", Reply: envelopeJSON(t, 1, "One", "wiki/one.md", "One body.")},
		{MatchOp: "absorb-single", MatchPrompt: "UNIQUE-TOKEN-TWO", Err: ErrRateLimit},
	}
	installStubLLM(t, stub)

	sc := &SyncCmd{Model: "sonnet"}
	absorbed, err := sc.absorbRaw(root)
	if err != nil {
		t.Fatalf("absorbRaw: %v (rate limit should stop cleanly, not error)", err)
	}
	if absorbed != 1 {
		t.Errorf("absorbed = %d, want 1", absorbed)
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "one.md")); err != nil {
		t.Errorf("first article's page missing: %v", err)
	}

	logData, err := os.ReadFile(filepath.Join(root, "wiki", "_absorb_log.json"))
	if err != nil {
		t.Fatalf("read absorb log: %v", err)
	}
	if !strings.Contains(string(logData), "2026-06-01-one.md") {
		t.Errorf("absorb log missing checkpoint for the finished article:\n%s", logData)
	}
	if strings.Contains(string(logData), "2026-06-02-two.md") {
		t.Errorf("absorb log must NOT contain the rate-limited article (it has to retry next run):\n%s", logData)
	}
}
