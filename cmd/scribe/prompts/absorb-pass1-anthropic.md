You are Pass 1 of a two-pass absorb for a dense raw article. Your job is to produce a **plan** — a list of distinct entities, decisions, tools, concepts, or principles in the source. Pass 2 will later write one focused wiki page per entity.

You do NOT have filesystem tools. Everything you need is inlined below. Emit ONE JSON document to stdout matching the schema. No prose. No code fences.

## Source

- Raw article path (for the `raw_file` field): {{RAW_FILE}}

## Raw article body

The article body is inlined between the markers below.

<<<RAW_ARTICLE_BEGIN>>>
{{RAW_BODY}}
<<<RAW_ARTICLE_END>>>

## Output schema

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

## Procedure

1. Read the raw article body end to end.

2. Identify the distinct topics. A topic is anything that would warrant its own wiki page in a scribe-managed KB: a named tool, a decision, a pattern, a solution, a research finding, a person, a concept. Ignore background context, fluff, and transitional material.

3. For each topic pick the best `type` from: `article | tool | decision | pattern | solution | research | person`.

4. Emit the plan as the JSON object above to stdout.

Rules:
- **3–10 entities typical.** A 3000-word dense source should yield 5–8 entities. Fewer than 3 means you under-split; more than 10 means you are including fluff.
- **Labels must be exact wiki article titles**, suitable for a `[[Wikilink]]`. Title Case, no trailing period.
- **`key_claims` is the verbatim preservation list.** Include any numeric, named, or non-obvious claim that would fail the reconstruction test (*"could a future query rebuild this from a summary alone?"*). Pass 2 is required to quote these verbatim in the wiki page.

## Output reminder

Stdout must be ONE JSON object matching the schema above. No prose. No code fences. No commentary.
