# Codex session mining (Phase C3)

Status: planned (2026-05-15). **Now unblocked** — its only stated
prerequisite ("100%-Ollama Phase 4C ships and proves the envelope
shape works for Claude sessions", `codex-discovery-plan.md:212`) landed
in 0.2.14. This plan supersedes the deferred C3 stub.

## Where this sits in the Codex story

- 0.2.15 — Codex *project discovery* (`scribe sync --discover` walks
  `~/.codex/sessions/`, dedupes `cwd`s into the manifest).
- 0.2.17 — Codex *handshake* (`~/.codex/AGENTS.md` block so Codex
  sessions query the KB and write drop files).
- **C3 (this) — Codex *session mining*.** Codex transcripts get the
  same triage → envelope → wiki treatment Claude sessions get via
  ccrider. Closes the loop: discovery finds the projects, the
  handshake makes sessions write drops, mining extracts the sessions
  themselves.

## The integration seam (why this is now small)

The Phase 4C Claude path is:

```
triageSessionIDs ──► []sessionID
                     │
mineSessionBatchesEnvelope (sync.go)
   └─ per session: fetchSessionTranscript(ccriderDB, id)  ── []sessionTurn
                    renderTranscriptForPrompt(turns, max)  ── string
                    runSessionEnvelopeOnce(provider, prompt) ── EnvelopeV2
                    parseEnvelopeV2 ─► applyWikiActions
```

Everything from `[]sessionTurn` onward is **agent-agnostic** —
`renderTranscriptForPrompt`, the envelope prompt, `parseEnvelopeV2`,
`applyWikiActions`, `MetaAction` (sessions-log + rolling memory) all
already work regardless of which agent produced the turns. C3 only
needs to produce `[]sessionTurn` from a Codex rollout instead of from
ccrider's SQLite, and a way to enumerate + score Codex sessions.

Two new pieces; the rest is reuse.

## Piece 1 — Codex transcript reader → `[]sessionTurn`

New `cmd/scribe/codex_transcript.go`:

```go
func fetchCodexTranscript(rolloutPath string) ([]sessionTurn, error)
```

Parses the rollout JSONL line-by-line (reuse the `bufio.Scanner` +
`codexMaxFirstLineBytes` ceiling pattern from `codex.go`). Each line
is `{"timestamp","type","payload"}` (`codexRolloutEnvelope`).

### Verified schema (probed 2026-05-15, real scribe rollout, 176 lines)

The earlier table in this plan was a **guess and was wrong**. Ground
truth from a real "review this project" Codex session is pinned at
`testdata/codex/rollout-transcript.jsonl` (12 representative events,
private paths + the encrypted-reasoning + base-instructions blobs
scrubbed). The on-the-wire shape is the **OpenAI Responses-API item
schema** (`input_text` / `output_text` / `function_call` /
`function_call_output`), which is stable and documented by OpenAI —
the parser keys off that, not a Codex-private format.

Two parallel streams exist in every rollout:

- **`response_item`** (137/176 here) — the canonical model I/O stream.
  **This is the only stream the transcript reader should consume.**
- **`event_msg`** (37/176) — the UI/telemetry event stream. Every
  content-bearing `event_msg` (`user_message`, `agent_message`) is a
  **duplicate** of a `response_item/message`; the rest is noise
  (`token_count` is the single most frequent event at 24×). Consuming
  both streams double-counts every turn — the original table's worst
  trap.

| Envelope `type` | `payload.type` (+`role`) | Maps to | Notes |
| --------------- | ------------------------ | ------- | ----- |
| `session_meta`  | —                        | (skip)  | line 0, already parsed by discovery |
| `turn_context`  | —                        | (skip)  | per-turn config snapshot, no content |
| `response_item` | `message` role=`user`    | `user`  | `content:[{type:"input_text",text}]` — concat text parts |
| `response_item` | `message` role=`assistant` | `assistant` | `content:[{type:"output_text",text}]`; also has `phase` |
| `response_item` | `function_call`          | `tool` → ToolText | `{name, arguments(JSON str), call_id}` |
| `response_item` | `function_call_output`   | `tool` → ToolText | `{call_id, output(str)}`; pair via `call_id` |
| `response_item` | `reasoning`              | (skip)  | **`content:null`, `encrypted_content` only — UNRECOVERABLE.** Plan's "best-effort text" was impossible |
| `event_msg`     | * (any)                  | (skip)  | duplicates `response_item` content + telemetry (`token_count`, `task_started/complete`) |

Net: the reader is **smaller** than the original table implied — one
envelope type (`response_item`), four content sub-cases, everything
else skipped. No "anything else → system" catch-all (it would ingest
`token_count` telemetry as fake turns).

`Sequence` = line index (rollouts are already append-ordered, unlike
ccrider which needed `ORDER BY sequence, id`). The first user
`response_item/message` is a synthetic `<environment_context>` block
(cwd/shell/date) Codex injects — the reader should drop a leading
user turn whose text is wholly an `<environment_context>` element so
it doesn't pollute triage scoring.

Robustness: a single malformed line is skipped (not fatal) — same
posture as `readCodexSessionMeta`. Empty transcript → empty slice, no
error (lets the caller skip cheaply).

## Piece 2 — enumerate + score Codex sessions

The Claude path scores via `scribe triage` → ccrider FTS5. Codex
rollouts are **not** in ccrider's DB. Two options:

### Option A (recommended): standalone Codex triage, no ccrider

A new lightweight enumerator + scorer that never touches ccrider:

- `walkCodexSessions` already exists (discovery). Add
  `walkCodexRollouts(root, sinceHours)` yielding `(rolloutPath,
  meta, mtime)` for rollouts modified in the lookback window.
- Score with the **existing keyword logic** `scribe triage` already
  uses (`TriageConfig` keyword categories + weights) but run it over
  the rendered transcript text in-process instead of via FTS5 BM25.
  Extract the scoring core out of `triage.go` into a pure
  `scoreText(cfg.Triage, text) int` helper; FTS5 path and Codex path
  both call it. Keeps one scoring definition.
- Threshold + cap with the same `MinScore` / `SessionsMax` knobs.

Pro: zero ccrider coupling, no schema migration, works even if the
user never installed ccrider (Codex-only users exist). Con: re-reads
rollouts each run (mitigated by the lookback window + a processed-set
log, mirroring `recordBatchOutcome`).

### Option B: index Codex rollouts into ccrider

Extend ccrider (separate tool, not in this repo) to ingest Codex
JSONL, so the existing triage+envelope path "just works" unchanged.

Pro: one triage path forever. Con: cross-repo change, ccrider release
coupling, blocks C3 on an external tool. **Rejected for C3** — revisit
only if maintaining two enumerators becomes painful.

Go with **A**. It matches scribe's "own the robust contract, walk
files directly" stance already taken for Codex discovery
(`codex-discovery-plan.md:258`, rejecting reading Codex's SQLite).

## Wiring into sync

New config block, mirroring how session-mine already routes:

```yaml
session_mine:
  codex_enabled: true        # default false until proven
  codex_lookback_hours: 24
  codex_min_score: 50
  codex_max_sessions: 3
```

`SyncCmd.Sessions` flow gains a Codex branch after the Claude batch:

```go
if cfg.SessionMine.CodexEnabled {
    ids := walkCodexRollouts(cfg.CodexSessionsDir, lookback)
    scored := filterByScore(ids, cfg.Triage, minScore)[:max]
    s.mineCodexBatchEnvelope(root, scored, cfg)   // reuses runSessionEnvelopeOnce
}
```

`mineCodexBatchEnvelope` is ~30 lines: for each rollout,
`fetchCodexTranscript` → `renderTranscriptForPrompt` →
`runSessionEnvelopeOnce` (the *exact* function the Claude envelope
path calls) → `parseEnvelopeV2` → `applyWikiActions`. Checkpoint +
`recordBatchOutcome` after each, same as Claude.

Forced envelope-only: there is no Codex MCP, so no "tools" mode
question — Codex mining is envelope from day one. Provider/model
inherit from `session_mine` → `llm` exactly like Claude session-mine
(so it's 100%-Ollama by default on a `llm.provider: ollama` KB).

## Dedupe across agents

A project touched from *both* agents could double-mine the same work.
`recordBatchOutcome` already writes a processed set. Key Codex entries
by rollout UUID (stable, in the filename:
`rollout-<ISO>-<uuid>.jsonl`). The sessions-log MetaAction dedupe and
the absorb-decision content-hash dedupe further protect against
duplicate articles. No new dedupe machinery needed.

## Phasing

- **C3.1 — transcript reader.** `codex_transcript.go` +
  `testdata/codex/` rollout fixture pinned from a real local rollout.
  Tests: event-type mapping, malformed-line skip, ordering, empty.
  No sync wiring yet — unit-testable in isolation.
- **C3.2 — extract `scoreText` core.** Refactor `triage.go` so FTS5
  and Codex share one scorer. Pure-function tests. No behavior change
  to existing `scribe triage`.
- **C3.3 — sync wiring.** Config knobs, `walkCodexRollouts`,
  `mineCodexBatchEnvelope`, checkpoint/outcome. Gated `codex_enabled:
  false` by default so it ships dark and is opt-in until proven on
  scriptorium.
- **C3.4 — doctor + docs.** `scribe doctor --section codex` row for
  "Codex mining: N rollouts in lookback, M above threshold". README
  Codex section. Flip default to `true` once a real run proves clean.

Each phase is independently shippable and reviewable.

## Tests

- `fetchCodexTranscript`: fixture-pinned event mapping; malformed
  line skipped; tool payload → ToolText; empty rollout → empty slice.
- `scoreText`: parity test — same text scored via the refactored
  helper and (where feasible) the old FTS5 path agree on ordering.
- `mineCodexBatchEnvelope`: a fake provider returning a known
  EnvelopeV2 lands the expected wiki actions + sessions-log MetaAction;
  rollout-UUID dedupe skips an already-processed rollout.
- All offline (`make test` has no network) — fixtures only, never
  `~/.codex`.

## Risks

- **Codex schema drift.** Codex churned `codex.md` → `instructions.md`
  → `AGENTS.md`; event payload shapes may move too. Mitigation: the
  fixture pins today's shape; a drift shows up as a failing test, and
  the per-line skip means partial drift degrades gracefully (some
  turns dropped) rather than crashing the run. A doctor probe (C3.4)
  surfaces it as a WARN like the discovery `session_meta` sentinel.
- **Double-mining cost** under Option A's re-read. Bounded by
  lookback-hours + processed-set; same shape as the Claude pending
  queue.

## Out of scope

- ccrider Codex ingestion (Option B) — explicitly rejected above.
- Codex `memories/` / `rules/` / `skills/` cross-agent sync —
  different problem (`codex-discovery-plan.md:259`).
- Real-time Codex watching (fsnotify on `~/.codex/sessions/`) — the
  `scribe watch` Claude analogue. Possible follow-up; the lookback
  poll is sufficient for v1 and matches the cron cadence.
