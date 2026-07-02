# Issue #19: envelope/dream progress heartbeat during long LLM calls

Status: planned, not implemented. This file is self-sufficient — an
implementation agent should be able to execute it without reading the
GitHub issue or doing further codebase exploration.

## 1. Problem & context

Every long-running LLM call in scribe emits exactly one log line before
the call and one after:

```
cmd/scribe/extract_envelope.go:64   logMsg("sync", " [%s] envelope: prompt %d chars, files %d chars, num_ctx=%d", ...)
cmd/scribe/extract_envelope.go:65   out, err := generateMaybeJSON(callCtx, provider, prompt)   // blocks here
cmd/scribe/extract_envelope.go:87   logMsg("sync", " [%s] envelope: applied %d action(s), %d errors: %v", ...)
```

Between those two lines the process is silent. For `scribe dream`
(`cmd/scribe/dream.go:101`, monolithic mode, timeout up to 3600s) and
`scribe deep` (`cmd/scribe/deep.go:209` tools mode / envelope mode via
`cmd/scribe/deep_orchestrator.go:41`), that gap is routinely 2+ minutes
and can run to the full configured timeout. In a cron log there is no
way to tell "still working" from "hung" until the timeout fires —
that's the forensics gap issue #19 asks to close.

**Key finding from exploration:** most scribe operations do NOT run
through the envelope path by default. `applyDreamDefaults` (
`cmd/scribe/config_llm.go:406-420`) defaults `cfg.Dream.Mode` to
`"monolithic"` for the stock (anthropic) provider — that path calls
`runClaude` (`cmd/scribe/claude.go:31`, `realRunClaude`) directly, not
`generateMaybeJSON`. Likewise `applyDeepIngestDefaults` /
`applyExtractDefaults` / `applyAssessDefaults`
(`cmd/scribe/config_llm.go:438-471`) default to `"tools"` mode, also
via `runClaude`. Only a non-anthropic `llm.provider` (ollama / hosted)
auto-flips these to `"orchestrator"`/`"envelope"` mode
(`forceNonAnthropicMode`, `cmd/scribe/config_llm.go:384`), which is the
`generateMaybeJSON` path.

So there are two independent blocking chokepoints that both need
coverage, not one:

1. **`realRunClaude`** (`cmd/scribe/claude.go:31`) — the legacy
   tools-mode `claude -p` subprocess. Default path for
   `dream.go:101`, `deep.go:209`, `assess.go:229/260`,
   `sync_extract.go:368`, `sync_sessions.go:414/504`,
   `sync_absorb.go:642`.
2. **The three `llmProviderGenerator` implementations** — each has
   exactly one blocking call, reached from every envelope-mode driver
   (`extract_envelope.go`, `dream_orchestrator.go`,
   `deep_orchestrator.go`, `session_mine_envelope.go`,
   `sync_absorb.go` x2, `relations_migrate.go`, `assess_orchestrator.go`,
   `absorb_chapter.go` x2) via `generateMaybeJSON`
   (`cmd/scribe/llm.go:44`), and also from five call sites that call
   `provider.Generate` directly and bypass `generateMaybeJSON`
   (`contextualize.go:289`, `absorb_facts.go:213`,
   `contradictions.go:79`, `identities.go:73`, `resolve.go:107`):
   - `anthropicProvider.Generate` — `cmd/scribe/llm.go:146` (`cmd.Run()`)
   - `ollamaProvider.generate` — `cmd/scribe/llm.go:503` (`client.Do(req)`)
   - `openaiCompatProvider.generate` — `cmd/scribe/llm_openai_compat.go:275`
     (`p.doRequest(...)`, which itself does the HTTP round trip,
     including one retry — see `doRequest` at
     `cmd/scribe/llm_openai_compat.go:319`)

Instrumenting these **4 functions** (not `generateMaybeJSON` itself,
which is just a type-assertion dispatcher with no blocking call of its
own) is the minimal chokepoint set: every blocking LLM call in the
binary — legacy tools-mode and all three envelope-mode backends —
passes through exactly one of them. No call site above needs to change
at all.

`ingest.go` (mentioned in the umbrella issue as "deep ingest") does not
call an LLM — it only drains `inbox/` into `raw/articles/`
(`cmd/scribe/ingest.go`, confirmed via `rg` — no `runClaude` /
`generateMaybeJSON` / `provider.Generate` hits). "Deep ingest" in the
LLM sense is `scribe deep` (`DeepIngestConfig`, `deep.go` +
`deep_orchestrator.go`), which is already covered above.

## 2. Design decisions

### 2.1 Where heartbeat lines go: stderr, written directly, not via `logMsg`

**Decision:** heartbeat lines write straight to a package var
`heartbeatWriter io.Writer = os.Stderr`, bypassing `slog`/`logMsg`
entirely.

**Why:**
- `logMsg` (`cmd/scribe/claude.go:408`) always resolves to
  `slog.Info(...)`, and the installed handler
  (`scribeTextHandler`, `cmd/scribe/logging.go:44`) always writes to
  `os.Stdout` — there is no existing stderr logging channel in this
  codebase to reuse.
- `SCRIBE_LOG_FORMAT=json` (`cmd/scribe/logging.go:38`) makes stdout
  strict NDJSON. A heartbeat line interleaved into stdout under that
  mode would corrupt any downstream line-by-line JSON parser. Stderr
  is unambiguously safe.
- The task brief that spawned this plan explicitly calls heartbeat
  lines "forensics, not summary" and directs them to stderr — this
  matches CLAUDE.md's stated (if not, on inspection, fully realized
  elsewhere) convention that errors/forensics are stderr and cron
  captures stdout as the summary.
- **Rejected:** adding a second `slog.Logger` bound to stderr. More
  moving parts for no benefit — heartbeat lines are a fixed one-line
  format, not structured log records other tooling needs to parse.

**Cron note:** `cmd/scribe/cron.go:276-277` sets `StandardOutPath` and
`StandardErrorPath` to the *same* `LogFile` for the LaunchAgent, so
under cron, heartbeat lines still land in the same log file as
everything else — this decision only matters for interactive/CI runs
where stdout and stderr are genuinely separate streams (e.g. someone
piping `scribe sync --json` stdout through `jq`).

### 2.2 Cadence: fixed 30s, not configurable, not action-count-based

**Decision:** a single package var `heartbeatInterval = 30 * time.Second`
(not a `scribe.yaml` knob).

**Why:** the issue asks for "every ~30s (or every N actions during
apply)". Wall-clock ticking is simpler than threading an action
counter through `applyWikiActions` (which is fast, local file I/O —
not the slow part; see 2.4) and doesn't require new config surface.
Adding a `scribe.yaml` field for a cosmetic log cadence is not
justified by the problem (cron-log forensics), so it's hardcoded.
`heartbeatInterval` is a `var`, not a `const`, purely so tests can
shrink it — same seam idiom already used for `runClaude` /
`newLLMProvider` (`cmd/scribe/claude.go:29`, `cmd/scribe/llm.go:55`).

### 2.3 Ticker/goroutine lifecycle: `startHeartbeat` returns a
`stop func()` that blocks until the goroutine has actually exited

**Decision:**

```go
func startHeartbeat(ctx context.Context, op string) (stop func())
```

Internally: one goroutine running a `time.Ticker` in a `select` over
`done` (closed by `stop()`), `ctx.Done()`, and `ticker.C`. `stop()`
closes `done` and then calls `wg.Wait()` on a `sync.WaitGroup` the
goroutine signals via `defer wg.Done()` — so `stop()` does not return
until the ticker goroutine has fully exited. `stop()` is idempotent
via `sync.Once`.

**Why:**
- **No goroutine leaks, verifiably.** The task brief calls this out
  explicitly. A `wg.Wait()`-backed `stop()` gives a hard guarantee
  (not a "probably stopped soon" one) and makes it trivially testable
  without `runtime.NumGoroutine()` polling or a `goleak` dependency
  (which would violate the "no new go.mod deps" constraint).
  **Rejected:** fire-and-forget goroutine with only a `done` channel
  and no `wg.Wait()` — technically bounded by `ctx.Done()` eventually,
  but "eventually" can be the full timeout (up to 3600s for dream),
  and there's no way for a test to assert the goroutine is gone
  without sleeping or polling.
- **Double-exit condition (`done` OR `ctx.Done()`).** Every call site
  uses `defer stop()` (see 2.4), so `done` is always the normal exit
  path. `ctx.Done()` is a defensive backstop only — e.g. if a future
  call site is added that forgets the `defer`, the goroutine still
  can't outlive the call's own context deadline. Belt-and-suspenders,
  costs nothing.
- **`stop()` idempotent.** `defer stop()` plus (in `ollamaProvider`,
  see 2.4) an inline call in one function would double-call otherwise
  — `sync.Once` makes that safe without callers needing to reason
  about it.

### 2.4 Call-site pattern: `defer stop()` immediately after `startHeartbeat`, placed right before the one blocking call in each function

**Decision:** in each of the 4 functions, insert exactly:

```go
stopHeartbeat := startHeartbeat(ctx, op)
defer stopHeartbeat()
```

immediately before the blocking call, reusing the `op` variable each
function already computes for its `CostEntry` (see implementation
steps — `op` is in scope at every insertion point already).

**Why immediately before the blocking call, not at function entry:**
`ollamaProvider.generate` calls `o.ensureReady(ctx)` first, which can
itself take minutes on a first-run model pull
(`cmd/scribe/llm.go:271-276`) — but that path already emits its own
coarse progress via `logMsg("llm", "ollama pull: %s", ev.Status)`
(`cmd/scribe/llm.go:396`). Starting the heartbeat before `ensureReady`
would produce confusing overlapping "still running" / "pulling"
output for an unrelated reason (model download, not a stuck
generation). Heartbeat starts only once the actual generate/exec call
begins.

**Why `defer` and not an inline `start...; do the call; stop()`:**
`defer` guarantees `stop()` runs on every return path out of the
function (error returns, rate-limit branch, JSON-decode-failure
branch, success), without having to remember to call it at each of
those branches individually. This matches how `entry.DurationMS`
cost-ledger `defer` already works in the same functions — same idiom,
same file, no new pattern introduced.

**`applyWikiActions` is explicitly NOT instrumented.** It's local
frontmatter/file-write logic (`cmd/scribe/wiki_actions.go:327`) with
no network or subprocess call — it's fast. The "silent for 2+ minutes"
symptom in the issue is entirely the LLM call, not the apply step.
Instrumenting it would be scope creep with no forensics value.

**The 5 direct-`provider.Generate` call sites that bypass
`generateMaybeJSON`** (`contextualize.go`, `absorb_facts.go`,
`contradictions.go`, `identities.go`, `resolve.go`) get heartbeat
coverage **for free** because the instrumentation lives inside the
provider implementations, not at the `generateMaybeJSON` dispatch
layer. This was the deciding factor for instrumenting the 4
provider/exec functions instead of wrapping `generateMaybeJSON` itself
— it's the same number of edits (arguably fewer, since
`generateMaybeJSON` would still need `realRunClaude` covered
separately) and it closes a gap the issue title doesn't explicitly
scope out but that "envelope/dream...long LLM calls" reasonably
implies (absorb-facts / contextualize prompts are shorter today, but
nothing prevents a future large-batch call from stalling there too).

### 2.5 Heartbeat line format

```
[2026-07-02 15:04:35] heartbeat: [dream] still running (90s elapsed, 1110s remaining)
```

or, when `ctx` has no deadline (shouldn't happen in practice — every
call site already wraps with `context.WithTimeout` before reaching
one of the 4 instrumented functions, see step-by-step confirmation in
section 3 — but handled defensively):

```
[2026-07-02 15:04:35] heartbeat: [dream] still running (90s elapsed)
```

`op` of `""` (untagged context) renders as `[unlabeled]` rather than
`[]`, so a grep for `heartbeat:` never returns a visually broken line.

**Why include "remaining":** the timeout value is exactly the number
someone doing cron forensics wants — "is this about to time out, or
does it have 20 more minutes" — and it's free: every instrumented
function's `ctx` already carries the deadline the caller set via
`context.WithTimeout`, so no new parameter threading is needed.

### 2.6 No behavior change to LLM calls

Heartbeat is purely additive I/O on a side goroutine. It does not
touch `ctx`, does not wrap/replace the `cmd.Run()` / `client.Do()`
/ `doRequest()` return values, and does not change any error path,
retry, or cost-ledger logic. Confirmed by construction: the diff at
each of the 4 sites is a 2-line insertion (`start...` / `defer
stop...`) with zero changes to surrounding lines.

## 3. Implementation steps

### 3.1 New file: `cmd/scribe/heartbeat.go`

```go
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// heartbeatInterval is how often a long-running LLM call logs a
// "still running" forensic line. Package var (not const) purely so
// tests can shrink it — production code never reassigns it. Mirrors
// the newLLMProvider / runClaude seam idiom (llm.go:55, claude.go:29).
var heartbeatInterval = 30 * time.Second

// heartbeatWriter is where heartbeat lines land. Deliberately stderr,
// separate from logMsg's stdout stream: stdout can be piped through
// SCRIBE_LOG_FORMAT=json as strict NDJSON, and an interleaved
// heartbeat line would break that parse. Heartbeat lines are
// forensics for a stuck/slow cron run, not part of the run summary.
// Package var so tests capture output without racing on the real
// os.Stderr.
var heartbeatWriter io.Writer = os.Stderr

// startHeartbeat begins logging a "still running" line to
// heartbeatWriter every heartbeatInterval. op is the withOpLabel
// value the caller already computed for its cost-ledger entry (e.g.
// "dream", "deep-extract", "extract") — reused here instead of adding
// a second labeling mechanism.
//
// Every call site MUST call the returned stop func via defer,
// immediately after calling startHeartbeat and immediately before the
// blocking call being watched (see llm.go / claude.go /
// llm_openai_compat.go for the four call sites). stop() blocks until
// the background goroutine has actually exited — not just been asked
// to — so there is no window after a call site returns where a
// heartbeat goroutine could still be running. stop() is idempotent
// and safe to call more than once.
//
// The background goroutine also exits on ctx.Done() as a backstop in
// case a future call site forgets the defer: it can never outlive the
// call's own timeout, only the (much shorter) heartbeat-tick
// granularity past it.
func startHeartbeat(ctx context.Context, op string) (stop func()) {
	start := time.Now()
	deadline, hasDeadline := ctx.Deadline()
	done := make(chan struct{})
	var closeOnce sync.Once
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				writeHeartbeatLine(op, now, start, deadline, hasDeadline)
			}
		}
	}()

	return func() {
		closeOnce.Do(func() { close(done) })
		wg.Wait()
	}
}

// writeHeartbeatLine formats and writes one heartbeat line. Split out
// from startHeartbeat's goroutine body so it's directly unit-testable
// without ticker timing.
func writeHeartbeatLine(op string, now, start time.Time, deadline time.Time, hasDeadline bool) {
	label := op
	if label == "" {
		label = "unlabeled"
	}
	elapsed := now.Sub(start).Round(time.Second)
	ts := now.Format("2006-01-02 15:04:05")
	if hasDeadline {
		remaining := deadline.Sub(now).Round(time.Second)
		fmt.Fprintf(heartbeatWriter, "[%s] heartbeat: [%s] still running (%s elapsed, %s remaining)\n",
			ts, label, elapsed, remaining)
		return
	}
	fmt.Fprintf(heartbeatWriter, "[%s] heartbeat: [%s] still running (%s elapsed)\n",
		ts, label, elapsed)
}
```

### 3.2 `cmd/scribe/claude.go` — `realRunClaude`

Current (lines 85-89):
```go
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = root
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
```
(`op` is already computed at line 67, before this block, and is in
scope here.)

Change to:
```go
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = root
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	stopHeartbeat := startHeartbeat(ctx, op)
	defer stopHeartbeat()
	err := cmd.Run()
```
`ctx` here is the timeout-bounded one reassigned at line 43
(`ctx, cancel := context.WithTimeout(ctx, timeout)`), so
`ctx.Deadline()` is always present.

### 3.3 `cmd/scribe/llm.go` — `anthropicProvider.Generate`

Current (lines 142-146):
```go
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
```
(`op` computed at line 129, in scope.)

Change identically to 3.2 (insert the two heartbeat lines directly
before `err := cmd.Run()`).

Note: `anthropicProvider.Generate` does **not** create its own
`context.WithTimeout` — it uses whatever `ctx` the caller passed. All
current callers already wrap with a timeout before calling in
(`extract_envelope.go:61`, `dream_orchestrator.go:52`,
`deep_orchestrator.go:39`, `session_mine_envelope.go:71`,
`sync_absorb.go:249/661`, `relations_migrate.go:490`,
`assess_orchestrator.go:55`, `absorb_chapter.go:62/241`). The five
direct-`Generate` callers that bypass `generateMaybeJSON` also each
wrap their own `context.WithTimeout` before calling: `contextualize.go:284`
(`context.WithTimeout(context.Background(), timeout)`, `timeout` from
`cfg.Absorb.Contextualize.TimeoutSec`), `absorb_facts.go:212`
(`context.WithTimeout(withOpLabel(gctx, "absorb-facts"), timeout)`),
`contradictions.go:77` (3 min), `identities.go:71` (3 min),
`resolve.go:105` (5 min). `hasDeadline` will be `true` in every real
call path; the `false` branch exists purely as a defensive fallback
(e.g. a unit test calling `Generate` with `context.Background()`, as
`llm_test.go` already does for the ollama provider).

### 3.4 `cmd/scribe/llm.go` — `ollamaProvider.generate`

Current (lines 494-503):
```go
	// No client-level timeout: every Ollama caller already bounds the
	// request via context.WithTimeout (dream 20m, session-mine 8m,
	// deep 10m, assess 10m, absorb-pass2 25m, …). A separate 10-min
	// client cap silently overrode those — with Stream: false the
	// /api/generate call buffers the whole response, so the client
	// timeout has to cover cold-load + full generation. Trusting the
	// per-op context is both correct and more honest about which
	// knob the user should tune.
	client := &http.Client{}
	resp, err := client.Do(req)
```
(`op` computed at line 473, in scope. This is the *second*
`client.Do(req)` in the file — the first, at line 296, is inside
`listedModels`'s `/api/tags` health check, which has its own 5s
client timeout and must NOT be instrumented; it's a fast liveness
probe, not the long generation call.)

Change to:
```go
	client := &http.Client{}
	stopHeartbeat := startHeartbeat(ctx, op)
	defer stopHeartbeat()
	resp, err := client.Do(req)
```

### 3.5 `cmd/scribe/llm_openai_compat.go` — `openaiCompatProvider.generate`

Current (line 275, inside `generate`, after the `entry`/cost-ledger
`defer` block that starts around line 260):
```go
	out, resp, err := p.doRequest(ctx, prompt, jsonMode)
```
(`op` computed at line 259, in scope.)

Change to:
```go
	stopHeartbeat := startHeartbeat(ctx, op)
	defer stopHeartbeat()
	out, resp, err := p.doRequest(ctx, prompt, jsonMode)
```

This covers `doRequest`'s internal one-retry-without-`response_format`
path (`llm_openai_compat.go:338-345`) as a single heartbeat span,
which is correct — from the caller's point of view it's one logical
"generate" call regardless of the internal retry.

## 4. Test plan

New file `cmd/scribe/heartbeat_test.go`. All tests are self-contained
(no real `claude` binary, no real Ollama server, no `t.Parallel()` —
matches the existing package convention of swapping package-level
vars, so parallel heartbeat tests would race on `heartbeatInterval` /
`heartbeatWriter` the same way existing stub-provider tests avoid
`t.Parallel()` per `llm_stub_test.go:25`).

| Test | Setup | Assertion |
|---|---|---|
| `TestStartHeartbeat_EmitsLines` | `heartbeatInterval = 5ms` (saved/restored via `t.Cleanup`); `heartbeatWriter` swapped to a `*bytes.Buffer` (saved/restored). `ctx` = `context.Background()`. `stop := startHeartbeat(ctx, "dream")`; `time.Sleep(23 * time.Millisecond)`; `stop()`. | Buffer contains `"heartbeat:"` and `"[dream]"` at least once. `stop()` returned (test doesn't hang — enforced by Go's default test timeout). |
| `TestStartHeartbeat_StopIsSynchronous` | Same shrink as above. `stop := startHeartbeat(ctx, "x")`; call `stop()` immediately (before any tick fires). | Returns without hanging; buffer is empty (no premature tick). Proves `stop()` doesn't block waiting for a tick that will never come — `wg.Wait()` only waits for goroutine *exit*, not for a tick. |
| `TestStartHeartbeat_StopIsIdempotent` | Shrink interval. `stop := startHeartbeat(ctx, "x")`; call `stop()` twice. | No panic (proves `sync.Once` guards `close(done)`). |
| `TestStartHeartbeat_ExitsOnContextCancel` | Shrink interval. `ctx, cancel := context.WithCancel(context.Background())`; `_ = startHeartbeat(ctx, "x")`; `cancel()`; sleep briefly; then call `stop()` (captured before cancel). | `stop()` returns promptly (goroutine already exited via `ctx.Done()` branch) — proves the backstop path works even without the caller's `defer stop()` firing first. |
| `TestWriteHeartbeatLine_WithDeadline` | Call `writeHeartbeatLine("extract", now, start, deadline, true)` directly with fixed `time.Time` values into a swapped `heartbeatWriter`. | Output matches the exact expected string, including "elapsed" and "remaining" substrings and rounded durations. |
| `TestWriteHeartbeatLine_NoDeadline` | Call with `hasDeadline=false`. | Output contains "elapsed" but not "remaining". |
| `TestWriteHeartbeatLine_UnlabeledOp` | Call with `op=""`. | Output contains `[unlabeled]`, not `[]`. |

**Regression check (no new test files needed):** run the full existing
suite — `make test`. None of `llm_test.go`, `llm_openai_compat_test.go`,
`llm_stub_test.go`, `deep_run_test.go`,
`sync_absorb_dense_test.go`'s existing cases should change behavior or
timing, because:
- `heartbeatInterval` default (30s) never fires during fast
  `httptest`-backed or stub-backed test calls (all complete in
  milliseconds).
- The 4 edits are pure insertions with no control-flow change.

Explicitly verify after implementing:
```sh
make test   # go test ./... -tags sqlite_fts5 — must stay network-free and pass
make check  # test + vet
```
Also run `go test ./... -tags sqlite_fts5 -race -run TestStartHeartbeat` /
`-run 'TestOllama|TestOpenAICompat|TestDeepRun|TestAbsorbDense'` once
with `-race` specifically, since this is the one change in the PR that
adds real concurrency (a background goroutine per LLM call) — worth
confirming the race detector is clean even though CI's default `make
test` may not run under `-race`.

## 5. Risks & edge cases

- **Goroutine leak if a call site adds a new blocking call without
  `defer stop()`.** Mitigated by the `ctx.Done()` backstop (2.3) —
  worst case the heartbeat outlives the call by nothing, since
  `ctx.Done()` fires at exactly the deadline the call itself is bound
  by; there's no unbounded leak. Code review should still catch a
  missing `defer stop()` at any future 5th call site — flag in PR
  description that this pattern must be copied exactly.
- **Interleaved output from concurrent calls.** `absorb_chapter.go:242`
  and `sync_sessions.go` batch paths run multiple LLM calls in
  parallel goroutines (errgroup). Each will start its own heartbeat
  goroutine, all writing to the same `heartbeatWriter`. A single
  `fmt.Fprintf` call is one `Write()` to the underlying `io.Writer`;
  for `os.Stderr` (a regular file/pipe fd) this is line-safe in
  practice for the same reason `scribeTextHandler.Handle`
  (`logging.go:90`, `fmt.Fprintln(os.Stdout, line)`) is already
  trusted not to interleave mid-line elsewhere in this codebase — no
  new risk class introduced, existing precedent followed. Not adding
  a mutex around the writer to stay consistent with that precedent;
  revisit only if garbled output is actually observed.
- **`ctx` without a deadline in a future call site.** Handled
  gracefully (2.5, "no deadline" branch) — never panics, just omits
  "remaining".
- **Ollama first-run model pull mistaken for a hang.** Explicitly
  scoped out (2.4) — `ensureReady`'s own pull-progress logging
  already covers that case; heartbeat starts only after `ensureReady`
  returns.
- **`heartbeatInterval` / `heartbeatWriter` are mutable package
  globals.** Same risk class as the existing `runClaude` /
  `newLLMProvider` seams — acceptable because tests never run
  `t.Parallel()` in this package when touching these seams (existing
  convention, `llm_stub_test.go:25`).

## 6. Interactions with other open issues

- **#4 (DX umbrella)** — this issue is explicitly part of it; no
  ordering dependency on other umbrella children found during
  exploration.
- **#43 (hosted OpenAI-compatible providers)** — already merged/landed
  (referenced in `llm_openai_compat.go`'s file-header comment as
  existing functionality, not a pending change) — `openaiCompatProvider`
  already exists and is one of the 4 instrumentation points; no
  conflict, this plan builds on top of it.
- No other open issue was found (via `rg` across `cmd/scribe/*.go`
  comments and `docs/issues-master-plan.md`) touching `llm.go`,
  `claude.go`, or `llm_openai_compat.go`'s blocking-call sites. Safe to
  implement independently of #42, #5, #27, or any other Phase-1 issue
  in the batch this plan was requested alongside.

## 7. Size estimate

**S** (small). One new file (~90 LOC including comments) + one new
test file (~90 LOC) + four 2-line insertions in three existing files.
Total: **~180 LOC added, 8 LOC changed** (the 4 x 2-line insertions).
No new dependencies, no config schema changes, no changes to any
prompt template, no changes to `applyWikiActions` or any envelope
parsing.
