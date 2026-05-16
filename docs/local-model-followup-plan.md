# Local-mode follow-up plan — fixing the five remaining items

Status: **draft**
Filed: 2026-05-13
Owner: Oliver
Parent: [[local-model-support-plan]] (Phase 4B layer 2 e2e validated 2026-05-13)

This plan picks up where `local-model-support-plan.md` ends. Phase 4B
layer 2 wiring shipped in commit `125b88a` — local pass-2 works on
gemma3:27b with 10/10 parse rate and 21/22 cited fact-IDs grounded in
the real fact pool. The five items below close out the loose ends and
prepare for Phase 4C.

Items are ordered by dependency, not size. Item 5 cannot ship before
item 2. Items 1, 2, 3, 4 are independent of each other; pick any
order based on appetite.

---

## Item 1 — Fact-ID validator (strip fabricated `[c00-fN]` brackets)

### Problem

`gemma3:27b` (and to a lesser extent qwen / mistral) sometimes invents
fact-ID brackets that do not correspond to any row in
`output/facts/<slug>.json`. The 2026-05-13 validation run produced 22
unique cited IDs across 11 wiki articles; **21 matched the pool, 1 was
fabricated** (`c12-f6` — chapter 12 doesn't exist; the article has
only 12 chapters indexed 0–11).

Without grounding, citations become decorative noise. The Phase 3B.5
audit story ("a later pass audits the wiki against the source") only
works if every `[c00-fN]` token corresponds to a real anchor.

Two prior attempts in the prompt ("Do NOT fabricate IDs when the facts
block is empty") have been ignored by every local model tested.
Prompt is the wrong layer — fix this in Go.

### Design

Defense-in-depth, Go side, runs between `parseEnvelope` and
`applyWikiActions`:

```go
// fact_citations.go
//
// Strips fabricated [cNN-fM] brackets from envelope content before the
// actions hit disk. A "real" ID is one that appears in the merged
// facts file for this raw article (output/facts/<slug>.json) — i.e.
// produced by the actual facts pass. Anything else is the model
// completing the citation pattern from training data.
//
// Strategy is strip-not-fail because the bracket is purely a
// downstream-audit footer. The blockquote + "— Source: <file>" is
// still a valid citation without it. Losing the whole wiki article
// over a stray bracket is worse than dropping the bracket.

var factIDRE = regexp.MustCompile(`\s*\[c\d+-f\d+\]`)

func stripUnknownFactIDs(content string, valid map[string]bool) (cleaned string, strippedIDs []string) {
    return factIDRE.ReplaceAllStringFunc(content, func(m string) string {
        id := strings.Trim(m, " []")
        if valid[id] {
            return m
        }
        strippedIDs = append(strippedIDs, id)
        return ""
    }), strippedIDs
}
```

Build the valid set from `MergedFacts.Facts[*].ID` (already loaded by
pass-2 via `loadMergedFacts`). When facts pass didn't run, the map is
empty → every bracket strips. That matches the prompt's "drop the
bracket entirely" instruction the models keep ignoring.

### Wiring

In `sync.go` json branch, before `applyWikiActions(...)`:

```go
if mergedFacts != nil {
    validIDs := map[string]bool{}
    for _, f := range mergedFacts.Facts {
        validIDs[f.ID] = true
    }
    var totalStripped int
    for i, a := range env.Actions {
        if a.Content == "" {
            continue
        }
        cleaned, stripped := stripUnknownFactIDs(a.Content, validIDs)
        env.Actions[i].Content = cleaned
        totalStripped += len(stripped)
    }
    if totalStripped > 0 {
        logMsg("sync", "pass2 entity %q: stripped %d fabricated fact-ID bracket(s)", ent.Label, totalStripped)
    }
}
```

### Files

- new: `cmd/scribe/fact_citations.go` (regex + `stripUnknownFactIDs`)
- new: `cmd/scribe/fact_citations_test.go`
- edit: `cmd/scribe/sync.go` — call site in the json branch (~10 lines)

### Tests

1. Strip when facts map is empty (every bracket goes)
2. Keep when ID is in map (no-op)
3. Mixed input — keep real, strip fake, preserve surrounding text
4. Multiple brackets on one line
5. Bracket spanning a line boundary (regex shouldn't cross newlines)
6. Pathological cases: `[c00-f0]`, `[c99-f999]`, malformed `[c0-f]` (don't match)

### Risks

- **Regex over-match**: `[c1-f2]` could be a legitimate markdown
  reference (low probability — fact-ID syntax is specific). Mitigation:
  the regex requires `\d+-f\d+`, not just letters.
- **Cost**: one regex scan per action.Content. Negligible for ~80-line
  articles.

### Estimate

~60 lines code + ~80 lines test. Half a session.

---

## Item 2 — Daily output-token ceiling for anthropic calls

### Problem

The 2026-05-11 run produced 7M output tokens in 35 hours, draining
~12% of the weekly Claude Max quota and ~56% of a 4-hour session
before wake-up. The trigger was a benign config combination
(chaptered absorb + facts pass + un-throttled cron). Heavy crons have
been unloaded ever since.

We need a hard backstop so this can't happen again — a circuit
breaker that aborts further anthropic calls once a configurable
output-token ceiling is crossed for the current day.

### Design

In-process gauge backed by `output/costs/<today>.jsonl`. Refreshed on
process start and every N seconds during a run. Local-provider calls
are exempt (no anthropic cost).

```go
// budget.go
//
// Daily anthropic-output-token ceiling. Read on every claude -p call
// and refreshed every 30s during long runs. When over the ceiling
// scribe aborts further anthropic calls with ErrDailyBudgetExhausted
// — the caller (sync, absorb) treats it like a rate-limit: commit
// whatever's done, exit clean, let the next day's run pick up.
//
// Local provider calls (ollama, future llama.cpp) bypass the check.
// The ceiling is about Anthropic quota, not LLM work in general.
//
// Env: SCRIBE_BYPASS_BUDGET=1 bypasses for emergency one-offs (e.g.
// "I know I'm over but I need to absorb this one article now").

type budgetState struct {
    mu          sync.Mutex
    lastRefresh time.Time
    usedTokens  int64
    limit       int64
}

var ErrDailyBudgetExhausted = errors.New("daily anthropic output-token ceiling reached")

func checkBudget(root string, limit int64) error {
    if limit <= 0 || os.Getenv("SCRIBE_BYPASS_BUDGET") == "1" {
        return nil
    }
    used := getDailyUsed(root)
    if used >= limit {
        return fmt.Errorf("%w: used %d / limit %d", ErrDailyBudgetExhausted, used, limit)
    }
    return nil
}
```

### Config surface

```yaml
sync:
  daily_anthropic_output_token_ceiling: 2_000_000  # 0 = disabled (default)
```

Sensible production default for Oliver's KB after the 2026-05-11
incident: **2M output tokens/day** (≈ 30% of the runaway).

### Wiring

Two call sites:

1. Top of `runClaude` (claude.go) — before exec
2. Top of `anthropicProvider.Generate` (llm.go) — before exec

Both return `ErrDailyBudgetExhausted` if over. Sync's outer loop maps
that to "commit progress, log, exit 0". Crons retry the next day; the
ceiling rolls over because the filename is date-stamped.

### Files

- new: `cmd/scribe/budget.go` (~80 lines)
- new: `cmd/scribe/budget_test.go` (~120 lines)
- edit: `cmd/scribe/config.go` — `SyncConfig.DailyAnthropicOutputTokenCeiling int64`
- edit: `cmd/scribe/claude.go` — hook at top of `runClaude`
- edit: `cmd/scribe/llm.go` — hook at top of `anthropicProvider.Generate`
- edit: `cmd/scribe/sync.go` — outer-loop handler for the new error
  (treat like `ErrRateLimit`)

### Tests

1. Disabled (limit=0) — never returns error
2. Under the limit — no error
3. Over the limit — `ErrDailyBudgetExhausted`
4. `SCRIBE_BYPASS_BUDGET=1` — bypasses even when over
5. Refresh debounce — repeated calls within 30s reuse cached count
6. New day rollover — uses today's file, not yesterday's
7. Concurrent calls during refresh — no race on the cache

### Risks

- **Off-by-one on refresh**: if the cache is too stale, two parallel
  calls can both pass the gate then push us slightly over. Acceptable
  — the ceiling is a backstop, not a precise budget. Drift of ~5%
  is fine.
- **Performance**: file I/O per call. Mitigation: cache for 30s.
- **Crashloop**: if a cron keeps tripping the limit on retry, treat
  the exit code as success (0) so launchd doesn't keep restarting.
  Already handled by the "commit + exit clean" pattern.

### Estimate

~200 lines code + tests. One session.

---

## Item 3 — Phase 4B layer 3: quality diff (tools vs json mode)

### Problem

We validated parse rate, frontmatter shape, citation grounding, and
related-array syntax across three local models. We have NOT done the
canonical "same article through both `pass2_mode=tools` and
`pass2_mode=json`, then diff the outputs" comparison the original
plan named. Without that, we can't say objectively how much
quality the envelope path costs vs the historical tools path.

### Two design options — pick one

**Option A — Bash + jq script** (~80 lines bash, no Go):

```sh
scripts/absorb-compare.sh <raw-file>
  # snapshots wiki/ → /tmp/scribe-compare-tools/
  # runs absorb with pass2_mode=tools
  # snapshots wiki/ → /tmp/scribe-compare-tools/after/
  # restores wiki/
  # runs absorb with pass2_mode=json
  # snapshots wiki/ → /tmp/scribe-compare-json/after/
  # produces report: file count, line count diff, wikilink count,
  # orphan wikilinks (resolve against current wiki/)
```

Pros: no new Go code, easy to iterate, fits one-shot research workflow.
Cons: harder to make deterministic; needs a clean test KB.

**Option B — new `scribe absorb-compare` subcommand** (~300 lines Go):

```sh
scribe absorb-compare <raw-file> --output report.json
```

Same shape as Option A but in Go: snapshots use `cp -r`, restore is
atomic, comparison logic is testable, reports as structured JSON.

Pros: deterministic, scriptable, reusable.
Cons: 4× the code for a tool that runs maybe once per quarter.

### Recommendation

**Option A.** This is a one-time-ish quality probe, not a production
feature. If we revisit local-mode quality every 6 months, a shell
script with `cp -r` and `diff -r` is the right cost.

Save it at `scripts/absorb-compare.sh` with a small README. The
absorb-pass2-json prompt + envelope schema rarely change; the
comparison harness shouldn't grow code surface to track them.

### Files

- new: `scripts/absorb-compare.sh` (~80 lines)
- new: `scripts/absorb-compare.md` (one-page README)

### Acceptance criteria

Run against the validated `test-linkedin.md` and produce a single
report showing:

- entity count per mode (target: equal ±1)
- total wiki lines per mode (target: json ≤ tools, within 20%)
- distinct wikilinks per mode (target: json ≥ 80% of tools)
- orphan wikilinks per mode (target: json ≤ tools + 20%)
- citation count per mode (target: json ≥ tools)

If json fails any criterion by more than the tolerance, the prompt
needs tightening before the heavy crons get re-enabled.

### Estimate

~120 lines shell + a small README. Half a session.

---

## Item 4 — Phase 4C: session mining via JSON envelope

### Problem

Session mining is the next-biggest anthropic spend after pass-2. It
reads ccrider session messages and writes:

- multiple wiki articles (covered by the existing `wiki_actions.go`)
- a rolling memory file (e.g. `wiki/_rolling_memory.json` or
  `<domain>/learnings.md` per the drop-file pattern)
- a session-log entry in `wiki/_sessions_log.json`

The last two live partly outside the `wikiDirs` sandbox and don't fit
the current envelope schema cleanly.

### Design

Add a second action category alongside `WikiAction`:

```go
// wiki_actions.go (extended)

type MetaActionOp string

const (
    MetaSessionsLogAppend  MetaActionOp = "sessions_log_append"
    MetaRollingMemoryAppend MetaActionOp = "rolling_memory_append"
)

type MetaAction struct {
    Op      MetaActionOp   `json:"op"`
    // SessionsLogAppend payload
    SessionID   string         `json:"session_id,omitempty"`
    Score       int            `json:"score,omitempty"`
    Status      string         `json:"status,omitempty"`
    Notes       string         `json:"notes,omitempty"`
    // RollingMemoryAppend payload
    Domain      string         `json:"domain,omitempty"`
    Target      string         `json:"target,omitempty"` // "learnings" | "decisions-log"
    Body        string         `json:"body,omitempty"`
}

type EnvelopeV2 struct {
    Entity   string         `json:"entity,omitempty"`
    Notes    string         `json:"notes,omitempty"`
    Actions  []WikiAction   `json:"actions,omitempty"`
    Meta     []MetaAction   `json:"meta,omitempty"`
}
```

Existing pass-2 envelopes are still valid (empty `Meta` slice).
Session-mine envelopes use both arrays.

### Sandboxing

- `sessions_log_append` is restricted to `wiki/_sessions_log.json`
  exactly. No path field exposed to the model.
- `rolling_memory_append` accepts only `(domain, target)` and resolves
  to `<domain>/<target>.md`. Domain must be in the user's
  configured domains list. Target must be in
  `["learnings", "decisions-log"]`.

Neither op can write to an arbitrary path. The sandbox refusal logic
gets a sibling function `validateMetaAction`.

### New prompt

`prompts/session-mine-json.md` — replaces the existing
`prompts/session-mine.md` for the json path. Inlines the session
transcript chunk, project context, and prior memory snippets;
instructs the model to emit an EnvelopeV2 with both wiki articles
and meta actions.

### Wiring

`session_mine.go` (or wherever the existing session-mine command
lives — needs path-tracing) gets a json-mode branch mirroring the
pass-2 pattern:

- if `cfg.SessionMine.Mode == "json"` (or auto-flipped from provider)
- load prompt with inlined transcript
- generate → extract JSON → parse EnvelopeV2 → apply wiki + meta

### Files

- edit: `cmd/scribe/wiki_actions.go` — add `MetaAction`,
  `EnvelopeV2`, `applyMetaActions`, `validateMetaAction`
- new: `cmd/scribe/wiki_actions_meta_test.go`
- new: `cmd/scribe/prompts/session-mine-json.md`
- edit: `cmd/scribe/session_mine.go` (or `sessions.go`) — json branch
- edit: `cmd/scribe/config.go` — `SessionMineConfig.Mode`,
  `SessionMineConfig.Provider`, `SessionMineConfig.Model`

### Tests

1. EnvelopeV2 unmarshal back-compat (envelopes without `Meta` still parse)
2. `sessions_log_append` writes to the right file
3. `rolling_memory_append` resolves domain+target → correct path
4. Refused: invalid domain, invalid target, missing fields
5. Concurrent meta actions don't corrupt the sessions log (file lock)
6. Integration: end-to-end session-mine envelope through ollama

### Risks

- **Sessions-log JSON corruption**: two parallel `sessions_log_append`
  calls racing on the same file. Mitigation: per-file mutex inside
  `applyMetaActions`, same pattern as the per-label lock in pass-2.
- **Prompt complexity**: session-mine is the most context-heavy
  pass. EnvelopeV2 inflates output too. A 14B local model may not
  handle a full session + EnvelopeV2 reliably; gemma3:27b is more
  likely the floor here.
- **Schema versioning**: EnvelopeV2 should set a `version: 2` field
  so the consumer can branch behavior cleanly. Bump
  `wiki_actions.go` to write that field on parse.

### Sequencing

Best done after Item 1 ships — fact-citation stripping should apply
to session-mine envelopes too, so the validator lives at the right
seam first.

### Estimate

1–2 weeks. Biggest item by far. Defer until items 1–3 prove out.

---

## Item 5 — Re-enable heavy crons

### Pre-conditions

Item 2 must ship first. Without the daily-token ceiling, re-enabling
the crons risks a repeat of 2026-05-11.

### Procedure

1. Land Item 2.
2. Set `sync.daily_anthropic_output_token_ceiling: 2_000_000` in
   scriptorium's `scribe.yaml`. (Headroom comparison: the 2026-05-11
   runaway was 7M; ceiling at 2M aborts ~70% of the way through.)
3. Run a manual `scribe sync` to confirm the ceiling kicks in on a
   forced over-spend (set ceiling to a trivially small number like
   1000 and watch it abort cleanly).
4. Reset ceiling to 2_000_000.
5. Re-enable:
   ```sh
   launchctl load ~/Library/LaunchAgents/com.scribe.sync-projects.plist
   launchctl load ~/Library/LaunchAgents/com.scribe.sync-sessions.plist
   ```
6. Watch `output/costs/$(date +%F).jsonl` for the first 24h.

### Rollback

```sh
launchctl unload ~/Library/LaunchAgents/com.scribe.sync-projects.plist
launchctl unload ~/Library/LaunchAgents/com.scribe.sync-sessions.plist
```

### Estimate

20 min, mostly waiting on the manual sync to confirm the ceiling
fires.

---

## Sequencing summary

| order | item | size | depends on |
|---|---|---|---|
| 1 | Fact-ID validator | 1 session | — |
| 2 | Daily output-token ceiling | 1 session | — |
| 3 | absorb-compare.sh quality diff | 0.5 session | — |
| 4 | Re-enable heavy crons | 20 min | Item 2 |
| 5 | Phase 4C session mining | 1–2 weeks | Item 1 (recommended) |

Items 1, 2, 3 can ship in parallel as three independent PRs. Item 4
is the gate to resuming production cron load. Item 5 is the
follow-on phase, not part of this followup.

## What's NOT in this plan (deliberately)

- **Layer-3 prompt iteration loop.** If `absorb-compare.sh` shows
  json-mode quality below threshold, that's a prompt-engineering
  session, not a code change. Don't pre-plan iterations against
  hypothetical metrics.
- **`absorbDefaultYAMLBlock` idempotent re-merge.** The append-on-
  missing behavior is annoying when adding pass2 knobs by hand
  (we hit this twice this session) but rewriting the merge is a
  100-line refactor that doesn't unlock any new capability.
  Track separately if it bites again.
- **Switching pass-1 chaptered to local.** Pass-1 generates JSON
  plans, but the prompt currently requires understanding chapter
  boundaries from a TOC sidecar — that's reading-comprehension
  workload local models can do but not cheaply at the small sizes.
  Skip until Phase 4D.
- **Multiple-provider routing per op.** "facts on ollama, pass-2 on
  anthropic, contextualize on a third backend" is already supported
  via the per-op provider knob, but the config surface is sprawling.
  Wait for a real second use case before normalizing.

---

## 2026-05-16 addendum — bugs surfaced by the pass-2 model A/B

Source: `.claude/research/2026-05-15-pass2-thermal-model-downgrade.md`.
An empirical A/B (gemma3:27b vs gemma3:12b on the real pass-2 envelope
path, same dense article, identical 2400 s budget) rejected the 12b
downgrade and surfaced three defects, listed by severity. The thermal
root cause itself (entity fan-out) is scoped separately in
`docs/entity-fanout-cap-plan.md`.

> **Status 2026-05-16: all three fixed.** `normalizeRelatedFrontmatter`
> + `normalizeEnvelopeRelated` (Bug 1), `errUnknownTopDir` sentinel +
> `ApplyOptions.RemapUnknownTopToWiki` (Bug 2), and `chunkOptions.
> MinChunkBytes` heading-coalesce (Bug 3 / Fix A) shipped with tests;
> `make check` green. Per-detail notes below kept for the rationale.

### Bug 1 — local-model `related:` frontmatter corruption (highest)

gemma3:12b pass-2 produced **invalid-YAML** `related:` on ~12% of files
(`related: [][AuthoredUp][LangChain]`, `related: [][AuthoredUp][LangChain][Harbor]`)
and bracket-stripped bare `related: [Terminal Bench 2.0, LangSmith, …]`
on ~29% (parses as plain strings → backlink edges silently lost). 27b
was clean (13/14 conservative `[]`, 1 perfect quoted block, 0 corrupt).

This is why the 2026-05-13 27b decision stands and 12b is rejected for
pass-2. **Proposed hardening (model-agnostic):** a Go-side `related:`
normalizer applied to every pass-2 envelope before apply —
re-wrap bare `Foo` / `[[Foo]]` → `"[[Foo]]"`, reject tokens that aren't
wikilinkable, and fail the envelope (triggering the existing
`runPass2JSONOnce` corrective retry) if `related:` is non-parseable
YAML. Hardens all local models, not just 12b, and would let smaller/
cooler models back into contention later. Pairs with Item 1's
fact-ID validator — same "sanitize envelope before apply" seam.

### Bug 2 — 27b out-of-bounds create-path hallucination (medium)

gemma3:27b pass-2 emitted `create "middleware/loop-detection-middleware.md"`
— `middleware/` is not in the allowed wiki dirs. The executor correctly
rejected it (`0 applied, 1 errors`), but the entity was lost with no
recovery. **Proposed:** on an out-of-bounds path, remap to the
nearest valid wiki dir by the entity's `type` (tool→`tools/`,
pattern→`patterns/`, …) instead of dropping; or feed the rejection
into the corrective-retry loop the way envelope-parse failures already
are. Low frequency (1/16 here) but silent data loss.

### Bug 3 — gemma3:4b pass-1 over-extraction + instability under fan-out (medium)

On the 588-word note: 35 entities from 13 ~45-word "chapters". On the
4927-word doc: `context deadline exceeded` on pass-1 chapters 15/16 and
`json: cannot unmarshal string/array into … entities` on chapter plans
7/15/16/21/25. Both are downstream of the chunker emitting many tiny
racy chunks. **Fixed by `docs/entity-fanout-cap-plan.md` Fix A**
(coalesce sub-`MinChunkBytes` heading sections → far fewer, larger,
coherent pass-1 calls). Tracked here only so the timeout/parse-error
class isn't mistaken for an unrelated Ollama flake — it is a
fan-out symptom, not a model bug.
