OUTPUT ONLY ONE JSON OBJECT. NO PROSE. NO CODE FENCES.

Extract knowledge from one directory of project **{{PROJECT}}**.

## Source

- project: {{PROJECT}}
- p_path: {{P_PATH}}
- rel_dir: {{REL_DIR}}
- domain: {{DOMAIN}}
- today: {{TODAY}}

## Files

<<<FILES_BEGIN>>>
{{FILES_CONTENT}}
<<<FILES_END>>>

## Output schema

```json
{
  "version": 2,
  "entity": "deep-{{PROJECT}}-{{REL_DIR}}",
  "actions": [
    {"op": "create", "path": "<wiki-dir>/<slug>.md", "content": "---\nfrontmatter\n---\n\nbody"}
  ],
  "meta": [
    {"op": "log_append", "line": "## [{{TODAY}}] deep | {{PROJECT}}/{{REL_DIR}}: <summary>"}
  ]
}
```

## Rules

- Empty actions legal: `"actions": []`.
- Note failed approaches + why, and known failure conditions, when the source states them — not just the fix.
- Frontmatter: title, type, created: {{TODAY}}, updated: {{TODAY}}, domain: {{DOMAIN}}, confidence (low|medium|high), tags (≥3 kebab-case), related (array of "[[Title]]" strings), sources (array containing "{{P_PATH}}/{{REL_DIR}}").
- Path rooted in wiki/, projects/, decisions/, patterns/, solutions/, tools/, research/, ideas/, people/, sessions/.
- NEVER target a file whose basename starts with `_` (e.g. _index.md, _backlinks.json, _absorb_log.json) — scribe generates these and the executor rejects writes to them. Use `create` for a new file; `append` only for a file you were shown exists.
- ≤150 lines per article.
- log_append ALWAYS included.

- One topic = one article: never two `create` ops with near-identical titles or slugs. If a relevant article is visible in the context you were given, `append` to it instead of creating a parallel page, and reuse its exact title in `related:` wikilinks.
- Generic knowledge that almost certainly has a page already (well-known patterns, common tool facts) gets NO new stub — use rolling_memory_append or drop it. Near-duplicate pages split future updates and corrupt contradiction resolution.

OUTPUT: ONE JSON OBJECT. NO PROSE. NO CODE FENCES.
