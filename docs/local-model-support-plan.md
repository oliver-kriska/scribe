# Local Model Support — Planning Note

Status: **planning, not yet started**
Owner: Oliver
Filed: 2026-04-28

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

This note is intentionally a plan, not a TODO list. The next concrete step is Phase 4A: wire the existing provider abstraction into `runFactsPass`. That's a one-day task once the current Phase 3 work settles. Do it then.
