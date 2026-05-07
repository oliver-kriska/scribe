# Directory taxonomy

A scribe-managed KB has a fixed directory layout. Each directory hosts one article type, and `scribe lint` rejects type/path mismatches.

```
<KB root>/
├── scribe.yaml             KB config: domains, owner, capture handles, paths
├── CLAUDE.md               KB-level agent guide (auto-managed by `scribe init`)
├── decisions/              Long-lived decisions and policies
├── solutions/              Concrete fixes for specific bugs/tasks
├── patterns/               Reusable techniques applied across projects
├── research/               Deep-dive investigations with sources
├── tools/                  Tool/library evaluations with verdict
├── people/                 Identity pages with aliases (handles, emails)
├── projects/<name>/        Per-project memory (overview + rolling files)
├── ideas/                  Idea drafts that may become decisions later
├── sessions/               Mined Claude Code sessions (auto-created by triage)
├── wiki/                   Generated indexes: _index.md, _hot.md, _sections/, _backlinks.json
└── raw/
    └── articles/           Captured / ingested content (the inbox)
```

Plus optional state under `output/`, `scripts/`, etc. — those are tooling concerns, not content.

## Choosing the right directory

| Article kind | Directory | Type frontmatter |
|---|---|---|
| Long-lived decision (architecture, policy, tool choice) | `decisions/` | `type: decision` |
| Concrete bug fix or workaround | `solutions/` | `type: solution` |
| Reusable technique that applies across projects | `patterns/` | `type: pattern` |
| Investigation with multiple sources, draws conclusions | `research/` | `type: research` |
| Tool/library evaluation with verdict | `tools/` | `type: tool` |
| Identity page (person, account, alias cluster) | `people/` | `type: person` |
| Per-project overview, learnings log, decisions log | `projects/<name>/` | `type: project` |
| Future-work idea draft | `ideas/` | `type: idea` |

When unsure, prefer the **most specific** directory. A bug fix that *also* teaches a reusable lesson is a `solution` (specific) plus a `pattern` (reusable) — write two articles and cross-link, don't try to make one article do both jobs.

## Per-project rolling files

Every `projects/<name>/` typically has:

- `overview.md` — what the project is, key links, current status
- `learnings.md` — append-only log of "what I learned." `rolling: true` in frontmatter.
- `decisions-log.md` — append-only log of decisions made within this project. `rolling: true`.

Both rolling files have a soft 150-line limit before they should rotate to `learnings-archive-YYYY.md` / `decisions-log-archive-YYYY.md`. `scribe lint` warns at 200 lines.

## What lives where — examples

| Topic | File |
|---|---|
| Why we chose Postgres over SQLite for this project | `decisions/postgres-vs-sqlite-<project>.md` |
| The Postgres TS-Vectors search pattern itself | `patterns/postgres-ts-vectors-full-text-search.md` |
| A specific Postgres bug we hit + fix | `solutions/postgres-vacuum-stalls-on-large-tables.md` |
| A deep dive into Postgres TOAST behavior | `research/postgres-toast-performance.md` |
| Whether to use `pgvector` | `tools/pgvector.md` (with `verdict: use`) |

## raw/articles/ is the inbox

`raw/articles/` is the buffer where capture and ingest land content before it's absorbed into wiki articles. Don't write there manually — the daemon owns that directory. To push content from another project, write a drop file (see DROP_FILES.md).

`raw/articles/` files have lighter frontmatter:

```yaml
title: "<page or capture title>"
source_url: "<url if any>"
captured: 2026-05-07
fetched_via: trafilatura | jina | fxtwitter | defuddle | arxiv-html | stub | inbox
type: article
domain: general
tags: [imessage]
```

`fetched_via: stub` means capture only got the URL, not the body. The daemon's `scribe capture --refetch` cleans those up over time.
