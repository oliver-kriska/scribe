package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestApplyMetaSessionsLogAppend records a session ID into wiki/_sessions_log.json.
func TestApplyMetaSessionsLogAppend(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wiki"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	env := WikiActionEnvelope{
		Version: 2,
		Meta: []MetaAction{
			{Op: "sessions_log_append", SessionID: "abc-123"},
		},
	}
	res, err := applyWikiActions(root, env, ApplyOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	data, err := os.ReadFile(filepath.Join(root, "wiki", "_sessions_log.json"))
	if err != nil {
		t.Fatalf("read sessions log: %v", err)
	}
	var got struct {
		Processed map[string]string `json:"processed"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got.Processed["abc-123"]; !ok {
		t.Fatalf("session not recorded: %s", string(data))
	}
}

// TestApplyMetaLogAppend appends a line to log.md.
func TestApplyMetaLogAppend(t *testing.T) {
	root := t.TempDir()
	env := WikiActionEnvelope{
		Version: 2,
		Meta: []MetaAction{
			{Op: "log_append", Line: "Dream cycle: 12 entries consolidated."},
		},
	}
	if _, err := applyWikiActions(root, env, ApplyOptions{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "log.md"))
	if err != nil {
		t.Fatalf("read log.md: %v", err)
	}
	if !strings.Contains(string(data), "Dream cycle") {
		t.Fatalf("log missing line: %q", string(data))
	}
}

// TestApplyMetaLogAppendRejectsMultiline collapses embedded newlines.
func TestApplyMetaLogAppendRejectsMultiline(t *testing.T) {
	root := t.TempDir()
	env := WikiActionEnvelope{
		Version: 2,
		Meta:    []MetaAction{{Op: "log_append", Line: "first\nsecond\nthird"}},
	}
	if _, err := applyWikiActions(root, env, ApplyOptions{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "log.md"))
	s := strings.TrimRight(string(data), "\n")
	if strings.Contains(s, "\n") {
		t.Fatalf("expected single-line append, got %q", s)
	}
}

// TestApplyMetaRollingAppendRequiresValidDomain rejects unknown domains.
func TestApplyMetaRollingAppendRequiresValidDomain(t *testing.T) {
	root := t.TempDir()
	// No scribe.yaml → AllDomains returns only universal {personal, general}.
	env := WikiActionEnvelope{
		Version: 2,
		Meta: []MetaAction{
			{Op: "rolling_memory_append", Domain: "made-up-domain", Target: "learnings", Content: "..."},
		},
	}
	res, _ := applyWikiActions(root, env, ApplyOptions{})
	if len(res.Errors) == 0 {
		t.Fatalf("expected an error for invalid domain")
	}
}

// TestApplyMetaRollingAppendRequiresValidTarget rejects unknown targets.
func TestApplyMetaRollingAppendRequiresValidTarget(t *testing.T) {
	root := t.TempDir()
	env := WikiActionEnvelope{
		Version: 2,
		Meta: []MetaAction{
			{Op: "rolling_memory_append", Domain: "general", Target: "ideas", Content: "..."},
		},
	}
	res, _ := applyWikiActions(root, env, ApplyOptions{})
	if len(res.Errors) == 0 {
		t.Fatalf("expected an error for invalid target")
	}
}

// TestApplyMetaRollingAppendCreatesFile creates a rolling memory file when absent.
func TestApplyMetaRollingAppendCreatesFile(t *testing.T) {
	root := t.TempDir()
	env := WikiActionEnvelope{
		Version: 2,
		Meta: []MetaAction{
			{Op: "rolling_memory_append", Domain: "general", Target: "learnings", Content: "Found a new pattern."},
		},
	}
	res, err := applyWikiActions(root, env, ApplyOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	path := filepath.Join(root, "projects", "general", "learnings.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rolling file: %v", err)
	}
	if !strings.Contains(string(data), "Found a new pattern.") {
		t.Fatalf("rolling file missing content: %q", string(data))
	}
	if !strings.HasPrefix(string(data), "---") {
		t.Fatalf("rolling file missing frontmatter: %q", string(data))
	}
}

// TestApplyMetaUnknownOpErrors surfaces unknown ops as errors.
func TestApplyMetaUnknownOpErrors(t *testing.T) {
	root := t.TempDir()
	env := WikiActionEnvelope{
		Version: 2,
		Meta:    []MetaAction{{Op: "frobnicate"}},
	}
	res, _ := applyWikiActions(root, env, ApplyOptions{})
	if len(res.Errors) == 0 {
		t.Fatalf("expected error for unknown op")
	}
}

// TestEnvelopeV2BackCompat verifies V1 envelopes (no Meta) still parse cleanly.
func TestEnvelopeV2BackCompat(t *testing.T) {
	js := `{"entity":"x","actions":[{"op":"create","path":"wiki/x.md","content":"---\ntitle: x\n---\n"}]}`
	env, err := parseEnvelope(js)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if env.Version != 0 {
		t.Errorf("expected V1 (version=0), got %d", env.Version)
	}
	if len(env.Meta) != 0 {
		t.Errorf("expected no meta, got %d", len(env.Meta))
	}
}

// TestApplyMetaRollingAppend_InitFrontmatterLintValid is the 0.2.26
// regression guard. The old init block wrote `type: article` (not in
// validTypes) plus only title/type/domain/rolling, so every
// auto-created rolling file failed lint on BOTH invalid-type and
// missing-required-fields the instant it was created. The frontmatter
// must now pass validateFile and match the established convention
// (type: project, full required set, Title Case name).
func TestApplyMetaRollingAppend_InitFrontmatterLintValid(t *testing.T) {
	root := t.TempDir()
	env := WikiActionEnvelope{
		Version: 2,
		Meta: []MetaAction{
			{Op: "rolling_memory_append", Domain: "general", Target: "decisions-log", Content: "A decision."},
		},
	}
	if _, err := applyWikiActions(root, env, ApplyOptions{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	path := filepath.Join(root, "projects", "general", "decisions-log.md")
	if errs := validateFile(root, path); len(errs) != 0 {
		t.Fatalf("auto-created rolling file is not lint-valid: %v", errs)
	}
	data, _ := os.ReadFile(path)
	for _, want := range []string{
		"type: project", "domain: general", "rolling: true",
		`title: "General Decisions Log"`, "confidence: medium",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing %q in created rolling file:\n%s", want, data)
		}
	}
}
