# Codex completion plan — read-only contract + C3 session mining

Status: approved 2026-05-15. Two independent workstreams, shipped in
order: **A first (one patch release ~0.2.21), then B (C3 feature).**
A is a prerequisite for B's clean sync integration (it produces the
`ReadOnly()` / `--dry-run` signal B's driver must respect).

This doc is the master plan. B's verified Codex rollout schema lives
in `codex-session-mining-plan.md` (already corrected against a real
176-event scribe rollout; fixture pinned at
`cmd/scribe/testdata/codex/rollout-transcript.jsonl`).

Origin: the three A findings came out of a real Codex CLI "review this
project" session run inside this repo on 2026-05-15 — the same session
whose rollout became the C3 fixture. All three were verified against
the code, not taken on faith.

---

## Workstream A — read-only / portability contract hardening

Ships as one cohesive release (~0.2.21), one CHANGELOG entry. Do **not**
over-split within A. Regression tests are part of the same release.

### A1 — `doctor`/`status`/`--dry-run` must not write run records

**Bug:** `main.go:90` calls `writeRunRecord` unconditionally after
every command; `writeRunRecord` (main.go:122) only skips `version`.
So `scribe doctor` / `scribe status` / any `--dry-run` mutate
`output/runs/*.jsonl`, which auto-commits to the KB repo — diagnostics
are self-modifying.

**Fix:** optional interface

```go
type readOnlyCmd interface{ ReadOnly() bool }
```

`main()` checks `if ro, ok := ctx.Selected().Target.Interface().(readOnlyCmd); ok && ro.ReadOnly()` (or equivalent on the bound struct) and skips the run-record write.

- `DoctorCmd.ReadOnly() => true`, `StatusCmd.ReadOnly() => true`.
- Commands with a dry-run flag return `c.DryRun` (`SyncCmd`,
  `IngestCmd`, `CaptureCmd`, …).
- Keep the existing `version`/empty-cmdPath guard.

### A2 — `loadConfig` rewrites `scribe.yaml` on plain reads

**Bug:** `config.go:1590` calls `appendAbsorbBlockQuiet` (temp-write +
rename) whenever the file lacks a top-level `absorb:` key — fires for
*any* command including read-only ones.

**Fix (decided):** keep the backfill **implicit-on-sync**, do NOT add
a `scribe config backfill` command (the feature only has value if it
fires automatically; an explicit command nobody runs is dead code).

- Make `loadConfig` pure — delete the write from it.
- Extract `maybeBackfillAbsorbBlock(cfgPath, raw string)` and call it
  explicitly from the mutating entrypoints only: `SyncCmd.Run` and
  `InitCmd` (cron `sync` backfills within one cycle → identical UX).
- Keep `SCRIBE_NO_CONFIG_BACKFILL` honored as the strict-purity
  escape hatch.

### A3 — `doctor` FDA check hard-fails on Linux / capture-off macOS

**Bug:** `doctor.go:129` always emits `statusFail` when `chat.db` is
unreadable and points at `scribe fda` (macOS-only, `fda.go:38`).
README documents capture as optional and Linux as supported, so this
is a false hard failure off the capture happy-path.

**Fix:** capability-aware probe in `checkDeps()`:

- add `runtime` import,
- thread `cfg` into `checkDeps`,
- only `statusFail` when `runtime.GOOS == "darwin"` **and** capture is
  configured (`len(resolveSelfChatHandles(cfg.Capture)) > 0`),
- otherwise `statusWarn` ("capture not configured — skipped") or omit
  the check entirely.

### A-tests

Regression tests asserting **no writes**:

- `doctor` and `status` leave `output/runs/` and `scribe.yaml`
  byte-identical (build a temp KB via embedded templates).
- `sync --dry-run` writes no run record.
- `loadConfig` on an `absorb:`-less config does not modify the file.
- `checkDeps` on a non-darwin GOOS / capture-unconfigured cfg does not
  emit `statusFail` for chat.db.

### A-ship

`make ci` green → CHANGELOG `## [0.2.21]` ("read-only / portability
contract hardening") → commit → push → tag `v0.2.21` → GitHub release
→ `make install`. Same pattern as 0.2.18/19/20.

---

## Workstream B — C3 Codex session mining (100% Codex support)

Starts on a clean tree after A ships. Schema is verified; the seam is
one function. Detailed event-type mapping: see
`codex-session-mining-plan.md` (authoritative, schema-corrected).

The entire ccrider-specific surface is `mineSessionEnvelope`
(session_mine_envelope.go:137) → `fetchAndRenderTranscript` →
`fetchSessionTranscript`. Everything downstream
(`renderTranscriptForPrompt`, `runSessionEnvelopeOnce`,
`applyWikiActions`, EnvelopeV2, the `session-extract-*` prompts) is
transcript-source-agnostic and reused unchanged.

### B1 — `cmd/scribe/codex_transcript.go`

`fetchCodexTranscript(rolloutPath string) ([]sessionTurn, error)`.
Parse **only `response_item`** (verified canonical stream), reuse
`codexRolloutEnvelope` + `bufio.Scanner`/`codexMaxFirstLineBytes`:

- `message` role=user/assistant → `Text` (concat `content[].text`
  for `input_text`/`output_text`)
- `function_call` → `ToolText` (`name(arguments)`)
- `function_call_output` → `ToolText` (paired via `call_id`)
- skip `reasoning` (encrypted, unrecoverable), all `event_msg`
  (duplicates + telemetry), `turn_context`, `session_meta`
- drop the leading synthetic `<environment_context>` user turn
- `Sequence` = line index; malformed line skipped (non-fatal);
  empty → empty slice, no error
- pin against `testdata/codex/rollout-transcript.jsonl`

### B2 — `walkCodexRollouts(root, sinceHours)` in `codex.go`

Sibling to `walkCodexSessions`; yields `(rolloutPath, meta, mtime)`
for rollouts modified within the lookback window.

### B3 — pure scorer `scoreText(keywords, weights, text) int`

`TriageConfig.Resolve()` (config.go:1408) already returns the
keyword/weight maps. Triage stays SQL/FTS5; the Codex path scores the
rendered transcript in-process via `scoreText`. Threshold via
`MinScore`, cap via `SessionsMax`.

### B4 — idempotency `wiki/_codex_sessions_log.json`

Mirrors `_sessions_log.json` (rollouts aren't in ccrider so re-reads
need a processed-set keyed by rollout id). Reuse `updateJSONFile` /
`loadProcessedSessionIDs` patterns.

### B5 — driver `mineCodexSessions(root)` + sync hook

Parallels `mineSessions` (sync.go:1321): walk → score → threshold →
per session reuse the existing envelope path with the Codex transcript
swapped in (a `fetchAndRenderCodexTranscript` mirroring
`fetchAndRenderTranscript`). Hook into `sync.go` alongside
`mineSessions` / after `discoverCodex` (sync.go:363). **Respects the
A1 `ReadOnly()`/`--dry-run` signal.** Gated by config.

### B6 — config `codex:` block

`CodexConfig{ Mine bool; SessionsMax int; LookbackHours int; MinScore int }`
with defaults; surface in `doctor`. Discovery (0.2.15) + handshake
(0.2.17) already shipped — B6 closes the loop.

### B7 — tests

`fetchCodexTranscript` against the fixture (turn count, roles, tool
pairing via call_id, env-context drop, malformed/encrypted skip, no
`event_msg` double-count), `scoreText` table test, `walkCodexRollouts`
lookback/dedupe.

### B-ship

Reuses `session-extract-{ollama,anthropic}.md` (rendered transcript is
provider-agnostic — no new prompt files). Feature minor: CHANGELOG
entry, commit, push, tag, GitHub release, `make install`.

---

## Out of scope / left alone

- The unrelated working-tree change `M .gitignore` +
  `.claude/skills/getscribe-site-sync/` (intentional skill-vendoring
  decision, owner: user) — not folded into either workstream.
- Splitting `cmd/scribe` into internal packages (Codex finding
  suggestion) — noted, deferred; revisit only if the package crosses
  the 3000-LOC / second-binary threshold from CLAUDE.md.
