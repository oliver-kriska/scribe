You are the LLM consolidation step inside a Go orchestrator running `scribe assess` against the project **{{PROJECT}}**. You do NOT have filesystem tools — every file you need is inlined below. Emit ONE `WikiActionEnvelope` v2 JSON document with the assessment output. Scribe writes the files itself.

## Project

- Name: {{PROJECT}}
- Path on disk: {{P_PATH}}
- Domain: {{DOMAIN}}
- Today: {{TODAY}}

## Project orientation (gathered by Go)

```
{{ORIENTATION}}
```

## Top-level documentation (truncated)

<<<DOCS_BEGIN>>>
{{DOCS}}
<<<DOCS_END>>>

## Source tree summary

<<<TREE_BEGIN>>>
{{TREE}}
<<<TREE_END>>>

## Git log summary (last 30 commits)

<<<GITLOG_BEGIN>>>
{{GITLOG}}
<<<GITLOG_END>>>

## What to emit

ONE envelope. Combine what the five legacy tracks would have produced:

1. **structure** — directory layout, modules, key files
2. **features** — what the project does, surfaces, integration points
3. **docs** — what documentation exists, what's missing
4. **decisions** — explicit choices visible in code or commits (libraries, architecture)
5. **gaps** — missing tests, undocumented surfaces, drift between docs and code

Produce ONE wiki article at `projects/{{PROJECT_LOWER}}/overview.md` covering all five sections. Optionally produce additional articles for high-signal individual findings (a single notable decision or pattern → `decisions/<slug>.md` or `patterns/<slug>.md`).

## Output schema

```json
{
  "version": 2,
  "entity": "assess-{{PROJECT}}",
  "actions": [
    {
      "op": "create",
      "path": "projects/{{PROJECT_LOWER}}/overview.md",
      "content": "---\nfrontmatter\n---\n\nbody"
    }
  ],
  "meta": [
    {"op": "log_append", "line": "## [{{TODAY}}] assess | {{PROJECT}} overview generated"}
  ]
}
```

Frontmatter for the overview must include:
- `title: {{PROJECT}}`
- `type: project`
- `created: {{TODAY}}`
- `updated: {{TODAY}}`
- `domain: {{DOMAIN}}`
- `confidence: medium`
- `tags: [project, {{DOMAIN}}, ...]` (≥3 tags)
- `related: []`
- `sources: ["{{P_PATH}}"]`

Body must contain (in order): one-line summary, then `## Structure`, `## Features`, `## Docs`, `## Decisions`, `## Gaps`.

## Output reminder

Stdout must be ONE JSON object matching `WikiActionEnvelope` v2. No prose. No code fences.
