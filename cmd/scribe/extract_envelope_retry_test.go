package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const extractEnvEmpty = `{"version":2,"entity":"extract-p","actions":[],"meta":[{"op":"log_append","line":"## [2026-11-12] extract | p: nothing new"}]}`

func runExtractForTest(t *testing.T, stub *stubLLM) ([]string, error) {
	t.Helper()
	_, consumed, err := runExtractInKB(t, stub, stubHarnessKB(t, denseAbsorbYAML))
	return consumed, err
}

func runExtractInKB(t *testing.T, stub *stubLLM, root string) (string, []string, error) {
	t.Helper()
	proj := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, "README.md"), []byte("# p\n\nA small project.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installStubLLM(t, stub)
	cfg := loadConfig(root)
	consumed, err := runExtractEnvelope(context.Background(), root, cfg, nil, "p", &ProjectEntry{Path: proj, Domain: "general"}, nil)
	return root, consumed, err
}

// A small project with nothing new used to come back as a bare "{}",
// which failed the project on every sync. One corrective retry recovers
// the legal empty envelope.
func TestExtractEnvelope_BareEmptyObjectIsRetried(t *testing.T) {
	stub := &stubLLM{Rules: []*stubRule{{MatchOp: "extract", Reply: `{}`, Once: true}}, DefaultReply: extractEnvEmpty}
	if _, err := runExtractForTest(t, stub); err != nil {
		t.Fatalf("want the retried empty envelope accepted, got %v", err)
	}
	calls := stub.Calls()
	if len(calls) != 2 || !strings.Contains(calls[1].Prompt, "## CORRECTION") {
		t.Fatalf("want 2 calls, the second corrective; got %d", len(calls))
	}
}

func TestExtractEnvelope_TwoBareEmptyRepliesFail(t *testing.T) {
	stub := &stubLLM{DefaultReply: `{}`}
	if _, err := runExtractForTest(t, stub); !errors.Is(err, errBareEmptyEnvelope) {
		t.Fatalf("want errBareEmptyEnvelope, got %v", err)
	}
	if n := len(stub.Calls()); n != 2 {
		t.Fatalf("want exactly one retry, got %d calls", n)
	}
}

// Transport and rate-limit errors are not retried.
func TestExtractEnvelope_RateLimitNotRetried(t *testing.T) {
	stub := &stubLLM{DefaultErr: ErrRateLimit}
	if _, err := runExtractForTest(t, stub); !errors.Is(err, ErrRateLimit) {
		t.Fatalf("want ErrRateLimit, got %v", err)
	}
	if n := len(stub.Calls()); n != 1 {
		t.Fatalf("rate limit must not be retried; got %d calls", n)
	}
}

// A drop the model saw but wrote nothing from is parked for review, not
// deleted by finishDropStaging.
func TestExtractEnvelope_ParksDropsWhenNothingWritten(t *testing.T) {
	root := stubHarnessKB(t, denseAbsorbYAML)
	drop := writeDrop(t, dropStagingDir(root, "p"), "2026-11-12-note.md", "---\nscriptorium: true\n---\nA real handoff.\n")
	stub := &stubLLM{DefaultReply: extractEnvEmpty}
	_, consumed, err := runExtractInKB(t, stub, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(consumed) != 1 || consumed[0] != drop {
		t.Fatalf("want the drop reported consumed, got %v", consumed)
	}
	parked := filepath.Join(root, "output", "drops-unextracted", "p", "2026-11-12-note.md")
	if _, err := os.Stat(parked); err != nil {
		t.Fatalf("drop must be parked at %s: %v", parked, err)
	}
	if _, err := os.Stat(drop); !os.IsNotExist(err) {
		t.Fatalf("drop must leave staging, stat err = %v", err)
	}
}

// When the model writes a page, consumed drops are left for
// finishDropStaging to remove as before.
func TestExtractEnvelope_DoesNotParkWhenPageWritten(t *testing.T) {
	root := stubHarnessKB(t, denseAbsorbYAML)
	drop := writeDrop(t, dropStagingDir(root, "p"), "2026-11-12-note.md", "a handoff")
	stub := &stubLLM{DefaultReply: envelopeJSON(t, 2, "X", "wiki/x.md", "X body.")}
	if _, _, err := runExtractInKB(t, stub, root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(drop); err != nil {
		t.Fatalf("drop must stay staged for finishDropStaging: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "output", "drops-unextracted")); !os.IsNotExist(err) {
		t.Fatalf("nothing must be parked, stat err = %v", err)
	}
}

// On a non-Ollama provider (the stub reports "anthropic", as Together
// does) the envelope prompt must carry the gathered files. It used to
// load the tool-mode extract-anthropic.md, which has no FILES_CONTENT.
func TestExtractEnvelope_PromptCarriesProjectFiles(t *testing.T) {
	stub := &stubLLM{DefaultReply: extractEnvEmpty}
	if _, err := runExtractForTest(t, stub); err != nil {
		t.Fatal(err)
	}
	p := stub.Calls()[0].Prompt
	if !strings.Contains(p, "A small project.") || !strings.Contains(p, "<<<FILES_BEGIN>>>") {
		t.Fatalf("prompt must inline the project's files:\n%.400s", p)
	}
}

// The prompt's rolling_memory_append example must use the field the
// executor reads ("content"); it said "entry", so every rolling-memory
// note from project extraction failed with "empty content".
func TestExtractEnvelopePromptRollingFieldMatchesExecutor(t *testing.T) {
	data, err := promptFS.ReadFile("prompts/" + extractEnvelopePrompt)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if strings.Contains(s, `"entry":`) || !strings.Contains(s, `"op": "rolling_memory_append"`) {
		t.Fatal(`rolling_memory_append example must carry "content", not "entry"`)
	}
}

// Hosted models leave the rolling-memory domain and the log line empty,
// and the executor rejected those ops (4 of 8 in one live run). The
// extraction knows the project's domain, so it fills them in.
func TestExtractEnvelope_FillsEmptyMetaFields(t *testing.T) {
	root := stubHarnessKB(t, denseAbsorbYAML)
	reply := `{"version":2,"actions":[],"meta":[` +
		`{"op":"log_append","content":"## [2026-11-13] extract | p: one learning"},` +
		`{"op":"rolling_memory_append","target":"learnings","content":"## 2026-11-13 | a learning\n\nContext: x."}]}`
	stub := &stubLLM{DefaultReply: reply}
	installStubLLM(t, stub)
	proj := t.TempDir()
	cfg := loadConfig(root)
	if _, err := runExtractEnvelope(context.Background(), root, cfg, nil, "p", &ProjectEntry{Path: proj, Domain: "general"}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "projects", "general", "learnings.md"))
	if err != nil || !strings.Contains(string(got), "a learning") {
		t.Fatalf("learning must land in projects/general/learnings.md: err=%v body=%q", err, got)
	}
	log, _ := os.ReadFile(filepath.Join(root, "log.md"))
	if !strings.Contains(string(log), "extract | p: one learning") {
		t.Fatalf("log line must fall back to the content, log.md=%q", log)
	}
}
