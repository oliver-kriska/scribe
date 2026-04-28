package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

//go:embed prompts/*.md
var promptFS embed.FS

// runClaude invokes `claude -p` with the given prompt and returns the output.
// ErrRateLimit is returned when claude -p hits Anthropic rate limits.
var ErrRateLimit = fmt.Errorf("rate limit hit")

func runClaude(ctx context.Context, root, prompt, model string, tools []string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"-p", prompt,
		"--no-session-persistence",
		"--add-dir", root,
		"--model", model,
		// Disable all hooks to avoid noise and SessionEnd failures in headless mode.
		"--settings", `{"hooks":{}}`,
		// Phase 3D.5: structured output gives us real token counts
		// and total_cost_usd from the result envelope. Falls back to
		// the text-mode classifier if the JSON parse fails (e.g.
		// older claude CLI without the flag, or claude crashed
		// before emitting an envelope).
		"--output-format", "json",
	}
	if len(tools) > 0 {
		args = append(args, "--allowedTools", strings.Join(tools, ","))
	}

	// Phase 3D: start timer for the cost ledger. The deferred append
	// fires regardless of return path so even errors get recorded.
	started := time.Now()
	op := opLabelFromContext(ctx)
	entry := CostEntry{
		Timestamp:   started.UTC().Format(time.RFC3339),
		Model:       model,
		Op:          op,
		PromptChars: len(prompt),
	}
	defer func() {
		entry.DurationMS = time.Since(started).Milliseconds()
		appendCostEntry(root, entry)
	}()

	// Capture stdout (the JSON envelope) and stderr (banner / tool
	// noise / rate-limit messages from claude itself) separately so
	// JSON parsing isn't poisoned by stderr lines. Combined view
	// stays available for legacy text-mode fallback.
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = root
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	stdoutStr := stdoutBuf.String()
	stderrStr := stderrBuf.String()
	combined := stdoutStr + "\n" + stderrStr

	// Rate-limit text detection runs first across the combined
	// output. Some rate-limit responses come back as well-formed
	// JSON envelopes with is_error=true; others crash claude before
	// it emits one. The text matcher is the safety net.
	if isRateLimited(combined) {
		entry.OK = false
		entry.ErrKind = "rate_limit"
		return tailLines(combined, 5), ErrRateLimit
	}

	if err != nil {
		entry.OK = false
		switch ctx.Err() {
		case context.DeadlineExceeded:
			entry.ErrKind = "timeout"
		case context.Canceled:
			entry.ErrKind = "canceled"
		default:
			entry.ErrKind = "other"
		}
		return tailLines(combined, 15), fmt.Errorf("claude -p: %w\n%s", err, tailLines(combined, 15))
	}

	// Try to parse the JSON envelope. claude -p --output-format json
	// emits one top-level object with usage and cost fields.
	var env claudeResultEnvelope
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(stdoutStr)), &env); jsonErr == nil && env.Type == "result" {
		// Capture token / cost numbers regardless of success/error
		// status — even rate-limited or partial calls bill some
		// input tokens and the user wants to see them.
		if env.Usage.InputTokens > 0 {
			in := env.Usage.InputTokens
			entry.InputTokens = &in
		}
		if env.Usage.OutputTokens > 0 {
			out := env.Usage.OutputTokens
			entry.OutputTokens = &out
		}
		if env.Usage.CacheReadInputTokens > 0 {
			c := env.Usage.CacheReadInputTokens
			entry.CacheReadTokens = &c
		}
		if env.TotalCostUSD > 0 {
			cost := env.TotalCostUSD
			entry.CostUSD = &cost
		}

		if env.IsError {
			entry.OK = false
			if isRateLimitSubtype(env.Subtype) {
				entry.ErrKind = "rate_limit"
				return env.Result, ErrRateLimit
			}
			entry.ErrKind = "other"
			return env.Result, fmt.Errorf("claude -p: %s", env.Subtype)
		}

		entry.OK = true
		return env.Result, nil
	}

	// JSON parse failed. Either the CLI doesn't support
	// --output-format json on this system or the output got
	// mangled. Treat as text-mode success — the call exited 0 and
	// no rate-limit signal — and let summarizeCosts fall back to
	// char-based estimates for this row.
	entry.OK = true
	return tailLines(stdoutStr, 15), nil
}

// isRateLimited checks if claude output indicates a rate limit.
//
// String matching is the only reliable signal short of switching to
// claude -p --output-format json (deferred to a future phase). The
// list below covers what's been observed in practice from the CLI
// across 2025-2026: classic "rate limit", HTTP 429 echoes, the
// Anthropic CLI's own quota-message variants ("usage limit",
// "5-hour limit", "weekly limit"), and the catch-all "overloaded"
// for transient API capacity. The check is case-insensitive.
//
// False positives here cost a single retry; false negatives let
// errors land in the ledger as err_kind=other and pollute the
// "other" bucket. Bias toward false positives.
func isRateLimited(output string) bool {
	lower := strings.ToLower(output)
	for _, needle := range []string{
		"rate limit",
		"too many requests",
		"429",
		"overloaded",
		"usage limit",
		"5-hour limit",
		"5 hour limit",
		"weekly limit",
		"quota exceeded",
		"resource_exhausted",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// tailLines returns the last n non-empty lines from a string.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// loadPrompt reads an embedded prompt template and substitutes {{KEY}} placeholders.
func loadPrompt(name string, vars map[string]string) (string, error) {
	data, err := promptFS.ReadFile("prompts/" + name)
	if err != nil {
		return "", fmt.Errorf("load prompt %s: %w", name, err)
	}
	result := string(data)
	for k, v := range vars {
		result = strings.ReplaceAll(result, "{{"+k+"}}", v)
	}
	return result, nil
}

// runCmd runs a command and returns its trimmed stdout. Returns empty string on error.
// Synchronous by design — used by callers that have no ambient context (cron helpers,
// gitops, quick qmd status checks). If a caller needs cancellation, it can bypass this
// wrapper and use exec.CommandContext directly.
func runCmd(dir string, name string, args ...string) string {
	cmd := exec.Command(name, args...) //nolint:noctx // sync wrapper; see doc comment
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runCmdErr runs a command and returns stdout + error.
// See runCmd doc for why this is non-context.
func runCmdErr(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...) //nolint:noctx // sync wrapper; see doc comment
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// logMsg prints a timestamped log message via log/slog.
//
// Kept as an adapter so the ~150 existing call sites don't need to move to
// slog-native key/value form at once — the handler (see main.go) gives the
// whole codebase slog's AddSource + JSON-capable output via one switch.
// New call sites should use slog.Info/Warn/Error directly with key/value
// pairs for structured output.
func logMsg(script, format string, args ...any) {
	slog.Info(fmt.Sprintf(format, args...), "script", script)
}

// fileExists checks if a path exists and is a file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// dirExists checks if a path exists and is a directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
