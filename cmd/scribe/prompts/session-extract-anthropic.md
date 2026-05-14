You are extracting knowledge from one Claude Code session for project **{{PROJECT}}** ({{P_PATH}}). You do NOT have filesystem tools — the transcript is inlined below. Emit ONE `WikiActionEnvelope` v2 JSON document to stdout.

## Session

- Session IDs (echo into meta and `sources:`): {{SESSION_ID_LIST}}
- Project domain: {{DOMAIN}}

## Related sessions from this project

{{RELATED_SESSIONS}}

## Transcript

<<<TRANSCRIPT_BEGIN>>>
{{TRANSCRIPT}}
<<<TRANSCRIPT_END>>>

## What to extract

- Project-specific decisions, patterns, learnings discovered in this session.
- Architecture or tool choices unique to **{{PROJECT}}**.
- Bugs found + their root causes.

**Skip** conversation summaries and routine code changes.

## Output schema

```json
{
  "version": 2,
  "entity": "short label",
  "actions": [
    {
      "op": "create",
      "path": "projects/{{DOMAIN}}/<slug>.md",
      "content": "---\nfrontmatter\n---\n\nbody"
    }
  ],
  "meta": [
    {"op": "sessions_log_append", "session_id": "<sid>"},
    {"op": "rolling_memory_append", "domain": "{{DOMAIN}}", "target": "learnings", "content": "..."}
  ]
}
```

## Rules

- Always emit one `sessions_log_append` per session id in `SESSION_ID_LIST`.
- Frontmatter: `title`, `type`, `created: {{TODAY}}`, `updated: {{TODAY}}`, `domain: {{DOMAIN}}`, `confidence`, `tags` (≥3), `related` (array of `"[[Title]]"` strings), `sources` (array containing `session:<id>`).
- ≤150 lines per article. Quote claims with `> "..."\n> — Source: session:<id>`.
- For rolling_memory_append, target must be `learnings` or `decisions-log`.

## Output reminder

Stdout must be ONE JSON object matching `WikiActionEnvelope` v2. No prose. No code fences.
