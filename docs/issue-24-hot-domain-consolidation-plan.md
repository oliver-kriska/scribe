# Issue #24 — Hot-domain mini consolidation between weekly dreams (roadmap-04)

GitHub issue #24, fourth in the roadmap umbrella (#6): "A small dream-like
consolidation pass over the single most-touched domain since the last run,
scheduled between the weekly full dreams. Keeps the busiest area of the KB
coherent without paying the full-dream LLM cost daily."

This plan is self-sufficient: it does not assume you have read the issue or
explored the codebase beyond what's quoted here. Every design question is
settled below — do not re-derive or re-litigate them during implementation.

---

## 1. Problem & context

`scribe dream` (`cmd/scribe/dream.go:19` `DreamCmd.Run`) runs a full
4-phase KB consolidation once a week (cron entry `cmd/scribe/cron.go:96-102`,
Sunday 02:00). It has two execution modes selected by `cfg.Dream.Mode`
(`cmd/scribe/config_llm.go:86-98` `DreamConfig`):

- **monolithic** — one long `claude -p` call with file tools, driven by
  `cmd/scribe/prompts/dream.md` (the 4-phase prose protocol).
- **orchestrator** (Phase 4D, forced when `llm.provider` is non-anthropic) —
  `cmd/scribe/dream_orchestrator.go:31` `runDreamOrchestrator`: pure-Go
  Phase 1 (orient) and Phase 4 (prune/index), with Phase 2.5/3/3.5
  (contradictions, consolidation, stub creation) collapsed into ONE bounded
  `WikiActionEnvelope` v2 LLM subtask. Go gathers a compact "orient packet"
  (`dreamReadLogTail`, `dreamSampleInventory`, `dreamStaleCandidates`,
  `dreamContradictionCandidates`, all in `dream_orchestrator.go`) and the
  LLM emits one JSON envelope that `applyWikiActions` executes.

Both modes touch the **whole KB** every time, which is why it only runs
weekly — a full LLM sweep over every article costs real tokens. Issue #24
wants a **cheap daily pass scoped to one domain**: whichever domain has seen
the most article churn since the last consolidation (full or hot), so the
busiest part of the KB doesn't go 6 days without a look while everything
else stays on the weekly cadence.

Key facts this plan relies on, verified in the codebase:

- **`domain` is per-article frontmatter**, not a directory
  (`cmd/scribe/frontmatter.go:25` `Frontmatter.Domain`), matched against
  `cfg.AllDomains()` (`cmd/scribe/config.go:688-706`, user's `domains:` list
  plus the universal `personal`/`general`). Articles live anywhere under
  `wikiDirs` (`cmd/scribe/config.go:62-65`:
  `wiki, projects, research, solutions, tools, decisions, patterns, ideas,
  people, sessions`) regardless of domain.
- **The cheapest reliable "what changed" signal is git history the KB
  already commits**, not a new ledger. `cmd/scribe/digest.go:229-287`
  `gitDigestActivity` already does exactly this shape of work for the
  `_digest.md` dashboard: `git log --since=<anchor> --name-status --no-renames
  -- <wikiDirs>`, bucketing by author. `cmd/scribe/digest.go:190-200`
  `articleDomain(root, rel)` reads a path's **current on-disk** frontmatter
  domain (not the historical blob at each commit) — this plan reuses both
  precedents directly.
- **Run history lives in `output/runs/*.jsonl`**
  (`cmd/scribe/main.go:228-269` `writeRunRecord`, one row per CLI invocation:
  `command`, `status`, `timestamp`, `args`, plus anything the command wrote
  into the package-level `runStats` map before returning). `doctor.go:1034-1101`
  `loadRunRecords` already reads this for freshness checks but its map is
  keyed by command+flag and aggregates every matching row — it cannot
  distinguish "a hot run that skipped early" from "a hot run that did real
  work." This plan adds a dedicated reader (§3) because that distinction is
  load-bearing here (see §5).
- **The daily output-token ceiling is enforced INSIDE the provider**, not by
  the caller. `cmd/scribe/llm.go:108-114` (`anthropicProvider.Generate`) and
  the equivalent in `cmd/scribe/llm_openai_compat.go:230-234` both call
  `checkBudget(a.root, effectiveOutputTokenCeiling(cfg.Sync))` before every
  metered call and return `ErrDailyBudgetExhausted` when the ceiling (per-KB
  `sync.daily_anthropic_output_token_ceiling` / `daily_output_token_ceiling`,
  or the machine-wide `daily_output_token_ceiling` in the user config) is
  hit. `ollamaProvider` never hits this (local = free,
  `cmd/scribe/budget.go:51-55` `localProviders`). **This means the hot pass
  automatically inherits the budget ceiling for free by going through the
  same `generateMaybeJSON` → provider path the full dream orchestrator
  uses — no new budget-check code is needed.**
- **`scribe each`** (`cmd/scribe/each.go`, issue #26, already merged) is the
  KB-agnostic cron dispatcher: one LaunchAgent per job runs
  `scribe each -- <job>` across every registered KB, with per-KB `Cadence`
  overrides in `scribe.yaml`'s `each.cadence:` map
  (`cmd/scribe/each.go:27-29` `EachConfig`). The hot-domain job is added the
  same way every other job is (`cmd/scribe/cron.go:60-167` `scribeJobs`) —
  no special-casing needed, and a KB that wants a slower cadence can already
  set `each.cadence["dream --hot"]` without any code change here.

---

## 2. Design decisions (all settled — do not re-open)

### 2.1 CLI shape: `scribe dream --hot [--domain <d>]`

One command family, not a new subcommand. `DreamCmd`
(`cmd/scribe/dream.go:14-17`) gets two new fields:

```go
type DreamCmd struct {
	DryRun bool   `help:"Show what would happen." name:"dry-run"`
	Model  string `help:"Claude model to use." default:"sonnet"`
	Hot    bool   `help:"Run the daily hot-domain mini consolidation instead of the full weekly cycle." name:"hot"`
	Domain string `help:"Explicit domain override for --hot (skips auto-selection and the churn-threshold gate)." name:"domain"`
}
```

**Why not `scribe dream --domain <d> --mini`:** two flags for one concept
(mini-ness) is redundant — `--domain` alone can't say "run the mini pass on
the auto-selected domain," which is the default cron behavior. `--hot` is
the on/off switch; `--domain` is purely an override for manual runs
(`scribe dream --hot --domain tools` to force a specific domain, e.g. for
debugging). `--domain` without `--hot` is a no-op flag (ignored) — the full
dream protocol has no domain concept and is not being changed.

**Why not a new `HotDreamCmd` subcommand:** it would duplicate the
lock/lease/budget/prompt-loading/envelope-apply/commit machinery that
`DreamCmd` already owns, and it would need its own entry in every place
`dream` is referenced (cron, doctor freshness, README). Reusing `DreamCmd`
means `scribe dream --help` documents both modes in one place and
`output/runs/*.jsonl` rows all share `command: "dream"`, which is what makes
the run-history split in §3 tractable using `args` alone.

### 2.2 "Touched since last run" signal: git log, anchored to the last real consolidation

`hotDomainTouchCounts` (new, `cmd/scribe/dream_hot.go`) runs:

```
git log --since=<since> --pretty=format: --name-status --no-renames -- <wikiDirs>
```

filters to `.md` paths whose basename doesn't start with `_` (machinery
files, same exclusion as `gitDigestActivity`), keeps only `A`/`M` status
lines (added/modified — deletions and renames-as-D+A don't count as
"churn to consolidate"), **dedupes to distinct file paths** (one file
committed 5 times in the window counts once — otherwise an hourly
auto-commit habit on one article would dominate the signal, which
`gitDigestActivity`'s `fileSet` dedup pattern already establishes as the
right shape), then reads each touched file's **current** frontmatter domain
via `articleDomain(root, path)` and tallies counts per domain, filtered to
`cfg.AllDomains()` (an unrecognized/empty domain is excluded, not bucketed
under a catch-all — see §7 edge cases).

**`since` anchor — rejected alternatives and the chosen one:**

- ❌ Rolling wall-clock window (`--since=24.hours.ago`): drifts with
  whatever hour cron happens to run, and duplicates work already counted in
  a prior day's window if that domain didn't cross the threshold yesterday
  (churn should *accumulate* toward the threshold across quiet days, not
  reset daily).
- ❌ `wiki/_index.md`/`log.md` timestamps: those are regenerated by dream
  itself and by other commands (index, lint), so they don't mean "when did
  a human/absorb pipeline last touch a domain."
- ✅ **The more recent of the last successful full dream and the last
  successful hot dream that actually did work**, falling back to
  `now - cfg.Dream.HotLookbackDays` (new field, default 14) when neither has
  ever run. This is exactly "since the last time this domain's territory was
  consolidated," which is what "churn since last run" means for this
  feature. The "that actually did work" qualifier is the subtle part — see
  §3.

### 2.3 Self-gating (exit 0 fast, no LLM call) — two independent gates

Both must pass before the LLM is called, UNLESS `--domain` is explicitly
set (see below):

1. **Full-dream-just-covered-it gate.** If the last successful full dream
   (any `dream` run whose `args` do NOT contain `--hot`) happened less than
   `hotSkipIfFullWithin = 24 * time.Hour` ago, skip. Rationale: the hot job
   is scheduled daily; 24h matches that cadence exactly, so the Monday hot
   run after Sunday 02:00's full dream self-gates (dream ran ~25h20m
   earlier at the chosen 03:10 daily slot — see §2.5 — which is just past
   24h, so Monday's run does NOT skip and Tuesday onward behaves normally).
   This directly satisfies the issue's "must not run when the full weekly
   dream just covered it."
2. **Churn-threshold gate.** After computing per-domain touch counts since
   the anchor (§2.2), pick the domain with the highest count
   (`selectHotDomain`, ties broken alphabetically for determinism). If the
   winning count is below `cfg.Dream.HotMinTouches` (new field, default 3),
   skip — "no meaningful churn."

**`--domain <d>` bypasses gate 2 entirely** (there is no selection to gate —
the user named the domain) but **still respects gate 1** is the wrong
call — reject that. Settled: **`--domain` bypasses BOTH self-gating checks**
(it's an explicit human/cron override; if someone runs
`scribe dream --hot --domain tools` five minutes after a full dream, they
asked for it on purpose — e.g. debugging the hot path itself, or forcing
attention on a domain the auto-selector wouldn't pick). It still goes
through the lock, the team lease, and the budget ceiling — those are hard
safety constraints, not scheduling heuristics, and are never bypassable by
a flag.

A skip logs one line via `logMsg("dream", ...)` and returns `nil` — this
still writes a `status: "ok"` run record (§3 explains why that's correct
for freshness but wrong for the churn anchor if left unguarded).

### 2.4 Coordination: same lock, same team lease as the full dream — duplicated, not shared, code

The full dream holds a process-local file lock for the whole cycle
(`cmd/scribe/dream.go:53-62`, `lockPathFor(cfg.LockDir, "dream")` +
`acquireLock`/`releaseLock`) and, for team KBs, a git-committed cross-machine
lease (`cmd/scribe/dream.go:67-74`, `acquireDreamLease`/`releaseDreamLease`,
`cmd/scribe/dream_lease.go`).

**Decision: the hot pass reuses the literal same lock path (`"dream"`) and
the literal same team lease**, so a hot pass and a full dream can never run
concurrently on one machine, and on a team KB only one machine's dream
activity (full or hot) proceeds at a time. **The code for acquiring them is
duplicated** (~15 lines) into `runHotDream` rather than extracted into a
shared helper — deliberately, to avoid touching the working full-dream
lock/lease block in `dream.go` at all (lower regression risk for a plan an
unfamiliar agent implements). This is the one place in this plan where
duplication over extraction is the correct call; §2.5 below explains why the
*opposite* call is made for the post-LLM commit tail.

Rejected: giving the hot pass its own lock name (e.g. `"dream-hot"`). That
would let a hot pass and a full dream run genuinely concurrently, both
mutating `wiki/_backlinks.json`/`wiki/_index.md`/`log.md` and racing on the
git commit — the exact class of bug the existing lock exists to prevent.

### 2.5 Extract the post-LLM commit tail into a shared `commitDreamCycle` helper

`dream.go:111-180` (from `// Post-dream validation` to the final `logMsg("dream", "done")`)
is generic once the LLM step has produced its mutations: article-count
safety guard (never delete >5 articles), `git status --porcelain` check,
backlinks/index rebuild, secret-gated commit + push, qmd reindex, hot-cache
rewrite. The hot pass needs the identical sequence, just with a different
commit-message prefix and (for the safety guard) its own `preCount`.

**Decision: extract this into `commitDreamCycle(root, today, commitMsgPrefix
string, preCount int) error`**, defined in `dream.go` right after `Run()`
(replacing lines 111-180 with `return commitDreamCycle(root, today, "dream",
preCount)`), and call it from both `DreamCmd.Run` and the new
`runHotDream`. Unlike §2.4, extraction is the right call here because:

- It's a pure, mechanical code move (same git/qmd calls, same order,
  no branching added) — low regression risk.
- **It fixes a real latent bug as a side effect, which is in scope to fix
  here because the new code depends on NOT repeating it.** Original
  `dream.go:116-120` does `runStats = map[string]any{...}` — a bare
  **reassignment**, not a merge. In orchestrator mode, `runDreamOrchestrator`
  (`dream_orchestrator.go:91-97`) sets `runStats["mode"] = "orchestrator"`
  and the envelope-count fields *before* returning to `dream.go`, and that
  whole map is then silently discarded when the old code overwrites
  `runStats` a few lines later — so `output/runs/*.jsonl` never actually
  records `mode: "orchestrator"` for the full dream today. The hot pass
  needs its `mode: "hot"` field to survive this exact same point in the
  flow (§3 depends on it being present in the JSONL row), so
  `commitDreamCycle` uses **additive** assignment:
  `if runStats == nil { runStats = map[string]any{} }` then individual key
  sets, never a bare `=` on the whole map. This is a one-line behavior
  change to the existing full-dream path (an existing field that should
  have been recorded now is), not a functional change to what dream does —
  call this out explicitly in the PR description. Add a regression test
  (§4) proving `commitDreamCycle` preserves pre-existing `runStats` keys.

### 2.6 New `DreamConfig` fields — no new top-level YAML block

Add two fields to `DreamConfig` (`cmd/scribe/config_llm.go:86-98`), not a
new `dream_hot:` block — the hot pass is a mode of dream, and it already
inherits `Provider`/`Model`/`OllamaURL`/`Mode`/`TimeoutMin`/`NumCtx` from
`cfg.Dream` unchanged (same provider routing, same envelope machinery):

```go
type DreamConfig struct {
	Provider   string `yaml:"provider"`
	Model      string `yaml:"model"`
	OllamaURL  string `yaml:"ollama_url"`
	Mode       string `yaml:"mode"`
	TimeoutMin int    `yaml:"timeout_min"`
	NumCtx     int    `yaml:"num_ctx"`
	// HotMinTouches is the minimum number of distinct touched articles a
	// domain must accumulate since the last dream (hot or full) before
	// `scribe dream --hot` spends an LLM call on it. Below this, the
	// auto-selected domain is judged to have no meaningful churn and the
	// command exits 0 without calling the provider. Ignored when --domain
	// explicitly names a domain.
	HotMinTouches int `yaml:"hot_min_touches"`
	// HotLookbackDays caps the git-log churn window when neither a full
	// nor a hot dream run record exists yet (a brand-new KB), so the very
	// first hot pass doesn't scan the KB's entire history.
	HotLookbackDays int `yaml:"hot_lookback_days"`
}
```

`applyDreamDefaults` (`cmd/scribe/config_llm.go:406-420`) gets two new lines
before the final `coerceProviderModel` call:

```go
if cfg.HotMinTouches <= 0 {
	cfg.HotMinTouches = 3
}
if cfg.HotLookbackDays <= 0 {
	cfg.HotLookbackDays = 14
}
```

No `templates/scribe.yaml` change needed — that template only documents the
top-level `llm:` and `sync:` blocks (verified: no `dream:`, `session_mine:`,
`assess:`, or `each:` block is present there either); every other per-op
config block relies on code defaults + README documentation, and this
follows the same precedent.

### 2.7 Cron cadence: daily, self-gating does the rest

New entry in `scribeJobs` (`cmd/scribe/cron.go:60-167`), inserted right
after the existing `"dream"` job (after line 102, before `"lint-fix"`):

```go
{
	Name:     "dream-hot",
	Desc:     "Daily hot-domain mini consolidation (self-gating)",
	Command:  each("dream --hot"),
	LogFile:  filepath.Join(logDir, "scribe-dream-hot.log"),
	Schedule: schedSpec{Calendar: []calTime{{Hour: 3, Minute: 10, Weekday: -1}}},
},
```

`Hour: 3, Minute: 10, Weekday: -1` = every day at 03:10 (`Weekday: -1` means
omit, matching the existing `hourlyAt`/`everyNHoursAt` daily-job pattern,
e.g. the `capture-refetch` job at `cron.go:132-137`). Picked to sit between
`sync-sessions`'s 03:00 slot and the Saturday 01:00-01:55 lint block, with
no time collision. Runs every day including Sunday — self-gating (§2.3) is
what makes that safe: the Sunday 03:10 run sees the 02:00 full dream from
the same morning and skips via gate 1; every other day it does real
selection. This is what "keep the command self-gating so scheduling stays
dumb" (per the task brief, in anticipation of issue #26's cron registry
work) means in practice — cron doesn't need to know anything about domains,
churn, or the weekly dream's timing.

### 2.8 `doctor` freshness: track it, but only as "was it invoked," not "did it find work"

Add one row to `freshnessSpecs` (`cmd/scribe/doctor.go:1023-1031`), right
after the existing `dream` row:

```go
{Command: "dream", ArgFlag: "--hot", Label: "dream (hot)", MaxGap: 36 * time.Hour, Fix: "scribe dream --hot"},
```

This uses the **existing, unmodified** `loadRunRecords`/`freshnessSpec`
machinery (`doctor.go:1041-1101`), which already tracks "last ok run of
`dream --hot`" the same way it tracks `sync --sessions` vs bare `sync`
(`cmd/scribe/doctor.go:1025`) — any successful invocation counts, including
a self-gated skip, because for freshness purposes "cron is still invoking
this job on schedule" is exactly what a self-gated `ok` exit proves.
`MaxGap: 36h` gives a full day of slack over the 24h cadence so one missed
tick (machine asleep, etc.) doesn't immediately WARN.

**This is intentionally a different definition of "recent" than the churn
anchor in §2.2/§3** — doctor's freshness and the churn window answer
different questions (see §3 for why conflating them is a bug, not a
simplification).

### 2.9 Prompt: a new domain-scoped envelope prompt pair, reusing the existing sampler functions with an added filter

`dream_orchestrator.go`'s three orient-packet builders currently sample the
**whole KB**. Add a `domain string` parameter to each (empty string = no
filter, preserving today's full-dream behavior exactly):

- `dreamSampleInventory(root string, maxEntries int) string` →
  `dreamSampleInventory(root, domain string, maxEntries int) string`
  (`dream_orchestrator.go:131`). Add, right after the existing
  `fm, err := parseFrontmatter(content)` nil-check (line ~137):
  ```go
  if domain != "" && fm.Domain != domain {
  	return nil
  }
  ```
- `dreamStaleCandidates(root string, days int) string` →
  `dreamStaleCandidates(root, domain string, days int) string`
  (`dream_orchestrator.go:171`). Same filter, added after the frontmatter
  parse/nil-check, before the `updated := stringFromAny(fm.Updated)` line.
- `dreamContradictionCandidates(root string) string` →
  `dreamContradictionCandidates(root, domain string) string`
  (`dream_orchestrator.go:203`). Same filter, added after the frontmatter
  parse/nil-check, before building the `article{...}` value (this scopes
  contradiction pairs to the domain even though they're grouped by tag —
  correct, since a cross-domain tag collision isn't this pass's job).

Update the three call sites in `runDreamOrchestrator`
(`dream_orchestrator.go:32-35`) to pass `""` explicitly — **zero behavior
change** for the full dream:

```go
logTail := dreamReadLogTail(root, 20)
inventory := dreamSampleInventory(root, "", 40)
stale := dreamStaleCandidates(root, "", 60)
contradictions := dreamContradictionCandidates(root, "")
```

**Why extend the existing functions instead of writing parallel
domain-scoped copies:** the alternative is two versions of near-identical
frontmatter-walking logic that drift over time (e.g. a future change to how
staleness is computed would need to land twice). A single optional filter
parameter is a 1-line diff per function and keeps one implementation.

New prompt files (embedded via the existing `go:embed` glob over
`prompts/`, `cmd/scribe/claude.go` — no manifest to update, any file under
`prompts/` is picked up automatically): `cmd/scribe/prompts/dream-hot-anthropic.md`
and `cmd/scribe/prompts/dream-hot-ollama.md`, loaded via
`promptForProvider("dream-hot", providerNameFor(provider))`
(`claude.go:319-332`, exact same dispatch pattern `runDreamOrchestrator`
already uses for `"dream"`). Exact contents:

**`cmd/scribe/prompts/dream-hot-anthropic.md`:**

```
You are the LLM consolidation step inside a Go orchestrator that runs the daily hot-domain mini-dream. You do NOT have filesystem tools — the orient packet is inlined below, scoped to ONE domain. Emit ONE `WikiActionEnvelope` v2 JSON document with the consolidation actions + meta log entry. Scribe applies the mutations and re-runs index/backlinks itself.

## Today

{{TODAY}}

## Domain

This pass is scoped to ONE domain: `{{DOMAIN}}`. Every article below belongs to this domain. Do not reference or modify articles outside it — you were not shown the rest of the KB, so any claim about another domain would be invented.

## Orient packet — what scribe gathered for you (domain={{DOMAIN}} only)

Recent log entries (last ~20 lines of log.md, whole-KB — for context only, other domains may appear here):

<<<LOG_BEGIN>>>
{{LOG_TAIL}}
<<<LOG_END>>>

Article inventory for this domain (title, type, domain, confidence, updated, path):

<<<INVENTORY_BEGIN>>>
{{INVENTORY}}
<<<INVENTORY_END>>>

Stale candidates in this domain (zero links + updated > 60 days ago — paths only):

<<<STALE_BEGIN>>>
{{STALE}}
<<<STALE_END>>>

Contradiction candidates in this domain (pairs sharing tags that may conflict — Go pre-filtered):

<<<CONTRADICTIONS_BEGIN>>>
{{CONTRADICTIONS}}
<<<CONTRADICTIONS_END>>>

## What to emit

ONE envelope, scoped to `{{DOMAIN}}` only. Cover:

1. **Contradictions** — for each Solid-vs-Solid pair, emit a `replace_section` or `create` action that rewrites the losing article in place and adds a `Status: reconsidered {{TODAY}} — superseded claim "X" with "Y"` line at the top. For Solid-vs-Vague, rewrite or remove the vague claim. Skip Vague-vs-Vague.

2. **Stale dates** — for any article where `updated:` is >90 days old AND the claim is still valid, emit an `update_frontmatter` action setting `updated: {{TODAY}}`.

3. **Stub creation** — for entity names appearing in 3+ of the articles shown above (this domain only) that don't have a wiki page, emit a `create` action with a 2-3 sentence stub plus wikilinks back to sources. Set `domain: {{DOMAIN}}` in the stub's frontmatter.

4. **Decay tagging** — ONLY for a path that appears **verbatim in the stale-candidates list above** and shows no signal of value, emit an `append` action adding `<!-- decay-candidate {{TODAY}} -->` as the last line. Never decay-mark a path that is not in that list. If the stale list is empty, emit **zero** decay actions.

5. **Meta** — emit ONE `log_append` MetaAction with a one-line summary: `## [{{TODAY}}] dream-hot | domain={{DOMAIN}} | <summary>`.

## Output schema

```json
{
  "version": 2,
  "entity": "dream-hot-{{DOMAIN}}-{{TODAY}}",
  "notes": "optional commentary",
  "actions": [ ... ],
  "meta": [
    {"op": "log_append", "line": "## [{{TODAY}}] dream-hot | domain={{DOMAIN}} | <one-line summary>"}
  ]
}
```

## Rules

- Path must be rooted in wiki/, projects/, research/, solutions/, tools/, decisions/, patterns/, ideas/, people/, sessions/.
- Every `create` action's frontmatter must set `domain: {{DOMAIN}}` — this pass must not scatter articles into other domains.
- Use `replace_section` when you only want to swap the body of one `## Heading`. Use `update_frontmatter` for date bumps. Use `append` for decay markers. Use `create` only for stub articles.
- An empty actions list is legal — emit `"actions": []` if nothing genuinely needs to change. Still include the `log_append`.
- NEVER target ANY file whose basename starts with `_` (e.g. `_index.md`, `_backlinks.json`, `_absorb_log.json`, `_hot.md`, `_staleness.jsonl`). Scribe generates these and writing one corrupts the KB. The executor rejects them. Use `create` for a new file; use `append` only for a file you were shown exists.
- Be conservative: this is a small daily pass, not the weekly dream. When in doubt, do less.

## Output reminder

Stdout must be ONE JSON object matching `WikiActionEnvelope` v2. No prose. No code fences.
```

**`cmd/scribe/prompts/dream-hot-ollama.md`:**

```
OUTPUT ONLY ONE JSON OBJECT. NO PROSE. NO CODE FENCES.

You are the LLM consolidation step inside Go's daily hot-domain mini-dream orchestrator. Emit one `WikiActionEnvelope` v2, scoped to ONE domain.

## Today

{{TODAY}}

## Domain

This pass covers ONLY domain `{{DOMAIN}}`. Every article below belongs to it. Do not touch or invent claims about any other domain.

## Recent log (whole-KB, for context only)

<<<LOG_BEGIN>>>
{{LOG_TAIL}}
<<<LOG_END>>>

## Article inventory (domain={{DOMAIN}} only)

<<<INVENTORY_BEGIN>>>
{{INVENTORY}}
<<<INVENTORY_END>>>

## Stale candidates in this domain (zero links, updated >60 days ago)

<<<STALE_BEGIN>>>
{{STALE}}
<<<STALE_END>>>

## Contradiction candidates in this domain (pre-filtered by Go)

<<<CONTRADICTIONS_BEGIN>>>
{{CONTRADICTIONS}}
<<<CONTRADICTIONS_END>>>

## Output schema (v2 envelope)

```json
{
  "version": 2,
  "entity": "dream-hot-{{DOMAIN}}-{{TODAY}}",
  "actions": [
    {"op": "update_frontmatter", "path": "<file>", "frontmatter": {"updated": "{{TODAY}}"}},
    {"op": "replace_section", "path": "<file>", "heading": "<heading>", "content": "<body>"},
    {"op": "create", "path": "<dir>/<slug>.md", "content": "---\nfrontmatter (domain: {{DOMAIN}})\n---\n\nbody"},
    {"op": "append", "path": "<file>", "content": "\n<!-- decay-candidate {{TODAY}} -->\n"}
  ],
  "meta": [
    {"op": "log_append", "line": "## [{{TODAY}}] dream-hot | domain={{DOMAIN}} | <one-line summary>"}
  ]
}
```

## Rules

- ALWAYS include one `log_append` in meta, even when actions is empty (`"actions": []`).
- Path rooted in: wiki/, projects/, research/, solutions/, tools/, decisions/, patterns/, ideas/, people/, sessions/.
- Every `create` action's frontmatter MUST set `domain: {{DOMAIN}}`.
- Use `update_frontmatter` for date bumps (cheapest action).
- Use `append` for decay markers — content `"\n<!-- decay-candidate {{TODAY}} -->\n"` — and ONLY on a path listed verbatim under "Stale candidates" above. If that list is empty, emit NO decay append.
- Use `replace_section` to swap a body without rewriting frontmatter.
- Use `create` for stub articles (entity referenced in 3+ of the shown articles but no wiki page yet, this domain only).
- NEVER target ANY file whose basename starts with `_`. Scribe generates these and writing one corrupts the KB. The executor rejects them.
- Be conservative: if unsure, emit `"actions": []` and a log_append explaining "no changes warranted". This is a small daily pass, not the weekly dream.

OUTPUT: ONE JSON OBJECT. NO PROSE. NO CODE FENCES.
```

---

## 3. Why the churn anchor needs a "did it actually work" filter (the one non-obvious piece)

If `hotDomainSince` (§2.2) anchors purely on "the last successful `dream
--hot` invocation" (any `status: "ok"` row, including self-gated skips),
the anchor **advances every single day regardless of whether real
consolidation happened**, because a self-gated skip still returns `nil` and
still gets a `status: "ok"` run record written by `writeRunRecord`
(`main.go:228`, called unconditionally by the CLI dispatcher for every
command that doesn't return an error).

Concretely: a domain accumulating 1 touch/day, threshold 3. Day 1: window is
"since day 0," sees 1 touch, below threshold, **skips**. If the skip still
advances the anchor to "day 1," day 2's window becomes "since day 1" (not
"since day 0") — it only sees the ONE touch from day 2, forever below
threshold, and the domain can never cross the threshold no matter how much
time passes. The anchor must only advance when a hot pass **did real
work** (reached the LLM call), not merely when it was invoked.

**Fix:** `dreamRunHistory` (new, `cmd/scribe/dream_hot.go`) parses
`output/runs/*.jsonl` rows as a raw `map[string]any` (not the typed
`runRecord` struct in `doctor.go`, which doesn't expose merged `runStats`
fields) and only counts a `--hot` row toward `lastHot` when the row also
carries `"mode": "hot"` — which `runHotDream` only stamps into `runStats`
**after** passing both self-gates, immediately before the LLM call (see
§4). A self-gated skip never sets it, so it never advances `lastHot`.

The full-dream side (`lastFull`) does NOT need this filter: a full dream
that ran and found nothing to change (`"no changes made"`,
`dream.go:172-173`) still executed the entire orient→LLM→apply pipeline —
that's a real "the whole KB was just reviewed" event, exactly what gate 1
in §2.3 wants to detect. The only way a full dream returns `status: "ok"`
without doing that work is lock contention (`"another dream cycle is
running — exiting"`), which is rare (the weekly full dream is the only
unflagged `dream` cron entry) and low-consequence if it slips through
(worst case: the hot pass skips one extra day and self-heals the next).
This asymmetry — high consequence for the hot side, low for the full side —
is why only `lastHot` gets the extra filter. Do not add it to `lastFull`;
it's unnecessary complexity for a near-zero-probability case.

```go
// dreamRunHistory scans output/runs/*.jsonl for the newest successful
// `dream` invocation of each kind: lastFull is the newest run whose args
// did NOT include --hot; lastHot is the newest --hot run that actually
// reached consolidation (runStats["mode"] == "hot" was merged into the
// row — see runHotDream). A self-gated --hot skip never sets that field,
// so it correctly does not advance lastHot; see plan §3 for why that
// matters.
func dreamRunHistory(root string) (lastFull, lastHot time.Time) {
	runsDir := filepath.Join(root, "output", "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return time.Time{}, time.Time{}
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(runsDir, e.Name()))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			var row map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
				continue
			}
			if s, _ := row["status"].(string); s != "ok" {
				continue
			}
			if c, _ := row["command"].(string); c != "dream" {
				continue
			}
			ts, err := time.Parse(time.RFC3339, fmt.Sprint(row["timestamp"]))
			if err != nil {
				continue
			}
			isHotInvocation := false
			if argsAny, ok := row["args"].([]any); ok {
				for _, a := range argsAny {
					if s, _ := a.(string); s == "--hot" {
						isHotInvocation = true
						break
					}
				}
			}
			if !isHotInvocation {
				if ts.After(lastFull) {
					lastFull = ts
				}
				continue
			}
			if mode, _ := row["mode"].(string); mode == "hot" && ts.After(lastHot) {
				lastHot = ts
			}
		}
		_ = f.Close()
	}
	return lastFull, lastHot
}
```

---

## 4. Implementation steps (file-by-file)

### 4.1 `cmd/scribe/config_llm.go`

- Add `HotMinTouches int` and `HotLookbackDays int` fields to `DreamConfig`
  (after `NumCtx`, around line 97) — full doc comments given in §2.6.
- In `applyDreamDefaults` (lines 406-420), add the two default-fill blocks
  from §2.6, before the final `cfg.Provider, cfg.Model =
  coerceProviderModel(...)` line.

### 4.2 `cmd/scribe/dream.go`

- Add `Hot bool` and `Domain string` fields to `DreamCmd` (§2.1).
- In `Run()`, insert immediately after the existing
  `if err := cfg.requireParseable(); err != nil { return err }` block
  (line 28) and before `today := time.Now().Format("2006-01-02")` (line 29):
  ```go
  if d.Hot {
  	return runHotDream(root, cfg, d.Domain, d.DryRun)
  }
  ```
  This makes the hot path skip the `effectiveModel`/mode logging (lines
  29-42), which only makes sense for the full-dream mode split.
- Replace lines 111-180 (from `// Post-dream validation` through the final
  `return nil` before the closing `}` of `Run`) with:
  ```go
  return commitDreamCycle(root, today, "dream", preCount)
  ```
- Immediately after `Run()` (before `countArticles`), add the extracted
  `commitDreamCycle` function — full body given in §2.5's rationale; the
  code is `dream.go:111-180` verbatim, with:
  - `runStats = map[string]any{...}` replaced by
    `if runStats == nil { runStats = map[string]any{} }` followed by three
    individual `runStats["articles_before"] = preCount` /
    `["articles_after"]` / `["articles_delta"]` assignments.
  - The hardcoded `"dream: %s (%d files)"` format replaced with
    `fmt.Sprintf("%s: %s (%d files)", commitMsgPrefix, today, changedCount)`.
  - Signature: `func commitDreamCycle(root, today, commitMsgPrefix string, preCount int) error`.

No other change to `dream.go`'s existing monolithic/orchestrator dispatch
(lines 76-109) — leave it exactly as-is.

### 4.3 `cmd/scribe/dream_orchestrator.go`

- Add a `domain string` parameter to `dreamSampleInventory`,
  `dreamStaleCandidates`, and `dreamContradictionCandidates`, with the
  filter line inserted as specified in §2.9.
- Update the three call sites inside `runDreamOrchestrator` (lines 32-35)
  to pass `""` as the new argument, preserving current behavior exactly.

### 4.4 New file `cmd/scribe/dream_hot.go`

Implements, in this order:

1. `hotDomainTouchCounts(root string, cfg *ScribeConfig, since time.Time) map[string]int`
   — §2.2's git-log + dedupe + domain-tally logic. Imports needed:
   `path/filepath`, `strings`, `time` (root/cfg already typed elsewhere in
   package). Uses `runCmd` (`cmd/scribe/claude.go:357`) and `articleDomain`
   (`cmd/scribe/digest.go:190`, same package, no import needed) and
   `wikiDirs` (`cmd/scribe/config.go:62`).

2. `selectHotDomain(counts map[string]int, minTouches int) (domain string, touches int, ok bool)`
   — sorts domain keys alphabetically (`sort.Strings`) before scanning for
   the max so ties resolve deterministically; returns `ok=false` if the
   winning count is below `minTouches`.

3. `dreamRunHistory(root string) (lastFull, lastHot time.Time)` — exact
   code in §3.

4. `hotDomainSince(cfg *ScribeConfig, lastFull, lastHot, now time.Time) time.Time`
   — `anchor := lastFull; if lastHot.After(anchor) { anchor = lastHot };
   if anchor.IsZero() { return now.AddDate(0, 0, -cfg.Dream.HotLookbackDays) };
   return anchor`.

5. `runHotDream(root string, cfg *ScribeConfig, domainOverride string, dryRun bool) error`
   — the orchestrating function:

   ```go
   func runHotDream(root string, cfg *ScribeConfig, domainOverride string, dryRun bool) error {
   	now := time.Now()
   	lastFull, lastHot := dreamRunHistory(root)

   	var domain string
   	var touches int
   	if domainOverride != "" {
   		domain = domainOverride
   		touches = -1
   	} else {
   		if !lastFull.IsZero() && now.Sub(lastFull) < 24*time.Hour {
   			logMsg("dream", "hot: full dream ran %s ago — skipping (weekly cycle just covered the whole KB)", shortDuration(now.Sub(lastFull)))
   			return nil
   		}
   		since := hotDomainSince(cfg, lastFull, lastHot, now)
   		counts := hotDomainTouchCounts(root, cfg, since)
   		d, t, ok := selectHotDomain(counts, cfg.Dream.HotMinTouches)
   		if !ok {
   			logMsg("dream", "hot: no domain crossed the churn threshold (%d touches) since %s — nothing to do", cfg.Dream.HotMinTouches, since.Format("2006-01-02"))
   			return nil
   		}
   		domain, touches = d, t
   	}

   	logMsg("dream", "hot: selected domain=%s touches=%d", domain, touches)
   	if dryRun {
   		logMsg("dream", "DRY RUN — would run hot-domain consolidation on domain=%s", domain)
   		return nil
   	}

   	lockPath := lockPathFor(cfg.LockDir, "dream")
   	lf, ok, lerr := acquireLock(lockPath)
   	if lerr != nil {
   		return fmt.Errorf("lock %s: %w", lockPath, lerr)
   	}
   	if !ok {
   		logMsg("dream", "hot: another dream cycle is running — exiting")
   		return nil
   	}
   	defer releaseLock(lf)

   	if cfg.Team {
   		acquired, holder := acquireDreamLease(root, now)
   		if !acquired {
   			logMsg("dream", "hot: dream lease held by %s — skipping this cycle", holder)
   			return nil
   		}
   		defer releaseDreamLease(root)
   	}

   	today := now.Format("2006-01-02")
   	preCount := countArticles(root)
   	ctx := context.Background()

   	logTail := dreamReadLogTail(root, 20)
   	inventory := dreamSampleInventory(root, domain, 40)
   	stale := dreamStaleCandidates(root, domain, 60)
   	contradictions := dreamContradictionCandidates(root, domain)

   	provider := newLLMProvider(cfg.Dream.Provider, cfg.Dream.Model, cfg.Dream.OllamaURL, root)
   	promptName := promptForProvider("dream-hot", providerNameFor(provider))
   	prompt, err := loadPrompt(promptName, map[string]string{
   		"KB_DIR":         root,
   		"TODAY":          today,
   		"DOMAIN":         domain,
   		"LOG_TAIL":       logTail,
   		"INVENTORY":      inventory,
   		"STALE":          stale,
   		"CONTRADICTIONS": contradictions,
   	})
   	if err != nil {
   		return fmt.Errorf("load dream-hot prompt: %w", err)
   	}

   	timeout := time.Duration(cfg.Dream.TimeoutMin) * time.Minute
   	tagged := withOllamaNumCtx(withOpLabel(ctx, "dream-hot"), cfg.Dream.NumCtx)
   	callCtx, cancel := context.WithTimeout(tagged, timeout)
   	defer cancel()

   	// Stamp mode/domain BEFORE the LLM call: this is the "real work
   	// started" marker dreamRunHistory (§3) keys off. A skip above never
   	// reaches this line.
   	if runStats == nil {
   		runStats = map[string]any{}
   	}
   	runStats["mode"] = "hot"
   	runStats["hot_domain"] = domain
   	runStats["hot_domain_touches"] = touches

   	out, err := generateMaybeJSON(callCtx, provider, prompt)
   	if err != nil {
   		if errors.Is(err, ErrRateLimit) || errors.Is(err, ErrDailyBudgetExhausted) {
   			logMsg("dream", "hot: rate limited / budget exhausted — cycle interrupted (%v)", err)
   			return nil
   		}
   		return fmt.Errorf("dream-hot LLM call: %w", err)
   	}
   	jsonText, ok := extractJSON(out)
   	if !ok {
   		return fmt.Errorf("dream-hot: no JSON envelope in provider output (%d bytes)", len(out))
   	}
   	env, err := parseEnvelopeV2(jsonText, "dream-hot")
   	if err != nil {
   		return fmt.Errorf("dream-hot: parse envelope: %w", err)
   	}
   	res, err := applyWikiActions(root, env, entityWriterApplyOptions())
   	if err != nil {
   		return fmt.Errorf("dream-hot: apply actions: %w", err)
   	}
   	runStats["envelope_actions_applied"] = len(res.Applied)
   	runStats["envelope_actions_errored"] = len(res.Errors)
   	if len(res.Errors) > 0 {
   		logMsg("dream", "hot: envelope: %d applied, %d errors: %v", len(res.Applied), len(res.Errors), res.Errors)
   	} else {
   		logMsg("dream", "hot: envelope: applied %d action(s)", len(res.Applied))
   	}

   	return commitDreamCycle(root, today, "dream-hot domain="+domain, preCount)
   }
   ```

   Note on the `ErrRateLimit`/`ErrDailyBudgetExhausted` early-return-nil
   path: this happens **after** `runStats["mode"] = "hot"` was set, so a
   rate-limited hot run still advances `lastHot` in `dreamRunHistory`. This
   is intentional and mirrors the full dream's identical rate-limit
   handling (`dream.go:102-106` — full dream also returns `nil` on rate
   limit without rolling back). Treat "we tried and got rate-limited" the
   same as "we tried and found nothing to change": the churn window resets
   either way, and retrying the exact same window next cycle wouldn't
   change the outcome.

   Imports for `dream_hot.go`: `bufio`, `context`, `encoding/json`,
   `errors`, `fmt`, `os`, `path/filepath`, `sort`, `strings`, `time`.

### 4.5 `cmd/scribe/cron.go`

Insert the `dream-hot` job into `scribeJobs` as specified in §2.7, right
after the existing `"dream"` entry (after line 102).

### 4.6 `cmd/scribe/doctor.go`

Add the `freshnessSpecs` row from §2.8, right after the existing `dream`
row (after line 1027).

### 4.7 `README.md` (documentation — do last, low risk)

- Line 41 (`- Sun 02:00: weekly Dream cycle...`): add a line for the daily
  hot pass, e.g. `- Daily 03:10: hot-domain mini-dream — auto-selects the
  most-touched domain since the last dream, self-gates when there's
  nothing to do.`
- Line 625 area (CLI command list): add
  `scribe dream --hot          # daily mini consolidation of the busiest domain (auto-gates)`
  under the existing `scribe dream` line.
- No change needed to the `llm:` inheritance table (line 370) — `dream
  --hot` inherits the same `dream:` block, already documented as covering
  "`dream`, `assess`, `deep`."

---

## 5. Test plan

All new tests live in `cmd/scribe/dream_hot_test.go` (pure/unit pieces) and
extend `cmd/scribe/dream_orchestrator_test.go` if that file exists yet, else
add domain-scoping cases to a new `TestDreamSamplers_DomainScoping` in
`dream_hot_test.go`. Every test must pass under `make test`
(`go test ./... -tags sqlite_fts5`) with no network access, matching the
repo-wide constraint. No LLM path in this feature needs a *live* model —
the stub harness below covers it exactly the way
`sync_absorb_dense_test.go` and `ollama_followup_test.go` already do for
the other envelope orchestrators.

### 5.1 Pure-function tests (no fixture needed)

- `TestSelectHotDomain_PicksHighestCountAboveThreshold` — `counts :=
  map[string]int{"tools": 5, "personal": 2}`, `minTouches: 3` → `domain ==
  "tools"`, `touches == 5`, `ok == true`.
- `TestSelectHotDomain_BelowThresholdReturnsNotOK` — max count 2, threshold
  3 → `ok == false`.
- `TestSelectHotDomain_TiesBreakAlphabetically` — `{"tools": 3, "personal":
  3}` → `domain == "personal"`.
- `TestHotDomainSince_UsesMostRecentOfFullOrHot` — table over
  (lastFull, lastHot) combinations including both zero (expect
  `now.AddDate(0,0,-cfg.Dream.HotLookbackDays)`), only one set, both set
  with hot newer, both set with full newer.

### 5.2 `dreamRunHistory` — synthetic JSONL fixtures, no git needed

`TestDreamRunHistory_SplitsFullVsHotAndIgnoresSkippedHotRuns`: build a
`t.TempDir()` KB root, write `output/runs/2026-07-01.jsonl` by hand with
four lines (`os.WriteFile`, each line a JSON object matching what
`writeRunRecord` actually emits):

1. `{"command":"dream","status":"ok","timestamp":"2026-07-01T02:00:00Z","args":["dream"]}` (full dream, no mode field — matches legacy monolithic path which never sets `mode`)
2. `{"command":"dream","status":"ok","timestamp":"2026-07-01T03:10:00Z","args":["dream","--hot"]}` (a **self-gated skip** — no `mode` field, since `runHotDream` never reached the stamp line)
3. `{"command":"dream","status":"ok","timestamp":"2026-07-02T03:10:00Z","args":["dream","--hot"],"mode":"hot","hot_domain":"tools"}` (real hot run)
4. `{"command":"dream","status":"error","timestamp":"2026-07-03T03:10:00Z","args":["dream","--hot"],"mode":"hot"}` (errored — must be excluded, status != "ok")

Assert `lastFull` == 2026-07-01T02:00:00Z, `lastHot` == 2026-07-02T03:10:00Z
(NOT 2026-07-01T03:10:00Z from the skip, and NOT 2026-07-03 from the
errored row).

### 5.3 `hotDomainTouchCounts` — real git fixture

Reuse the existing helpers `initTestGitRepo(t, name string) string`
(`cmd/scribe/contributor_test.go:22`), `writeKBFile(t, root, rel, content
string)` (`cmd/scribe/conflicts_test.go:10`), and `gitRun(t, dir string,
args ...string)` (`cmd/scribe/worktree_test.go:13`) — all in package
`main`, already used together exactly this way by
`TestGitDigestActivity` (`cmd/scribe/digest_test.go:33`), the direct
precedent for this test.

`TestHotDomainTouchCounts_DedupesFilesAndBucketsByCurrentDomain`:

```go
root := initTestGitRepo(t, "Alice")
writeKBFile(t, root, "wiki/a.md", "---\ntitle: A\ndomain: tools\n---\n\nbody\n")
writeKBFile(t, root, "wiki/b.md", "---\ntitle: B\ndomain: personal\n---\n\nbody\n")
gitRun(t, root, "add", ".")
gitRun(t, root, "commit", "-q", "-m", "seed")
since := time.Now().Add(-time.Hour)

// a.md touched twice (should count once), b.md touched once.
writeKBFile(t, root, "wiki/a.md", "---\ntitle: A\ndomain: tools\n---\n\nv2\n")
gitRun(t, root, "add", ".")
gitRun(t, root, "commit", "-q", "-m", "edit a")
writeKBFile(t, root, "wiki/a.md", "---\ntitle: A\ndomain: tools\n---\n\nv3\n")
gitRun(t, root, "add", ".")
gitRun(t, root, "commit", "-q", "-m", "edit a again")
writeKBFile(t, root, "wiki/b.md", "---\ntitle: B\ndomain: personal\n---\n\nv2\n")
gitRun(t, root, "add", ".")
gitRun(t, root, "commit", "-q", "-m", "edit b")

cfg := &ScribeConfig{Domains: []string{"tools"}} // personal comes from universalDomains
counts := hotDomainTouchCounts(root, cfg, since)
// want: {"tools": 1, "personal": 1} — a.md counted once despite 2 edits
```

Also add a case where a touched file's domain is NOT in `cfg.AllDomains()`
(e.g. a typo'd `domain: toolz`) and assert it's excluded from `counts`
entirely (not bucketed under `""`).

### 5.4 `runHotDream` integration tests — `stubHarnessKB` + `installStubLLM`

Follow the exact pattern in `cmd/scribe/llm_stub_test.go` (already used by
`sync_absorb_dense_test.go`): `stubHarnessKB(t, scribeYAML)` scaffolds a
temp KB with env redirects; `installStubLLM(t, stub)` swaps the
`newLLMProvider` seam; a `stubJSONLLM{}` with `Rules` matched by `MatchOp`
(`"dream-hot"` — the label `runHotDream` tags via `withOpLabel(ctx,
"dream-hot")`) supplies the scripted envelope reply via the existing
`envelopeJSON(t, 1, entity, path, body)` helper.

`stubHarnessKB` only creates `raw/articles, wiki, scripts, output` dirs and
is not itself a git repo — extend the fixture in each hot-dream test with
`initTestGitRepo`-style `git init`/`config` calls plus committed article
files (needed both for `hotDomainTouchCounts`'s git log and for
`gitAddWiki`/`gitCommit` inside `commitDreamCycle` to succeed). Write a
small local helper in `dream_hot_test.go`,
`gitInitHotDreamFixture(t *testing.T, root string)`, that runs `git init
-q`, `git config user.name/user.email`, and an initial commit of whatever
files the test already wrote via `writeKBFile` — do not try to reuse
`initTestGitRepo` directly since it creates its own tempdir rather than
git-initializing an existing `stubHarnessKB` root.

Required cases:

- `TestRunHotDream_SkipsWhenFullDreamRanRecently` — seed
  `output/runs/<today>.jsonl` with a full-dream `ok` row timestamped 1 hour
  ago; call `runHotDream`; assert it returns `nil`, the stub LLM recorded
  **zero** calls (`len(stub.Calls()) == 0`), and no new git commit was
  created (`git log --oneline` unchanged).
- `TestRunHotDream_SkipsWhenChurnBelowThreshold` — no prior run records
  (first-ever run), one committed article touched once in a domain, config
  `dream.hot_min_touches: 3`; assert zero LLM calls.
- `TestRunHotDream_AppliesEnvelopeScopedToSelectedDomain` — commit 3+
  articles in `tools` domain within the lookback window (so it wins
  selection) plus 1 in `personal`; script `stubJSONLLM` to require
  `MatchOp: "dream-hot"` and reply with `envelopeJSON(...)` creating a stub
  article; assert the created file exists, its frontmatter has `domain:
  tools`, the prompt sent to the stub (`stub.CallsWithOp("dream-hot")[0].Prompt`)
  contains the tools articles but NOT the personal one, and the resulting
  git commit message matches `^dream-hot domain=tools: `.
- `TestRunHotDream_ExplicitDomainOverrideBypassesChurnGate` — zero churn at
  all (empty git log window), call with `domainOverride: "personal"`;
  assert the stub LLM *was* called (override bypasses gate 2) with
  `{{DOMAIN}}` substituted as `personal` in the prompt.
- `TestRunHotDream_RateLimitReturnsNilAndStampsMode` — stub rule returns
  `Err: ErrRateLimit` for `MatchOp: "dream-hot"`; assert `runHotDream`
  returns `nil` (not an error) — mirrors the full dream's existing
  rate-limit contract.

### 5.5 Domain-scoping regression on the shared samplers

`TestDreamSamplers_DomainScoping`: build a small KB (via `writeKBFile`, no
git needed — these are pure frontmatter walkers) with 2 articles in
`tools` and 1 in `personal`; assert
`dreamSampleInventory(root, "tools", 10)` contains only the 2 tools titles,
`dreamSampleInventory(root, "", 10)` contains all 3, and the same shape for
`dreamStaleCandidates` and `dreamContradictionCandidates`. This is the
regression guard that the domain-scoping refactor (§4.3) didn't change
full-dream (`domain=""`) behavior.

### 5.6 `commitDreamCycle` runStats regression

`TestCommitDreamCycle_PreservesRunStatsSetBeforeCall`: in a `stubHarnessKB`
+ git-initialized fixture with no pending changes, manually set
`runStats = map[string]any{"mode": "orchestrator", "envelope_actions_applied": 4}`
before calling `commitDreamCycle(root, "2026-07-02", "dream", 10)`; assert
after the call `runStats["mode"] == "orchestrator"` still (proving the
additive-assignment fix from §2.5 didn't get lost) AND
`runStats["articles_before"] == 10` was added. Reset the package-level
`runStats = nil` in `t.Cleanup` since it's shared global test state (check
whether existing dream/absorb tests already do this reset pattern — grep
`runStats = nil` or `runStats = map` in existing `_test.go` files before
writing this, and follow whatever convention is already established for
resetting package globals between tests).

### 5.7 Config defaults

`TestApplyDreamDefaults_HotFieldsDefaultAndAreOverridable` — mirrors
existing tests in `ollama_followup_test.go` for `applyDreamDefaults`
(e.g. `TestApplyMetaDefaultsFallsBackToHistoricalPair` for the pattern):
zero-valued `DreamConfig{}` → `HotMinTouches == 3`, `HotLookbackDays ==
14`; a pre-set `DreamConfig{HotMinTouches: 10}` is left untouched.

---

## 6. Risks & edge cases

- **Budget ceiling is free (already covered).** `checkBudget` runs inside
  `anthropicProvider.Generate` / the hosted-provider path automatically for
  every `dream-hot` call through `generateMaybeJSON` — no separate check
  needed in `runHotDream` (verified: `cmd/scribe/llm.go:108-114`,
  `cmd/scribe/llm_openai_compat.go:230-234`). A machine running many
  registered KBs (issue #26) with the hot pass added to all of them
  multiplies daily LLM calls by up to `#KBs`, but the machine-wide ceiling
  (`machineOutputTokenCeiling`, `budget.go:123-127`) already caps total
  spend across all registered KBs regardless of which command trips it.
- **Must not run when the full weekly dream just covered it (explicit
  requirement) — covered by gate 1, §2.3.** Verified the 24h threshold
  against the actual 03:10 daily / Sunday 02:00 weekly schedule so Monday's
  run isn't accidentally skipped too (25h20m gap clears the 24h bar).
- **Team KBs: two machines could pick the same hot domain concurrently.**
  Mitigated by reusing the exact same `dreamLease` (§2.4) — only one
  machine's dream activity (full or hot) proceeds system-wide at a time.
  Worst case if the lease write itself races (rare — `acquireDreamLease`
  already retries once on a lost push), two machines both consolidate the
  same domain on the same day; the second machine's `git push` in
  `commitDreamCycle` fails and is logged as `"push failed (offline?)"`
  (existing behavior, not new) — no data loss, just a missed push that the
  next commit cycle retries. Not a new risk class introduced by this
  feature.
- **`hot_min_touches` too low on a very small/quiet KB** → the hot pass
  fires daily on trivial churn, spending tokens for little value. This is
  a config knob (`dream.hot_min_touches`) precisely so a user can raise it;
  default 3 was chosen as "more than one incidental edit" without being so
  high that a genuinely busy domain waits a week to get picked up. Not
  auto-tuned — out of scope for this issue.
- **First-ever run on an existing KB with years of git history.** Capped by
  `HotLookbackDays` (default 14) exactly to prevent an unbounded `git log`
  scan — confirmed by design in §2.2/§2.6, not left to chance.
- **A domain's articles all live in one heavily-edited file vs. spread
  across many files.** The dedupe-to-distinct-files counting (§2.2) means
  one file edited 50 times counts the same as one file edited once —
  intentional (matches "how much of the KB territory changed," not "how
  much total edit volume"), but worth stating so a reviewer doesn't mistake
  it for a bug.
- **Pre-existing `runStats` overwrite bug fix (§2.5) is in scope, not
  scope-creep.** Flagged explicitly because the new hot-mode code *depends*
  on the fix (its `mode: "hot"` stamp must survive to the JSONL row for §3
  to work) — this is not an unrelated cleanup riding along.
- **Interaction with issue #26 (KB registry / KB-agnostic cron, already
  merged as of this writing — `cmd/scribe/each.go` exists with `EachConfig`
  and per-job `Cadence`).** The new cron entry follows the identical
  `each("<job>")` wrapping every other `scribeJobs` entry already uses
  (§2.7) — no special-casing. A KB operator who wants a slower-than-daily
  hot cadence can already set `each.cadence["dream --hot"]: "3d"` in their
  `scribe.yaml` with zero code changes, because `cadenceSkipReason`
  (`each.go:109-128`) already generalizes over any command+flag pair.
- **Interaction with issue #19 (heartbeat)** — not investigated in detail
  here (out of this plan's scope); if #19 adds a liveness signal keyed off
  `output/runs/*.jsonl`, the new `dream --hot` rows are just more rows in
  the same schema (`command`, `status`, `timestamp`, `args`, plus
  `runStats`-merged fields) — no format divergence to reconcile.

---

## 7. Size estimate

**M** (medium). Rough LOC:

- `cmd/scribe/dream_hot.go` (new): ~180-220 LOC including comments.
- `cmd/scribe/dream.go`: ~10 lines added (Hot/Domain fields + branch), ~70
  lines moved (not net-new) into the extracted `commitDreamCycle`.
- `cmd/scribe/dream_orchestrator.go`: ~6 lines changed (3 signatures + 3
  filter lines) + 3 call-site edits.
- `cmd/scribe/config_llm.go`: ~15 lines (2 fields + 2 default blocks).
- `cmd/scribe/cron.go`: ~8 lines (1 job entry).
- `cmd/scribe/doctor.go`: 1 line.
- `cmd/scribe/prompts/dream-hot-anthropic.md`, `dream-hot-ollama.md` (new):
  ~75 + ~55 lines (prose, not code).
- `README.md`: ~2 lines.
- Tests (`cmd/scribe/dream_hot_test.go`, new, plus small additions
  elsewhere): ~350-450 LOC — this is the bulk of the effort given the
  number of gating/coordination paths that need independent coverage (§5
  lists 15+ distinct test cases).

Total: roughly 550-700 LOC across 8 source files + 2 new prompt files,
no new dependencies, one package (`cmd/scribe`), consistent with every
other Phase-4-era envelope orchestrator port in this codebase.
