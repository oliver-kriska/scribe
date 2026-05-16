# Entity fan-out cap plan — fixing the pass-2 thermal root cause

Filed: 2026-05-16
Parent: [[local-model-support-plan]] / `.claude/research/2026-05-15-pass2-thermal-model-downgrade.md`
Status: **Fix A SHIPPED 2026-05-16** (`chunkOptions.MinChunkBytes`,
default 6 KB, coalesces sub-min heading sections in `chunkByHeadings`;
tests in `chunker_test.go`; existing chaptered/headings fixtures
realigned to the corrected contract; `make check` green). **Fix B
(post-merge `MaxEntities` cap + generic-label filter) still pending** —
sequenced as "only if real PDFs still over-fan after A".

## Why this exists

A 2026-05-15 empirical A/B (gemma3:27b vs gemma3:12b on pass-2) rejected
the 12b downgrade (12b corrupts ~47% of `related:` frontmatter, incl.
invalid YAML) but incidentally exposed the real reason absorb runs the
GPU hot for ~80 min on a *trivial* article:

```
588-word note
  → chunkByHeadings splits at EVERY heading (no min-size coalesce)
  → 13 chunks of ~45 words each
  → 13 facts calls + 13 pass-1 calls (gemma3:4b)
  → each proposes 1–4 entities
  → mergeChapterPlans dedups by EXACT-STRING label only
  → near-dupes survive ("Harness" / "Harness Engineering" /
    "Middleware" / "LoopDetectionMiddleware" / "Tools" / "Models" /
    "System Prompt")
  → 35 entities, NO cap anywhere
  → pass-2 iterates all 35 serially at pass2_parallel=1
  → ~2.3 min/entity on 27b = ~80 min sustained GPU / article
```

Model size is a ~15% rounding error against this. The thermal fix is
here, and it costs zero quality (the dropped entities are noise).

## Root-cause map (verified, file:line)

| Stage | Location | Defect |
|---|---|---|
| Chunking | `chunker.go:84` `chunkByHeadings` | Splits at every `#`/`##`; `len(chunks)>1` accepted. No **minimum** chunk size — only the TOC path re-splits chunks that are too *big*; nothing merges chunks that are too *small*. 588 words → 13 chunks. |
| Merge | `absorb_chapter.go:325` `mergeChapterPlans` | Dedup is `byLabel[label]` — **exact-string only**. No case/whitespace/substring normalization, no salience rank, no cap. |
| Config | `config.go` `AbsorbConfig` (~895) | `MaxPerRun`, `Pass2Parallel`, chapter knobs exist; **no `MaxEntities`** field. Confirmed absent codebase-wide. |
| Pass-2 | `sync.go:1887` `for i, ent := range plan.Entities` | Iterates **all** merged entities, serial at `Pass2Parallel` (=1 in scriptorium). The thermal hot loop. |

## Design — two fixes, leverage order

### Fix A (primary, root cause): coalesce tiny heading chunks

A 45-word "chapter" is not a chapter. `chunkByHeadings` should merge
adjacent heading sections until a chunk reaches a **minimum size**
before it counts as a standalone chunk.

- New `chunkOptions` field `MinChunkBytes` (e.g. default ~6 KB ≈
  ~900–1000 words) alongside the existing `MaxBytes`.
- In `chunkByHeadings`: accumulate sections; only emit a chunk when the
  buffer ≥ `MinChunkBytes` (or input is exhausted). Preserve the first
  section's heading as the chunk title; concatenate bodies. Large
  sections still split via the existing `MaxBytes` secondary path —
  this only changes the *small* side.
- Effect: 588-word note → **1 chunk** → 1 facts + 1 pass-1 call →
  ~3–6 entities → ~6–12 min on 27b instead of ~80. Real chaptered
  PDFs (sections already ≥ MinChunkBytes) are unaffected.
- **Bonus:** also fixes the gemma3:4b pass-1 `context deadline
  exceeded` timeouts and `cannot unmarshal … entities` plan parse
  errors seen on the 4927-word doc — fewer, larger, coherent calls
  instead of 28 tiny racy ones (see local-model-followup-plan
  2026-05-16 addendum, bug 3).

Risk: a doc that is legitimately many short distinct topics gets fewer,
broader entities. Acceptable — pass-2 + the existing splitter still
cover it, and "one good page" beats "ten thin stubs" for KB quality.
Mitigated by Fix B's salience rank, not by finer chunking.

### Fix B (defense-in-depth): post-merge entity cap + generic filter

Even with correct chunking, long real PDFs can over-propose. Bound the
worst case between `mergeChapterPlans` and pass-2:

1. **Generic-label filter.** Drop entities whose label is a generic
   stop-term (`Tools`, `Models`, `Middleware`, `System Prompt`,
   `Harness`, …). Reuse the existing `IdentitiesConfig` stopword
   pattern (`config.go:803`) — add an `absorb.entity_stopwords` list
   with sane built-in defaults, user-extendable, same merge semantics
   as `mergeIdentityConfig`.
2. **Normalized dedup.** Before the cap, fold labels by
   case/whitespace-insensitive + containment match so "Harness" folds
   into "Harness Engineering" (longer, more specific label wins; merge
   key_claims as today). Keeps the exact-string fast path; adds a
   second normalized pass.
3. **Salience cap.** New `AbsorbConfig.MaxEntities` (`yaml:"max_entities"`,
   default ~12, 0 = unlimited). Rank survivors by salience
   (len(KeyClaims) desc, then has-grounding-facts, then first-seen
   order) and keep top-N. Log dropped labels so it's auditable.

Wiring point: a new `capAndFilterEntities(plan, cfg)` called in
`sync.go` right after the merge / before the `pass1 planned N entities`
log (sync.go:1802). Pure function, trivially unit-testable.

### Rejected: raise `pass2_parallel`

Running 2× 27b concurrently doubles *instantaneous* heat — wrong
direction for a thermal complaint, even though 64 GB has the RAM
headroom. Leave `pass2_parallel: 1`. Mention only as a knob.

## Files (anticipated)

- `cmd/scribe/chunker.go` — `MinChunkBytes` option + coalesce in
  `chunkByHeadings`; `defaultChunkOptions` default.
- `cmd/scribe/absorb_chapter.go` — `capAndFilterEntities`; normalized
  dedup helper.
- `cmd/scribe/config.go` — `AbsorbConfig.MaxEntities`,
  `absorb.entity_stopwords`; defaults; scribe.yaml comment block.
- `cmd/scribe/sync.go` — call `capAndFilterEntities` after merge; log
  dropped count.
- `cmd/scribe/chunker_test.go`, `absorb_chapter_test.go` — table tests:
  small-section coalesce, stopword drop, normalized fold, MaxEntities
  rank/truncate, MaxEntities=0 passthrough.

## Tests / acceptance

- The 588-word harness note → ≤ 2 chunks, ≤ 8 entities, pass-2
  wall-clock on 27b drops from ~80 min to < ~15 min.
- A real ≥ 8-chapter PDF (sections already large) → chunk count
  unchanged (no regression on the path that works).
- `MaxEntities: 0` and absent config → byte-identical to today.

## Sequencing

1. Fix A first (biggest payoff, smallest change, also clears bug 3).
2. Re-measure on the harness note + one large PDF.
3. Fix B if real PDFs still over-fan after A.
4. Only then revisit cadence/cron (config-only, tracked in
   `.claude/research/2026-05-15-…`, deliberately deferred per user).

## Estimate

Fix A: ~half-day incl. tests. Fix B: ~1 day incl. config surface +
tests. No prompt changes required (the pass-1 prompt already says
"0–4 per chapter"; the bug is mechanical, not promptable).
