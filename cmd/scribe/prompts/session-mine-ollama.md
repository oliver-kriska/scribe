OUTPUT ONLY ONE JSON OBJECT. NO PROSE. NO CODE FENCES.

Extract reusable knowledge from a coding-agent session transcript. Emit a `WikiActionEnvelope` v2 with wiki articles + meta operations.

## Session

- session_id (echo into meta + `sources:`): {{SESSION_ID}}
- project_path: {{PROJECT_PATH}}

## Related sessions (reference via [[wikilinks]] or `session:<id>`, don't duplicate)

{{RELATED_SESSIONS}}

## Transcript

<<<TRANSCRIPT_BEGIN>>>
{{TRANSCRIPT}}
<<<TRANSCRIPT_END>>>

## What to extract (be ruthless — skip noise)

KEEP: decisions with reasoning, architecture patterns, research findings with numerics, tool evaluations, debug lessons, failed approaches + why (when stated).
SKIP: conversation summaries, routine code changes, transient debug noise.

## Output schema

```json
{
  "version": 2,
  "entity": "short label",
  "actions": [
    {
      "op": "create",
      "path": "decisions/some-decision.md OR patterns/x.md OR ...",
      "content": "---\n<yaml fields>\n---\n\n<body>"
    }
  ],
  "meta": [
    {"op": "sessions_log_append", "session_id": "{{SESSION_ID}}"}
  ]
}
```

## Rules

- ALWAYS include `{"op": "sessions_log_append", "session_id": "{{SESSION_ID}}"}` in meta — even when actions is empty.
- Empty actions is legal: `"actions": []`.
- Path must be rooted in: wiki/, projects/, research/, solutions/, tools/, decisions/, patterns/, ideas/, people/, sessions/.
- NEVER target a file whose basename starts with `_` (e.g. _index.md, _backlinks.json, _absorb_log.json) — scribe generates these and the executor rejects writes to them. Use `create` for a new file; `append` only for a file you were shown exists.
- Slug: lowercase, spaces → `-`, strip punctuation, `.md`.
- Frontmatter required keys: title, type, created: {{TODAY}}, updated: {{TODAY}}, domain, confidence (low|medium|high), tags (≥3 kebab-case), related (array of "[[Title]]" strings), sources (array containing "session:{{SESSION_ID}}").
- ≤150 lines per article. Quote load-bearing claims as markdown blockquotes ending `— Source: session:{{SESSION_ID}}`.
- Optional rolling_memory_append: `{"op": "rolling_memory_append", "domain": "<domain>", "target": "learnings", "content": "one paragraph"}`. Target MUST be `learnings` or `decisions-log`. Domain MUST be one already in scribe.yaml.

- One topic = one article: never two `create` ops with near-identical titles or slugs. If a relevant article is visible in the context you were given, `append` to it instead of creating a parallel page, and reuse its exact title in `related:` wikilinks.
- Generic knowledge that almost certainly has a page already (well-known patterns, common tool facts) gets NO new stub — use rolling_memory_append or drop it. Near-duplicate pages split future updates and corrupt contradiction resolution.

OUTPUT: ONE JSON OBJECT. NO PROSE. NO CODE FENCES.
