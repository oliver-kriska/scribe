---
name: scribe-kb-tidy
description: Clean up a scribe knowledge base — split bloated articles, expand or merge thin stubs, archive overgrown rolling files, and merge KB-self-named directories. Use when `scribe lint` reports content-quality warnings (bloated article, thin article, rolling file overgrown, directory named after the KB), or when the user asks to tidy, prune, split, archive, or de-clutter their KB / scriptorium.
---

# scribe KB tidy — agent skill

`scribe lint` flags four content-quality problems it **cannot** fix mechanically
because each needs judgment: **bloated** articles (split), **thin** stubs
(expand or merge), **overgrown rolling files** (archive), and **directories
named after the KB** (merge). This skill is the procedure for working that
queue — the fixes `scribe lint --fix` deliberately never attempts.

This is destructive-adjacent work: you rewrite, split, and move real curated
knowledge. The golden rules below are not optional.

**Trust map (see `references/FIELD_NOTES.md` for the evidence).** Two of these
classes have a battle-tested procedure from real KB cleanups — reuse it.
The other two have never actually been hand-executed; this skill is their *first*
written procedure, grounded in the KB's stated principles.

| Class | Status | How to move |
|---|---|---|
| Rolling archive | **Proven** — done on ~12 real projects | Reuse the recipe below |
| Self-named dir | **Proven triage**, but merge is propose-only | Warn, distinguish, propose |
| Bloated split | **Greenfield** — never hand-done | Propose split points first |
| Thin expand/merge | **Greenfield** — never hand-done | Propose the target first |

## Golden rules (read before touching anything)

1. **Mechanical = safe; judgment = propose first.** Deterministic edits (moving
   whole rolling entries, backfilling frontmatter) you can just make — git is
   the safety net. Anything that decides *which page wins* or *which sentences
   fold in* (merges, splits) you show to the user before writing. Never guess a
   merge.
2. **The worklist comes from `scribe lint -v`.** Run it first; it lists every
   flagged file with its line count. Never guess what's flagged.
3. **One class at a time, smallest blast radius first.** Do all the rolling
   archives, re-lint, commit; then thin; then bloated; then self-named dirs.
   Mixing classes in one pass makes a bad edit hard to isolate.
4. **Never lose information.** Every claim, quote, `> — Source:` line,
   `session:UUID`/`[[wikilink]]` provenance, and `sources:` entry must survive.
   Splitting/merging redistributes content; it never deletes it. Superseded
   content stays with a `Status: superseded by [[X]]` note (the KB is
   append-only), not hard-deleted.
5. **Preserve frontmatter validity — including `authority`/`index_tier`.** Every
   new or rewritten file needs a valid frontmatter block (see the **scribe-kb**
   skill's `references/FRONTMATTER.md`). Live files carry `authority:` and
   `index_tier:` even though the schema doc omits them — copy them across;
   dropping them trips the `index_tier missing` warning. New sub-articles
   inherit `type`/`domain` from the parent, get their own `title`, fresh
   `created`/`updated`, and a copy of the parent's `sources:`.
6. **Verify with `scribe lint` after each class.** The flagged count for that
   class must drop and no new errors may appear. If the count didn't move, your
   edit didn't land — investigate, don't retry blindly (see the convergence
   trap in `references/FIELD_NOTES.md`).
7. **Let the KB commit itself in small batches.** Every edit is git-recoverable;
   that is your safety net, so commit per-class. Never force-push or rewrite
   history, and never run `scribe sync`/`dream` (they take a machine lock and
   are the daemon's job) — this skill only edits files and runs `lint`.

## Class 1 — rolling file overgrown (proven; do this first, highest impact)

**Signal:** a `rolling: true` file (`learnings.md`, `decisions-log.md`,
`performance-observations.md`) past threshold. The warning *fires* above 200
lines, but the convention is **archive at 150** — target 150 so the live file
stays comfortably under. These load into agent context every session, so an
oversized one is the most expensive problem to leave.

Rolling files are dated entries, **newest at the top**:

```
## YYYY-MM-DD | one-line summary
Context / insight paragraph(s).
Source: [[Article]] or session:UUID
---
```

**Procedure (battle-tested):**
1. Read the file. Keep the **newest** entries at the top of the live file;
   select the **oldest (bottom)** entries to move out, leaving the live file
   under ~150 lines.
2. Move whole entries only — never split an entry across the boundary. Append
   them (chronological within the file) to a sibling archive in the **same
   directory**. Naming: `learnings.md` → `learnings-archive-YYYY.md`, but
   `decisions-log.md` → `decisions-archive-YYYY.md` (**the `-log` is dropped**).
   Match any archive sibling that already exists.
3. Give the archive full frontmatter copied from the live file — keep
   `rolling: true`, `authority: contextual`, `index_tier: deep` — plus a
   `tags: [..., archive]` entry and `sources: ["<path to the live file>"]`.
4. Optionally add a `> Older entries archived to [[{base}-archive-YYYY]].`
   pointer to the live file (not consistently maintained in practice — nice, not
   required).
5. Re-lint: the live file drops under threshold; `*-archive-*` files are exempt
   from the size warning (they rotate by year, not size). Don't "fix" a rolling
   file's orphan status by linking it — rolling files are legitimately orphaned.

## Class 2 — thin article (<15 lines) — greenfield; propose the target first

**Signal:** `thin article (N lines, minimum 15)`. Three outcomes — **propose
which one, and the target page, before editing:**

- **Merge (default for a real-but-tiny topic):** `qmd query` for a **peer**
  article that should own this content — never merge a stub into a rolling or
  `*-archive-*` file (a 14-line note is not "the same article" as a 400-line
  log; that mistake produced tens of thousands of false pairs in past tuning).
  Fold the substance in, add the thin file's `title` to the host's `aliases:`
  so inbound `[[wikilinks]]` still resolve, move its `sources:` over, then remove
  the thin file. Verify no other article links to the removed title.
- **Expand:** if the topic deserves its own page and has real substance in its
  `sources:`, add the missing context (what/why/how, what was tried and
  rejected) until it clears 15 lines honestly. **Never pad with filler** to hit
  the count — a padded page is worse than a merged one, and fabricated content
  corrupts the KB.
- **Delete:** only for a true empty scaffold, greeting, or exact duplicate with
  nothing unique — and only after checking inbound links.

## Class 3 — bloated article (>150 lines) — greenfield; propose split points first

**Signal:** `bloated article (N lines, should split at 150)`. Read the spread:
151–170 is marginal — a single coherent, well-structured 160-line reference
beats two thin fragments, so leaving it (or tightening prose) is often right.
200+ usually glues several topics together.

**Procedure (first written recipe — move deliberately, propose before writing):**
1. `scribe sections list <article>` to see the H2/H3 structure — the seams.
2. Decide: **one long topic** (leave it or tighten) or **several topics glued
   together** (split). Use the KB's per-type length sense (Decision 40–70,
   Pattern/Solution 40–80, Project 60–100, Research 80–150) as a feel for
   "natural length," not a hard rule.
3. Show the user the proposed split — which H2 sections become which new
   pages — before writing.
4. To split: create one focused sub-article per major section under the **same
   directory**, each with valid frontmatter (own `title`, inherited
   `type`/`domain`, copied `sources:`, preserved `authority`/`index_tier`, fresh
   `created`/`updated`). Move the section's full content — prose, quotes,
   `> — Source:` lines — verbatim.
5. Turn the original into a short **hub** (2–3 line intro + `[[wikilinks]]` to
   each new child), or fold it into the most central child and redirect via
   `aliases:`. Keep the original `title` reachable, and cross-link siblings.
6. Re-lint: the parent drops under 150 and no orphan/missing-page warnings appear
   for the new titles (they're linked from the hub).

## Class 4 — directory named after the KB — proven triage, propose-only merge

**Signal:** `directory named after the KB itself — likely self-ingested pages`.
The KB mined its own curation sessions and filed pages under a folder named after
itself. This is **WARN-only by design and never auto-merged** — contents may hold
unique fragments, so the merge is a judgment call you propose, not apply blindly.

**First, distinguish two very different cases:**
- `projects/<kbname>/` is usually **legitimate meta-memory** — the KB's own
  project folder with its real `learnings.md`/`decisions-log.md`. **Do not merge
  it away.** (It may itself have a rolling file to archive — that's Class 1.)
- Stray fragment clusters look like dated stubs under `wiki/<kbname>/2026-MM-DD-*.md`.
  Those are the self-ingestion artifacts to clean up.

**Procedure (for genuine stray clusters):**
1. List the directory's files. For each, `qmd query` its topic to find the
   canonical article it duplicates or belongs near.
2. **Propose** the merges to the user (which fragment → which canonical page).
   On approval, fold unique content into the canonical article (preserve
   `sources:` and quotes); add an `aliases:` entry if the fragment had a distinct
   title others might link to.
3. Once every file is merged or confirmed redundant, remove the directory.
4. Re-lint: the self-named-dir warning clears and no new missing-page warnings.

## Verification loop (every class, every time)

```
scribe lint -v            # the worklist for this class
# ... make the edits ...
scribe lint --fix         # backfill frontmatter on anything you created
scribe lint               # confirm the class count dropped, no new errors
```

If `scribe lint` still shows a frontmatter **error** on a file you touched,
don't hand-hack it into looking valid — the YAML parser and the line-scanner
disagree about it (a trailing-whitespace fence, a duplicate key). Fix the actual
fence/key and re-check. See the convergence trap in `references/FIELD_NOTES.md`.

## Scaling a big cleanup

A real 23-warnings-to-0 pass ran **four parallel agents grouped by project**,
each owning one project's files. If warnings are spread across many folders,
partition by folder and run agents in parallel — then always run `scribe lint`
once more at the end.

## What NOT to do

- **Don't guess a merge or split** — propose it first (rule 1).
- **Don't pad a thin article** to clear 15 lines. Merge or delete instead.
- **Don't split a coherent 155-line reference** just to satisfy the threshold.
- **Don't archive by cutting mid-entry** — rolling entries move whole or not at all.
- **Don't merge `projects/<kbname>/`** — it's legitimate KB meta-memory.
- **Don't drop `sources:`, quotes, provenance, wikilink targets, or
  `authority`/`index_tier`.**
- **Don't run `scribe sync`/`dream`** as part of tidying.

## References

- `references/FIELD_NOTES.md` — what's battle-tested vs. greenfield, the archive
  naming/frontmatter reality, the 150-vs-200 gap, the convergence trap, and the
  near-duplicate tuning gotcha. Read it before working the queue.
- The **scribe-kb** skill (installed alongside this one) covers the frontmatter
  schema (`references/FRONTMATTER.md`), directory taxonomy
  (`references/STRUCTURE.md`), wikilink syntax (`references/WIKILINKS.md`), and
  qmd search (`references/QUERY.md`).
