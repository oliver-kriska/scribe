You are working in {{KB_DIR}}. Read CLAUDE.md for the full Dream protocol and writing standards.

Execute all 4 phases of the Dream cycle. Today is {{DATE}}.

## Phase 1 — ORIENT (read-only, no edits)

Read wiki/_index.md to understand what exists. Scan 10-15 articles across different
directories (projects/, research/, solutions/, tools/, decisions/, patterns/).
Read the last 20 entries of log.md. Form hypotheses about what feels stale,
contradictory, or missing. List your hypotheses before proceeding.

## Phase 2 — SIGNAL (read-only, targeted evidence gathering)

For each hypothesis from Phase 1, gather narrow, targeted evidence:
- Compare claims across articles sharing the same tags or domain
- Check if 'updated:' dates are >90 days old
- Look for articles that reference removed or renamed concepts
- Confirm or reject each hypothesis with evidence

### Phase 2.5 — Contradiction triage (dedicated pass)

Separately from the hypothesis work, run an explicit contradiction sweep. For every pair of articles that make a direct claim on the same subject (same concrete noun — a named tool, pattern, decision, or library), classify the pair using the `confidence:` field declared in each article's frontmatter:

- **Solid-vs-Solid** (`high` vs. `high`) — a genuine contradiction between two well-grounded claims. Treat as a major finding. Both articles looked right to their author at the time, so reality changed OR one author was wrong about their evidence. Do not silently overwrite either side. These are the contradictions to prioritize in Phase 3.
- **Solid-vs-Vague** (`high` vs. `medium`/`low`) — the vague side almost certainly loses. Schedule the vague claim for correction or removal in Phase 3.
- **Vague-vs-Vague** (`medium`/`low` vs. `medium`/`low`) — low signal. Note it but do not resolve unless a newer source has arrived. Speculation disagreeing with speculation is not a KB problem.

List every contradiction you found with its classification before proceeding to Phase 3. Target the Solid-vs-Solid cases first; they are where the KB is genuinely wrong.

## Phase 3 — CONSOLIDATE (the core work)

- Fix contradictions at source using the classification from Phase 2.5:
  - **Solid-vs-Vague:** rewrite or remove the vague claim. The high-confidence article wins.
  - **Solid-vs-Solid:** this is a Rule 11 + Rule 12 case. Determine which claim reflects current reality (prefer newer `updated:`, prefer primary sources in `sources:`, prefer the project closer to the subject). Rewrite the losing article in place to match reality, and append a `Status: reconsidered YYYY-MM-DD — superseded claim "X" with "Y" after dream contradiction pass` line at the top of the rewritten article. Do not delete the history, do not carry both versions in the same article.
  - **Vague-vs-Vague:** skip unless a primary source has appeared that settles it. Do not pick a winner between two speculations.
- Check projects/*/learnings.md and projects/*/decisions-log.md for insights worth promoting to standalone wiki articles (apply concrete noun test: 'X is a ___')
- Normalize all relative dates to absolute dates (replace 'last week', 'recently', 'soon' with actual dates where known)
- Remove stale claims where the source is >90 days old and cannot be verified
- Apply under-write guard: do not promote one-off observations or emotionally vivid but low-signal events into durable knowledge
- Update 'updated:' dates on every article you modify to today's date
- Verify that 'sources:' fields still point to files/URLs that exist

## Phase 3.5 — BREAKDOWN (entity emergence)

Scan all articles for entity names (tools, patterns, people, concepts) that appear
in 3 or more articles but do not have their own wiki page. For each:
- Count how many articles reference it
- If referenced in 3+ articles: create a stub article with proper frontmatter,
  a 2-3 sentence summary synthesized from the referencing articles, and wikilinks
  back to the sources. Apply the concrete noun test: 'X is a ___'. If it fails
  the test, skip it.
- If referenced in 2 articles: note it in the dream log as 'emerging entity' but
  do not create a page yet
- Do not create stubs for generic terms (e.g., 'database', 'deployment', 'testing')
  — only for specific named entities that carry project-specific meaning

## Phase 4 — PRUNE AND INDEX

- Remove entries from _index.md that no longer exist on disk
- Flag (do not delete) articles with zero inbound links AND zero outbound wikilinks AND updated >60 days ago — add a comment '<!-- decay-candidate {{DATE}} -->' to the article
- For articles already flagged as decay candidates in a previous dream: if still zero links and >30 additional days have passed, delete them
- Verify every entry in _index.md has a matching file on disk
- Rebuild wiki/_backlinks.json from scratch by scanning all wikilinks in all articles
- Update article_count in _index.md frontmatter

Log everything you did in log.md using this format:
## [{{DATE}}] dream | One-line summary
- Detail of what was fixed/promoted/pruned
- Statistics: articles touched, contradictions fixed, candidates flagged

Do NOT git commit. Do NOT touch raw/, scripts/, or project source code.
Do NOT create new articles unless promoting from rolling memory (learnings.md → wiki).
