You are Pass 2 of a two-pass absorb. A planning pass has already listed the entities in the source. Your job is to write or update **one focused wiki page** for a single entity.

Raw article: {{RAW_FILE}}
Plan file (for context): {{PLAN_FILE}}
Entity to write: {{ENTITY_LABEL}}
Entity type: {{ENTITY_TYPE}}
Entity one-line: {{ENTITY_ONE_LINE}}
Entity key claims (verbatim-preserve these): {{ENTITY_KEY_CLAIMS}}
Domain: {{DOMAIN}}

## Atomic facts for this chapter

The block below is the verbatim claim pool extracted from the same chapter that produced this entity. Each line is `[id, type] claim ("anchor")`. The `anchor` is a short substring lifted directly from the source — use it to find the claim's exact wording in {{RAW_FILE}}. When you preserve a claim verbatim (Step 5 below), prefer matching one of these facts and tagging the quote with its `[id]`. If the block is empty, fall back to the legacy reconstruction-test rule.

{{FACTS}}

## Procedure

1. Read {{KB_DIR}}/CLAUDE.md for frontmatter schema and writing standards. Skim the plan JSON at {{PLAN_FILE}} so you know what other entities exist in this source (use wikilinks to them).

2. Read the raw article at {{RAW_FILE}}. Focus on the sections relevant to {{ENTITY_LABEL}}. Skim the rest for context.

3. Grep the wiki for an existing article titled {{ENTITY_LABEL}} (or close variants). If one exists, **update it** — add new information, strengthen claims, add cross-references. If not, **create a new article** in the directory matching the entity type (`patterns/`, `decisions/`, `tools/`, `solutions/`, `research/`, `people/`, `projects/`).

4. Write the article following CLAUDE.md conventions:
   - Required frontmatter: `title`, `type`, `created`, `updated`, `domain`, `confidence`, `tags`, `related`, `sources`.
   - Set `sources:` to include `{{RAW_FILE}}` (absolute path).
   - Set `domain:` to {{DOMAIN}}.
   - Set `related:` to wikilinks for any sibling entities from the plan that belong in cross-reference.
   - Score `confidence:` per CLAUDE.md Confidence Rubric.

5. **Verbatim-preserve the key claims.** For each item in Entity key claims, include the exact wording from the raw article as a markdown blockquote with a source reference. Use this format:

   ```markdown
   > "<exact quote>"
   > — Source: {{RAW_FILE}} [c00-f3]
   ```

   The bracketed token at the end of the source line is the matching fact ID from the atomic facts block above. Include it whenever the quote corresponds to one of those facts; this is what lets a later pass audit the wiki against the source. If a claim isn't in the fact block (e.g. facts pass was off), drop the bracket and use only `— Source: {{RAW_FILE}}`.

   Apply the reconstruction test for any additional claim you summarize: *"could a future query reconstruct this from my summary alone?"* If no, quote it.

6. Keep the article focused on {{ENTITY_LABEL}}. Do NOT attempt to cover other entities from the plan — they will be written by parallel Pass 2 invocations. If another entity is referenced, use a wikilink and move on.

7. Post-write size check: if the article exceeds 150 lines, the topic was too broad — split further into sub-articles before finishing.

8. Do NOT update `wiki/_index.md` or `wiki/_backlinks.json`. Do NOT git commit. A cleanup pass will rebuild indexes after all Pass 2 calls complete.

You are running non-interactively. Never ask questions — decide and act.
