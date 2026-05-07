# Structured Memory — Planning Note

Status: **Draft, not started**
Owner: Oliver
Filed: 2026-05-07
Last updated: 2026-05-07

## Why

Scribe today is good at the **capture → ingest → absorb → reindex** pipeline. It produces clean wiki articles, atomic facts, and verbatim citations, and a 1700-article KB on top of that. But three recent external pieces highlight specific gaps in how scribe shapes that knowledge once it's on disk:

1. **gxl.ai / Paperclip** — pre-process structure once, expose retrieval primitives (section, line, sql) instead of just whole-article search; tier the index so abstracts/stubs don't dilute long-form ranking.
2. **kepano/obsidian-skills** — package KB conventions as an agent-skill bundle so any Claude Code / Codex / OpenCode session can read and write the vault consistently; declarative `.base` files for views; defuddle as a cleaner alternative to trafilatura.
3. **Sentra "Company Brain, Part 2: Factual Memory"** — durable memory needs provenance, typed relationships, freshness, contradiction detection, and proactive surfacing. The quality of relationships determines the quality of the memory.

The common thread: **scribe extracts structure during absorb, then throws most of it away**. Chapters are computed for pass-1 chunking and discarded. `related:` is a flat string list. `sources:` is a path list with no relationship typing. Stubs are indexed alongside long-form. There's no spec for "this decision contradicts that one." There's no agent-installable skill that teaches the conventions to fresh sessions in unrelated repos. The KB is queryable only by full-text + vector — no declarative slice grammar.

This plan addresses those gaps in three tracks (5×, 6×, 7×) plus one speculative track (8).

## Current state

What scribe has today (relevant to this plan):

| Capability | Where | Notes |
|---|---|---|
| TOC sidecar | Phase 3A chunker | Computed in `cmd/scribe/chunker.go`; consumed only by chapter-aware absorb |
| Chapter-aware pass-1 | Phase 3A.5 | Splits dense articles for parallel haiku contextualize |
| Atomic facts ledger | Phase 3B | `wiki/_atomic_facts/` per article |
| Contradiction detection | Phase 3B.5 | Pass-2 verbatim citation enforcement; no surfacing UI |
| Cost ledger | Phase 3D | `output/runs/*.jsonl`, `scribe cost` subcommand |
| Wiki action envelope | Phase 4B groundwork | `cmd/scribe/wiki_action.go`; consumed by nothing yet |
| Frontmatter validation | `cmd/scribe/validate.go` | Closed type/domain/status sets, not queryable |
| Capture / ingest fetchers | `cmd/scribe/fetch.go` + `fetch_arxiv.go` | trafilatura → jina cascade; arxiv-aware tier |
| Stub vs full distinction | `fetched_via` field | Already exists; not used for index weighting |
| Cron orchestration | `cmd/scribe/cron.go` | macOS LaunchAgent install/status |

What scribe does *not* have:

- Section-level retrieval (chapters → forgotten)
- Tiered indexing hint passed to qmd
- Typed `related:` (decision-supersedes vs solution-applies-to vs research-extends)
- Surfaced contradictions
- Staleness detection
- Agent-skill bundle for KB conventions
- Declarative views (Bases-style)
- Defuddle as fetcher tier
- Proactive surfacing hooks

---

## Phase 5A — Section Sidecar

**Inspired by:** Paperclip's "search, grep, cat, map" primitives over structured papers (gxl.ai).

**Goal:** Persist the chapter/section structure that's already computed during chapter-aware pass-1, so qmd and downstream tools can retrieve at section granularity (not just article).

**Design:**

- Per absorbed wiki article, emit `<article>.sections.json` next to the `.md`. Schema:
  ```json
  {
    "article": "wiki/research/azure-extraction-consistency.md",
    "sections": [
      {"id": "s1", "title": "## Methods", "lines": [12, 47], "byte_range": [201, 1842], "tokens": 412},
      {"id": "s2", "title": "## Results", "lines": [49, 88], "byte_range": [1844, 4002], "tokens": 380}
    ],
    "extracted_at": "2026-05-07T12:34:56Z",
    "extractor": "chunker.go@3A"
  }
  ```
- Source: extend `cmd/scribe/chunker.go` to write the sidecar whenever it computes chapter splits. Already has the data — just needs to persist.
- Add CLI: `scribe section get <article> <section-id>` (prints the section), `scribe section list <article>` (prints the index). These are thin wrappers, hot path is fast.
- qmd integration: scribe writes a `.qmd-sections` collection mirror so qmd can index section bodies as separate documents while keeping the article-level entry. Out of scope here whether qmd grows native section-awareness; the sidecar is the contract.

**Code touch:** `chunker.go`, new `section.go` (CLI + sidecar reader), prompt nothing (no LLM calls).

**Effort:** S (1–2 days). The data already exists; persistence + CLI is plumbing.

**Risk:** Low. Sidecars are additive; no existing path reads them.

**Success metric:** For 100% of articles where chapter-aware pass-1 ran, a sidecar exists and `scribe section list` produces a non-empty index. For non-chaptered articles, no sidecar (heading-strategy fallback can compute one but not required for v1).

**Decisions applied (see §Decisions):**
- Sidecar lives at `wiki/_sections/<article-slug>.json` (parallel tree).
- Headings-strategy articles get a sidecar too, since heading strategy is the default for ≥3-section docs (v0.2.1).

---

## Phase 5B — Tiered Index Hint

**Inspired by:** Paperclip's "50M of 150M abstracts indexed" — selective tiering avoids dilution.

**Goal:** Tag every article with an `index_tier` so qmd ranking can weight long-form research above tweet stubs and link previews. Today a 10-word fxtwitter quote can outrank a 200-line research note on shared keywords.

**Design:**

- Compute tier at ingest/absorb time and store in frontmatter (`index_tier: stub | brief | standard | deep | reference`):
  - `stub` — `fetched_via: stub` + body < 80 words (capture-only, never refetched successfully)
  - `brief` — fxtwitter, single-tweet captures, body < 200 words
  - `standard` — body 200–2000 words, normal article
  - `deep` — body > 2000 words OR has section sidecar with ≥5 sections
  - `reference` — explicit human marker (decisions, canonical patterns)
- Source: extend `cmd/scribe/lint.go` and `absorb.go` to compute the tier on every pass; persist into frontmatter.
- qmd integration: emit a sidecar `.qmd-weights` map (or piggyback on the qmd `boost`/`weight` knob if it exists). When qmd doesn't natively support weighting, scribe can post-filter: `scribe query --tier deep,reference,standard` excludes stubs/briefs at the application layer.
- Validation: lint adds `index_tier` to required fields, with a closed set of values. Backfill script for existing articles (one-off, like `fix-slug-titles.sh`).

**Code touch:** `lint.go` (new field), `absorb.go` (compute tier), `validate.go` (closed set), new `cmd/scribe/tier.go` for the CLI subcommand. One-off backfill script in scriptorium/scripts/.

**Effort:** S (1–2 days) for the field and computation. M (3–4 days) if qmd needs a feature request for native weighting.

**Risk:** Low for the field. Medium for qmd integration — depends on qmd's weighting story.

**Dependencies:** None hard. Phase 5A's sidecar makes the `deep` tier classification easier (count sections) but isn't required.

**Success metric:** All wiki articles have a tier. Search for any keyword that exists in both a stub and a long-form article ranks the long-form first when `--tier deep,standard` is applied.

**Decisions applied (see §Decisions):**
- `index_tier` is computed by default; `index_tier_override:` frontmatter key lets a human pin a value, lint validates against the closed set and preserves the override.
- Lint warns (not errors) on missing tier so the field rolls out without a flag-day migration.
- Stubs in `raw/articles/` stay indexed by qmd but tier `stub` is excluded from `scribe query` results by default — keeps them grep-able but not search-rankable.

---

## Phase 6A — Typed Relationships

**Inspired by:** Sentra "the quality of the relationships determines the quality of the memory."

**Goal:** Replace the flat `related: ["[[Foo]]", "[[Bar]]"]` list with typed edges so an agent can ask "what does this decision supersede?" or "what solutions apply to this pattern?" without scanning bodies.

**Design:**

- Frontmatter syntax extension. Two options:
  - **Option A (verbose):** dedicated keys per relation type
    ```yaml
    related: ["[[Foo]]"]      # untyped, generic
    supersedes: ["[[Bar Decision]]"]
    superseded_by: ["[[Baz Decision]]"]
    contradicts: ["[[Old Pattern]]"]
    derived_from: ["[[Source Note]]"]
    applies_to: ["[[Pattern X]]"]
    instance_of: ["[[Pattern Y]]"]
    ```
  - **Option B (compact):** keep `related:` but allow inline typing
    ```yaml
    related:
      - "[[Foo]]"                          # untyped (default: generic)
      - "supersedes: [[Bar Decision]]"
      - "applies_to: [[Pattern X]]"
    ```
  - Recommendation: **Option A**. Verbose keys are easier to grep, simpler to validate, and play well with kepano's Bases (each becomes a filterable column).
- Closed set of relation types per article type:
  - `decision` → `supersedes`, `superseded_by`, `contradicts`
  - `solution` → `applies_to` (pattern), `derived_from` (research)
  - `pattern` → `instance_of` (more general pattern), `specializes`
  - `research` → `extends`, `cited_by`, `informs`
  - All types share `related` (untyped, free-form) for genuinely loose connections
- Bidirectional integrity: when X declares `supersedes: [[Y]]`, Y should declare `superseded_by: [[X]]`. `scribe link` already injects "See Also" sections; extend to maintain typed reciprocals.
- Lint extends to validate typed edges resolve to real articles, and the inverse exists on the target.

**Code touch:** `validate.go` (per-type allowed relations), `link.go` (bidirectional injector), `lint.go` (orphan detection respects typed edges), new `cmd/scribe/graph.go` for `scribe graph <article>` (prints the relationship neighborhood).

**Effort:** M (4–6 days) including migration of existing `related:` entries to typed edges. Migration runs LLM auto-classify by default (`scribe relations migrate`), with a `--assisted` mode that prompts the human per edge for the first sweep.

**Migration mechanics:**
- Default: `scribe relations migrate` walks every article's `related:` list, sends each pair (subject, object, both bodies) to the classifier model, writes typed edges directly. Each typed edge gets a hidden audit trail in `wiki/_relations_migration_<ts>.jsonl` (model, confidence, original `related:` value) so a single `scribe relations revert <ts>` rolls it back.
- Opt-in: `--assisted` prompts confirm/edit per edge — slower, appropriate for the first batch the user wants to eyeball before trusting auto.
- Per-article skip: `relations_locked: true` frontmatter key tells the migrator to leave that article alone (hand-curated cases).
- Low-confidence threshold: edges below the model's confidence floor stay as `related:` (untyped) instead of being mis-classified.

**Risk:** Medium. Migration is the work; the schema is small. The audit trail + revert command keeps blast radius reversible. Typed edges add cognitive load for the human writer — keep `related:` as the easy-out so authors don't stall.

**Dependencies:** None.

**Success metric:** All `decision` articles either declare `supersedes:` or have an explicit `supersedes: []` (empty). `scribe graph` prints a non-trivial typed neighborhood for any article with ≥3 typed edges.

**Out of scope:** A graph DB. The frontmatter is the source of truth; graph queries are filesystem grep.

---

## Phase 6B — Contradiction Ledger

**Inspired by:** Sentra "is it still current? is it contradicted by something newer?" — combined with scribe's existing pass-2 contradiction detection (Phase 3B.5) and the wiki action envelope (Phase 4B groundwork).

**Goal:** When absorb pass-2 detects that a new article contradicts an existing claim, persist the contradiction as a first-class artifact instead of just logging it. Surface it in `scribe doctor`.

**Design:**

- Reuse the wiki action envelope (`cmd/scribe/wiki_action.go`) to define a `contradict` action shape:
  ```json
  {
    "kind": "contradict",
    "actor": "absorb-pass-2",
    "subject": "wiki/decisions/use-azure-gpt5.md",
    "object": "wiki/decisions/migrate-to-bedrock.md",
    "claim_subject": "Azure GPT-5.3 is the chosen extraction model",
    "claim_object": "Bedrock Claude Sonnet 4.6 is the chosen extraction model",
    "evidence_subject": "raw/articles/2026-04-12-decision-azure-gpt5.md",
    "evidence_object": "raw/articles/2026-04-28-bedrock-migration-rationale.md",
    "first_observed_at": "2026-04-28T15:02:11Z",
    "resolved": false
  }
  ```
- Append to `wiki/_contradictions.jsonl` (line-appended, like cost ledger).
- `scribe doctor --section contradictions` reads the ledger, filters `resolved: false`, prints a list with file paths and a one-line excerpt.
- Resolution: human edits one of the contradicting articles (typically adds `superseded_by:` from Phase 6A) and runs `scribe doctor --resolve <ledger-id>` to mark resolved.

**Code touch:** `wiki_action.go` (new action kind), `absorb.go` (emit on pass-2 contradiction), `doctor.go` (new section), prompt template extended for pass-2 to emit structured contradiction envelopes instead of free-text mentions.

**Effort:** M (3–5 days). Pass-2 already detects; persistence is plumbing. The prompt change is the trickiest part — must not regress citation enforcement.

**Risk:** Medium. Pass-2 prompt changes have historically been brittle (Phase 3B.5 took two iterations).

**Dependencies:** Phase 4B wiki action envelope (already groundwork-done). Benefits from Phase 6A typed relationships (lets human resolve via `superseded_by:`).

**Success metric:** A real contradiction in scriptorium (e.g., the Azure → Bedrock migration if/when that happens) appears in `scribe doctor --section contradictions` within one absorb cycle.

---

## Phase 6C — Staleness Detection

**Inspired by:** Sentra "is this current? is it contradicted by something newer?"

**Goal:** Compute and surface staleness signals based on `updated:` age, source-URL HTTP status, and downstream-link deltas. Don't auto-mark articles stale — surface candidates for human review.

**Design:**

- Compute three independent signals per article:
  1. **Date staleness** — `updated:` older than the article-type's half-life. Decisions: 180 days. Patterns: 365 days. Solutions: 365 days. Research: 90 days for `status: active`, never for `superseded`. Configurable via `scribe.yaml`.
  2. **Source staleness** — for articles with `source_url:`, scribe periodically HEAD-checks; 404/410/dead-link → `source_dead: true`.
  3. **Reference staleness** — when an article cites `[[X]]` and X has been updated more recently with a `supersedes:` chain not pointing back, flag it.
- Persist into a `wiki/_staleness.jsonl` ledger (mirrors contradiction ledger shape).
- New `scribe stale` subcommand: prints a triaged list (date / source / reference) with a one-line "why."
- New cron job (weekly) runs `scribe stale --recompute`. Don't run it inline with `scribe sync`.

**Code touch:** New `cmd/scribe/stale.go`, `cron.go` (additional plist), `scribe.yaml` template (half-life config). HTTP HEAD logic is small.

**Effort:** M (3–5 days).

**Risk:** Low for date + source. Medium for reference staleness — has to walk `supersedes:` chains, which depends on Phase 6A.

**Dependencies:** Phase 6A for reference staleness. Date + source can ship independently.

**Success metric:** `scribe stale` produces a useful triage list (≤20 candidates per run for a 1700-article KB). False-positive rate <30% on the date signal.

---

## Phase 7A — Skill Bundle (`scriptorium-skill`)

**Inspired by:** kepano/obsidian-skills — agent-skill packaging following the [agentskills.io spec](https://agentskills.io/specification).

**Goal:** Ship `scribe init --skill` (or a parallel `scribe skill install`) that drops a `SKILL.md` bundle into the user's project so any Claude Code / Codex / OpenCode session can read+write the KB with the right conventions.

**Design:**

- Bundle structure (mirrors kepano):
  ```
  scriptorium-skills/
    SKILL.md                   # Top-level: when to use, vault layout, query patterns
    references/
      FRONTMATTER.md           # Required fields, type-specific fields, validation rules
      WIKILINKS.md             # Wikilink syntax, alias support, block IDs
      DROP_FILES.md            # How to write `.claude/scriptorium/YYYY-MM-DD-*.md` from another project
      QUERY.md                 # qmd query patterns: lex/vec/hyde, when to use which
      STRUCTURE.md             # Directory taxonomy (decisions/ vs solutions/ vs patterns/ vs research/ vs ideas/)
  ```
- The skill is **published from scribe itself** (embedded under `cmd/scribe/skills/` via `go:embed`) so a one-binary install delivers the skill as a side effect.
- Two modes:
  - **Inside the KB:** `scribe skill install` writes to `.claude/skills/scriptorium/` in the KB root.
  - **In other projects:** `scribe skill install --remote ~/Projects/some-other-repo` writes the bundle there so that project's Claude Code sessions know how to file drop files.
- Auto-update path: skill bundle is versioned with scribe; `scribe skill install --check` warns if the installed bundle is older than the binary's embedded one.

**Distribution (decided — see §Decisions):** dual-publish.
- Primary source of truth: `cmd/scribe/skills/` (embedded via `go:embed`), shipped with `scribe skill install`.
- Mirror: public `oliver-kriska/scriptorium-skills` repo, populated on each scribe tag by a release-pipeline step (extends `.goreleaser.yml` or a sibling workflow). Discoverable on agentskills.io / `npx skills add` / `/plugin install`.
- Single edit path — content always written under `cmd/scribe/skills/`; the public repo is generated.

**Code touch:** New `cmd/scribe/skill.go` + new embed tree under `cmd/scribe/skills/` + a release-pipeline step that pushes the embedded tree to `oliver-kriska/scriptorium-skills`. CLI surface is small. The bundle content is the bulk of the work — codifying scriptorium's conventions into a SKILL.md is essentially writing the missing CLAUDE.md for the KB pattern itself.

**Effort:** M (4–6 days). The CLI/embed plumbing is half a day; the release-mirror step is half a day; the content is the rest. Worth investing because it pays back every time a fresh Claude Code session in a fresh project needs to file drop files correctly.

**Risk:** Low. Additive.

**Dependencies:** None hard. Phases 6A/6B/6C make the skill richer if shipped first (typed relations, staleness commands to teach).

**Success metric:** A fresh Claude Code session in a non-KB project can correctly write a drop file with valid frontmatter on first try, without needing to read scribe's source. Bonus: the public repo gets ≥1 external installer outside Oliver's machine within 30 days.

**Out of scope:** Multi-vendor skill packaging beyond the agentskills.io spec. Ship one format; let users install it wherever.

---

## Phase 7B — Bases-Style Views

**Inspired by:** kepano/obsidian-skills — `.base` files are declarative YAML views (filter + formula + view shape).

**Goal:** Let humans (and agents) declare reusable KB slices as files, instead of recomputing them via shell pipelines. "All active decisions in domain enaia, sorted by updated date" becomes a one-line view file.

**Design:**

- File extension: `.scribe-view.yaml` (decided — see §Decisions). Documented as a subset of Obsidian Bases semantics so future `.base` compat is achievable but not pre-engineered. Revisit `.base` once ≥5 real views exist.
- Schema (subset of Bases, scribe-flavored):
  ```yaml
  filters:
    and:
      - type == "decision"
      - status == "active"
      - domain == "enaia"
  sort:
    - field: updated
      direction: desc
  view:
    type: table
    columns: [title, updated, confidence, related]
    limit: 20
  ```
- Live in `wiki/_views/` so they're committed alongside the KB.
- New CLI: `scribe view <name>` runs the view and prints the result. `scribe view --list` lists registered views. Output formats: `--md` (table), `--json`, `--csv`.
- Agent-friendly: same view file consumable by qmd or by an agent that wants a reproducible slice ("give me the same list I had yesterday").

**Code touch:** New `cmd/scribe/view.go` with a small expression evaluator (no need for a full DSL — start with `==`, `!=`, `in`, `contains`, `>`, `<`, `and`, `or`). Frontmatter loader already exists in `validate.go`.

**Effort:** M (4–7 days). Bases-style filters are simple; the table renderer is the chunky bit.

**Risk:** Medium. Risk of scope creep into a query language. **Hard limit: no joins, no aggregates beyond count/group, no full-table scans of body content.** Frontmatter-only filters.

**Dependencies:** Phase 6A typed relations make views much more useful (filter by `supersedes`).

**Success metric:** Three concrete views committed and useful in scriptorium:
- `wiki/_views/active-decisions-enaia.scribe-view.yaml`
- `wiki/_views/stale-research.scribe-view.yaml`
- `wiki/_views/orphan-patterns.scribe-view.yaml`

**Out of scope:** Body content search (qmd already does that). Charting / visualization. Cross-vault views.

---

## Phase 7C — Defuddle as Fetcher Tier

**Inspired by:** kepano/obsidian-skills — defuddle is a Node CLI that extracts clean markdown from web pages, often cleaner than trafilatura on JS-heavy sites.

**Goal:** Add defuddle as a fetcher tier between trafilatura and jina. Maintains the cascade philosophy (cheaper/faster first, expensive last) while adding a tier that handles JS-heavy modern sites trafilatura struggles on.

**Design:**

- New `cmd/scribe/fetch_defuddle.go` mirroring `fetch.go::fetchTrafilatura`. Invokes `defuddle parse <url> --md` and parses the markdown output.
- Cascade order updates:
  ```
  arxiv-aware → fxtwitter → trafilatura → defuddle → jina
  ```
  Rationale: trafilatura is fastest (Python local) and works for ~80% of sites. Defuddle is Node, slightly slower, but better on JS-heavy SPAs. Jina is hosted-API last resort.
- Optional: `scribe.yaml` knob to disable defuddle if user doesn't have Node/npm.
- Doctor: extend `scribe doctor convert` to detect defuddle presence and recommend installation (`npm install -g defuddle`).

**Code touch:** New file, small. Extend `fetch.go::fetchURL` cascade.

**Effort:** S (1–2 days).

**Risk:** Low. Additive tier; failure falls through to next tier as today.

**Dependencies:** None.

**Success metric:** On a curated test set of 20 known-bad-trafilatura URLs, defuddle succeeds on ≥10 of them.

---

## Phase 8 — Proactive Surfacing (speculative)

**Inspired by:** Sentra "factual memory cannot just sit in a search box waiting for a query."

**Goal:** When an agent (Claude Code, Codex, etc.) is about to do work in a project that has a related KB article, surface that article *before* the agent searches.

**Design (sketch only, this phase is exploratory):**

- Mechanism: Claude Code SessionStart hook (already used for ccrider integration) reads the project name, project domain (from `scribe.yaml`), recently-edited files, and the user's first prompt. Queries qmd with that context and injects a short "you may want to check these articles" preamble.
- Concrete trigger: when the SessionStart hook sees the user prompt mentions an entity name that matches a frontmatter `aliases:` or article title, inject `[[Article Title]]` references.
- Boundaries: surface ≤3 articles, ≤200 words total. Never overrides the user's request; just front-loads context.
- Privacy: scribe's KB is local; no data leaves the machine. The hook is opt-in via `~/.config/scribe/config.yaml`.

**Why this is the last phase:** Hooks are brittle, the proactive UX is easy to get wrong (annoying), and the phases above need to land first so there's something worth surfacing. Don't build this until 5A/5B/6A/7A exist.

**Effort:** L (1–2 weeks if done well).

**Risk:** High. UX risk dominates. Easy to make worse, not better.

**Dependencies:** Phases 5A, 5B, 6A, 7A.

**Success metric:** Subjective. "Did the surface help me find a relevant prior article I'd otherwise have missed?" — measured by a small per-session telemetry counter, opt-in.

---

## Out of scope (for this plan)

- **Non-markdown vaults.** scribe is markdown-only by design.
- **Multi-user permissions.** Sentra's permissions discussion is for organizations; scribe is single-user.
- **Real-time collaboration.** Drop files + cron sync is the answer.
- **Replacing qmd with a custom search engine.** scribe orchestrates qmd; that boundary stays.
- **A graph DB.** Frontmatter is the source of truth; graph queries are filesystem operations.
- **Embeddings/inference inside scribe.** scribe shells out to `claude -p` (Phase 4 has the local-model alternative). Don't pull a model into the binary.

---

## Sequencing recommendation

Three independent tracks, do in any order within each track, but follow track order for coherence:

**Near-term (high ROI, low risk):**
1. **Phase 5A — Section sidecar** (1–2 days, S)
2. **Phase 5B — Tiered index hint** (1–2 days, S)
3. **Phase 7C — Defuddle tier** (1–2 days, S)
4. **Phase 7A — Skill bundle** (4–6 days, M; high external value — every other project benefits)

**Medium-term (depends on near-term, structural):**
5. **Phase 6A — Typed relationships** (4–6 days, M)
6. **Phase 6B — Contradiction ledger** (3–5 days, M; depends on 6A + 4B groundwork)
7. **Phase 6C — Staleness detection** (3–5 days, M; depends on 6A)
8. **Phase 7B — Bases-style views** (4–7 days, M; depends on 6A for typed-edge filters)

**Long-term (speculative):**
9. **Phase 8 — Proactive surfacing** (L, only after the above)

Total near-term: ~10–14 days. Total medium-term: ~15–25 days. Don't pre-commit to all of it — re-evaluate after each phase ships.

---

## Why this order

- **5A + 5B unlock everything else.** Section sidecar + tier hint are cheap and additive; once they exist, every later phase (typed relations, views, proactive surfacing) gets to use them.
- **7C is independent and small** — easy to ship while thinking about the structural phases.
- **7A (skill bundle) before 6× because** the skill teaches the conventions; once it exists, *humans and agents* writing into the KB do it correctly, which makes the structural work (typed relations, etc.) less of a migration.
- **6A before 6B/6C/7B** because typed edges are foundational — contradiction surfacing, staleness reasoning, and view filters all benefit.
- **8 last** because it's high risk, UX-heavy, and only valuable if there's structured memory to surface.

---

## Decisions (locked 2026-05-07)

1. **Phase 5A sidecar location — parallel tree.** Sidecars live at `wiki/_sections/<article-slug>.json`, mirroring the existing `wiki/_atomic_facts/` convention. Wiki dirs stay clean for human navigation; agents and tooling read the parallel tree.

2. **Phase 5B `index_tier` — computed by default, override-capable.** scribe computes the tier on every absorb/lint pass. Frontmatter optionally carries `index_tier_override:` (closed set, lint-validated) which lint preserves and tier-computation respects. Lint warns (not errors) on missing tier so the field can roll out without a flag-day migration.

3. **Phase 6A typed-relations migration — LLM auto by default, `--assisted` opt-in.** Two modes:
   - **Default (`scribe relations migrate`):** fully automatic LLM classification of existing flat `related:` entries into typed edges (`supersedes`, `applies_to`, `derived_from`, etc.). Writes results directly. Each typed edge carries a hidden `_classifier: {model, confidence, ts}` annotation in a sidecar so the audit trail exists without polluting frontmatter.
   - **Opt-in (`scribe relations migrate --assisted`):** prompts the human to confirm/edit each proposed edge before write. Slower but appropriate for the first batch the user wants to eyeball.
   - Per-article opt-out: a `relations_locked: true` frontmatter key tells the migrator to skip that article entirely (for hand-curated cases).
   - Backout: every migration writes a `wiki/_relations_migration_<ts>.jsonl` log so a single command can revert.

4. **Phase 7A skill bundle distribution — both.** scribe embeds the SKILL.md tree under `cmd/scribe/skills/` (via `go:embed`) so `scribe skill install` always ships the version that matches the binary. The same content is mirrored to a public `oliver-kriska/scriptorium-skills` repo so it's discoverable on agentskills.io and installable via `npx skills add` / `/plugin install` for users who don't have scribe installed. Single source of truth lives in the scribe repo; a release-pipeline step pushes the bundle to the public repo on each tag.

5. **Phase 7B view filename — `.scribe-view.yaml` for v1.** Scribe-flavored YAML, documented as a subset of Obsidian Bases semantics. Revisit full `.base` compat once there are ≥5 real views in scriptorium and the actual filter shapes are known. Don't pre-design for compat that may never be needed.

---

## What this plan does NOT promise

- It does not promise a Sentra-style "Company Brain" — scribe is single-user; the multi-tenant memory layer Sentra describes is a different product.
- It does not promise replacing Obsidian — kepano's vault tools are vault-side; scribe is pipeline-side. They compose.
- It does not promise that every phase ships. The point of phasing is to ship the cheapest highest-ROI first and decide each subsequent phase against current usage.

---

## Appendix A — Logseq compatibility analysis

### Question

Should scribe (a) move to Logseq as primary vault format, (b) support Logseq compatibility alongside the existing markdown vault, or (c) ignore Logseq?

### Short answer

**(b) — soft compatibility, no migration.** Add it to existing phases where the cost is near zero; do not retool around block-outliner semantics.

### Where Logseq differs from current scribe shape

| Logseq concept | Scribe analogue | Compatible? |
|---|---|---|
| Page-as-bullets (block outliner) | Article-as-paragraphs-and-headings | **Incompatible at primary level.** A solution like "Postgres TS Vectors" reads better as paragraphs with H2 sections than as nested bullets. Forcing it into outline form damages the writing. |
| Block IDs (`((block-id))`) | Section sidecar IDs (Phase 5A) | **Compatible at section granularity.** Adopt Obsidian-style `[[Article#^section-id]]` block-anchor syntax in Phase 5A sidecar IDs — this works in both Obsidian and Logseq with the same on-disk syntax. |
| Block properties (`key:: value` per block) | Page-level frontmatter | **Incompatible primary, compatible secondary.** Scribe articles are paragraph-shaped, not bullet-shaped. Per-section properties could ride in the Phase 5A sidecar instead of inline. |
| Datalog queries via Datascript | Phase 7B view DSL (proposed) | **Compatible as future direction.** YAML-Bases for v1, Datalog as the eventual richer query surface. Don't implement Datalog in v1. |
| Journal-first (`journals/2026-05-07.md`) | `raw/articles/` capture buffer | **Same philosophy, different shape.** scribe's `raw/articles/` already plays the journal-buffer role: time-stamped, append-only, inbox-shaped. Worth renaming or aliasing for legibility. |
| TODO/DOING/DONE blocks | None (out of scope) | Skip. Task management is not a KB concern. |
| Org-mode dialect | None | Skip. Stay markdown-pure. |
| Datascript graph DB | `wiki/_backlinks.json` + frontmatter | **Same role, different implementation.** scribe rebuilds the graph from frontmatter on every `scribe sync`. Don't introduce a DB layer. |

### Why not Logseq primary

Three blockers:

1. **Article-shape content damage.** Decisions, solutions, patterns, and research notes are paragraph-and-heading shaped. Logseq's block outliner forces every paragraph into a top-level bullet. Reading a 130-line solution article as nested bullets is worse than reading it as prose. The KB's *value* is the articles; degrading them to fit a vault tool is the wrong trade.

2. **Pipeline coupling.** scribe's chunker, absorb pass-1, atomic-fact extraction, and section sidecar (Phase 5A) all assume markdown-with-headings. Logseq's bullet-as-block format would require rewriting every content extraction stage. Two-format support is twice the maintenance for the same KB.

3. **Tool-independence is a stated value.** scribe's CLAUDE.md and the existing local-model plan explicitly aim for the KB to be readable without any specific tool. Logseq-shape content fails that test — the bullets render in plain markdown viewers but the block-refs and queries don't.

### Why soft compat is cheap

Logseq reads markdown. Existing scriptorium articles render in Logseq's preview today (they just don't get the block-outliner treatment). Three additions make compat near-zero-cost:

1. **Phase 5A sidecar emits Obsidian-style `^section-id` anchors.** Same syntax works in both Obsidian and Logseq. No format fork.
2. **Phase 7A skill bundle includes a Logseq-compat note.** Tells agents that the vault is markdown-first and which subset of Obsidian-flavored markdown is in use; same content useful for Logseq users.
3. **Document `raw/articles/` as the journal-equivalent buffer in the README.** Helps Logseq-leaning users mentally map scribe's pipeline to Logseq's daily-journal pattern.

None of these require new code paths. They're documentation + sidecar-syntax choices.

### Logseq ideas that *do* improve scribe

Two concrete pulls:

1. **Datalog as the v2 query language for Phase 7B views.** Note this in the Phase 7B section as a future direction. Logseq's syntax is the most expressive PKM query language in the wild and is the natural endpoint if YAML-Bases proves limiting. Don't pre-build it.

2. **Section-level properties via sidecar (Phase 5A enrichment).** Logseq's per-block `key:: value` properties are useful for "this section has confidence: high" — finer-grained than article-level frontmatter. Build into the section sidecar JSON, not into the markdown body, so the article stays clean prose.

### Plan changes

- **Phase 5A** — sidecar IDs use Obsidian/Logseq block-anchor syntax (`^id`), and the JSON schema gets an optional `properties: {confidence: high, ...}` per section.
- **Phase 7A skill bundle** — adds a `references/COMPAT.md` documenting that scriptorium articles are also viewable in Logseq with the caveats above.
- **Phase 7B views** — explicit note that Datalog is the v2 target; v1 stays YAML-Bases.

No new phase needed. Logseq compat ships as side-effects of phases already in this plan.

### What this rules out

- A `scribe init --logseq` that scaffolds a block-outliner KB. Don't ship it.
- A Logseq-mode for absorb that emits bullet-shaped articles. Don't ship it.
- A Datascript-backed `scribe view` in v1. Stay declarative-YAML.
