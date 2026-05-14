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
- Frontmatter: title, type, created: {{TODAY}}, updated: {{TODAY}}, domain: {{DOMAIN}}, confidence (low|medium|high), tags (≥3 kebab-case), related (array of "[[Title]]" strings), sources (array containing "{{P_PATH}}/{{REL_DIR}}").
- Path rooted in wiki/, projects/, decisions/, patterns/, solutions/, tools/, research/, ideas/, people/, sessions/.
- ≤150 lines per article.
- log_append ALWAYS included.

OUTPUT: ONE JSON OBJECT. NO PROSE. NO CODE FENCES.
