You are Pass 1 of a two-pass absorb for a dense raw article. Your job is to produce a **plan** — a list of distinct entities, decisions, tools, concepts, or principles in the source. Pass 2 will later write one focused wiki page per entity.

Raw article: {{RAW_FILE}}
Plan output path: {{PLAN_FILE}}

## Procedure

1. Read the raw article end to end.

2. Identify the distinct topics. A topic is anything that would warrant its own wiki page in a scribe-managed KB: a named tool, a decision, a pattern, a solution, a research finding, a person, a concept. Ignore background context, fluff, and transitional material.

3. For each topic pick the best `type` from: `article | tool | decision | pattern | solution | research | person`.

4. Write the plan as JSON to {{PLAN_FILE}} using this exact schema:

```json
{
  "raw_file": "{{RAW_FILE}}",
  "source_title": "<exact title from the raw article's frontmatter>",
  "domain": "<domain from frontmatter, or general>",
  "entities": [
    {
      "label": "Proposed Wiki Article Title",
      "type": "pattern",
      "one_line": "Single-sentence hook that orients the Pass 2 writer.",
      "key_claims": [
        "non-obvious claim 1 that Pass 2 must preserve",
        "numeric or named decision 2"
      ]
    }
  ]
}
```

Rules:
- **3–10 entities typical.** A 3000-word dense source should yield 5–8 entities. Fewer than 3 means you under-split; more than 10 means you are including fluff.
- **Dedupe against existing wiki articles first.** Before proposing an entity, Grep the wiki for the proposed label (and close variants). If an article already exists, set `"label"` to its exact title so Pass 2 updates it instead of creating a duplicate.
- **Labels must be exact wiki article titles**, suitable for a `[[Wikilink]]`. Title Case, no trailing period.
- **`key_claims` is the verbatim preservation list.** Include any numeric, named, or non-obvious claim that would fail the reconstruction test (*"could a future query rebuild this from a summary alone?"*). Pass 2 is required to quote these verbatim in the wiki page.

5. Do not write any wiki articles. Do not touch `_index.md` or `_backlinks.json`. Only write the plan JSON.

You are running non-interactively. Never ask questions — decide and act.
