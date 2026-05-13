# Local Model Support — Planning Note

Status: **Phase 4A in production; Phase 4B layer 2 validated end-to-end on local qwen2.5-coder:14b (9/9 envelopes parsed and applied, zero anthropic spend); reliability fixes #1–5 from the post-test review shipped; layer 3 quality tuning still pending**
Owner: Oliver
Filed: 2026-04-28
Last updated: 2026-05-13 (Phase 4B layer 2 e2e validated on ollama; reliability fixes 1–5 landed)

## Why

Scribe runs on `claude -p` for every LLM call. On a real KB (1000+ sessions, hundreds of dense articles), the bill adds up:

- Session mining: ~$0.03–$0.05 per session at sonnet → 1300 sessions ≈ $40–65
- Absorb on a 60-chapter PDF: ~$1–3 (haiku for facts/pass-1, sonnet for pass-2)
- A single full sync with overnight session drain can hit $5–10 in API spend

For people running scribe casually — or contributors who want to try the toolchain without registering a paid Anthropic account — the cost is a real adoption barrier. Anthropic also rate-limits aggressively under usage spikes (verified during Phase 3B testing), which stalls overnight cron drains.

A first-class local-model path solves both problems for users willing to spend GPU time instead of API spend.

## Current state

Scribe already has the right abstraction in *one* place: `llmProviderGenerator` in `cmd/scribe/llm.go` exposes `Generate(ctx, prompt) (string, error)` with two implementations — `anthropicProvider` and `ollamaProvider`. Contextualize, contradictions, identities, and resolve all dispatch through this interface. Setting `absorb.contextualize.provider: ollama` is enough to flip those passes to a local model today.

Everything else hard-codes `runClaude` → `claude -p`:

- `runPass1Whole`, `runPass1Chaptered`, `absorbDenseTwoPass`'s pass-2 fan-out
- `runFactsPass` (Phase 3B)
- `absorbSinglePass`
- Session mining (`session-mine`, `session-mine-batch`, `session-extract`)
- Deep extraction
- Dream cycle
- Assess (5 parallel tracks + consolidate)

These all use `claude -p` because they need **tool use**: Read, Write, Edit, Glob, Grep. The pass-2 prompt tells Claude to read the raw article, grep the wiki for existing entities, and write a markdown article. Pure text-in/text-out won't cut it.

## The blocker: local models without tool use

Most local model serving stacks (Ollama, LM Studio, llama.cpp) don't have a Claude-CLI-equivalent agentic harness. They expose a chat completion endpoint. Function-calling support varies; even where it exists (Ollama 0.4+ for `qwen2.5-coder`, `llama3.3`, `mistral-small3`), the model has to be smart enough to chain tool calls reliably across 5–10 turns of pass-2 work.

In practice, local models below ~70B params don't produce stable multi-turn tool sequences. We'd burn tokens on retries and broken edits.

## The architectural fix: JSON-action envelopes

The cleaner long-term answer — independent of local-vs-remote — is to refactor the tool-using passes to **emit a structured JSON action envelope instead of executing tools directly**. The model writes:

```json
{
  "actions": [
    {"op": "create", "path": "wiki/patterns/foo.md", "content": "..."},
    {"op": "update", "path": "wiki/_index.md", "patch": [...]}
  ]
}
```

Scribe parses the envelope and applies the actions itself. Three big wins:

1. **Local-model friendly**: the model only has to produce one JSON document, not orchestrate multi-turn tool calls. A 7B–13B local model can do this reliably.
2. **Deterministic and reviewable**: actions are auditable before they hit disk. Dry-run mode comes for free.
3. **Cheaper**: no tool-call round-trips means fewer tokens and shorter wallclock.

This pattern is already how scribe's facts pass and pass-1 plan work — they emit JSON to a known path, scribe consumes it. Extending the same shape to pass-2 entity writing and session mining is the right next step.

## Phased plan

### Phase 4A — Local for tool-less ops (low effort)

Wire `llmProviderGenerator` into the passes that already emit JSON to a path:

- `runFactsPass` — already JSON. The prompt tells claude to write to `{{FACTS_FILE}}`. Replacing `runClaude` with `provider.Generate(prompt)` + `os.WriteFile` is mechanical.
- `runPass1Whole` and `runPass1Chaptered` — both write a plan JSON. Same shape.

These two changes alone make the most expensive haiku-backed parts of scribe runnable on Ollama for free. Estimated effort: 1–2 days.

Config surface:

```yaml
absorb:
  pass1_provider: anthropic   # or ollama
  pass1_model: haiku           # or qwen3:4b, gemma3:4b, etc.
  facts_provider: anthropic
  facts_model: haiku
```

Defaults stay anthropic so existing users see no change.

### Phase 4B — Pass-2 via JSON envelope (medium effort)

Refactor pass-2 to emit a JSON action envelope. Update `prompts/absorb-pass2.md` to describe the schema and forbid tool calls. Add a Go executor that consumes the envelope (create/update/append actions on wiki paths). Reuse the same provider abstraction; pass-2 is now local-friendly.

This is the architectural payoff. After 4B, ~80% of scribe's claude spend can run on Ollama if the user wants.

Estimated effort: 1 week, including prompt iteration to get JSON output stable.

### Phase 4C — Session mining via JSON envelope (high effort)

Session mining has the most complex tool use (Read messages, walk project, write multiple wiki files, update rolling memory, write _sessions_log entry). Same JSON-envelope approach but bigger schema. Defer until 4A/4B prove out.

Estimated effort: 2 weeks.

### Out of scope (for now)

- **Dream**: weekly cycle, runs once. Not worth optimizing for local until the architecture is settled.
- **Deep extract**: similar — runs occasionally, batch-style.
- **Assess**: same.

## Provider matrix (target end state)

| Pass | 4A | 4B | 4C |
|---|---|---|---|
| contextualize | already done | — | — |
| facts | local-ready | — | — |
| pass-1 plan | local-ready | — | — |
| pass-2 entity | claude only | local-ready | — |
| session mine | claude only | claude only | local-ready |
| dream | claude only | claude only | claude only |

## Open questions

1. **Which local model is the right default?** Ollama recommendations as of late 2025: `gemma3:4b` (3.3 GB, fast), `qwen3:4b` (richer prose), `llama3.2:3b` (smaller). For pass-2 JSON envelope generation, larger models are likely needed — `qwen2.5-coder:14b` or `mistral-small3:24b` if RAM permits.
2. **Cost of the JSON-envelope refactor for accuracy.** Existing pass-2 has direct filesystem access; JSON-envelope adds a parse step. Is action-application as reliable as letting the model write directly? Probably yes, but needs measurement.
3. **Hybrid mode**: should scribe support routing different ops to different providers in one sync? E.g., facts on local, pass-2 on anthropic? The provider knob per-op gives this for free, but config-blow-up risk is real.
4. **Cost ledger integration**: Phase 3D's cost ledger currently uses Anthropic's pricing table. Local calls have wallclock cost (electricity + opportunity cost) but no USD. Probably emit a separate "local-time" rollup.

## Cost motivation (concrete numbers)

From a real run on 2026-04-28 against a 1083-article KB:

- 122 successful claude calls (sonnet + haiku) = ~$0.50–$1.30 estimated
- 1290 sessions remaining to mine ≈ $30–50 sonnet
- 63 articles remaining to absorb ≈ $5–15 mixed haiku/sonnet

A single user with a heavy Claude Code week might generate 100+ sessions. Without local mode, that's ~$3–5 per week of overhead just for KB maintenance. With local mode for the cheap-but-numerous ops (facts, pass-1 plans), the weekly cost drops to ~$1–2.

---

## Progress log (most recent first)

### 2026-05-13 (afternoon) — Layer 2 e2e validated + reliability fixes

End-to-end runs against `2026-05-13-linkedin-x-tooling-comparison-may-2026.md`
in a clean test KB:

| run | provider/model | entities | parsed OK | applied OK | wallclock |
|---|---|---|---|---|---|
| anthropic/sonnet (tools) | 12/12 | n/a | 12 | ~5 min |
| anthropic/sonnet (json) | 12/12 | 12 | 12 | ~5 min |
| **ollama/qwen2.5-coder:14b (json + format:"json")** | 9/9 | **9** | **9** | ~19 min |

Reliability fixes shipped post-test (commit pending):

1. `loadConfig` logs yaml.Unmarshal errors (`config.go`) — was silent;
   one duplicate key wiped the whole absorb block. Surfaced during this
   round's debugging.
2. `ollamaProvider` now records cost-ledger entries with token counts
   and durations (`llm.go`). Local pass-2 shows up in `scribe cost`
   alongside anthropic calls.
3. New `jsonModeProvider` interface + `ollamaProvider.GenerateJSON`
   sets Ollama's `format: "json"` flag. qwen2.5-coder:14b now emits
   parseable envelopes 9/9 first try (vs 8/8 without the flag — same
   pass rate on this corpus, but the runtime constraint matters more
   for smaller models).
4. `{{TODAY}}` placeholder added to the json prompt and inlined in
   `absorbDenseTwoPass`. Fixes the local-model date-hallucination
   (`created: 2023-10-06` before, `created: 2026-05-13` after).
5. One-shot corrective retry on `parseEnvelope` failure via new
   `runPass2JSONOnce` helper. Installed but not triggered in any of
   the validation runs — `format: "json"` was sufficient on qwen.

Prompt tightening for known local-model misbehavior:

- Explicit "YAML frontmatter, NOT TOML `+++`" instruction.
- "`related:` must be wikilink strings `"[[X]]"`, not bare strings."
- "Do NOT fabricate `[c00-fN]` IDs when the facts block is empty."

Remaining local-model quirks (not blocking; tracked):

- qwen2.5-coder:14b still emits `related:` as bare YAML flow
  sequence `[[A]], [[B]]` instead of quoted `"[[A]]"`. yaml.v3 parses
  this as nested-list. The wiki backlink walker may or may not cope.
- qwen ignored the "don't fabricate IDs" instruction in the validation
  run with no facts pass. Switching on `atomic_facts: true` produces
  the real ID set the model can reference; otherwise drop the fact-ID
  bracket entirely.

Both look like instruction-resistance issues that vary by model. The
Perplexity research prompt (saved separately) targets exactly this
question: which Ollama models actually follow nested YAML/markdown
instructions inside a JSON envelope.

### 2026-05-13 — Phase 4B layer 2 wired

Files:
- `cmd/scribe/prompts/absorb-pass2-json.md` — new prompt. Inlines raw
  article body, plan JSON, facts block, and entity fields; instructs
  the model to emit exactly one `WikiActionEnvelope` JSON object to
  stdout (no tools, no prose, no fences).
- `cmd/scribe/config.go` — `AbsorbConfig.Pass2Mode` (default `tools`)
  and `Pass2Provider` (default `anthropic`). Coherence: any non-
  anthropic provider auto-flips mode to `json`; ollama + Claude alias
  swaps to `ollamaRecommendedModel` with a log line, same shape as
  Phase 4A's facts plumbing.
- `cmd/scribe/sync.go` — `absorbDenseTwoPass` branches on the mode in
  the per-entity goroutine: tools path is unchanged; json path runs
  `provider.Generate` → `extractJSON` → `parseEnvelope` →
  `applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true})`
  and logs applied/error counts.
- `cmd/scribe/absorb_pass2_json_test.go` — defaults + coherence +
  prompt-placeholder coverage.

Reused without modification: `extractJSON`, `parseEnvelope`,
`applyWikiActions`, `newLLMProvider`, `Contextualize.OllamaURL`.
Deliberately deferred: the "pre-search wiki for entity neighbors and
inline hint paths" idea from the original layer-2 list. Layer 3
(quality tuning) is where neighbor hints come in if envelope output
turns out to miss cross-references.

Flip on: in `scribe.yaml`,
```yaml
absorb:
  pass2_provider: ollama
  pass2_model: qwen2.5-coder:14b  # or mistral-small3:24b; gemma3:4b last resort
```
Mode auto-flips to `json`. `pass2_mode: json` with `pass2_provider:
anthropic` is also valid — useful for measuring envelope quality
against the tools path on the same model.

### 2026-05-12 — Token-budget audit; facts pass cut over to ollama

Trigger: overnight cron drained ~12% of weekly Claude Max quota and ~56% of a 4-hour session before wake-up. Heavy LaunchAgents (`com.scribe.sync-projects`, `com.scribe.sync-sessions`) unloaded; lockfiles released.

Yesterday's `output/costs/2026-05-11.jsonl` rolled up:

| op | calls | output tokens | wall time |
|---|---:|---:|---:|
| absorb-pass2 | 734 | 2.6M | 17.9 h |
| absorb-pass1-chapter | 658 | 2.1M | 10.1 h |
| absorb-facts | 657 | 2.1M | 6.3 h |
| absorb-single | 3 | 26K | 0.2 h |
| session-mine | 3 | 48K | 0.3 h |
| session-extract | 3 | 47K | 0.3 h |
| contextualize | 16 | 17K | 0.1 h |
| **Total OK** | **2,074** | **~7M** | **~35 h** |
| Failed (retries) | 11,220 | — | 40 min |

11,220 fast-failing retries (likely rate-limited / claude-missing-from-PATH) is itself a fix candidate — they're not free even when they don't reach the model.

Facts pass cutover landed in `scriptorium/scribe.yaml`:

```yaml
absorb:
  atomic_facts: true
  facts_provider: ollama
  facts_model: gemma3:4b
```

Ollama is running (`/opt/homebrew/opt/ollama/bin/ollama serve`, `gemma3:4b` pulled). Expected next-run reclaim: the 6.3h / 2.1M-token facts share, ~30% of total spend.

Pass-1 chapter (10.1h) + pass-2 (17.9h) = 28h / 4.7M output tokens still on Anthropic — that's the prize for Phase 4B layer 2.

### 2026-04-28 — Phase 4B groundwork landed (commit `bf021fa`)

Wiki action envelope schema + executor + 21 unit tests. Foundation
ready for the prompt + goroutine wiring to land on top.

Files:
- `cmd/scribe/wiki_actions.go` — `WikiActionEnvelope`, `WikiAction`,
  `applyWikiActions`, `validateActionPath`, `writeFileAtomic`,
  `appendToFile`, `replaceSection`, `updateFrontmatter`,
  `parseEnvelope`.
- `cmd/scribe/wiki_actions_test.go` — every op happy + error path,
  KB-rooting refusals, dry-run, partial-failure continuation.

Ops supported in the envelope: `create`, `append`, `replace_section`,
`update_frontmatter`. Sandboxed to the `wikiDirs` set; absolute paths
and `..` traversal refused.

### 2026-04-28 — Phase 4A landed (commit `c50207b` + `339734b`)

Facts pass routes through `llmProviderGenerator`. Setting
`absorb.facts_provider: ollama` in `scribe.yaml` keeps the per-chunk
fact extraction off Anthropic quota with no other change.

Validated end-to-end against `gemma3:4b` on `localhost:11434`:
2 chapters in parallel, 18 facts merged in 24.77s, all type values
valid (definition / claim / numeric / decision / citation). Zero
Anthropic spend on the run.

Integration test: `cmd/scribe/absorb_facts_integration_test.go`,
build-tag `integration`. Runs only when ollama is up + gemma3:4b is
pulled, otherwise t.Skip.

Files touched:
- `cmd/scribe/prompts/absorb-facts.md` — inlines `{{CHUNK_BODY}}`,
  asks for stdout JSON only (no `Write` tool).
- `cmd/scribe/absorb_facts.go` — `runFactsPass` reads chunk content,
  calls `provider.Generate`, runs `extractJSON`, writes per-chunk
  file directly.
- `cmd/scribe/absorb_facts.go` — `extractJSON` walks brace depth
  respecting strings/escapes; tolerates fenced JSON, preambles,
  trailing prose, string-internal braces. 8 unit tests.
- `cmd/scribe/config.go` — `AbsorbConfig.FactsProvider` (default
  `"anthropic"`) + ollama+Claude-alias coherence fixup.

### Earlier — Phase 3D.5, 3D, 3C, 3B, 3B.5

See git log; all production-validated through the 2026-04-28 sync run
(5 absorbs, 2 marker timeouts unrelated to this work).

---

## Resume plan

The next concrete step is Phase 4B layer 2: prompt + goroutine wiring
on top of the foundation that landed in `bf021fa`. Start by running
`make ci` to confirm the tree is still green before changing anything,
then work the layer-2 list below.

**Pre-flight before touching code:**
1. Re-enable Ollama if it stopped:
   `brew services start ollama && curl -s http://localhost:11434/api/tags | jq .`
2. Verify a successful facts-pass run since 2026-05-12 — grep
   `output/costs/$(date +%F).jsonl` for `ollamaProvider` /
   ollama-model entries, confirm absorb-facts duration dropped to
   sub-second per call.
3. Heavy LaunchAgents are unloaded as of 2026-05-12. Decide whether to
   re-enable before or after layer 2 lands — re-enabling before just
   resumes the Anthropic burn on pass-1 + pass-2.

### Phase 4B layer 2 — Prompt + wiring  ✅ landed 2026-05-13

Original work list, marked up with what shipped:

1. Create `cmd/scribe/prompts/absorb-pass2-json.md`. Same job as
   `absorb-pass2.md` but emits one `WikiActionEnvelope` JSON object
   to stdout instead of using Read/Write/Edit/Glob/Grep tools. The
   raw article body, plan JSON, neighboring article hints, and
   facts block all need to inline into the prompt (no filesystem
   access).

   Pre-search hint to inline: for each entity in the plan, run a
   wiki grep before pass-2 and pass the candidate paths + first 30
   lines of any matches into the prompt. The model picks
   "create new" vs "replace_section in <existing>" with that hint.

2. Add `Pass2Mode string` to `AbsorbConfig` (default `"tools"`,
   values `"tools" | "json"`). Auto-flip to `"json"` whenever
   `Pass2Provider` is set to a non-anthropic provider — the tools
   path doesn't work without claude -p.

3. Add `Pass2Provider string` to `AbsorbConfig` (default
   `"anthropic"`). Mirrors `FactsProvider` plumbing including the
   ollama+Claude-alias coherence fixup.

4. Branch in `absorbDenseTwoPass` (cmd/scribe/sync.go around line
   1730 inside the goroutine):
   - mode=tools → existing `runClaude` path (unchanged)
   - mode=json → `provider.Generate` → `extractJSON` →
     `parseEnvelope` → `applyWikiActions(root, env, ApplyOptions{
     AllowOverwrite: true})`. Log the result counts; treat
     `len(res.Errors) > 0` the same way the tools path treats a
     non-zero claude exit (warn, continue).

5. Tests:
   - Unit: prompt-template loading with all placeholders filled
   - Unit: pass-2 goroutine routes correctly based on Pass2Mode
   - Integration (build-tag): drive a real pass-2 envelope through
     ollama with gemma3:4b against a small raw article. Bigger model
     (qwen2.5-coder:14b or mistral-small3:24b) probably needed for
     reliable envelope generation; gemma3:4b may struggle on the
     more elaborate JSON shape. Falling back to qwen3:4b is the
     first thing to try if gemma3:4b misbehaves.

6. Update `absorbDefaultYAMLBlock` to surface the new knobs
   (commented-out, like Phase 4A's `facts_provider`).

7. Validate against the same `2026-04-28-articles-context_engineering_*`
   raw articles that already absorbed via the tools path, so we
   have a quality baseline to compare against.

### Phase 4B layer 3 — Quality tuning

Once layer 2 lands and the round-trip works, the actual quality
question opens: do the wiki articles produced via JSON envelope
match the quality of the tool-using path? Spot-check by:
- diffing the two output trees after running both modes against the
  same raw article
- checking the verbatim-citation rate (Phase 3B.5's `[c00-fN]` tags
  in quotes)
- counting orphaned wikilinks in the json-mode output (model can't
  grep, so cross-reference links may go stale faster)

If quality is meaningfully worse, the prompt's "neighbor hints"
need richer context (more lines of nearby articles), or we accept
that local-mode pass-2 is graceful-degradation rather than parity.

### Phase 4C — Session mining

Defer until 4B layer 2+3 prove out. Bigger schema (multiple wiki
files + rolling memory + sessions log) so envelope expressiveness
needs to grow first. The action types in `wiki_actions.go` already
cover most of what mining writes, but session mining also touches
`_sessions_log.json` — that's an indexes-only mutation that doesn't
fit the wiki-dirs sandbox; it would need a separate
`SessionsLogAction` op or a controlled escape hatch.

### Operational notes (post-2026-05-12)

Heavy crons stay disabled until 4B layer 2 ships:

```sh
launchctl unload ~/Library/LaunchAgents/com.scribe.sync-projects.plist
launchctl unload ~/Library/LaunchAgents/com.scribe.sync-sessions.plist
```

Light crons still running (no `claude -p` calls): `ingest-drain`,
`commit`, `auto-commit`, `lint`, `apply-identities`, `dream` (weekly),
`capture-imessage`, `capture-refetch`, `watch`.

Re-enable with `launchctl load <plist>` once 4B layer 2 lands and a
day of cost-ledger data shows pass-2 wall time has dropped.

Concurrent guardrail to consider adding alongside 4B: a daily output-
token ceiling in `runClaude` that aborts further Anthropic calls when
`output/costs/<today>.jsonl` crosses a configured limit. Belt-and-
braces against another runaway like 2026-05-11.
