# Field notes — what's battle-tested vs. greenfield

Read this before working the `scribe lint` content queue. It records what has
actually been done to a real KB versus what this skill is inventing, so you know
where to trust a recipe and where to move slowly and propose before writing.

## The load-bearing rule

**Mechanical/structural fixes are safe to auto-apply — git is the safety net.
Anything that requires judgment ("which page wins", "which sentences fold in")
is propose-first: show the user the plan and get a nod before you write.**

`scribe lint --fix` already embodies this: it repairs frontmatter, removes
byte-identical duplicates, and renames `*.md.md`, but it *never* merges content
or splits an article. This skill works the queue `--fix` deliberately leaves —
so the same discipline applies. When in doubt, propose.

## Class-by-class reality

### Class 3 — rolling archive: battle-tested, reuse the recipe
- Designed and first executed on a real KB (archive files exist for ~12
  projects). The procedure that stuck: newest entries stay at the top of the
  live file; move the **oldest (bottom)** entries into `{base}-archive-YYYY.md`;
  the archive carries `rolling: true` plus full frontmatter.
- **Naming drift to respect:** the archive base drops a `-log` suffix.
  `learnings.md` → `learnings-archive-2026.md`, but
  `decisions-log.md` → `decisions-archive-2026.md` (**not**
  `decisions-log-archive-2026.md`). Match the sibling archive that already
  exists in that directory if there is one.
- **Frontmatter reality:** a `status: archived` field was designed but never
  actually applied. Real archives use `authority: contextual` +
  `index_tier: deep` (copied from the live file) + a `tags: [..., archive]`
  entry + `sources: ["<path to live file>"]`. Preserve `authority`/`index_tier`
  when you rewrite — the schema doc omits them but every live rolling file has
  them, and dropping them trips the `index_tier missing` warning.
- **The 150 vs. 200 gap:** `scribe lint` only *fires* the warning above 200
  lines, but the message and the convention both say "archive at 150." Target
  150 — leave the live file comfortably under threshold, not right at it.
- **The backlink is aspirational:** a bottom-of-file `See also: [[…Archive]]`
  pointer was designed but is *not* consistently maintained in practice. Add one
  if you like; don't treat its absence as a bug to chase.

### Class 4 — self-named directory: rich experience, but merge is propose-only
- Root cause: the KB conscripts itself as a source (session-mining re-ingests
  the curated wiki), producing `readme.md.md`, `CLAUDE_md.md`, and KB-named
  directories (`wiki/<kbname>/`, `projects/<kbname>/`).
- `scribe lint --fix` auto-removes the doubled-extension and byte-identical
  cases. `findSelfNamedDirs` surfaces the directory as **WARN only — never
  auto-merged**, because contents may hold unique fragments. The KB punts the
  actual merge to human judgment (`scribe lint --contradictions --resolve`
  *proposes* a winner and writes proposals; it never auto-applies).
- **Critical distinction:** `projects/<kbname>/` is often **legitimate
  meta-memory** — the KB's own project folder, holding its real
  `learnings.md`/`decisions-log.md`. That is not a stray fragment cluster and
  must not be merged away. The stray clusters look like dated stub pages under
  `wiki/<kbname>/2026-MM-DD-*.md`. Tell them apart before touching anything; if
  unsure, ask.

### Class 1 — bloated split: GREENFIELD, no prior execution
- A search of every past session found **zero** cases of a bloated wiki article
  actually being split. Every "split into" in the logs is about research
  reports, commits, or issues — never an article. This skill's split procedure
  is the *first* written recipe, grounded in the KB's stated anti-cramming
  principle, not in prior execution.
- So: move deliberately, propose the split points (H2 boundaries) before
  writing, and prefer leaving a coherent 151–170-line article intact over
  fragmenting it. Per-type length targets the KB documents (use as a sense of
  "how long is natural", not hard limits): Tool/Person 20–40, Decision 40–70,
  Pattern/Solution 40–80, Project 60–100, Research 80–150; hard minimum 15.

### Class 2 — thin expand/merge: GREENFIELD, no prior execution
- No hand-executed thin-merge episode exists either. The nearest real tooling is
  duplicate detection (`scribe lint --duplicates`), which is **report-only** —
  it flags a stub "largely contained in a longer canonical article" but never
  merges. Only byte-identical twins are auto-removed.
- **Hard-won tuning gotcha:** overlap-coefficient alone produced **20,533
  garbage pairs** — tiny notes "contained in" huge rolling files. The lesson:
  before proposing any merge, exclude `rolling: true` and `*-archive-*` files
  and cap the size ratio (a 14-line stub is not "the same article" as a
  400-line learnings log). Merge a thin article into a *peer*, not into a log.
- **Append-only nuance:** superseded content is not deleted — it stays with a
  `Status: superseded by [[X]]` note. Only delete a thin file when it is a true
  empty scaffold/greeting/exact duplicate with nothing unique, and only after
  checking inbound links.

## The convergence trap (applies to every rewrite)

The YAML parser and the line-level scanner can read the same malformed file
differently — a trailing-whitespace opening fence (`--- \n`), a duplicate
`domain:` key (validate reads last-wins, an older fixer read first-wins), or a
nested `frontmatter:` map echoed by a weak model. When that happens, `lint`
reports an error forever while `--fix` no-ops, or `tier write` oscillates
`wrote=1` every run. The fixers now carry an **honesty guard**: `autoFixArticle`
re-parses its own output and *skips* anything still unparseable rather than
writing valid-looking garbage.

Practical consequence for this skill: after you create or rewrite a file, run
`scribe lint --fix` and then `scribe lint`. If a file you touched still shows a
frontmatter error, don't hand-hack it into looking right — the parser and
scanner disagree about it; fix the actual fence/key and re-check.

## Scaling the work (from a real bulk cleanup)

A real 23-warnings-to-0 cleanup ran **four parallel agents grouped by project**
(each agent owned one project's rolling files + articles), producing 7 archives,
16 trimmed articles, and 5 splits in ~45 minutes. If a KB has warnings spread
across many project folders, partitioning by folder and running agents in
parallel is the proven shape. Always run `scribe lint` once more at the end —
the same cleanup learned that bulk imports must be followed by a lint pass.

## Sources

Grounded in scribe's own `.claude/research/` (self-extraction duplicates,
content-duplication detection, ingestion-quality root cause, lint-fix
convergence), the KB's `CLAUDE.md` writing rules, and real archive commits.
These are procedures distilled from actual runs — not invented for this file.
