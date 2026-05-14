You are extracting reusable knowledge from one Claude Code session transcript. You do NOT have filesystem tools — the transcript is inlined below. Emit ONE `WikiActionEnvelope` JSON document to stdout. Scribe will apply the file mutations itself.

## Session

- Session ID (echo into `sessions_log_append` meta and into the wiki article's `sources:`): {{SESSION_ID}}
- Project path (informational): {{PROJECT_PATH}}

## Related sessions from the same project (reference via wikilinks or `session:<id>` — do not duplicate their content)

{{RELATED_SESSIONS}}

## Transcript

<<<TRANSCRIPT_BEGIN>>>
{{TRANSCRIPT}}
<<<TRANSCRIPT_END>>>

## What to extract

Pull only the **non-obvious, reusable** knowledge:
- decisions with reasoning (named tradeoffs, alternatives considered)
- architecture patterns that apply across projects
- research findings with data (numerics, benchmarks, measured outcomes)
- tool evaluations (which one, why, what alternatives)
- lessons learned (debug discoveries, surprising behaviors)

**Skip:** conversation summaries, routine code changes, transient debug noise, tool-call mechanics.

## Output schema

Emit EXACTLY ONE JSON object to stdout. No prose. No code fences.

```json
{
  "version": 2,
  "entity": "<short label for logs>",
  "actions": [
    {
      "op": "create",
      "path": "<wiki-dir>/<slug>.md",
      "content": "<full file contents with YAML frontmatter>"
    }
  ],
  "meta": [
    {
      "op": "sessions_log_append",
      "session_id": "{{SESSION_ID}}"
    },
    {
      "op": "rolling_memory_append",
      "domain": "<domain>",
      "target": "learnings",
      "content": "<one paragraph>"
    }
  ]
}
```

### Action ops

- `create` — full file write. Frontmatter must include `title`, `type`, `created: {{TODAY}}`, `updated: {{TODAY}}`, `domain`, `confidence`, `tags` (≥3 kebab-case), `related` (array of `"[[Title]]"` strings), `sources` (array containing `session:{{SESSION_ID}}`).
- `append` — add to existing file. Caller-supplied leading newline.
- `replace_section` — swap body of `## <heading>`. Field: `heading`.

### Meta ops (Phase 4C side channel — scribe-controlled paths)

- `sessions_log_append` — **always emit one** with `session_id: "{{SESSION_ID}}"`. Marks the session as processed in `wiki/_sessions_log.json`.
- `rolling_memory_append` — optional. Appends a paragraph to `projects/<domain>/<target>.md`. `target` must be `"learnings"` or `"decisions-log"`. Use this for cross-project insights that don't warrant their own wiki article.

### Path rules

- Wiki dirs only: `wiki/`, `projects/`, `research/`, `solutions/`, `tools/`, `decisions/`, `patterns/`, `ideas/`, `people/`, `sessions/`.
- Pick the directory matching the entity type.
- Slugify: lowercase, spaces → `-`, strip punctuation, `.md`.

## Rules

- **Always emit a `sessions_log_append` meta op** even when actions is empty. Without it scribe will re-process this session next run.
- An empty actions list is legal — emit `"actions": []` if the transcript had no extractable knowledge.
- Stay under 150 lines per article.
- Quote load-bearing claims verbatim with markdown blockquote + `— Source: session:{{SESSION_ID}}`.

## Output reminder

Stdout must be ONE JSON object matching `WikiActionEnvelope` v2. No prose. No code fences.
