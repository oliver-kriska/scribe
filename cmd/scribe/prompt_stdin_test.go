package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// installFakeClaude puts a `claude` shim first on PATH that records its
// argv and stdin to files and answers with a minimal result envelope.
func installFakeClaude(t *testing.T) (argvFile, stdinFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv.txt")
	stdinFile = filepath.Join(dir, "stdin.txt")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$SCRIBE_TEST_ARGV\"\n" +
		"cat > \"$SCRIBE_TEST_STDIN\"\n" +
		"printf '%s' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"OK\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SCRIBE_TEST_ARGV", argvFile)
	t.Setenv("SCRIBE_TEST_STDIN", stdinFile)
	return argvFile, stdinFile
}

func assertPromptOverStdin(t *testing.T, argvFile, stdinFile, prompt string) {
	t.Helper()
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("claude shim was not invoked: %v", err)
	}
	if strings.Contains(string(argv), prompt) || strings.Contains(string(argv), "SECRET-BODY") {
		t.Errorf("prompt body leaked into argv:\n%s", argv)
	}
	if !strings.Contains(string(argv), "-p\n") {
		t.Errorf("-p flag missing from argv:\n%s", argv)
	}
	if !strings.Contains(string(argv), "--no-session-persistence") {
		t.Errorf("--no-session-persistence missing from argv:\n%s", argv)
	}
	stdin, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("stdin not captured: %v", err)
	}
	if string(stdin) != prompt {
		t.Errorf("stdin = %q, want the prompt", stdin)
	}
}

// TestRealRunClaude_PromptOverStdin: the prompt used to be an argv value
// (`-p <body>`), visible in `ps` and capped by ARG_MAX.
func TestRealRunClaude_PromptOverStdin(t *testing.T) {
	root := testKB(t, "")
	argvFile, stdinFile := installFakeClaude(t)
	prompt := "SECRET-BODY line one\nline two — with unicode ✓\n"
	ctx := context.Background()
	if _, err := realRunClaude(ctx, root, prompt, "haiku", []string{"Read"}, 30*time.Second); err != nil {
		t.Fatalf("realRunClaude: %v", err)
	}
	assertPromptOverStdin(t, argvFile, stdinFile, prompt)
}

func TestAnthropicGenerate_PromptOverStdin(t *testing.T) {
	root := testKB(t, "")
	argvFile, stdinFile := installFakeClaude(t)
	prompt := "SECRET-BODY generate me something\n"
	p := &anthropicProvider{model: "haiku", root: root}
	if _, err := p.Generate(context.Background(), prompt); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	assertPromptOverStdin(t, argvFile, stdinFile, prompt)
}

func TestTruncateBytesAndTail(t *testing.T) {
	s := "héllo wörld ✓✓✓"
	for n := 0; n <= len(s)+2; n++ {
		got := truncateBytes(s, n)
		if len(got) > n {
			t.Errorf("n=%d: len %d > n", n, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("n=%d: invalid UTF-8 %q", n, got)
		}
		if !strings.HasPrefix(s, got) {
			t.Errorf("n=%d: %q is not a prefix", n, got)
		}
		tail := tailBytes(s, n)
		if len(tail) > n || !utf8.ValidString(tail) || !strings.HasSuffix(s, tail) {
			t.Errorf("tail n=%d: %q", n, tail)
		}
	}
	if truncateBytes(s, 100) != s || tailBytes(s, 100) != s {
		t.Error("budget above length must return s unchanged")
	}
}

// TestBuildContradictionsPacket_Caps: a wide --since window used to
// inline every article whole.
func TestBuildContradictionsPacket_Caps(t *testing.T) {
	root := t.TempDir()
	articles := make([]string, 0, 15)
	for i := range 15 {
		p := filepath.Join(root, "wiki", "a"+strings.Repeat("x", i)+".md")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Repeat("é", contradictionArticleMaxChars)), 0o644); err != nil {
			t.Fatal(err)
		}
		articles = append(articles, p)
	}
	packet, dropped := buildContradictionsPacket(root, articles)
	if len(packet) > contradictionPacketMaxChars {
		t.Errorf("packet %d bytes > budget %d", len(packet), contradictionPacketMaxChars)
	}
	if dropped == 0 {
		t.Error("15 × 24 KB must not all fit in a 120 KB packet")
	}
	if !utf8.ValidString(packet) {
		t.Error("packet cut inside a rune")
	}
	if !strings.Contains(packet, "[… article truncated") {
		t.Error("per-article truncation marker missing")
	}
	if got := strings.Count(packet, "\n---\n\n### "); got+dropped != len(articles) {
		t.Errorf("included %d + dropped %d != %d articles", got, dropped, len(articles))
	}
}

func TestCapRawBody(t *testing.T) {
	body := strings.Repeat("ü", 100)
	if got := capRawBody(body, 0, "x.md"); got != body {
		t.Error("0 must disable the cap")
	}
	if got := capRawBody(body, 500, "x.md"); got != body {
		t.Error("under budget must pass through")
	}
	got := capRawBody(body, 51, "x.md")
	if !strings.HasPrefix(got, strings.Repeat("ü", 25)) || strings.HasPrefix(got, strings.Repeat("ü", 26)) {
		t.Errorf("cut at 51 bytes should keep 25 two-byte runes: %q", got[:60])
	}
	if !strings.Contains(got, "max_single_pass_chars") {
		t.Error("truncation marker must name the knob")
	}
}

func TestDreamReadLogTail_ByteCap(t *testing.T) {
	root := t.TempDir()
	lines := make([]string, 0, 20)
	for i := range 20 {
		lines = append(lines, "line"+strings.Repeat("é", 400)+"END"+string(rune('a'+i)))
	}
	if err := os.WriteFile(filepath.Join(root, "log.md"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := dreamReadLogTail(root, 20)
	if len(got) > dreamLogTailMaxBytes+64 {
		t.Errorf("tail is %d bytes, cap is %d", len(got), dreamLogTailMaxBytes)
	}
	if !strings.HasSuffix(got, "ENDt") {
		t.Errorf("newest line must survive: %q", got[len(got)-20:])
	}
	if !utf8.ValidString(got) {
		t.Error("cut inside a rune")
	}
	if !strings.HasPrefix(got, "[… earlier log text cut …]") {
		t.Error("cut marker missing")
	}
}
