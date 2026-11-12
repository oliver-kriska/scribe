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
	root := stubHarnessKB(t, denseAbsorbYAML)
	proj := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, "README.md"), []byte("# p\n\nA small project.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installStubLLM(t, stub)
	cfg := loadConfig(root)
	return runExtractEnvelope(context.Background(), root, cfg, nil, "p", &ProjectEntry{Path: proj, Domain: "general"}, nil)
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
