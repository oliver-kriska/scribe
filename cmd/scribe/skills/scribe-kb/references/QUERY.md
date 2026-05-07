# Searching the KB with qmd

`qmd` is the search engine over a scribe-managed KB: BM25 keyword search + vector similarity, hybrid-ranked. Three query types (`lex`, `vec`, `hyde`), one expand mode, plus structural retrieval helpers. qmd works from any directory — the collection uses absolute paths, so you don't need to `cd` into the KB.

## When to use which

| You want to find... | Use |
|---|---|
| An exact phrase or technical term | `lex` (BM25) — fast, exact-match weighted |
| A concept that may be phrased differently across articles | `vec` (vector similarity) |
| Something where you can write a *plausible answer paragraph* but don't know the right keywords | `hyde` (hypothetical document expansion) |
| You're not sure / first attempt | natural-language query, qmd auto-expands |

## Patterns

```bash
# Natural-language (auto-expand): the default, good first attempt
qmd query "how does scribe handle the iMessage chat database"

# Structured: combine multiple search types in one call
qmd query $'lex: postgres ts_vector\nvec: how does full-text search rank results'

# HyDE: write what the answer might look like
qmd query $'hyde: PgBouncer in transaction-pool mode reuses connections after each transaction completes, so the application-side pool can be much smaller than the worker count'

# Exact keyword (no LLM): fastest path
qmd search "fetched_via stub"

# Vector only (no BM25): when keywords miss the concept
qmd vsearch "knowing when to give up on a feature"
```

When using the qmd MCP tool inside Claude Code, every query call accepts a structured `searches` array — pass multiple types in one call instead of running three separate queries.

## Reading results

qmd returns ranked snippets with the absolute file path of each hit. To read the full article, follow up with the Read tool on that path. For long articles, use `qmd get <path>:<line>` to read just a slice, or `scribe sections get <article> <id>` for a specific section.

## Always pass `intent` (MCP tool)

When using the qmd MCP tool, set `intent` to the *user-facing* question you're trying to answer. qmd uses it for snippet selection and reranking — it materially improves result quality, especially for ambiguous queries.

```
intent: "decide whether to use pgvector vs postgres ts_vector for fuzzy product search"
searches: [{type:'lex', query:'pgvector'}, {type:'vec', query:'fuzzy product search ranking'}]
```

## Filtering & follow-ups

```bash
# More results
qmd query "..." -n 20

# Filter weak matches
qmd query "..." --min-score 0.5

# Files-only output (good for scripts)
qmd query "..." --files

# Restrict to one collection (when the user has multiple)
qmd query "..." -c scriptorium
```

## Follow the graph — one hop

qmd returns ranked flat hits; it doesn't traverse `[[wikilinks]]` automatically. If a top result mentions a wikilink that's central to the user's question, fetch that neighbor with `qmd get` before answering. Stop at one hop unless a second is clearly needed — over-expanding bloats context and drifts off-topic.

## Common workflows

**"Have I done this before?"**

```bash
qmd query "<the problem in one sentence> solution"
qmd query "<the problem> decision reasoning"
```

If results turn up a `decisions/` or `solutions/` article, cite it back to the user with the wikilink: *"per [[Postgres Pool Sizing]], you chose X because Y — is that still current?"*

**"Should I use library X?"**

```bash
qmd query "<library> evaluation verdict"
qmd query "alternatives to <library>"
```

`tools/<library>.md` may exist with `verdict: use | evaluate | skip`. Don't recommend something already in `skip`.

**"What do I know about X?"**

```bash
qmd query "X overview"
qmd query "X" -n 20
```

For a topic with broad coverage, return ≤3 most relevant articles by wikilink, then synthesize. Don't dump 20 search results into the user's screen.

## What `qmd` is not

- It's not a graph traversal engine. Use `[[wikilinks]]` + `_backlinks.json` for graph queries.
- It's not a frontmatter filter. To find "all decisions with status: active in domain enaia", grep frontmatter directly or wait for Phase 7B `.scribe-view.yaml` views.
- It's not a real-time index. The user's `scribe sync` cron rebuilds qmd; if you wrote a drop file in this session, qmd hasn't seen it yet.
