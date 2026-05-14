OUTPUT ONLY ONE JSON OBJECT. NO PROSE. NO CODE FENCES.

You are the LLM consolidation step inside Go's weekly Dream orchestrator. Emit one `WikiActionEnvelope` v2.

## Today

{{TODAY}}

## Recent log

<<<LOG_BEGIN>>>
{{LOG_TAIL}}
<<<LOG_END>>>

## Article inventory (sampled)

<<<INVENTORY_BEGIN>>>
{{INVENTORY}}
<<<INVENTORY_END>>>

## Stale candidates (zero links, updated >60 days ago)

<<<STALE_BEGIN>>>
{{STALE}}
<<<STALE_END>>>

## Contradiction candidates (pre-filtered by Go)

<<<CONTRADICTIONS_BEGIN>>>
{{CONTRADICTIONS}}
<<<CONTRADICTIONS_END>>>

## Output schema (v2 envelope)

```json
{
  "version": 2,
  "entity": "dream-{{TODAY}}",
  "actions": [
    {"op": "update_frontmatter", "path": "<file>", "frontmatter": {"updated": "{{TODAY}}"}},
    {"op": "replace_section", "path": "<file>", "heading": "<heading>", "content": "<body>"},
    {"op": "create", "path": "<dir>/<slug>.md", "content": "---\nfrontmatter\n---\n\nbody"},
    {"op": "append", "path": "<file>", "content": "\n<!-- decay-candidate {{TODAY}} -->\n"}
  ],
  "meta": [
    {"op": "log_append", "line": "## [{{TODAY}}] dream | <one-line summary>"}
  ]
}
```

## Rules

- ALWAYS include one `log_append` in meta, even when actions is empty (`"actions": []`).
- Path rooted in: wiki/, projects/, research/, solutions/, tools/, decisions/, patterns/, ideas/, people/, sessions/.
- Use `update_frontmatter` for date bumps (cheapest action).
- Use `append` for decay markers — content should be `"\n<!-- decay-candidate {{TODAY}} -->\n"`.
- Use `replace_section` to swap a body without rewriting frontmatter.
- Use `create` for stub articles (entity referenced in 3+ articles but no wiki page yet).
- DO NOT touch wiki/_index.md or wiki/_backlinks.json — Go rebuilds them.
- Be conservative: if you're unsure, emit `"actions": []` and a log_append explaining "no changes warranted".

OUTPUT: ONE JSON OBJECT. NO PROSE. NO CODE FENCES.
