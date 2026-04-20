# {{.KBName}} — Personal Knowledge Base

{{.KBName}} is an LLM-managed knowledge base following the three-layer pattern:

1. **Raw sources** (`raw/`) — immutable source documents. Articles, papers, imports. The LLM reads but never modifies.
2. **The wiki** (`wiki/`, `projects/`, `research/`, `solutions/`, `tools/`, `decisions/`, `patterns/`, `ideas/`, `people/`, `sessions/`) — LLM-generated markdown. The LLM owns this layer entirely.
3. **The schema** (this file) — tells the LLM how the KB is structured, what conventions to follow, what workflows to run.

## Owner Context (~120 tokens, loaded every session)

{{.OwnerContext}}

---

## Directory Structure

```
raw/                    Immutable source material — NEVER modify
  articles/             Captured articles from pipeline or web clipper
  assets/               Downloaded images referenced by articles
  imports/              Bulk imports from projects or external sources

wiki/                   LLM-compiled knowledge articles
  _index.md             Master index — every article listed with one-line summary
  _backlinks.json       Reverse link index {target: [sources]}
  _sessions_log.json    Tracks processed Claude Code session IDs

projects/               Per-project extracted knowledge (projects/{name}/overview.md + subtopics)
  {name}/.repo.yaml     Links KB dir back to source repo
  {name}/learnings.md   Rolling memory: insights and lessons (rolling: true, append-only)
  {name}/decisions-log.md  Rolling memory: decisions with context (rolling: true, append-only)
research/               Topic-based research deep dives
solutions/              Reusable technical solutions
tools/                  Tools evaluated or used — capabilities, setup, verdict
decisions/              Architectural decisions with reasoning and outcome
patterns/               Recurring patterns observed across projects
ideas/                  Unrealized project ideas and exploration notes
people/                 Contacts, collaborators, context
sessions/               Extracted Claude Code session insights (via ccrider MCP)
output/                 Transient query results and reports (gitignored)

scripts/                State files
  projects.json         Manifest of tracked projects
  imessage-state.json   Tracks captured URLs and last capture date

log.md                  Chronological record of all operations
```

---

## Frontmatter Conventions

Every wiki article requires YAML frontmatter. Seven entity types:

### Required fields (all types)

```yaml
---
title: "Article Title"
type: project | tool | person | decision | pattern | solution | research
created: YYYY-MM-DD
updated: YYYY-MM-DD
domain: {{.DomainsPipe}}
confidence: high | medium | low
tags: [tag1, tag2]
related: ["[[Other Article]]"]
sources: ["raw/imports/filename.md", "https://url"]
---
```

### Type-specific fields

**project**: `status` (active | paused | completed | idea), `stack`
**tool**: `url`, `verdict` (use | evaluate | skip), `alternatives`
**person**: `role`, `context`
**decision**: `status` (decided | reconsidering | superseded), `date_decided`
**pattern**: `applies_to` (list of project names or "general")
**solution**: `problem`, `applies_to`
**research**: `status` (active | completed | stale), `depth` (shallow | moderate | deep)

### Domain values

Configured in `scribe.yaml` under `domains:`. This KB uses:

{{range .Domains}}- `{{.}}`
{{end}}
The `personal` and `general` domains are always accepted for cross-cutting content.

---

## Operations

Run via the `scribe` CLI:

- `scribe sync` — discover projects, extract changed repos, absorb raw articles, reindex qmd.
- `scribe sync --sessions` — mine Claude Code sessions via ccrider.
- `scribe capture --fetch` — capture self-sent iMessage URLs, fetch content.
- `scribe dream` — weekly memory consolidation.
- `scribe lint --changed` — validate frontmatter on uncommitted changes.
- `scribe triage` — score sessions by knowledge density.
- `scribe doctor` — health check over deps, config, cron, state, freshness, errors.

See `scribe --help` for the full command tree.

---

## Core Rules

1. **Every task produces two outputs** — the answer AND updates to relevant wiki articles.
2. **Cross-domain tags from day one** — the `domain:` field is required.
3. **Source citation** — every claim traces back to a raw entry, URL, or session.
4. **Raw is immutable** — never modify files in `raw/`.
5. **Directories emerge from data** — don't pre-create empty category subdirectories.
6. **Anti-cramming** — if adding a third paragraph about a sub-topic, that sub-topic deserves its own page.
7. **Anti-thinning** — a stub with 3 vague sentences is a failure. Enrich or don't create.
8. **Canonical project paths** — `projects/{lowercase-name}/overview.md`.
9. **Post-write size check** — split articles over 150 lines.
10. **Rolling memory files** use `rolling: true` frontmatter and append-newest-first.
11. **Reality wins** — fix contradicted articles in place, don't carry both versions.
12. **No knowledge deletion** — the KB is append-only; mark superseded, never delete.
