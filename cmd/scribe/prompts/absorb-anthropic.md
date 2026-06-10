You are running an absorb pass on one raw article. You do NOT have filesystem tools — the article body is inlined below. Emit ONE `WikiActionEnvelope` JSON document to stdout describing every file you want scribe to write or update. Scribe applies those mutations itself.

## Source

- Raw article path (for `sources:` frontmatter): {{RAW_FILE}}
- Brief threshold: < {{BRIEF_WORDS}} words AND ≤ {{BRIEF_HEADINGS}} H2/H3
- Dense threshold: ≥ {{DENSE_WORDS}} words OR ≥ {{DENSE_HEADINGS}} H2/H3 sections

## Raw article body

<<<RAW_ARTICLE_BEGIN>>>
{{RAW_BODY}}
<<<RAW_ARTICLE_END>>>

## Output schema

Emit EXACTLY ONE JSON object to stdout. No prose before or after. No markdown fences.

```json
{
  "entity": "<short label for logs, optional>",
  "notes": "<freeform commentary, optional>",
  "actions": [
    {
      "op": "create",
      "path": "<wiki-dir>/<slug>.md",
      "content": "<full file contents including frontmatter>"
    }
  ]
}
```

### Supported ops

- `create` — full file write. Overwrites are allowed.
- `append` — add lines to an existing file. Caller-supplied leading newline.
- `replace_section` — swap the body of `## <heading>` until the next H2 or EOF. Field: `heading`.
- `update_frontmatter` — merge keys into existing frontmatter. Field: `frontmatter`.

### Path rules

- Paths are relative to KB root, rooted in one of: `wiki/`, `projects/`, `research/`, `solutions/`, `tools/`, `decisions/`, `patterns/`, `ideas/`, `people/`, `sessions/`.
- Pick the directory matching the entity type. Fall back to `wiki/`.
- Slugify the title: lowercase, spaces → `-`, strip punctuation, `.md` suffix.

## Procedure

1. **Classify density.** brief / standard / dense per the thresholds above.

2. **Plan the output:**
   - **brief** — one short article OR an `append`/`replace_section` into an existing wiki page if there's a natural home.
   - **standard** — one focused article (`op: "create"`).
   - **dense** — multiple `create` actions, one per distinct topic. A 3000-word paper with 6 distinct ideas → 6 articles.

3. **Frontmatter (every `create` content must start with YAML between `---` lines):**
   - `title`, `type`, `created: {{TODAY}}`, `updated: {{TODAY}}`, `domain`, `confidence` (low|medium|high), `tags` (≥3, kebab-case), `related` (array of `"[[Title]]"` strings, brackets inside the string), `sources: [{{RAW_FILE}}]`.

4. **Verbatim preservation.** For load-bearing claims (numerics, named decisions, non-obvious trade-offs, exact configs):
   ```markdown
   > "<exact quote>"
   > — Source: {{RAW_FILE}}
   ```
   Apply the reconstruction test: if a future query reading only the wiki page couldn't reconstruct the claim, quote it verbatim.

5. **Cross-reference** other articles with `[[Wikilinks]]`. Do not write `wiki/_index.md` or `wiki/_backlinks.json` — those are rebuilt afterward.

6. **Confidence:** assertive language + primary evidence → `high`; mixed → `medium`; hedging/speculation → `low`. Default `medium`.

7. **Article size:** stay under 150 lines per article. If you exceed it, the topic was too broad — split into more `create` actions.

## Avoid duplicates

- One topic = one article: never emit two `create` actions with near-identical titles or slugs.
- If a natural home for a brief claim exists in an article visible in your context, prefer `append`/`replace_section` over creating a parallel page, and reuse its exact title in `[[Wikilinks]]`.
- Do not create a thin stub for knowledge that almost certainly has a page already — fold it into the closest planned page or drop it. A near-duplicate page splits future updates across files and corrupts contradiction resolution.

## Output reminder

Stdout must be ONE JSON object matching `WikiActionEnvelope`. No prose. No code fences.
