OUTPUT ONLY ONE JSON OBJECT. NO PROSE. NO CODE FENCES.

You are absorbing one raw article into a wiki. Emit a `WikiActionEnvelope` with one or more `create` actions, one per distinct topic.

## Source

- Raw article path (echo into `sources:`): {{RAW_FILE}}
- Brief: < {{BRIEF_WORDS}} words AND ≤ {{BRIEF_HEADINGS}} headings
- Dense: ≥ {{DENSE_WORDS}} words OR ≥ {{DENSE_HEADINGS}} headings

## Raw article body

<<<RAW_ARTICLE_BEGIN>>>
{{RAW_BODY}}
<<<RAW_ARTICLE_END>>>

## Output schema

```json
{
  "entity": "short label for logs",
  "actions": [
    {
      "op": "create",
      "path": "wiki/<slug>.md OR <type-dir>/<slug>.md",
      "content": "---\nfrontmatter\n---\n\nbody"
    }
  ]
}
```

## Rules

- Path rooted in one of: wiki/, projects/, research/, solutions/, tools/, decisions/, patterns/, ideas/, people/, sessions/.
- Brief → 1 action. Standard → 1 action. Dense → multiple actions, one per topic.
- Each `content` MUST start with YAML between `---` lines containing:
  - `title:` Title Case
  - `type:` article | tutorial | tool | reference | paper | research | decision | pattern | solution
  - `created: {{TODAY}}`
  - `updated: {{TODAY}}`
  - `domain:` from frontmatter or `general`
  - `confidence:` low | medium | high (default medium)
  - `tags:` array of ≥3 kebab-case tags
  - `related:` array of `"[[Title]]"` strings (brackets INSIDE the string)
  - `sources:` array containing `{{RAW_FILE}}`
- Body: lead with one-line definition, then sections. Quote load-bearing claims verbatim using markdown blockquotes with `— Source: {{RAW_FILE}}`.
- Note failed approaches + why, and known failure conditions, when the source states them — not just the fix.
- ≤150 lines per article. If you need more, split into more `create` actions.

- One topic = one article: never two `create` ops with near-identical titles or slugs. If a natural home for a brief claim exists in an article visible in your context, `append` to it instead of creating a parallel page.
- Do not create a thin stub for knowledge that almost certainly has a page already — fold it into the closest planned page or drop it. Near-duplicate pages split future updates and corrupt contradiction resolution.

OUTPUT: ONE JSON OBJECT. NO PROSE. NO CODE FENCES.
