# Drop files: cross-project KB contributions

When you produce reusable knowledge in a non-KB project (an Elixir app, a Python script, anything that isn't the KB itself), you don't write directly into the user's KB. You write a **drop file** in the current project, and the user's `scribe sync` cron job absorbs it into the KB on the next cycle.

This keeps two boundaries clean:

- The KB stays the daemon's domain — only the daemon writes wiki articles.
- The current project repo can be committed without leaking KB-specific paths.

## Where the drop file goes

```
.claude/<kb_name>/YYYY-MM-DD-{slug}.md
```

`<kb_name>` is whatever the user named their KB. If the user's CLAUDE.md mentions `scriptorium`, use `scriptorium`. If it mentions `kb` or `notes` or another name, use that. The user's `scribe.yaml` has the canonical value as `kb_name:`. When in doubt, ask.

`YYYY-MM-DD-{slug}.md` is the standard scribe drop-file naming. Slug should be a hyphenated lowercase phrase summarizing the article.

## Required frontmatter

```yaml
---
scriptorium: true            # or <kb_name>: true — matches the dir name
action: create | update | append
title: "Article Title"       # quoted
type: project | tool | person | decision | pattern | solution | research | idea
domain: <domain>             # one of the user's configured domains
tags: [tag1, tag2]
---
```

The first key is the **kb-name marker**. If the user's KB is called `scriptorium`, write `scriptorium: true`. If it's called `vault`, write `vault: true`. The daemon uses this marker to find drop files; without it, the file is invisible to absorb.

`action` controls how the daemon merges the drop:

- **`create`** — write a new article. The daemon picks the directory from `type:` and the filename from `title:`. Will fail if a same-titled article already exists.
- **`update`** — replace an existing article's body. The daemon matches by `title:`. Use this when you've definitively superseded prior content.
- **`append`** — append a timestamped entry to an existing rolling file (learnings.md, decisions-log.md). See "Rolling-target appends" below.

## Optional fields for rolling-target appends

When an insight belongs to a specific project's append-only log:

```yaml
---
scriptorium: true
action: append
title: "Postgres TS Vectors Full-Text Search"  # the host project article (overview)
rolling_target: learnings | decisions-log
type: project
domain: enaia
tags: [postgres]
---

## 2026-05-07 — TOAST sequential scan tax

(your appended entry here)
```

`rolling_target: learnings` appends to `projects/<project>/learnings.md` instead of writing a new article. Same for `decisions-log`. This is how cross-project sessions feed per-project memory without creating new article files.

## Body content

After the closing `---`, write the actual article content. Keep it self-contained — the daemon doesn't fetch sources at absorb time:

- Lead with the fact, decision, or technique.
- Show code when code teaches it. Inline.
- Cross-reference other articles with `[[Title]]` wikilinks (the daemon resolves these against the existing KB).
- Cite external sources with regular markdown links.

## A complete worked example

Suppose you're working in `~/Projects/myapp` and you've just figured out a non-obvious pattern for Postgres connection pooling. The user's KB is named `scriptorium`. Write:

`~/Projects/myapp/.claude/scriptorium/2026-05-07-postgres-pool-pgbouncer-transaction-mode.md`:

```markdown
---
scriptorium: true
action: create
title: "PgBouncer Transaction Mode Pool Sizing"
type: pattern
domain: general
tags: [postgres, pgbouncer, connection-pooling]
---

# PgBouncer Transaction Mode Pool Sizing

When running PgBouncer in transaction-pooling mode, the maximum concurrent
Postgres connections you actually need equals the number of in-flight
*transactions*, not active clients. With N web workers each holding a
client connection but only briefly opening transactions, your DB pool can
be ~N/4 instead of N.

(...rest of the article, including code, gotchas, and `[[Postgres Connection Limits]]` cross-links...)

## Sources

- [pgbouncer docs](https://www.pgbouncer.org/usage.html)
- Production benchmark in `myapp` after migrating to transaction-mode (commit abc1234)
```

Tell the user: *"Filed a drop file at `.claude/scriptorium/2026-05-07-postgres-pool-pgbouncer-transaction-mode.md`. Will land in the KB on the next `scribe sync`."*

## What NOT to file

Don't fabricate drop files for trivial facts. The drop-file pattern is for **reusable, cross-project, non-obvious** knowledge:

- Yes: a tested pattern, a non-obvious decision rationale, a specific bug fix worth reusing, a tool evaluation with a verdict, a research conclusion with sources.
- No: "I just refactored this function." "The tests pass." "Here's what I did today." (Those belong in commit messages, not the KB.)

If the session produced no reusable knowledge, file nothing. The KB is signal-only.
