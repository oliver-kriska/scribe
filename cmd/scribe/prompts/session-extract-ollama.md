OUTPUT ONLY ONE JSON OBJECT. NO PROSE. NO CODE FENCES.

Extract project-specific knowledge from one Claude Code session for the project at `{{PROJECT_PATH}}`.

## Session

- session_ids: {{SESSION_ID_LIST}}
- project_path: {{PROJECT_PATH}}

## Related sessions (don't duplicate)

{{RELATED_SESSIONS}}

## Transcript

<<<TRANSCRIPT_BEGIN>>>
{{TRANSCRIPT}}
<<<TRANSCRIPT_END>>>

## Output schema (v2 envelope)

```json
{
  "version": 2,
  "entity": "short label",
  "actions": [
    {
      "op": "create",
      "path": "decisions/<slug>.md",
      "content": "---\nfrontmatter\n---\n\nbody"
    }
  ],
  "meta": [
    {"op": "sessions_log_append", "session_id": "<sid>"}
  ]
}
```

## Rules

- Emit one sessions_log_append per session id.
- Empty actions list is legal: `"actions": []`.
- Frontmatter required keys: title, type, created: {{TODAY}}, updated: {{TODAY}}, domain: general, confidence (low|medium|high), tags (≥3 kebab-case), related (array of "[[Title]]" strings), sources (array containing "session:<id>").
- Allowed `type` values (exactly one): decision, pattern, solution, research, tool, project, idea, person. The directory MUST match the type — decisions/→decision, patterns/→pattern, solutions/→solution, research/→research, tools/→tool, ideas/→idea, people/→person.
- Path rooted in: research/, solutions/, tools/, decisions/, patterns/, ideas/, people/. Pick the directory whose type fits the entity.
- NEVER target a file whose basename starts with `_` (e.g. _index.md, _backlinks.json, _absorb_log.json) — scribe generates these and the executor rejects writes to them. Use `create` for a new file; `append` only for a file you were shown exists.
- ≤150 lines per article.
- Optional rolling_memory_append: `{"op": "rolling_memory_append", "domain": "general", "target": "learnings", "content": "one paragraph"}`. Target MUST be `learnings` or `decisions-log`.

OUTPUT: ONE JSON OBJECT. NO PROSE. NO CODE FENCES.
