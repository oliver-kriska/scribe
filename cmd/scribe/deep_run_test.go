// deep_run_test.go — driver tests for DeepCmd.Run against the stub LLM
// harness (llm_stub_test.go). Pins the per-directory batch loop's
// observable semantics for both protocols: envelope mode through the
// newLLMProvider seam and legacy tools mode through the runClaude seam —
// manifest checkpointing, rate-limit stop, per-directory error
// tolerance, dry-run, already-extracted skip, and the batch cap.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// deepKB scaffolds a KB + a fake project with the given knowledge dirs
// and registers the project in the manifest. Also sandboxes PATH with a
// no-op qmd so the post-batch reindex can't touch a real index (and the
// absent git makes gitIsDirty fall out false, skipping commit/push).
// Returns (kbRoot, projectDir).
func deepKB(t *testing.T, mode string, dirs ...string) (string, string) {
	t.Helper()
	yaml := `kb_name: stubkb
deep_ingest:
  mode: ` + mode + `
  provider: anthropic
  model: haiku
`
	root := stubHarnessKB(t, yaml)

	proj := t.TempDir()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(proj, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
		note := filepath.Join(proj, d, "notes.md")
		if err := os.WriteFile(note, []byte("# Notes in "+d+"\n\ncontent for "+d+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", note, err)
		}
	}

	manifest := map[string]any{
		"projects": map[string]any{
			"acme": map[string]any{"path": proj, "domain": "general"},
		},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "projects.json"), data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// PATH sandbox: a fake bin dir whose only binary is a no-op qmd.
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "qmd"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake qmd: %v", err)
	}
	t.Setenv("PATH", fakeBin)

	// runStats is package-global telemetry the envelope path mutates;
	// isolate it per test.
	origStats := runStats
	runStats = nil
	t.Cleanup(func() { runStats = origStats })

	return root, proj
}

// reloadProject re-reads the manifest and returns the acme entry, resolved
// by its display Name — the manifest map key is now a canonical path (see
// manifest.go), not the legacy "acme" string these fixtures seed.
func reloadProject(t *testing.T, root string) *ProjectEntry {
	t.Helper()
	m, err := loadManifest(root)
	if err != nil {
		t.Fatalf("reload manifest: %v", err)
	}
	e, err := m.resolve("acme")
	if err != nil {
		t.Fatalf("acme missing from manifest after run: %v", err)
	}
	return e
}

// deepEnvelope renders an EnvelopeV2 creating one wiki page for a dir.
func deepEnvelope(t *testing.T, dir string) string {
	t.Helper()
	return envelopeJSON(t, 2, "Deep "+dir, "wiki/deep-"+dir+".md", "Extracted from "+dir+".")
}

// TestDeepRun_EnvelopeMode_ExtractsAndCheckpointsManifest: every
// knowledge directory gets one envelope call, the resulting wiki pages
// land via applyWikiActions, and the manifest records the extracted
// dirs + timestamps ("no-git" for a project without git history).
func TestDeepRun_EnvelopeMode_ExtractsAndCheckpointsManifest(t *testing.T) {
	root, _ := deepKB(t, "envelope", "alphadocs", "betadocs")

	stub := &stubLLM{}
	stub.Rules = []*stubRule{
		{MatchOp: "deep-extract", MatchPrompt: "alphadocs", Reply: deepEnvelope(t, "alphadocs")},
		{MatchOp: "deep-extract", MatchPrompt: "betadocs", Reply: deepEnvelope(t, "betadocs")},
	}
	reqs := installStubLLM(t, stub)

	d := &DeepCmd{Project: "acme", BatchMax: 5, Model: "sonnet"}
	if err := d.Run(); err != nil {
		t.Fatalf("DeepCmd.Run: %v", err)
	}

	calls := stub.CallsWithOp("deep-extract")
	if len(calls) != 2 {
		t.Fatalf("deep-extract calls = %d, want 2", len(calls))
	}
	for _, c := range calls {
		if c.NumCtx <= 0 {
			t.Errorf("deep call num_ctx = %d, want >0", c.NumCtx)
		}
		if !strings.Contains(c.Prompt, "acme") {
			t.Errorf("deep prompt missing project name")
		}
	}
	// Config routing reached the seam: deep_ingest provider/model pair.
	if pairs := reqs.Pairs(); len(pairs) == 0 || pairs[0] != "anthropic/haiku" {
		t.Errorf("provider requests = %v, want anthropic/haiku first", pairs)
	}

	for _, dir := range []string{"alphadocs", "betadocs"} {
		if _, err := os.Stat(filepath.Join(root, "wiki", "deep-"+dir+".md")); err != nil {
			t.Errorf("missing wiki page for %s: %v", dir, err)
		}
	}

	e := reloadProject(t, root)
	if e.ExtractedDirs != "alphadocs,betadocs" {
		t.Errorf("ExtractedDirs = %q, want %q", e.ExtractedDirs, "alphadocs,betadocs")
	}
	if e.LastExtracted == "" {
		t.Errorf("LastExtracted not stamped")
	}
	if e.LastSHA != "no-git" {
		t.Errorf("LastSHA = %q, want no-git", e.LastSHA)
	}
}

// TestDeepRun_EnvelopeMode_RateLimitStopsRun: a rate-limited directory
// stops the batch loop immediately — the rate-limited dir is NOT marked
// extracted (it has to retry next run) and later dirs are never asked.
// Run itself returns nil so cron exits clean.
func TestDeepRun_EnvelopeMode_RateLimitStopsRun(t *testing.T) {
	root, _ := deepKB(t, "envelope", "alphadocs", "betadocs")

	stub := &stubLLM{}
	stub.Rules = []*stubRule{
		{MatchOp: "deep-extract", MatchPrompt: "alphadocs", Err: ErrRateLimit},
	}
	installStubLLM(t, stub)

	d := &DeepCmd{Project: "acme", BatchMax: 5, Model: "sonnet"}
	if err := d.Run(); err != nil {
		t.Fatalf("DeepCmd.Run: %v (rate limit should stop cleanly)", err)
	}
	if got := len(stub.CallsWithOp("deep-extract")); got != 1 {
		t.Errorf("deep-extract calls = %d, want 1 (betadocs must not be attempted)", got)
	}
	e := reloadProject(t, root)
	if e.ExtractedDirs != "" {
		t.Errorf("ExtractedDirs = %q, want empty — rate-limited dir must retry next run", e.ExtractedDirs)
	}
}

// TestDeepRun_EnvelopeMode_ErrorContinuesToNextDir: a non-rate-limit
// failure on one directory is logged and the loop moves on; the failed
// dir is still marked extracted (matching the legacy claude-error
// behavior) so one persistently broken dir can't wedge the batch loop
// forever.
func TestDeepRun_EnvelopeMode_ErrorContinuesToNextDir(t *testing.T) {
	root, _ := deepKB(t, "envelope", "alphadocs", "betadocs")

	stub := &stubLLM{}
	stub.Rules = []*stubRule{
		{MatchOp: "deep-extract", MatchPrompt: "alphadocs", Reply: "not an envelope at all"},
		{MatchOp: "deep-extract", MatchPrompt: "betadocs", Reply: deepEnvelope(t, "betadocs")},
	}
	installStubLLM(t, stub)

	d := &DeepCmd{Project: "acme", BatchMax: 5, Model: "sonnet"}
	if err := d.Run(); err != nil {
		t.Fatalf("DeepCmd.Run: %v", err)
	}
	if got := len(stub.CallsWithOp("deep-extract")); got != 2 {
		t.Errorf("deep-extract calls = %d, want 2", got)
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "deep-alphadocs.md")); !os.IsNotExist(err) {
		t.Errorf("alphadocs page should not exist, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "wiki", "deep-betadocs.md")); err != nil {
		t.Errorf("betadocs page missing: %v", err)
	}
	if e := reloadProject(t, root); e.ExtractedDirs != "alphadocs,betadocs" {
		t.Errorf("ExtractedDirs = %q, want both dirs recorded", e.ExtractedDirs)
	}
}

// TestDeepRun_ToolsMode_UsesRunClaude: legacy tools mode shells out per
// directory through the runClaude seam with the broad file/git tool
// set, the CLI-selected model, and the 600s timeout.
func TestDeepRun_ToolsMode_UsesRunClaude(t *testing.T) {
	root, proj := deepKB(t, "tools", "alphadocs", "betadocs")

	claude := &stubClaude{Reply: "extracted"}
	installStubClaude(t, claude)

	d := &DeepCmd{Project: "acme", BatchMax: 5, Model: "sonnet"}
	if err := d.Run(); err != nil {
		t.Fatalf("DeepCmd.Run: %v", err)
	}

	calls := claude.Calls()
	if len(calls) != 2 {
		t.Fatalf("runClaude calls = %d, want 2", len(calls))
	}
	var sawAlpha, sawBeta bool
	for _, c := range calls {
		if c.Model != "sonnet" {
			t.Errorf("model = %q, want sonnet", c.Model)
		}
		if c.Op != "deep-extract" {
			t.Errorf("op = %q, want deep-extract", c.Op)
		}
		if c.Timeout != 600*time.Second {
			t.Errorf("timeout = %v, want 600s", c.Timeout)
		}
		joined := strings.Join(c.Tools, ",")
		if !strings.Contains(joined, "Write") || !strings.Contains(joined, "Bash(git log:*)") {
			t.Errorf("tools %v missing expected entries", c.Tools)
		}
		if !strings.Contains(c.Prompt, proj) {
			t.Errorf("prompt missing project path")
		}
		if strings.Contains(c.Prompt, "alphadocs") {
			sawAlpha = true
		}
		if strings.Contains(c.Prompt, "betadocs") {
			sawBeta = true
		}
	}
	if !sawAlpha || !sawBeta {
		t.Errorf("per-dir prompts incomplete: alpha=%v beta=%v", sawAlpha, sawBeta)
	}
	if e := reloadProject(t, root); e.ExtractedDirs != "alphadocs,betadocs" {
		t.Errorf("ExtractedDirs = %q, want both dirs", e.ExtractedDirs)
	}
}

// TestDeepRun_ToolsMode_RateLimitStops: same stop semantics on the
// legacy path — first rate limit breaks the loop, nothing is recorded.
func TestDeepRun_ToolsMode_RateLimitStops(t *testing.T) {
	root, _ := deepKB(t, "tools", "alphadocs", "betadocs")

	claude := &stubClaude{Script: func(claudeCall) (string, error) {
		return "", ErrRateLimit
	}}
	installStubClaude(t, claude)

	d := &DeepCmd{Project: "acme", BatchMax: 5, Model: "sonnet"}
	if err := d.Run(); err != nil {
		t.Fatalf("DeepCmd.Run: %v", err)
	}
	if got := len(claude.Calls()); got != 1 {
		t.Errorf("runClaude calls = %d, want 1", got)
	}
	if e := reloadProject(t, root); e.ExtractedDirs != "" {
		t.Errorf("ExtractedDirs = %q, want empty", e.ExtractedDirs)
	}
}

// TestDeepRun_DryRun: dry run lists files without any LLM call and
// without touching the manifest.
func TestDeepRun_DryRun(t *testing.T) {
	root, _ := deepKB(t, "envelope", "alphadocs")

	stub := &stubLLM{}
	installStubLLM(t, stub)
	claude := &stubClaude{}
	installStubClaude(t, claude)

	d := &DeepCmd{Project: "acme", BatchMax: 5, Model: "sonnet", DryRun: true}
	if err := d.Run(); err != nil {
		t.Fatalf("DeepCmd.Run: %v", err)
	}
	if got := len(stub.Calls()); got != 0 {
		t.Errorf("provider calls = %d, want 0 in dry run", got)
	}
	if got := len(claude.Calls()); got != 0 {
		t.Errorf("runClaude calls = %d, want 0 in dry run", got)
	}
	e := reloadProject(t, root)
	if e.ExtractedDirs != "" || e.LastExtracted != "" {
		t.Errorf("dry run must not checkpoint the manifest: dirs=%q extracted=%q", e.ExtractedDirs, e.LastExtracted)
	}
}

// TestDeepRun_SkipsExtractedAndHonorsBatchMax: directories already in
// extracted_dirs are skipped, and batch-max caps how many new dirs one
// run takes on — the remainder waits for the next run.
func TestDeepRun_SkipsExtractedAndHonorsBatchMax(t *testing.T) {
	root, _ := deepKB(t, "envelope", "alphadocs", "betadocs", "gammadocs")

	// Pre-seed alphadocs as already extracted.
	m, err := loadManifest(root)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	entry, err := m.resolve("acme")
	if err != nil {
		t.Fatalf("acme missing from manifest: %v", err)
	}
	entry.ExtractedDirs = "alphadocs"
	if err := m.save(); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	stub := &stubLLM{}
	stub.Rules = []*stubRule{
		{MatchOp: "deep-extract", MatchPrompt: "betadocs", Reply: deepEnvelope(t, "betadocs")},
	}
	installStubLLM(t, stub)

	d := &DeepCmd{Project: "acme", BatchMax: 1, Model: "sonnet"}
	if err := d.Run(); err != nil {
		t.Fatalf("DeepCmd.Run: %v", err)
	}

	calls := stub.CallsWithOp("deep-extract")
	if len(calls) != 1 {
		t.Fatalf("deep-extract calls = %d, want 1 (batch-max 1)", len(calls))
	}
	if !strings.Contains(calls[0].Prompt, "betadocs") {
		t.Errorf("expected betadocs (first unextracted dir) to be processed")
	}
	if e := reloadProject(t, root); e.ExtractedDirs != "alphadocs,betadocs" {
		t.Errorf("ExtractedDirs = %q, want alphadocs,betadocs (gammadocs waits)", e.ExtractedDirs)
	}
}

// TestDeepRun_UnknownProjectErrors: a project missing from the manifest
// fails fast with a pointer at sync --discover.
func TestDeepRun_UnknownProjectErrors(t *testing.T) {
	deepKB(t, "envelope", "alphadocs")

	stub := &stubLLM{}
	installStubLLM(t, stub)

	d := &DeepCmd{Project: "ghost", BatchMax: 5, Model: "sonnet"}
	err := d.Run()
	if err == nil || !strings.Contains(err.Error(), "not in manifest") {
		t.Fatalf("err = %v, want not-in-manifest error", err)
	}
	if got := len(stub.Calls()); got != 0 {
		t.Errorf("provider calls = %d, want 0", got)
	}
}
