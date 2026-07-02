# Issue #42 — Extraction prompts: capture failure traces + failure conditions, not just the fix

Implementation plan. Self-contained — the implementer does not need to read
the GitHub issue.

## 1. Problem & context

scribe's embedded LLM prompts under `cmd/scribe/prompts/` (loaded via
`//go:embed prompts/*.md` in `cmd/scribe/claude.go:19` and read by
`loadPrompt()` at `cmd/scribe/claude.go:257`) drive every extraction path:
project-file extraction, Claude Code / Codex session mining, directory
"deep extract", and raw-article absorb. Across all of them, the instruction
for *what to write into a wiki article* asks for the resolved fact only —
decisions, patterns, tool verdicts, research findings — never for the
dead ends that were tried and rejected along the way, or the conditions
under which the recorded solution stops working.

Example — `cmd/scribe/prompts/extract.md:7` (byte-identical to
`extract-anthropic.md:7`, confirmed via `diff`):

> "Step 4: For each piece of extractable knowledge (decisions, patterns,
> tools, research findings, evaluations, analyses), write or update wiki
> articles... Score the `confidence:` field... Do not inflate..."

Nothing here asks the model to record *what didn't work*. Same pattern in
`session-mine-anthropic.md:20-24`'s "## What to extract" bullet list, and in
every other family.

This under-captures exactly the content Stack Overflow for Agents identified
as most reusable for a future agent: TIL posts are "the full reasoning
trace: what was broken, what was tried, what worked"; Blueprint posts record
"tradeoffs and failure conditions." A KB that only stores the resolved fact
forces a future reader (human or agent) to rediscover a known dead end from
scratch — often the more expensive half of the original debugging session.

**Scope**: add a *guideline* (not a required section, not a lint-enforced
field) to the prompt templates that already cover solution/pattern/decision
extraction, asking them to capture (a) approaches tried that didn't work and
why, and (b) the conditions under which the chosen approach breaks — when
the source actually contains that information. Do not add a rigid template,
a new frontmatter field, or a lint check. Out of scope: a first-class
Question/unsolved-problem artifact type (separate future issue if wanted).

## 2. Design decisions

All open questions below are settled. No "option A or B" is left for the
implementer.

### 2.1 Which prompt families are in scope

**Decision**: touch exactly these 15 files (all families that read
project/session/article content and decide what becomes a wiki article):

- `extract.md`, `extract-anthropic.md`, `extract-ollama.md`
- `session-extract.md`, `session-extract-anthropic.md`,
  `session-extract-ollama.md`, `session-extract-large.md`
- `session-mine-anthropic.md`, `session-mine-ollama.md`
- `deep-extract.md`, `deep-extract-anthropic.md`, `deep-extract-ollama.md`
- `absorb.md`, `absorb-anthropic.md`, `absorb-ollama.md`

**Why**: this is the exact file set touched by the closest precedent commit,
`ce20ee5` ("research-before-create dedup protocol in all extraction
prompts"), which added a different cross-cutting guideline to the same 14
files (see `git show --stat ce20ee5`) — confirming this is scribe's
established "all extraction prompts" set. `session-extract-large.md` is
added as a 15th file even though `ce20ee5` skipped it: it is a live,
actively-used prompt (see 2.3 for its call site) and the issue explicitly
scopes to `session-extract*.md`.

Confirmed live call sites for every file (so none of these are dead code):
- `extract.md` — `cmd/scribe/sync_extract.go:335` (legacy tools-mode,
  loaded directly, no provider branch)
- `extract.md`/`extract-anthropic.md`/`extract-ollama.md` — also reachable
  via `promptForProvider("extract", ...)` at
  `cmd/scribe/extract_envelope.go:34` (envelope mode)
- `session-extract.md` — `cmd/scribe/sync_sessions.go:679` (legacy
  tools-mode, normal-size sessions)
- `session-extract-large.md` — `cmd/scribe/sync_sessions.go:700` (legacy
  tools-mode, >300-message sessions)
- `session-extract-anthropic.md`/`session-extract-ollama.md` —
  `promptForProvider("session-extract", ...)` inside
  `runSessionEnvelopeOnce` (`cmd/scribe/session_mine_envelope.go:63`),
  reached from Codex mining (`cmd/scribe/codex_mine.go:96,107`)
- `session-mine-anthropic.md`/`session-mine-ollama.md` —
  `promptForProvider("session-mine", ...)` via the same
  `runSessionEnvelopeOnce`, reached from
  `mineSessionBatchesEnvelope` (`cmd/scribe/session_mine_envelope.go:280`)
  for **both** normal and large Claude Code sessions in envelope mode
  (`promptBaseForSessionLabel` at `session_mine_envelope.go:197-206`
  defaults any label that isn't `session-extract*` to `"session-mine"`)
- `deep-extract.md` — `cmd/scribe/deep.go:191` (legacy tools-mode
  `scribe deep`)
- `deep-extract-anthropic.md`/`deep-extract-ollama.md` — via
  `promptForProvider("deep-extract", ...)` at
  `cmd/scribe/deep_orchestrator.go:23` (envelope mode)
- `absorb.md`/`absorb-anthropic.md`/`absorb-ollama.md` — via
  `promptForProvider("absorb", ...)` at `cmd/scribe/sync_absorb.go:232`
  (single/multi-page absorb for brief/standard articles)

**Rejected alternative**: also touch the dense-article two-pass pipeline
(`absorb-pass1.md`, `absorb-pass1-anthropic.md`, `absorb-pass1-ollama.md`,
`absorb-pass1-chapter.md`, `absorb-pass1-chapter-anthropic.md`,
`absorb-pass1-chapter-ollama.md`, `absorb-pass2.md`, `absorb-pass2-json.md`,
`absorb-facts.md`) — 9 more files matched by a literal `absorb*.md` glob.
Rejected because:
1. That pipeline is architecturally a plan-then-write split around a fixed
   atomic-fact taxonomy (`definition | claim | numeric | decision |
   citation` — see `absorb-facts.md:29-34`). Adding "failure trace" as a
   fact type would be a **schema change** (new fact `type`, new JSON field
   consumed by `absorb_facts.go`'s `chapterFacts` struct and
   `absorb_chapter.go`'s `chapterPlan` struct), not a guideline-text change
   — out of scope for what the issue calls an "isolated prompt change."
2. Its sources are external papers/long-form articles absorbed via
   `scribe capture`/inbox, where "what was tried and failed" is rarely
   present in the source prose to begin with. The highest-value targets
   (git-repo decisions/research docs, Claude Code sessions full of retried
   approaches) are exactly the 15 files already in scope.

If dense-article absorb turns out to need this too, it's a follow-up issue,
not part of this change.

### 2.2 Wording — three registers, one canonical guideline

**Decision**: the guideline is expressed in three registers matched to each
file's existing style, all paraphrasing the same idea so behavior stays
consistent across providers (per the instruction to "keep variants
consistent"). Ollama variants get the shortest form — this mirrors the
existing asymmetry in every one of these files today (e.g. compare
`session-mine-anthropic.md`'s 5-bullet "What to extract" vs
`session-mine-ollama.md`'s one-line "KEEP:/SKIP:").

**Register A — narrative/Step-based files** (agent has filesystem/MCP
tools, long-form paragraph style): `extract.md`, `extract-anthropic.md`,
`deep-extract.md`, `session-extract.md`, `session-extract-large.md`,
`absorb.md`. Insert this full sentence, appended to the existing
extraction-scope sentence in each file (exact insertion points in §3):

```
When the source documents a debugging or decision process, also capture what was tried and rejected (and why) and the conditions under which the chosen approach breaks — not just the fix or verdict; a reader avoiding a known dead end is often more valuable than the fix itself. This is a guideline, not a required section — skip it when the source has no failed attempts or known failure conditions to report.
```

**Register B — Anthropic JSON-envelope files** (no filesystem tools,
"## What to extract" bullet lists, more prose than the Ollama sibling):
`session-extract-anthropic.md`, `session-mine-anthropic.md`,
`deep-extract-anthropic.md`, `absorb-anthropic.md`. Insert this bullet:

```
- Failure traces (when present): what was tried and didn't work, and why; the conditions under which the chosen approach breaks. Not just the final fix — include this alongside it, not instead of it. Skip if the source has nothing of the kind.
```

**Register C — Ollama terse JSON-envelope files** (already minimal, avoid
padding — local models get worse with longer prompts per this repo's
established convention): `extract-ollama.md`, `session-extract-ollama.md`,
`session-mine-ollama.md`, `deep-extract-ollama.md`, `absorb-ollama.md`.
Insert this bullet:

```
- Note failed approaches + why, and known failure conditions, when the source states them — not just the fix.
```

**Rejected alternative**: a single verbatim string copy-pasted into all 15
files. Rejected because every file in this repo's prompt set already varies
register between tools-mode/envelope-mode/ollama (see any existing
guideline, e.g. the dedup protocol from `ce20ee5`, or the "Avoid
duplicates" sections already present in `session-extract-anthropic.md:55-59`
and siblings) — matching that established pattern keeps this change
consistent with the codebase's existing convention rather than introducing
a new one.

**Rejected alternative**: a mandatory `## Failure Conditions` or `##
Dead Ends` heading in the article body, or a required frontmatter field
(e.g. `known_failure_modes:`). Rejected per the issue's explicit
instruction — "Keep it a guideline in the prompt, not a rigid required
section" — and because `lint.go`'s `lintFrontmatter`
(`cmd/scribe/lint.go:137`) only validates frontmatter keys, never body
sections; adding an enforced body section would need new lint logic scribe
doesn't have today and the issue explicitly doesn't want.

### 2.3 KB-wide convention text

**Decision**: also add one new Core Rule to
`cmd/scribe/templates/kb-CLAUDE.md` (the embedded template that becomes
every KB's own `CLAUDE.md`), as rule 13, after the existing 12:

```
13. **Capture failure traces** — when a source shows what was tried and rejected (and why), or the conditions under which a solution breaks, record that alongside the fix. A reader avoiding a known dead end is often more valuable than the fix itself. This is a guideline: don't pad an article with a failure-trace section it doesn't need.
```

**Why**: `kb-CLAUDE.md` is read by every prompt via "Step 1: Read
{{KB_DIR}}/CLAUDE.md for schema and frontmatter conventions" (e.g.
`extract.md:3`), and by humans/agents writing to the KB outside the
extraction pipeline (manual edits, `scribe dream`, future prompts). Stating
it once as a standing KB convention means future prompt additions inherit
the expectation without re-deriving it, which directly answers the "should
any lint/template/conventions text also mention failure traces" prompt from
the task brief. It stays a guideline (rule 7 "Anti-thinning" and rule 6
"Anti-cramming" are already phrased the same soft way — this rule matches
that register, not a MUST/REQUIRED style).

**Rejected alternative**: skip `kb-CLAUDE.md` and rely on the per-prompt
guidelines alone. Rejected because the "Frontmatter Conventions" and "Type
-specific fields" sections of `kb-CLAUDE.md` already document body-writing
expectations that apply across all extraction paths (the reconstruction
test used by `absorb.md`'s verbatim-preservation rule is a **prompt**-level
convention with no KB-level anchor either, for comparison — but this rule
is cheap, one line, and gives dream/human writers the same nudge extraction
prompts get) — worth the ~6 lines given how directly it answers the task
brief's explicit question.

### 2.4 No lint, no test-assertion of prompt prose, no schema change

**Decision**: `cmd/scribe/lint.go` is not touched. `cmd/scribe/wiki_actions.go`
(envelope executor / frontmatter clamp) is not touched. No new Go struct
fields. This is a pure `.md` prompt-text change plus one `.md` template
change.

**Why**: enforcing failure-trace capture mechanically would turn a
guideline into a requirement, which the issue explicitly rejects. The
existing `TestLoadPrompt_StripsUnsubstitutedPlaceholders` test
(`cmd/scribe/wiki_actions_test.go:1040`) calls
`loadPrompt("session-extract-ollama.md", map[string]string{})` and asserts
no `{{...}}` token survives — since this change only adds plain prose (no
`{{PLACEHOLDER}}` syntax), that test is unaffected and must still pass
unmodified.

## 3. Implementation steps — file by file

For every insertion below, match the file's existing markdown formatting
exactly (no new headings introduced except where noted). Do not reformat
surrounding text.

### 3.1 `cmd/scribe/prompts/extract.md` (Register A)

Line 7 currently ends:
`...Do not inflate — dream uses \`confidence:\` as the arbiter when contradictions surface across domains, so a falsely \`high\` article corrupts later resolution.`

Append the Register A sentence to the end of that same line (Step 4), as a
new trailing sentence in the same paragraph:

```
...so a falsely `high` article corrupts later resolution. When the source documents a debugging or decision process, also capture what was tried and rejected (and why) and the conditions under which the chosen approach breaks — not just the fix or verdict; a reader avoiding a known dead end is often more valuable than the fix itself. This is a guideline, not a required section — skip it when the source has no failed attempts or known failure conditions to report.
```

### 3.2 `cmd/scribe/prompts/extract-anthropic.md` (Register A)

Byte-identical to `extract.md` today (verified via `diff`). Apply the
**exact same edit** as §3.1 to line 7, so the two files stay byte-identical
after this change (matches how `ce20ee5` kept them in lockstep — both got
the identical one-line change).

### 3.3 `cmd/scribe/prompts/extract-ollama.md` (Register C)

Line 48 currently reads:
`- Do NOT extract file summaries, code listings, or test scaffolding — extract the non-obvious *reusable* knowledge only.`

Insert a new bullet immediately after it (before the blank line / "One
topic = one article" bullet that follows):

```
- Note failed approaches + why, and known failure conditions, when the source states them — not just the fix.
```

### 3.4 `cmd/scribe/prompts/session-extract.md` (Register A)

Line 12 currently reads:
`2. Identify extractable knowledge: decisions with reasoning, architecture patterns, research findings, tool evaluations, lessons learned`

Append to the end of that line:

```
2. Identify extractable knowledge: decisions with reasoning, architecture patterns, research findings, tool evaluations, lessons learned. When the source documents a debugging or decision process, also capture what was tried and rejected (and why) and the conditions under which the chosen approach breaks — not just the fix or verdict; a reader avoiding a known dead end is often more valuable than the fix itself. This is a guideline, not a required section — skip it when the source has no failed attempts or known failure conditions to report.
```

### 3.5 `cmd/scribe/prompts/session-extract-anthropic.md` (Register B)

Lines 18-24 currently:
```
## What to extract

- Project-specific decisions, patterns, learnings discovered in this session.
- Architecture or tool choices unique to this project.
- Bugs found + their root causes.

**Skip** conversation summaries and routine code changes.
```

Insert a new bullet after `- Bugs found + their root causes.` and before
the blank line / `**Skip**` line:

```
- Failure traces (when present): what was tried and didn't work, and why; the conditions under which the chosen approach breaks. Not just the final fix — include this alongside it, not instead of it. Skip if the source has nothing of the kind.
```

### 3.6 `cmd/scribe/prompts/session-extract-ollama.md` (Register C)

This file has no "## What to extract" section (confirmed by reading the
full file) — go straight from Session/Related/Transcript to Output schema.
The `## Rules` section (lines 39-51) is the insertion point. Line 41
currently reads:
`- Emit one sessions_log_append per session id.`

Insert the new bullet immediately after it:

```
- Note failed approaches + why, and known failure conditions, when the source states them — not just the fix.
```

### 3.7 `cmd/scribe/prompts/session-extract-large.md` (Register A)

Line 15 currently reads:
`5. Focus on extracting: decisions with reasoning, architecture patterns, research findings with data, tool evaluations with verdicts, lessons learned from mistakes`

Append to the end of that line:

```
5. Focus on extracting: decisions with reasoning, architecture patterns, research findings with data, tool evaluations with verdicts, lessons learned from mistakes. When the source documents a debugging or decision process, also capture what was tried and rejected (and why) and the conditions under which the chosen approach breaks — not just the fix or verdict; a reader avoiding a known dead end is often more valuable than the fix itself. This is a guideline, not a required section — skip it when the source has no failed attempts or known failure conditions to report.
```

### 3.8 `cmd/scribe/prompts/session-mine-anthropic.md` (Register B)

Lines 18-27 currently:
```
## What to extract

Pull only the **non-obvious, reusable** knowledge:
- decisions with reasoning (named tradeoffs, alternatives considered)
- architecture patterns that apply across projects
- research findings with data (numerics, benchmarks, measured outcomes)
- tool evaluations (which one, why, what alternatives)
- lessons learned (debug discoveries, surprising behaviors)

**Skip:** conversation summaries, routine code changes, transient debug noise, tool-call mechanics.
```

Insert a new bullet after `- lessons learned (debug discoveries, surprising
behaviors)` and before the blank line / `**Skip:**` line:

```
- failure traces (when present): what was tried and didn't work, and why; conditions under which the chosen approach breaks — not just the final fix, include it alongside
```

(Lowercase leading word to match the existing bullet style in this list —
all five existing bullets start lowercase.)

### 3.9 `cmd/scribe/prompts/session-mine-ollama.md` (Register C)

Lines 20-23 currently:
```
## What to extract (be ruthless — skip noise)

KEEP: decisions with reasoning, architecture patterns, research findings with numerics, tool evaluations, debug lessons.
SKIP: conversation summaries, routine code changes, transient debug noise.
```

Modify the `KEEP:` line in place, appending a clause (do not add a new
bullet — this file's convention is one KEEP line, one SKIP line):

```
KEEP: decisions with reasoning, architecture patterns, research findings with numerics, tool evaluations, debug lessons, failed approaches + why (when stated).
```

### 3.10 `cmd/scribe/prompts/deep-extract.md` (Register A)

Line 11 currently reads:
`Extract the non-obvious knowledge — research findings, design decisions with reasoning, evaluation results, quantitative data, patterns. Not file summaries.`

Append to the end of that line:

```
Extract the non-obvious knowledge — research findings, design decisions with reasoning, evaluation results, quantitative data, patterns. Not file summaries. When the source documents a debugging or decision process, also capture what was tried and rejected (and why) and the conditions under which the chosen approach breaks — not just the fix or verdict; a reader avoiding a known dead end is often more valuable than the fix itself. This is a guideline, not a required section — skip it when the source has no failed attempts or known failure conditions to report.
```

### 3.11 `cmd/scribe/prompts/deep-extract-anthropic.md` (Register B)

Lines 17-19 currently:
```
## What to extract

For this directory: decisions, architecture patterns, learnings, and tool evaluations that would warrant their own wiki article. Skip routine code, build artifacts, and conversational summaries.
```

Append a new sentence to the end of that paragraph (line 19):

```
For this directory: decisions, architecture patterns, learnings, and tool evaluations that would warrant their own wiki article. Skip routine code, build artifacts, and conversational summaries. Failure traces (when present): what was tried and didn't work, and why; the conditions under which the chosen approach breaks. Not just the final fix — include this alongside it, not instead of it. Skip if the source has nothing of the kind.
```

### 3.12 `cmd/scribe/prompts/deep-extract-ollama.md` (Register C)

This file has no "## What to extract" section — go straight from Files to
Output schema to Rules. The `## Rules` section (lines 34-44) is the
insertion point. Line 36 currently reads:
`- Empty actions legal: \`"actions": []\`.`

Insert the new bullet immediately after it:

```
- Note failed approaches + why, and known failure conditions, when the source states them — not just the fix.
```

### 3.13 `cmd/scribe/prompts/absorb.md` (Register A, adapted)

This file's structure is a numbered procedure (density classification →
type classification → plan output → verbatim preservation → write →
confidence → size check → indexes). The "candidates for verbatim
preservation" list at step 4 (lines 25-33) is the natural home — it already
enumerates the categories worth preserving verbatim (numeric evidence,
named decisions, trade-offs, quoted principles, exact commands). Lines
25-33 currently:

```
4. **Verbatim preservation for load-bearing claims.** For any claim in the raw article, apply the reconstruction test:
   > *"If a future query reads only this wiki page, could it reconstruct this specific claim accurately from the summary alone?"*
   If the answer is **no**, preserve the claim verbatim in the wiki page using a markdown blockquote with a source reference. Typical candidates:
   - numeric evidence ("49% reduction in failed retrievals")
   - named decisions ("we chose Postgres over SQLite because …")
   - non-obvious trade-offs or constraints
   - principles quoted verbatim from the author
   - specific commands, config values, API shapes
   Do NOT quote background context that embeddings can trivially surface from raw. Quote only what would be hard to re-derive from a summary.
```

Insert a new bullet into that "Typical candidates" list, after "non-obvious
trade-offs or constraints" and before "principles quoted verbatim from the
author":

```
   - approaches the source says were tried and abandoned, and why, or the conditions under which the recommended approach breaks
```

### 3.14 `cmd/scribe/prompts/absorb-anthropic.md` (Register B, adapted)

Same structure as `absorb.md`, JSON-envelope framed. The equivalent
"Verbatim preservation" step is item 4 (lines 58-63):

```
4. **Verbatim preservation.** For load-bearing claims (numerics, named decisions, non-obvious trade-offs, exact configs):
   ```markdown
   > "<exact quote>"
   > — Source: {{RAW_FILE}}
   ```
   Apply the reconstruction test: if a future query reading only the wiki page couldn't reconstruct the claim, quote it verbatim.
```

Edit the parenthetical list on the first line of item 4, inserting a new
category before the closing parenthesis:

```
4. **Verbatim preservation.** For load-bearing claims (numerics, named decisions, non-obvious trade-offs, exact configs, approaches tried and rejected and why, failure conditions for the recommended approach):
```

### 3.15 `cmd/scribe/prompts/absorb-ollama.md` (Register C)

Line 46 currently reads:
`- Body: lead with one-line definition, then sections. Quote load-bearing claims verbatim using markdown blockquotes with \`— Source: {{RAW_FILE}}\`.`

Insert the new bullet immediately after it:

```
- Note failed approaches + why, and known failure conditions, when the source states them — not just the fix.
```

### 3.16 `cmd/scribe/templates/kb-CLAUDE.md`

Lines 114-127, the "## Core Rules" numbered list, currently ends at item
12:

```
12. **No knowledge deletion** — the KB is append-only; mark superseded, never delete.
```

Append a new item 13 immediately after it:

```
13. **Capture failure traces** — when a source shows what was tried and rejected (and why), or the conditions under which a solution breaks, record that alongside the fix. A reader avoiding a known dead end is often more valuable than the fix itself. This is a guideline: don't pad an article with a failure-trace section it doesn't need.
```

## 4. Test plan

This is a `.md`-only content change (14 prompt files + 1 template file).
No new Go code, no new Go tests required for the change itself. Verify the
existing suite still passes and the embed still loads correctly.

| # | Check | Command | Expect |
|---|-------|---------|--------|
| 1 | Full offline suite still green | `make test` (i.e. `go test ./... -tags sqlite_fts5`) | PASS, no network |
| 2 | `go vet` clean | `make check` | PASS |
| 3 | Embed still loads every touched file, no `{{...}}` leaks in the one file already under direct test | `go test ./cmd/scribe/ -tags sqlite_fts5 -run TestLoadPrompt_StripsUnsubstitutedPlaceholders -v` | PASS unchanged |
| 4 | `promptForProvider` mapping unaffected (content edits don't touch filenames) | `go test ./cmd/scribe/ -tags sqlite_fts5 -run 'TestPromptForProviderPicksOllamaVariant|TestPromptForProviderFallsBackToLegacy' -v` (exact names, `ollama_followup_test.go:111,121`) | PASS unchanged |
| 5 | `deep-extract` envelope stub-driven tests unaffected (they match on `Op`/prompt substring "alphadocs"/"betadocs", not on the guideline text) | `go test ./cmd/scribe/ -tags sqlite_fts5 -run TestDeepRun_ -v` (matches all 7 `TestDeepRun_*` funcs in `deep_run_test.go`) | PASS unchanged |
| 6 | Every touched `.md` file is still valid embeddable text (no accidental non-UTF8, no unbalanced code fences introduced) | `gofmt -l cmd/scribe/*.go` (confirms nothing else was touched) + manual `git diff --stat cmd/scribe/prompts/ cmd/scribe/templates/` shows only the 15 expected files | Only the 15 files listed in §2.1 + kb-CLAUDE.md appear in the diff; no `.go` file changes |
| 7 | New prose contains no `{{` / `}}` (would trip the placeholder-strip guard silently) | `grep -n '{{' cmd/scribe/prompts/extract.md cmd/scribe/prompts/extract-anthropic.md cmd/scribe/prompts/extract-ollama.md cmd/scribe/prompts/session-extract.md cmd/scribe/prompts/session-extract-anthropic.md cmd/scribe/prompts/session-extract-ollama.md cmd/scribe/prompts/session-extract-large.md cmd/scribe/prompts/session-mine-anthropic.md cmd/scribe/prompts/session-mine-ollama.md cmd/scribe/prompts/deep-extract.md cmd/scribe/prompts/deep-extract-anthropic.md cmd/scribe/prompts/deep-extract-ollama.md cmd/scribe/prompts/absorb.md cmd/scribe/prompts/absorb-anthropic.md cmd/scribe/prompts/absorb-ollama.md \| grep -v '{{[A-Z0-9_]*}}'` | No output (every `{{` on a line is part of an existing, already-substituted-elsewhere placeholder token, not new stray braces from the inserted prose) |
| 8 | `extract.md` / `extract-anthropic.md` stay byte-identical (established invariant, not enforced by a test, but preserved by convention — see `ce20ee5`) | `diff cmd/scribe/prompts/extract.md cmd/scribe/prompts/extract-anthropic.md` | No diff output |

No golden-file / snapshot test is being added for prompt prose — this repo
doesn't have one for existing guideline text (e.g. the `ce20ee5` dedup
protocol has no dedicated test asserting its wording), consistent with the
"guideline, not enforced" nature of the change.

### Manual verification (recommended, not gated on CI)

Per the issue's own ask ("verify extraction quality on a few real sessions
before/after"), after landing this change run one real extraction against a
KB with a session that had a documented dead end (e.g. a bug-fix session
that tried approach A, reverted, then landed approach B) and confirm the
resulting wiki article gained a sentence about the rejected approach without
growing an explicit new heading. This is exploratory, not part of `make
test`.

## 5. Risks & edge cases

- **Local-model prompt bloat.** Ollama variants are already tuned terse
  because local models degrade with longer prompts (this repo's own stated
  convention — see the `-ollama.md` files' "OUTPUT ONLY ONE JSON OBJECT"
  headers and consistently shorter rule lists). Register C is kept to one
  line for exactly this reason; do not expand it even if Register B's
  wording looks more complete.
- **Over-triggering on ordinary sessions.** Most sessions/articles have no
  failed attempt to report. Every register explicitly says "when present" /
  "skip it when the source has no failed attempts" — the risk is a model
  fabricating a dead end to satisfy the guideline. Mitigate by the explicit
  skip clause already built into the wording (§2.2); do not strengthen the
  language to sound mandatory, which would increase fabrication risk, not
  decrease it.
- **`extract.md`/`extract-anthropic.md` drift.** These two files are
  maintained as duplicates by convention, not by tooling (no symlink, no
  test enforcing equality — confirmed via `ls -la` inode check and grep for
  a parity test, neither found). The implementer must apply the identical
  edit to both or they silently diverge. Test-plan item 8 catches this
  before commit.
- **`absorb-anthropic.md`/`absorb.md` edits touch a *list of examples*
  inside a longer instruction**, not a standalone bullet — a careless edit
  could break the parenthetical's grammar. §3.13/§3.14 give the exact
  before/after text to avoid this.
- **150-line article cap.** Every prompt family already enforces "≤150
  lines per article" (e.g. `extract-ollama.md:24`, `kb-CLAUDE.md` rule 9).
  A failure-trace sentence adds a few words per article at most, not a new
  section — no interaction with the size cap expected, but if a model
  starts writing a dedicated "## Known Failure Modes" subsection that pushes
  an article over 150 lines, the existing size-check step (already present
  in every prompt) already tells it to split — no new logic needed.

## 6. Interactions with other open issues

- **#21 "Drop authoring CLI + agent skill"** — drop files (`.claude/scribe/*.md`
  handoffs, referenced by `extract.md`'s `{{DROP_INSTRUCTION}}` step) are
  themselves absorbed by the extract prompts touched here. No file overlap,
  but if #21 lands a structured drop schema later, it should carry a
  `failure_traces:` or similar hint through to extraction consistent with
  this issue's guideline — worth a one-line cross-reference when #21 is
  planned, not a blocker for #42.
- **#25 "Stop-words filter"** — operates upstream of these prompts (holds
  documents out of the KB entirely); no interaction.
- **#9 "stub LLM provider harness"** — if a future test wants to assert
  prompt content or behavior against a stub, it would consume these same
  files; no conflict, this change lands first and is orthogonal.
- No other open issue (#2, #3, #5, #8, #19, #22, #23, #24, #26, #27, #28,
  #40, #41) touches `cmd/scribe/prompts/` or `cmd/scribe/templates/
  kb-CLAUDE.md`.

## 7. Size estimate

**S** (small). ~90-110 net new lines across 16 files (15 prompt/template
files with one guideline sentence/bullet each, most 1-3 lines; the largest
single insertion is the Register A full sentence at ~70 words). Zero Go
code changes. Estimated diff: `+90/-2` (the `-2` from the two in-place line
edits at §3.9 KEEP line and §3.14 parenthetical list, which modify an
existing line rather than pure-append).
