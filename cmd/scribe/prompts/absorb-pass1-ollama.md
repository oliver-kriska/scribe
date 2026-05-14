OUTPUT ONLY ONE JSON OBJECT. NO PROSE. NO CODE FENCES.

You are Pass 1 of a two-pass absorb. Read the raw article body and produce a JSON plan listing 3–10 distinct entities (tools, decisions, patterns, solutions, research findings, people, concepts) that each warrant their own wiki page.

## Raw article path (echo into the `raw_file` field)

{{RAW_FILE}}

## Raw article body

<<<RAW_ARTICLE_BEGIN>>>
{{RAW_BODY}}
<<<RAW_ARTICLE_END>>>

## Output schema (emit exactly this shape)

```json
{
  "raw_file": "{{RAW_FILE}}",
  "source_title": "<title from frontmatter>",
  "domain": "<domain from frontmatter or 'general'>",
  "entities": [
    {
      "label": "Wiki Article Title (Title Case, no period)",
      "type": "article|tool|decision|pattern|solution|research|person",
      "one_line": "One sentence orienting Pass 2.",
      "key_claims": [
        "verbatim non-obvious claim 1",
        "numeric or named claim 2"
      ]
    }
  ]
}
```

## Rules

- 3–10 entities total. Fewer than 3 = under-split. More than 10 = noise.
- `label` must be Title Case wiki article titles (no trailing period, no quotes).
- `type` must be exactly one of: article, tool, decision, pattern, solution, research, person.
- `key_claims` must contain verbatim non-obvious facts Pass 2 will quote. Numerics, named decisions, dates, model names — preserve exact wording.
- Ignore fluff, transitions, background context, anecdotes.

OUTPUT: ONE JSON OBJECT. NO PROSE. NO CODE FENCES. THE OBJECT IS THE ENTIRE RESPONSE.
