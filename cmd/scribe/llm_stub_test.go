// llm_stub_test.go — the stub LLM provider harness (issue #9).
//
// The three LLM-driving paths flagged with `nolint:gocognit`
// (absorbDenseTwoPass, ScanCmd.Run, DeepCmd.Run) had 0% test coverage
// because every test would have shelled out to `claude -p` or hit a
// local Ollama server. This file provides the fake that breaks that
// dependency:
//
//   - stubLLM implements llmProviderGenerator with scripted responses,
//     recorded prompts (plus the op label and num_ctx pulled from the
//     call context), injectable failures, and rate-limit / budget
//     signals. It also tracks concurrency (max in-flight calls and
//     which prompts were active when each call entered) so tests can
//     assert errgroup SetLimit caps and the per-label mutex.
//   - stubClaude replaces the runClaude seam for the legacy
//     tools-mode paths.
//
// Injection happens through two package-level seams that production
// code never reassigns:
//
//	newLLMProvider (llm.go)  — swapped via installStubLLM
//	runClaude      (claude.go) — swapped via installStubClaude
//
// Both helpers restore the real implementation in t.Cleanup, and none
// of the tests using them call t.Parallel(), so the package-global
// swap is race-free.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"
	"time"
)

// stubCall is one recorded provider invocation.
type stubCall struct {
	// Prompt is the full rendered prompt the driver sent.
	Prompt string
	// JSONMode is true when the call arrived via GenerateJSON.
	JSONMode bool
	// Op is the op label tagged on the context (withOpLabel) — lets
	// tests distinguish absorb-pass1-whole / absorb-pass2 / absorb-single
	// / deep-extract calls without fragile prompt sniffing.
	Op string
	// NumCtx is the Ollama num_ctx tagged on the context
	// (withOllamaNumCtx); 0 when untagged.
	NumCtx int
	// ActiveAtEntry snapshots the prompts of calls already in flight
	// when this call entered. Used to prove serialization (per-label
	// mutex) and parallelism (SetLimit).
	ActiveAtEntry []string
}

// stubRule is one scripted response. Rules are evaluated in order;
// the first matching (and, for once-rules, unconsumed) rule wins.
type stubRule struct {
	// MatchOp, when non-empty, requires the context op label to equal it.
	MatchOp string
	// MatchPrompt, when non-empty, requires the prompt to contain it.
	MatchPrompt string
	// Reply is returned when Err is nil.
	Reply string
	// Err is returned instead of Reply when non-nil.
	Err error
	// Once consumes the rule after its first match — lets tests script
	// "fail first, succeed on retry" sequences.
	Once bool

	used bool
}

// stubLLM is a scripted llmProviderGenerator. Safe for concurrent use.
//
// The zero value answers every call with DefaultReply (""), so tests
// only script what they assert on.
type stubLLM struct {
	mu gosync.Mutex

	// Rules are matched in order; see stubRule.
	Rules []*stubRule
	// DefaultReply / DefaultErr answer calls no rule matched.
	DefaultReply string
	DefaultErr   error
	// Delay is slept (outside the lock) before returning, giving
	// concurrency assertions a window to observe overlap.
	Delay time.Duration
	// RendezvousOp + Rendezvous make calls whose op label equals
	// RendezvousOp wait (up to rendezvousTimeout) until Rendezvous
	// calls are in flight simultaneously. Proves SetLimit actually
	// admits N concurrent workers without sleeping blind.
	RendezvousOp string
	Rendezvous   int

	calls         []stubCall
	activePrompts []string
	inFlight      int
	maxInFlight   int
}

const rendezvousTimeout = 2 * time.Second

func (s *stubLLM) Name() string { return "stub/test" }

func (s *stubLLM) Generate(ctx context.Context, prompt string) (string, error) {
	return s.respond(ctx, prompt, false)
}

func (s *stubLLM) respond(ctx context.Context, prompt string, jsonMode bool) (string, error) {
	// Mirror the real providers: a canceled/expired context fails the
	// call. absorbDenseTwoPass checks gctx.Err() before dispatching, so
	// in practice the stop-the-world tests never reach this — but the
	// stub should not answer through a dead context either way.
	if err := ctx.Err(); err != nil {
		return "", err
	}

	s.mu.Lock()
	call := stubCall{
		Prompt:        prompt,
		JSONMode:      jsonMode,
		Op:            opLabelFromContext(ctx),
		NumCtx:        ollamaNumCtxFromContext(ctx),
		ActiveAtEntry: append([]string(nil), s.activePrompts...),
	}
	s.calls = append(s.calls, call)
	s.activePrompts = append(s.activePrompts, prompt)
	s.inFlight++
	if s.inFlight > s.maxInFlight {
		s.maxInFlight = s.inFlight
	}
	// Rendezvous: wait for the configured number of same-op calls to
	// be in flight at once. Poll-with-deadline keeps the implementation
	// trivially race-free; on a mis-capped errgroup the wait times out
	// instead of deadlocking and the test's maxInFlight assert fails.
	if s.Rendezvous > 0 && call.Op == s.RendezvousOp {
		deadline := time.Now().Add(rendezvousTimeout)
		for s.inFlight < s.Rendezvous && time.Now().Before(deadline) {
			s.mu.Unlock()
			time.Sleep(time.Millisecond)
			s.mu.Lock()
		}
	}
	reply, err := s.pickLocked(prompt, call.Op)
	delay := s.Delay
	s.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}

	s.mu.Lock()
	s.inFlight--
	for i, p := range s.activePrompts {
		if p == prompt {
			s.activePrompts = append(s.activePrompts[:i], s.activePrompts[i+1:]...)
			break
		}
	}
	s.mu.Unlock()

	return reply, err
}

// pickLocked resolves the scripted response for a call. Caller holds s.mu.
func (s *stubLLM) pickLocked(prompt, op string) (string, error) {
	for _, r := range s.Rules {
		if r.Once && r.used {
			continue
		}
		if r.MatchOp != "" && r.MatchOp != op {
			continue
		}
		if r.MatchPrompt != "" && !strings.Contains(prompt, r.MatchPrompt) {
			continue
		}
		r.used = true
		return r.Reply, r.Err
	}
	return s.DefaultReply, s.DefaultErr
}

// Calls returns a snapshot of every recorded call.
func (s *stubLLM) Calls() []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubCall(nil), s.calls...)
}

// CallsWithOp returns the recorded calls whose context op label equals op.
func (s *stubLLM) CallsWithOp(op string) []stubCall {
	var out []stubCall
	for _, c := range s.Calls() {
		if c.Op == op {
			out = append(out, c)
		}
	}
	return out
}

// MaxInFlight reports the high-water mark of concurrent calls.
func (s *stubLLM) MaxInFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxInFlight
}

// stubJSONLLM is stubLLM plus the jsonModeProvider capability, so
// generateMaybeJSON dispatches to GenerateJSON (the Ollama-shaped path).
// Calls record JSONMode=true.
type stubJSONLLM struct {
	stubLLM
}

func (s *stubJSONLLM) GenerateJSON(ctx context.Context, prompt string) (string, error) {
	return s.respond(ctx, prompt, true)
}

// installStubLLM points the newLLMProvider seam at p for the duration of
// the test, recording the (provider, model) pairs the driver asked for.
// Restored in t.Cleanup. Tests using this must not call t.Parallel().
func installStubLLM(t *testing.T, p llmProviderGenerator) *providerRequests {
	t.Helper()
	reqs := &providerRequests{}
	orig := newLLMProvider
	newLLMProvider = func(provider, model, _, _ string) llmProviderGenerator {
		reqs.mu.Lock()
		reqs.pairs = append(reqs.pairs, provider+"/"+model)
		reqs.mu.Unlock()
		return p
	}
	t.Cleanup(func() { newLLMProvider = orig })
	return reqs
}

// providerRequests records the provider/model pairs requested through the
// newLLMProvider seam, so tests can assert config routing (e.g. pass-1
// model vs pass-2 model) without caring which stub answered.
type providerRequests struct {
	mu    gosync.Mutex
	pairs []string
}

func (p *providerRequests) Pairs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.pairs...)
}

// claudeCall is one recorded runClaude invocation (tools-mode paths).
type claudeCall struct {
	Root    string
	Prompt  string
	Model   string
	Tools   []string
	Timeout time.Duration
	Op      string
}

// stubClaude is a scripted stand-in for runClaude. Script, when set, is
// consulted per call; otherwise every call succeeds with Reply.
type stubClaude struct {
	mu     gosync.Mutex
	Script func(call claudeCall) (string, error)
	Reply  string

	calls []claudeCall
}

func (s *stubClaude) run(ctx context.Context, root, prompt, model string, tools []string, timeout time.Duration) (string, error) {
	call := claudeCall{
		Root:    root,
		Prompt:  prompt,
		Model:   model,
		Tools:   append([]string(nil), tools...),
		Timeout: timeout,
		Op:      opLabelFromContext(ctx),
	}
	s.mu.Lock()
	s.calls = append(s.calls, call)
	script := s.Script
	reply := s.Reply
	s.mu.Unlock()
	if script != nil {
		return script(call)
	}
	return reply, nil
}

func (s *stubClaude) Calls() []claudeCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]claudeCall(nil), s.calls...)
}

// installStubClaude points the runClaude seam at a stubClaude for the
// duration of the test. Restored in t.Cleanup. Tests using this must not
// call t.Parallel().
func installStubClaude(t *testing.T, c *stubClaude) {
	t.Helper()
	orig := runClaude
	runClaude = c.run
	t.Cleanup(func() { runClaude = orig })
}

// --- shared fixtures ----------------------------------------------------

// stubHarnessKB scaffolds a minimal KB in a tempdir: the directory
// layout absorb/deep need, a scribe.yaml with the caller's content, and
// the env redirects that keep every write inside the sandbox
// (XDG_CONFIG_HOME for the trust record + user config, HOME for the
// discovery-path defaults, SCRIBE_KB for kbDir, SCRIBE_SKIP_REINDEX so
// post-run reindex never re-executes the test binary).
func stubHarnessKB(t *testing.T, scribeYAML string) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"raw/articles", "wiki", "scripts", "output"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(scribeYAML), 0o644); err != nil {
		t.Fatalf("write scribe.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "projects.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write projects.json: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCRIBE_KB", root)
	t.Setenv("SCRIBE_SKIP_REINDEX", "1")
	// Clear the pass-2 A/B env overrides so a developer shell that has
	// them exported can't flip the mode under the test's scribe.yaml.
	t.Setenv("SCRIBE_PASS2_MODE", "")
	t.Setenv("SCRIBE_PASS2_PROVIDER", "")
	t.Setenv("SCRIBE_PASS2_MODEL", "")
	return root
}

// planJSON renders an absorb pass-1 plan document.
func planJSON(t *testing.T, rawFile, domain string, entities ...absorbEntity) string {
	t.Helper()
	b, err := json.Marshal(absorbPlan{RawFile: rawFile, SourceTitle: "Stub Source", Domain: domain, Entities: entities})
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return string(b)
}

// envelopeJSON renders a minimal valid WikiActionEnvelope creating one
// article at path. The frontmatter carries every field the clamp seam
// checks so apply is byte-deterministic.
func envelopeJSON(t *testing.T, version int, entity, path, body string) string {
	t.Helper()
	content := fmt.Sprintf(`---
title: %s
type: research
domain: general
created: 2026-06-12
updated: 2026-06-12
confidence: medium
tags: []
---

%s
`, entity, body)
	env := WikiActionEnvelope{
		Version: version,
		Entity:  entity,
		Actions: []WikiAction{{Op: "create", Path: path, Content: content}},
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(b)
}
