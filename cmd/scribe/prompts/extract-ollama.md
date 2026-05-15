OUTPUT ONLY ONE JSON OBJECT. NO PROSE. NO CODE FENCES.

Extract reusable knowledge from project **{{PROJECT}}** (domain `{{DOMAIN}}`) into the knowledge base at `{{KB_DIR}}`.

## Source

- project: {{PROJECT}}
- p_path: {{P_PATH}}
- domain: {{DOMAIN}}
- today: {{TODAY}}

## Files (gathered by Go — read these only)

<<<FILES_BEGIN>>>
{{FILES_CONTENT}}
<<<FILES_END>>>

## Wiki schema rules

- Frontmatter (required on every `create`): `title`, `type`, `created: {{TODAY}}`, `updated: {{TODAY}}`, `domain: {{DOMAIN}}`, `confidence` (low|medium|high), `tags` (≥3 kebab-case), `related` (array of `"[[Title]]"` strings, can be `[]`), `sources` (array containing at least one path under `{{P_PATH}}`).
- Allowed `type` values: `decision`, `pattern`, `solution`, `research`, `tool`, `project`, `idea`, `person`.
- Path roots: `wiki/`, `projects/`, `decisions/`, `patterns/`, `solutions/`, `tools/`, `research/`, `ideas/`, `people/`, `sessions/`.
- Project articles go in `projects/{lowercase-name}/overview.md`, never `projects/Name.md`.
- ≤150 lines per article body.
- `confidence: high` only for assertive, code-backed, deployed claims. `medium` for evaluated-but-not-shipped. `low` for hedging or single-opinion sources.

## Output schema

```json
{
  "version": 2,
  "entity": "extract-{{PROJECT}}",
  "actions": [
    {"op": "create", "path": "<wiki-dir>/<slug>.md", "content": "---\n<frontmatter>\n---\n\n<body>"}
  ],
  "meta": [
    {"op": "log_append", "line": "## [{{TODAY}}] extract | {{PROJECT}}: <one-line summary of what landed>"},
    {"op": "rolling_memory_append", "domain": "{{DOMAIN}}", "target": "learnings", "entry": "## {{TODAY}} | <one-line insight>\n\nContext: <2-3 sentences>\nSource: {{P_PATH}}/<file>"}
  ]
}
```

## Rules

- Empty actions are legal: `"actions": []` when the files contain no new reusable knowledge.
- `log_append` ALWAYS included.
- `rolling_memory_append` only when there's a genuine new learning or decision (not a summary of existing wiki content). Valid `target` values: `learnings`, `decisions-log`.
- Do NOT extract file summaries, code listings, or test scaffolding — extract the non-obvious *reusable* knowledge only.
- One topic per article. If a file mentions three distinct decisions, that's three `create` actions, not one combined.
- NEVER target a file whose basename starts with `_` (e.g. _index.md, _backlinks.json, _absorb_log.json) — scribe generates these and the executor rejects writes to them. Use `create` for a new file; `append` only for a file you were shown exists.

OUTPUT: ONE JSON OBJECT. NO PROSE. NO CODE FENCES.
