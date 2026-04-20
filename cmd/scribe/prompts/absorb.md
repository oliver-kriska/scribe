You are working in {{KB_DIR}}. Read CLAUDE.md for schema conventions.

Absorb this raw article into the wiki: {{RAW_FILE}}

The wiki layer exists for **structure** (typed entity pages, wikilinks, backlinks, confidence scoring, cross-references) — not compression. The raw article stays on disk forever and is chunk-indexed by qmd, so retrieval of verbatim claims is always possible from raw. Your job is to make the source **navigable**, not to summarize it down.

## Procedure

1. **Classify density.** Read the article and classify it:
   - **brief** — short post or status, < {{BRIEF_WORDS}} words with at most {{BRIEF_HEADINGS}} H2/H3 heading. Usually single-topic.
   - **standard** — single-topic essay or tutorial between the brief and dense thresholds.
   - **dense** — research paper, long thread, chapter, evaluation, or multi-topic post. ≥ {{DENSE_WORDS}} words OR ≥ {{DENSE_HEADINGS}} distinct H2/H3 sections OR ≥ 3 clearly independent topics/entities/decisions.

   If the article frontmatter already has a `density:` field, trust it and skip the heuristic.

2. **Classify type** per CLAUDE.md (article | tutorial | tool | reference | paper | research | decision | pattern | solution).

3. **Plan the output based on density:**
   - **brief** — merge into an existing wiki page if a natural home exists (add a bullet / quote / paragraph with wikilinks). Otherwise write a single short stub. Do NOT pad to hit a length target.
   - **standard** — one focused wiki page.
   - **dense** — identify the distinct entities / decisions / tools / concepts / principles in the article (call them "topics"). Plan **one focused wiki page per topic**, each citing the same raw file via `sources:`. A 3000-word paper with 6 distinct ideas should produce 6 pages, not one compressed 400-line summary. This is CLAUDE.md rule 6 (anti-cramming) applied at absorb time.

   Write each page to its type-appropriate directory (tools/, decisions/, patterns/, solutions/, research/, etc.). Sub-articles inherit the source citation.

4. **Verbatim preservation for load-bearing claims.** For any claim in the raw article, apply the reconstruction test:
   > *"If a future query reads only this wiki page, could it reconstruct this specific claim accurately from the summary alone?"*
   If the answer is **no**, preserve the claim verbatim in the wiki page using a markdown blockquote with a source reference. Typical candidates:
   - numeric evidence ("49% reduction in failed retrievals")
   - named decisions ("we chose Postgres over SQLite because …")
   - non-obvious trade-offs or constraints
   - principles quoted verbatim from the author
   - specific commands, config values, API shapes
   Do NOT quote background context that embeddings can trivially surface from raw. Quote only what would be hard to re-derive from a summary.

   Format:
   ```markdown
   > "<exact quote>"
   > — Source: {{RAW_FILE}} (section or paragraph reference if useful)
   ```

5. **Write / update wiki articles** following CLAUDE.md frontmatter conventions. Set `sources:` to include the raw file path. Prefer updating an existing article over creating a near-duplicate — search first with Grep on the topic title and likely wikilinks.

6. **Score `confidence:`** using the Confidence Rubric in CLAUDE.md. Do linguistic forensics on the source: assertive language + primary evidence + action taken → `high`; mixed register + secondary source + evaluated-but-not-committed → `medium`; hedging ("maybe", "probably", "could") + single unverified opinion + speculation → `low`. Default to `medium` when unsure. Do not inflate to look authoritative — dream uses this field as the arbiter when contradictions come up, so miscalibration corrupts later resolution.

7. **Post-write size check.** If any article you wrote exceeds 150 lines, split it per CLAUDE.md rule 9. For dense sources this should rarely trigger because you already split by topic in step 3; if it does, the topic was too broad — split again.

8. **Update `wiki/_index.md` and `wiki/_backlinks.json`.**

9. **Do NOT git commit.**

## Operating mode

You are running non-interactively. Never ask questions — decide and act. If content is thin or unfetchable, create a minimal stub article with what you have rather than asking for clarification.

Remember: the wiki's purpose is navigation + typing + cross-linking. Raw + qmd is the recall layer. Under-summarize over overfit.
