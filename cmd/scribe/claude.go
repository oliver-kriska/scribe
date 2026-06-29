package main

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	gosync "sync"
	"time"
)

//go:embed prompts/*.md
var promptFS embed.FS

// runClaude invokes `claude -p` with the given prompt and returns the output.
// ErrRateLimit is returned when claude -p hits Anthropic rate limits.
var ErrRateLimit = fmt.Errorf("rate limit hit")

// runClaude is a package variable (pointing at realRunClaude) purely so
// driver tests can swap in a scripted stub (see llm_stub_test.go) without
// shelling out to a real `claude` binary; production code never reassigns it.
var runClaude = realRunClaude

func realRunClaude(ctx context.Context, root, prompt, model string, tools []string, timeout time.Duration) (string, error) {
	// Daily metered output-token ceiling: read once per call so a
	// long-running absorb that crosses the ceiling mid-run aborts at
	// the next claude -p invocation rather than barreling through.
	// Falls open (no error) when the knob is zero or the env bypass
	// is set — same shape as the rate-limit safety net below.
	if cfg := loadConfig(root); cfg != nil {
		if err := checkBudget(root, effectiveOutputTokenCeiling(cfg.Sync)); err != nil {
			return "", err
		}
	}

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
		Provider:    "anthropic",
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

	// Rate-limit text detection runs first as a safety net for cases
	// where claude crashes before emitting a JSON envelope. We scan
	// **stderr only** — never stdout. Stdout carries the model's
	// response (the result string, the JSON envelope), and ~10% of
	// real-world articles in this corpus discuss rate-limiting as a
	// topic ("HTTP 429", "rate limit", "quota exceeded"). Matching
	// against that content produced massive false positives that
	// stranded entire absorb/contextualize runs. Genuine API
	// rate-limit responses come back as JSON envelopes with
	// is_error=true and a rate-limit subtype, which is handled
	// structurally below. CLI-banner rate-limit messages from the
	// claude binary itself land in stderr — that's what we still
	// match here.
	if isRateLimited(stderrStr) {
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
		// Stash the full tails in output/errors/ so the terminal can stay
		// terse while debugging still has the context it needs.
		appendErrorRecord(root, ErrorRecord{
			Timestamp:   started.UTC().Format(time.RFC3339),
			Op:          op,
			Model:       model,
			ErrKind:     entry.ErrKind,
			DurationMS:  time.Since(started).Milliseconds(),
			PromptChars: len(prompt),
			Err:         err.Error(),
			StderrTail:  tailLines(stderrStr, 50),
			StdoutTail:  tailLines(stdoutStr, 50),
		})
		return tailLines(combined, 15), fmt.Errorf("claude -p: %w\n%s", err, tailLines(combined, 15))
	}

	// Try to parse the JSON envelope. claude -p --output-format json
	// emits one top-level object on a single line, but hooks
	// (SessionEnd, CMUX, plugin postlude) can leak text on
	// surrounding lines that breaks a whole-buffer parse. Scan
	// line-by-line for the first line that starts with `{` and
	// unmarshals as a result envelope.
	env, ok := parseClaudeResult(stdoutStr)
	if ok {
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
			appendErrorRecord(root, ErrorRecord{
				Timestamp:   started.UTC().Format(time.RFC3339),
				Op:          op,
				Model:       model,
				ErrKind:     entry.ErrKind,
				DurationMS:  time.Since(started).Milliseconds(),
				PromptChars: len(prompt),
				Err:         "claude -p subtype=" + env.Subtype,
				StderrTail:  tailLines(stderrStr, 50),
				StdoutTail:  tailLines(stdoutStr, 50),
			})
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
// **Caller contract: only call on stderr, never on stdout or the model's
// result content.** A meaningful slice of any tech corpus discusses
// rate-limiting as a topic ("HTTP 429", "rate limit exceeded",
// "quota exceeded"), so feeding stdout — which carries the model's
// response — through this matcher produces catastrophic false positives
// that strand entire absorb/contextualize runs. Genuine API rate-limit
// responses surface structurally via the JSON envelope (is_error=true
// + rate-limit subtype) and are handled separately by callers; this
// function is only a safety net for CLI-banner errors that arrive on
// stderr before the JSON envelope is emitted.
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
// "other" bucket. Bias toward false positives — but only over
// stderr, where false positives are rare.
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
	// Centralized placeholder guard. A `{{VAR}}` the caller never
	// supplied is always a caller/prompt contract bug (e.g. session-
	// extract referencing {{DOMAIN}} that runSessionEnvelopeOnce never
	// puts in vars). Left intact it ships verbatim to the model, which
	// dutifully echoes it into paths and frontmatter — the literal
	// `projects/{{DOMAIN}}/…` / `domain: {{DOMAIN}}` corruption class.
	// Strip rather than error: a missing substitution should degrade the
	// prompt, not abort the run, and the frontmatter clamp turns the
	// resulting empty value into a valid `general`. Log so the broken
	// (prompt, caller) pair is findable.
	if leftover := promptPlaceholderRE.FindAllString(result, -1); len(leftover) > 0 {
		result = promptPlaceholderRE.ReplaceAllString(result, "")
		logMsg("prompt", "%s: stripped %d unsubstituted placeholder(s): %s", name, len(leftover), strings.Join(uniqueStrings(leftover), " "))
	}
	return result, nil
}

// promptPlaceholderRE matches a residual {{NAME}} substitution token
// (uppercase, digits, underscore — the convention every prompt var
// follows). Prompts never contain literal double-brace text outside
// substitution tokens, so a post-substitution match is unambiguously an
// unfilled var.
var promptPlaceholderRE = regexp.MustCompile(`\{\{[A-Z0-9_]+\}\}`)

// uniqueStrings returns the input with duplicates removed, order
// preserved. Keeps the placeholder log line terse when the same token
// (e.g. {{DOMAIN}}) appears many times in one prompt.
func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// promptForProvider picks between `<base>-anthropic.md` and
// `<base>-ollama.md` based on the provider name. Established in
// docs/100-percent-ollama-plan.md decision 3: each provider gets its
// own prompt variant because Ollama benefits from explicit JSON-only
// caps and a smaller context budget while Anthropic prompts can stay
// narrative.
//
// Falls back to `<base>.md` if neither variant is embedded — keeps the
// pre-Phase-4A.2 prompts loadable during the migration without forcing
// every task to land its pair atomically. The fallback is logged once
// per (base, provider) pair when ollama is the caller — the
// anthropic-tuned legacy prompts lack the "OUTPUT ONLY JSON" caps and
// produce noisier output through local models.
func promptForProvider(base, provider string) string {
	suffix := "-anthropic.md"
	if strings.EqualFold(provider, "ollama") {
		suffix = "-ollama.md"
	}
	candidate := base + suffix
	if _, err := promptFS.ReadFile("prompts/" + candidate); err == nil {
		return candidate
	}
	if strings.EqualFold(provider, "ollama") {
		warnPromptFallbackOnce(base)
	}
	return base + ".md"
}

// warnPromptFallbackOnce emits exactly one log line per base prompt
// per process when the ollama-specific variant is missing. Keeps the
// signal in the sync log without flooding when the same prompt is
// reused across N parallel sessions.
var (
	promptFallbackOnceMu gosync.Mutex
	promptFallbackOnce   = map[string]bool{}
)

func warnPromptFallbackOnce(base string) {
	promptFallbackOnceMu.Lock()
	defer promptFallbackOnceMu.Unlock()
	if promptFallbackOnce[base] {
		return
	}
	promptFallbackOnce[base] = true
	logMsg("llm", "prompts/%s-ollama.md missing — falling back to %s.md (anthropic-tuned). Add the ollama variant for cleaner JSON output.", base, base)
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

// runCmdRaw runs a command and returns stdout byte-exact: no trimming,
// no stderr merge. Reach for this whenever the output is column- or
// byte-sensitive — runCmd's TrimSpace eats a leading status column on
// the first porcelain line and would corrupt blob content.
func runCmdRaw(dir string, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...) //nolint:noctx // sync wrapper; see runCmd doc
	if dir != "" {
		cmd.Dir = dir
	}
	return cmd.Output()
}

// runCmdErr runs a command and returns stdout + error.
// See runCmd doc for why this is non-context.
func runCmdErr(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...) //nolint:noctx // sync wrapper; see doc comment
	if dir != "" {
		cmd.Dir = dir
	}
	// When the spawned command is scribe itself (most callers — lint,
	// scan, index, backlinks, sections build, contradictions build,
	// triage, etc.), suppress the auto-flip config log lines in the
	// child so the parent's sync log doesn't echo the same 6 lines per
	// subprocess. Plain logMsg output from the child is unaffected;
	// only logAutoFlipOnce respects this env var.
	if filepath.Base(name) == "scribe" {
		cmd.Env = append(os.Environ(), "SCRIBE_QUIET_CONFIG=1")
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
