OUTPUT ONLY ONE JSON OBJECT. NO PROSE. NO CODE FENCES.

Extract project-specific knowledge from one Claude Code session for **{{PROJECT}}**.

## Session

- session_ids: {{SESSION_ID_LIST}}
- project_path: {{P_PATH}}
- domain: {{DOMAIN}}

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
      "path": "projects/{{DOMAIN}}/<slug>.md",
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
- Frontmatter required keys: title, type, created: {{TODAY}}, updated: {{TODAY}}, domain: {{DOMAIN}}, confidence (low|medium|high), tags (≥3 kebab-case), related (array of "[[Title]]" strings), sources (array containing "session:<id>").
- Path rooted in: wiki/, projects/, research/, solutions/, tools/, decisions/, patterns/, ideas/, people/, sessions/.
- ≤150 lines per article.
- Optional rolling_memory_append: `{"op": "rolling_memory_append", "domain": "{{DOMAIN}}", "target": "learnings", "content": "one paragraph"}`. Target MUST be `learnings` or `decisions-log`.

OUTPUT: ONE JSON OBJECT. NO PROSE. NO CODE FENCES.
