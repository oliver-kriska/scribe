OUTPUT ONLY ONE JSON OBJECT. NO PROSE. NO CODE FENCES.

Assess project **{{PROJECT}}** and produce a wiki overview as a single envelope.

## Project

- name: {{PROJECT}}
- path: {{P_PATH}}
- domain: {{DOMAIN}}
- today: {{TODAY}}

## Orientation (Go pre-gathered)

```
{{ORIENTATION}}
```

## Docs (top-level, truncated)

<<<DOCS_BEGIN>>>
{{DOCS}}
<<<DOCS_END>>>

## Source tree

<<<TREE_BEGIN>>>
{{TREE}}
<<<TREE_END>>>

## Recent git log

<<<GITLOG_BEGIN>>>
{{GITLOG}}
<<<GITLOG_END>>>

## Output schema (one envelope)

```json
{
  "version": 2,
  "entity": "assess-{{PROJECT}}",
  "actions": [
    {
      "op": "create",
      "path": "projects/{{PROJECT_LOWER}}/overview.md",
      "content": "---\ntitle: {{PROJECT}}\ntype: project\ncreated: {{TODAY}}\nupdated: {{TODAY}}\ndomain: {{DOMAIN}}\nconfidence: medium\ntags: [project, {{DOMAIN}}]\nrelated: []\nsources: [\"{{P_PATH}}\"]\n---\n\nOne-line summary.\n\n## Structure\n\n...\n\n## Features\n\n...\n\n## Docs\n\n...\n\n## Decisions\n\n...\n\n## Gaps\n\n..."
    }
  ],
  "meta": [
    {"op": "log_append", "line": "## [{{TODAY}}] assess | {{PROJECT}} overview generated"}
  ]
}
```

## Rules

- Single overview article. Sections: Structure, Features, Docs, Decisions, Gaps.
- Each section ≤30 lines. Total ≤150 lines.
- Frontmatter keys: title, type=project, created, updated, domain, confidence, tags (≥3), related, sources.
- Path rooted in projects/<project>/. Slug lowercase.
- NEVER target a file whose basename starts with `_` (e.g. _index.md, _backlinks.json, _absorb_log.json) — scribe generates these and the executor rejects writes to them. Use `create` for a new file; `append` only for a file you were shown exists.
- log_append ALWAYS included.

OUTPUT: ONE JSON OBJECT. NO PROSE. NO CODE FENCES.
