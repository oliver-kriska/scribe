# Road to 100% Ollama — Plan

Status: **shipped** (one-shot implementation pass, 2026-05-14)
Filed: 2026-05-14

## Status — what's on disk after the implementation pass

| Phase | Status | Notes |
|---|---|---|
| 4A.4 relations-migrate | ✅ shipped | `callLLMForRelationsMigrate` + `relationsProviderModel` |
| 4A.2 pass-1 (whole + chaptered) | ✅ shipped | `Pass1Provider` knob; new `absorb-pass1-{anthropic,ollama}.md` pairs |
| 4A.3 absorb-single | ✅ shipped | Envelope mode by default; `SinglePassProvider` knob |
| 4A.5 top-level LLMConfig | ✅ shipped | `llm:` block; `inheritProviderFromLLM` + `coerceProviderModel` |
| 4C session mining | ✅ shipped | `EnvelopeV2` + `MetaAction`; `SessionMineConfig.Mode = envelope`; Go-side transcript fetcher |
| 4D Dream | ✅ shipped | `DreamConfig.Mode = orchestrator`; Go runs orient + apply; LLM emits one envelope |
| 4E Assess + Deep | ✅ shipped | `AssessConfig` + `DeepIngestConfig` with `Mode = envelope`; single-envelope assess |
| Phase 5 onboarding | ✅ shipped | `scribe init --provider ollama` pre-pulls + probes; doctor checks `llm.model` |

Open: production validation against scriptorium (the "100% Ollama week" acceptance test). The session-extract path in `extractProject` (sync.go:847) is still on the tools path — it's a project-level extraction with Bash git log, less ROI than the heavyweight session-mine work.

Owner: Oliver
Parents:
- [[local-model-support-plan]] (Phase 4A facts + 4B pass-2 landed)
- [[local-model-followup-plan]] (Items 1, 2, 3 in flight; 4 + 5 sequence into this plan)

Goal: every scribe subcommand can run end-to-end against a local Ollama
server with **zero `claude -p` calls**, controlled by a single config
switch. Anthropic stays the default — this plan is about adding a
first-class free path, not deprecating the paid one.

The acceptance test for "done" is one sentence:

> Set `llm.provider: ollama` in `scribe.yaml`, run every subcommand
> against a fresh test KB for a week, and `output/costs/*.jsonl`
> contains no `provider: "anthropic"` rows.

---

## Inventory: every `claude -p` call site as of 2026-05-14

| # | Op label | File | Tools used | Port phase |
|---:|---|---|---|---|
| 1 | `absorb-pass1-whole` | `absorb_chapter.go:56` | R/W/E/G/G | **4A.2 (unfinished)** |
| 2 | `absorb-pass1-chapter` | `absorb_chapter.go:213` | R/W/E/G/G | **4A.2 (unfinished)** |
| 3 | `absorb-single` | `sync.go:1633` | R/W/E/G/G + Bash(wc) | **4A.3** |
| 4 | `absorb-pass2` (tools branch) | `sync.go:1907` | R/W/E/G/G | done (4B json branch) |
| 5 | `relations-migrate` | `relations_migrate.go:479` | none | **4A.4 (trivial)** |
| 6 | `session-extract` | `sync.go:847` | R/W/E/G/G + Bash | **4C** |
| 7 | `session-mine` | `sync.go:1158` | tools + MCP ccrider | **4C** |
| 8 | `session-mine-batch` | `sync.go:1243` | tools + MCP ccrider | **4C** |
| 9 | `dream` | `dream.go:66` | R/W/E/G/G + Bash | **4D** |
| 10 | `deep-extract` | `deep.go:127` | R/W/E/G/G + Bash | **4E** |
| 11 | `assess-track` (×5) | `assess.go:200` | R/W/E/G/G | **4E** |
| 12 | `assess-consolidate` | `assess.go:231` | R/W/E/G/G | **4E** |

Already on the provider abstraction (`newLLMProvider` → either backend):

| Op | File | Status |
|---|---|---|
| `contextualize` | `contextualize.go:268` | local-ready since Phase 3 |
| `contradictions` | `contradictions.go:71` | local-ready |
| `identities` | `identities.go:65` | local-ready |
| `resolve` | `resolve.go:98` | local-ready |
| `absorb-facts` | `absorb_facts.go:159` | local-ready since 4A |
| `absorb-pass2` (json mode) | `sync.go:1720` | local-ready since 4B |

Two outputs from this inventory are worth calling out:

- **Phase 4A only half-shipped.** Facts went through `llmProviderGenerator`
  (commit `339734b`) but pass-1 never did. Two `runClaude` call sites
  still gate the absorb pipeline behind anthropic on every dense
  article. Closing this is the single biggest unblocker.
- **The config surface is already sprawling.** Each port adds another
  `<op>_provider`, `<op>_model`, sometimes an `<op>_mode`. Without a
  unifying top-level knob, "100% Ollama" means flipping 8 knobs by
  hand — high-friction onboarding.

---

## Architecture decisions

### 1. Add `LLMConfig` at the top level

Right now every op has its own provider/model fields and they don't
inherit. Add to `ScribeConfig`:

```go
type LLMConfig struct {
    Provider  string `yaml:"provider"`   // anthropic (default) | ollama
    Model     string `yaml:"model"`      // default model when per-op model is ""
    OllamaURL string `yaml:"ollama_url"` // default http://localhost:11434
}
```

Per-op config keeps its `Provider`/`Model` fields, but **empty means
inherit from `LLMConfig`**. Coherence fixups (ollama + Claude alias →
swap to recommended local model) move up to one helper so they only
live in one place.

Result: `llm.provider: ollama` in scribe.yaml flips the whole pipeline
in one line. Per-op overrides still work — e.g., keep facts on a
cheap model while pass-2 runs on a 14B model.

### 2. One envelope schema family, not many

The followup plan's `EnvelopeV2` with `Meta []MetaAction` becomes the
**single shape** every JSON-emitting LLM call returns. Today's
`WikiActionEnvelope` is a subset (empty `Meta`). All future ports
target this schema.

New `MetaAction` ops, in order of priority:

- `log_append` — append a line to `log.md` at KB root
  (Dream Phase 5 + session mining session-log)
- `sessions_log_append` — write to `wiki/_sessions_log.json`
  (session mining)
- `rolling_memory_append` — append to `<domain>/<target>.md`
  (session mining drop-file writes; `target ∈ {learnings, decisions-log}`)
- `plan_emit` — emit an absorb-plan JSON to `output/absorb-plans/<slug>.json`
  (used by pass-1 ports so the schema reuses the same parser path)

Sandboxing rules stay in Go; the model never names a path outside its
op's allowed targets.

### 3. Two prompt files per task — `<task>-anthropic.md` + `<task>-ollama.md`

Established in the previous Dream research note. Reasons:

- Ollama benefits from explicit "OUTPUT ONLY JSON, NO PROSE" caps.
- Ollama context window is smaller; anthropic prompts can inline more
  neighbour material.
- Anthropic prompts can be narrative; ollama prompts work better as
  imperative bullets.
- Diffing between providers is much easier when each file is separate.

A 5-line resolver in shared code (already sketched in the Dream note)
picks the right file by provider.

### 4. Go orchestrator + bounded LLM subtasks (for agentic passes)

Pattern proven by Phase 4B and the Dream research note. Replaces
`claude -p running for an hour with full tools` with:

- Go walks the inputs deterministically (file scan, KB read, ccrider
  query).
- Go assembles a tight prompt packet per work item.
- LLM returns one envelope per packet.
- Go applies the envelope.

Used by 4C (per-session envelope), 4D (per-article envelope), 4E
(per-track envelope).

---

## Phased plan

Ship order chosen by **value-per-engineering-day**, not by phase number
in the existing docs. Each phase is independently mergeable.

### Phase 4A.2 — Finish pass-1 (small)

**What**: port `runPass1Whole` + `runPass1Chaptered` from `runClaude` to
`llmProviderGenerator.Generate`. Same shape as the facts pass port:
inline the raw article body (or chapter slice), remove the Read/Write
tool requirement, emit JSON plan to stdout, Go writes the plan file.

**Files**:
- `cmd/scribe/absorb_chapter.go` — replace both `runClaude` call sites
- `cmd/scribe/prompts/absorb-pass1-anthropic.md` (rename current
  `absorb-pass1.md`)
- `cmd/scribe/prompts/absorb-pass1-ollama.md` (new — tightened bullets,
  format reminder)
- `cmd/scribe/prompts/absorb-pass1-chapter-anthropic.md` (rename)
- `cmd/scribe/prompts/absorb-pass1-chapter-ollama.md` (new)
- `cmd/scribe/config.go` — wire `Pass1Provider` into `AbsorbConfig`,
  matching `FactsProvider`/`Pass2Provider` shape

**Risks**: chaptered pass-1 inlines TOC sidecars; for very long PDFs
(60+ chapters) the per-chapter prompt may exceed Ollama's default
`num_ctx`. Bump to 16384 like the Dream plan suggests.

**Estimate**: 1 session.

### Phase 4A.3 — Port `absorb-single`

**What**: brief-article single-pass absorb. Currently uses tools to
read the raw and write the wiki article. Inline the raw body, emit
one `WikiActionEnvelope` with a single `create` action — almost
identical to pass-2 envelope but without plan/facts/entity context.

**Files**:
- `cmd/scribe/sync.go:1617` `absorbSinglePass` — branch on provider
- `cmd/scribe/prompts/absorb-anthropic.md` (rename `absorb.md`)
- `cmd/scribe/prompts/absorb-ollama.md` (new)
- `cmd/scribe/config.go` — `AbsorbSingleProvider` (or reuse Pass2Provider
  with a doc note — single-pass and pass-2 share the "write one wiki
  article" shape)

**Risk**: very low. Pattern is identical to pass-2 envelope.

**Estimate**: half a session.

### Phase 4A.4 — Port `relations-migrate`

**What**: `relations_migrate.go:479` already calls `runClaude` with
`nil` tools — it's a pure text generation call. Trivial 5-line refactor
to use `newLLMProvider` + `Generate`.

**Files**:
- `cmd/scribe/relations_migrate.go` — swap call site
- `cmd/scribe/config.go` — surface `relations.provider` or just inherit
  from `LLMConfig` (recommended: inherit only — relations-migrate runs
  rarely and per-op tuning isn't needed)

**Risk**: trivial.

**Estimate**: 15 minutes.

### Phase 4A.5 — Top-level `LLMConfig` knob

**What**: introduce `llm:` section in scribe.yaml, plumb the inheritance
through every per-op resolver. Centralize the
`claudeModelAliases + ollamaRecommendedModel` swap into one helper.

**Files**:
- `cmd/scribe/config.go` — `LLMConfig` struct, defaults, fixup helper,
  inheritance logic in `applyAbsorbDefaults` and per-op equivalents
- `cmd/scribe/init.go` / templates — emit the new section in fresh
  `scribe.yaml`s, with comments explaining inheritance

**Risk**: backward-compat. Existing scribe.yaml files don't have the
`llm:` block. Default `LLMConfig{Provider:"anthropic"}` preserves
today's behavior exactly. Tests must cover "empty llm block → existing
config still works."

**Estimate**: 1 session.

### Phase 4B.5 — Land followup-plan items in parallel

This isn't new work; it's a reminder that
[[local-model-followup-plan]] Items 1, 2, 3 are pre-requisites for
trusting the 100%-Ollama claim:

- **Item 1 (fact-ID validator)** — already landed (see
  `sync.go:1858`).
- **Item 2 (daily output-token ceiling)** — landed (see
  `sync.go:1163`, `ErrDailyBudgetExhausted` plumbing). Needs scriptorium
  config flip to actually engage.
- **Item 3 (absorb-compare.sh)** — defer until 4C+4D land; the same
  harness probes both phases at once.

No additional code in this phase; just verify the three items are
production-trusted before flipping to Ollama everywhere.

### Phase 4C — Session mining via envelope (medium-large)

**What**: port `session-mine`, `session-mine-batch`, `session-extract`
to `EnvelopeV2 + Meta` shape. This is the **biggest Anthropic spend**
on a real KB and the largest single ROI from this plan.

The previous followup plan already sketches the design in detail
(Item 4 of `local-model-followup-plan.md`). Key shape:

- Go reads ccrider session transcript via existing helpers
  (`queryRelatedSessions`, transcript fetcher).
- Per-session envelope contains:
  - `Actions []WikiAction` — the wiki articles to create/update
  - `Meta []MetaAction` — `sessions_log_append` + optional
    `rolling_memory_append` + optional `log_append`
- Sandbox: `_sessions_log.json` writes through `MetaAction`, never
  through a free-form `WikiAction` path.

**Files** (delta from the followup plan):
- `cmd/scribe/wiki_actions.go` — `EnvelopeV2` (back-compat: parses
  envelopes without `meta`), `MetaAction`, `applyMetaActions`,
  `validateMetaAction`
- `cmd/scribe/wiki_actions_meta_test.go` (new)
- `cmd/scribe/prompts/session-mine-anthropic.md` +
  `prompts/session-mine-ollama.md`
- `cmd/scribe/prompts/session-extract-anthropic.md` +
  `prompts/session-extract-ollama.md`
- `cmd/scribe/sync.go` — branch on provider in the three call sites
- `cmd/scribe/config.go` — `SessionMineConfig{Provider, Model, Mode}`
  inheriting from `LLMConfig`

**Risks**:
- Session transcripts can be very large. Need a chunker (split at
  ~6K-token boundaries for Ollama, ~30K for Claude) and a merge step.
  Reuse the absorb chapter chunker pattern.
- Model floor: probably qwen2.5-coder:14b or mistral-small3:24b. Test
  on a real session before claiming reliability.
- `sessions_log_append` concurrency — per-file mutex inside
  `applyMetaActions`, same shape as Phase 4B's per-label lock.
- Schema versioning — `EnvelopeV2` should set a `version: 2` field on
  parse so future bumps don't break old envelopes.

**Estimate**: 1–2 weeks.

### Phase 4D — Dream via Go orchestrator (large)

**What**: full plan in
[[.claude/research/2026-05-14-phase-4c-dream-ollama-envelope]]. Summary:
turn `DreamCmd.Run` into a Go orchestrator that runs Phase 1/4/5 in
pure Go and dispatches bounded LLM subtasks for 2.5/3/3.5.

**Files** (top-level):
- `cmd/scribe/config.go` — `DreamConfig{Provider, Model, Mode, ...}`
- `cmd/scribe/dream.go` — orchestrator skeleton
- `cmd/scribe/wiki_actions.go` — `log_append` op in `MetaAction`
  (shared with 4C)
- `cmd/scribe/prompts/dream-triage-anthropic.md` +
  `prompts/dream-triage-ollama.md`
- `cmd/scribe/prompts/dream-consolidate-anthropic.md` +
  `prompts/dream-consolidate-ollama.md`
- `cmd/scribe/prompts/dream-stub-anthropic.md` +
  `prompts/dream-stub-ollama.md`

**Risks**:
- Quality cliff on the consolidation prose rewrites. Mitigation:
  refuse envelopes that drop `>` blockquotes or `[c00-fN]` brackets
  (post-write diff check).
- Pair explosion in contradiction triage on large KBs. Pre-filter in
  Go by shared tags/domains before LLM scoring.
- Order: ship 4C first so the `MetaAction` infrastructure is paid
  for; Dream piggybacks on it.

**Estimate**: 2–3 weeks.

### Phase 4E — Assess + Deep + remaining stragglers

**What**: port the project-ingestion commands (`scribe assess`,
`scribe deep`) to the orchestrator pattern. Both are "read N files,
write structured output" — exactly the shape that envelopes handle
cleanly.

**Assess**:
- Today: 5 parallel `claude -p` tracks (structure, features, docs,
  decisions, gaps) + 1 consolidate call, each with full tool access.
- After: per-track Go orchestrator inlines the bounded file list
  for that track, LLM emits one envelope per track. Consolidation
  reads the on-disk track outputs via Go (no LLM file reads),
  envelope writes the final `projects/{name}/overview.md`.
- Each track has its own `<track>-anthropic.md` + `<track>-ollama.md`
  prompt pair (10 files total). Significant prompt work but each
  is small.

**Deep**:
- Today: per-batch `claude -p` invocation per directory.
- After: per-directory Go scan inlines the `.md` file contents (up to
  some byte budget — Ollama's context limit forces a per-directory
  chunker), LLM emits one envelope creating/appending project wiki
  articles.

**Files**:
- `cmd/scribe/assess.go` — orchestrator refactor
- `cmd/scribe/deep.go` — orchestrator refactor
- 10+ new prompt files (5 tracks × 2 providers + assess-consolidate
  pair + deep-extract pair)
- `cmd/scribe/config.go` — `AssessConfig{Provider,Model}` +
  `DeepConfig{Provider,Model}` inheriting from `LLMConfig`

**Risks**:
- Both run rarely (once per project, batch-mode). Quality bar is
  higher — they produce the canonical project overview a user reads
  weeks later. Local models below ~14B may produce visibly thinner
  prose here.
- File-list inlining can blow context on big projects. Per-file
  truncation (head/tail 200 lines) is acceptable for these "scan
  for entities, write structure" tasks.

**Estimate**: 1–2 weeks total (both commands together).

### Phase 5 — Onboarding UX

The technical work above gets the user a working 100%-Ollama install,
but `scribe init` should make it easy to choose at first run.

**Changes**:
- `scribe init --provider ollama` — bootstraps a KB with the
  100%-Ollama `scribe.yaml` (top-level `llm.provider: ollama`,
  recommended models per op).
- `scribe init` (no flag) — interactive prompt: "Use Anthropic (paid)
  or Ollama (free, requires `brew install ollama`)?"
- Pre-pull check: if `--provider ollama`, hit `/api/tags` and pull
  the recommended models in parallel with a progress line. Fail
  init with a clear error if Ollama isn't running.
- `scribe doctor` — add a `local-mode reachable` section that probes
  `/api/tags`, lists models, and warns about missing pulls.

**Files**:
- `cmd/scribe/init.go` — provider prompt, model pre-pull
- `cmd/scribe/doctor.go` — new section
- `cmd/scribe/templates/scribe.yaml` — emit both provider-defaults
  blocks (commented-out alternative) so the user can switch later

**Risk**: very low — UX-only.

**Estimate**: 1 session.

---

## Recommended ship order

| order | phase | size | gates |
|---:|---|---|---|
| 1 | 4A.4 relations-migrate | 15 min | — |
| 2 | 4A.2 pass-1 | 1 session | — |
| 3 | 4A.3 absorb-single | half session | — |
| 4 | 4A.5 top-level LLMConfig | 1 session | 4A.2/4A.3 (so we have ≥3 ops to inherit) |
| 5 | 4C session mining | 1–2 weeks | 4A.5 (config surface), followup Items 1+2 done |
| 6 | 4D Dream | 2–3 weeks | 4C (MetaAction infra) |
| 7 | 4E Assess + Deep | 1–2 weeks | 4D (orchestrator pattern proven on Dream) |
| 8 | Phase 5 onboarding | 1 session | 4A.5 |

Three independent PR streams can run in parallel:

- **Stream A — small ports**: 4A.4 → 4A.2 → 4A.3 → 4A.5 (4 PRs)
- **Stream B — agentic ports**: 4C → 4D → 4E (3 PRs, sequential)
- **Stream C — onboarding**: Phase 5 (1 PR, blocked on 4A.5)

Stream A is the unblocker — it both reduces the visible Anthropic
spend in the cost ledger and pays for the config refactor that
Stream B depends on.

---

## Validation strategy

### Per-phase acceptance

Each phase ships with:

1. Unit tests for new envelope ops / config inheritance.
2. Integration test (build-tag `integration`) hitting the real
   Ollama path on a fixture KB.
3. A scriptorium dogfood run before re-flipping the heavy crons.

### End-to-end "100% Ollama week"

After 4E lands, run a full week of cron drains with `llm.provider:
ollama` against a scriptorium clone. Acceptance:

- `output/costs/*.jsonl` rolls up to **zero** rows with
  `provider: "anthropic"`.
- Article count delta over the week within ±10% of an equivalent
  anthropic-run baseline.
- No more than 3 envelope parse failures per 100 LLM calls
  (matches Phase 4B's measured pass rate on qwen2.5-coder:14b).
- Daily wallclock ≤ 6× anthropic baseline (qwen2.5-coder:14b runs
  ~4× slower than sonnet on the pass-2 e2e; budget some headroom).

### Cost-ledger view

`scribe cost` already separates rows by `provider`. Add a small
header in the report:

```
Anthropic ops:   0   ($0.00)
Ollama ops:    127   (~ 4.3 GPU-hr)
```

Zero is the production signal — anything non-zero means an op path
slipped back to claude (likely a config inheritance bug; the
acceptance test will catch this).

---

## What's deliberately NOT in scope

- **Multiple-provider routing in one sync.** Mixing
  "facts on ollama, pass-2 on anthropic" inside a single run already
  works via per-op knobs; the **default** to one or the other is
  what `LLMConfig` cleans up. No fancier routing logic.
- **Llama.cpp / vLLM / LM Studio backends.** Ollama is the only
  local backend we ship support for. Anyone running llama.cpp can
  reuse the Ollama provider (it speaks the same `/api/generate`
  shape on most builds).
- **Quality parity guarantees.** This plan ships the *option* of
  100% Ollama. Anthropic remains the recommended default for
  quality-sensitive ops (Dream consolidation, Assess overview).
  Users opt in to the local path with eyes open.
- **GPU scheduling.** Concurrent Ollama calls share one model load
  on the GPU. Don't bump per-op `parallel` knobs in the 100%-Ollama
  config — saturating the GPU just queues. The existing
  `ChapterParallel=2` default is conservative enough.
- **Test-mode anthropic stubbing.** Tests already build a tiny KB
  via `t.TempDir()` and skip real LLM calls. Nothing here changes
  that.

---

## Open questions

1. **Single envelope schema for pass-1 plans too?** Pass-1 emits an
   `absorbPlan` (different shape). Keep separate, or fold into
   `WikiActionEnvelope.meta_plan`? Recommend **keep separate** —
   pass-1 plans are an internal artifact, not a file write, and
   bending the envelope schema to fit a "produce JSON to disk"
   case adds confusion.
2. **Should `LLMConfig.Model` be required when `Provider: ollama`?**
   Currently per-op config swaps the recommended local model when
   the user leaves it blank. Should the top-level config do the
   same? Recommend **yes** — fail-soft is the established pattern.
3. **Auto-pull at first run vs. on-demand.** `ensureReady` in
   `llm.go` already does on-demand pulling. Should `scribe init`
   front-load all configured models, or let them pull lazily?
   Recommend **front-load** — first-run experience matters more
   than disk space.
4. **What's the cost-ledger "GPU-hour" estimate based on?** No real
   formula today. For now report only wallclock seconds; convert
   to USD only when someone needs it (the user paying for cloud
   GPU rental — not the local-laptop case).

---

## Sequencing summary (one-liner)

Finish Phase 4A (small ports + config refactor), then take Phase 4C
(session mining) — that single step takes scribe past the
">50% of typical user spend now free" mark. Phase 4D (Dream) and
4E (Assess+Deep) come after; they're rarer, lower-impact runs that
benefit from the orchestrator pattern proven in 4C.

The 100%-Ollama story is unblocked end-to-end after 4E lands.
