# Frontmatter schema

Every article in a scribe-managed KB carries YAML frontmatter between two `---` delimiters at the top of the file. `scribe lint` and `scribe validate` enforce the schema. Drift fails the pre-commit hook.

## Required fields (all article types)

```yaml
---
title: "Exact title; quoted if it contains punctuation, : or @"
type: project | tool | person | decision | pattern | solution | research | idea
created: 2026-05-07     # YYYY-MM-DD; written once at article creation
updated: 2026-05-07     # YYYY-MM-DD; bump on every edit
domain: <domain>        # see scribe.yaml `domains:` list + universal "personal", "general"
confidence: high | medium | low
tags: [tag1, tag2]      # YAML list, lowercase, hyphenated, no spaces inside a tag
related: ["[[Article A]]", "[[Article B]]"]  # wikilinks to neighbors
sources: ["raw/articles/...", "https://..."]  # paths and URLs the article rests on
---
```

`title` is the canonical wikilink target. Use exactly the title in `[[wikilinks]]` from other articles. If you rename a title, every existing inbound link breaks unless `aliases:` carries the old name.

## Type-specific required values

Some types add extra closed-set fields that lint validates:

| Type | Extra field | Allowed values |
|---|---|---|
| `project` | `status` | `active`, `paused`, `completed`, `idea` |
| `tool` | `verdict` | `use`, `evaluate`, `skip` |
| `decision` | `status` | `decided`, `reconsidering`, `superseded` |
| `research` | `status` | `active`, `completed`, `stale`, `superseded` |
| `research` | `depth` | `shallow`, `moderate`, `deep` |

A `decision` with `status: decided` means it's the current call. `reconsidering` means re-evaluating. `superseded` means a newer decision replaces it (point to the replacement via `related:` or a typed `superseded_by:` edge if the KB has Phase 6A enabled).

## Optional fields

```yaml
authority: canonical | contextual | opinion
# canonical = intentional decisions/policies; wins contradictions
# contextual = curated solutions/patterns; wins over opinion
# opinion = raw captures, tweets, excerpts; loses by default
# Default: contextual for wiki pages, opinion for raw articles.

aliases:
  - "Old Title You Renamed From"
  - "@twitter-handle"
# Alternate names this article responds to. Wikilinks pointing at any
# alias resolve here. Useful for rename history and identity merging
# (e.g. `aliases: ["@karpathy"]` on Andrej Karpathy's page).

rolling: true
# Marks an append-only log file (learnings.md, decisions-log.md).
# Lint applies different size rules; do not write paragraphs in
# rolling files — only timestamped entries.

stack: [Go, SQLite, CGO]
# Optional list of technologies; can be a YAML list OR a string.

verdict: use | evaluate | skip      # only valid on type: tool
problem: "One-line problem statement"  # optional on type: solution
status: ...                          # see Type-specific table above
depth: shallow | moderate | deep     # only valid on type: research

index_tier: stub | brief | standard | deep | reference
# Phase 5B (scribe v0.2.5+). Computed automatically — humans rarely
# write this. Tier weights qmd ranking so stubs don't dilute long-form.

index_tier_override: <one-of-above>
# Pin a tier that survives recomputes. Set this when the computed
# value is wrong (e.g. an explicitly canonical "reference" article
# that's only 100 words).
```

## Date handling subtlety

Go's YAML parser auto-converts `YYYY-MM-DD` to `time.Time`. Both quoted strings and bare scalars work:

```yaml
created: 2026-05-07          # ok
created: "2026-05-07"        # also ok
created: 2026-5-7            # NOT ok — single-digit month/day rejected
```

`scribe validate` enforces YYYY-MM-DD. Don't use other date formats.

## A complete worked example

```yaml
---
title: "Postgres TS Vectors Full-Text Search"
type: pattern
created: 2026-04-12
updated: 2026-05-07
domain: enaia
confidence: high
authority: contextual
tags: [postgres, full-text-search, ts-vector, search-ranking]
related:
  - "[[Semantic Vector Search with pgvector]]"
  - "[[PostgreSQL Search Performance Tuning]]"
sources:
  - "raw/articles/2026-04-10-postgres-fts-deep-dive.md"
  - "https://www.postgresql.org/docs/current/textsearch.html"
index_tier: deep
---

# Postgres TS Vectors Full-Text Search

(article body...)
```

## Common lint failures

- **`missing required fields: ...`** — add the listed fields. Most likely `tags`, `related`, or `sources` (often empty `[]`).
- **`invalid type: '<x>'`** — typo. The closed set is in this file under "Required fields".
- **`invalid domain: '<x>'`** — domain is not in the user's `scribe.yaml`. Add it there or pick a valid one.
- **`invalid YAML frontmatter: ...`** — usually unquoted `@handle` or `:` inside a value. Quote the value.
- **`tags should be a list, got: string`** — `tags: foo, bar` is wrong. Use `tags: [foo, bar]`.
- **`<field> not in YYYY-MM-DD format`** — fix the date.
