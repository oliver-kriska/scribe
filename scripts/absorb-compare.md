# absorb-compare.sh

Quality probe for the Phase 4B pass-2 envelope path. Runs `scribe sync`
twice against the same raw article — once with `pass2_mode: tools`, once
with `pass2_mode: json` — and reports a side-by-side comparison of the
resulting wiki output.

The pass-2 envelope path (json mode) shipped in commit `125b88a`. We
validated parse rate, frontmatter shape, citation grounding, and
related-array syntax across three local models, but we never ran the
canonical "same article through both modes, diff outputs" comparison.
This script closes that gap.

Expected cadence: a few times per quarter when revisiting whether the
envelope path's quality justifies leaving it on by default for new
KBs. Not part of the test suite; not run by cron.

## When to run it

- Before changing `cfg.Absorb.Pass2Mode` defaults across all users.
- After meaningful prompt edits to `prompts/absorb-pass2-json.md` or
  `prompts/absorb-pass2.md` — to see if the change pulled the json
  path closer to or further from the tools baseline.
- When evaluating a new local model in pass-2: run once on each model
  and compare the json column across runs.

## Requirements

- Run from a KB root (the directory holding `scribe.yaml`).
- `jq`, `sed`, `diff`, `find`, `rsync`, `awk`, `comm` on PATH.
- The target raw article already exists under `raw/articles/`.

The script switches modes via the `SCRIBE_PASS2_MODE` env var
(introduced alongside `SCRIBE_PASS2_PROVIDER` and `SCRIBE_PASS2_MODEL`).
No `scribe.yaml` edits needed — the env vars override whatever the
yaml says for the duration of the run.

## Usage

```sh
scripts/absorb-compare.sh raw/articles/2026-04-09-test-linkedin.md
```

By default snapshots and the report land in `/tmp/scribe-compare-<pid>/`
and are deleted when the script exits cleanly. To keep them:

```sh
KEEP_OUT=1 scripts/absorb-compare.sh raw/articles/<file>
OUT_DIR=./compare-runs/2026-05-13 KEEP_OUT=1 scripts/absorb-compare.sh ...
```

If `scribe` is not on PATH:

```sh
SCRIBE_BIN=./bin/scribe scripts/absorb-compare.sh raw/articles/<file>
```

## What it mutates

While running, the script exports `SCRIBE_PASS2_MODE` for each sync
invocation (`scribe.yaml` is never touched), deletes the target
file's entry from `wiki/_absorb_log.json`, and replaces the
wiki/output trees between runs from a baseline snapshot. A
`trap EXIT` handler restores everything before the script exits.

If the script crashes mid-run:

```sh
rsync -a --delete /tmp/scribe-compare-<pid>/baseline/wiki/ wiki/
# similar for projects/, research/, output/, etc.
```

The baseline snapshot is the authoritative pre-run state. As long as
`/tmp/scribe-compare-<pid>/baseline/` exists, the KB can be returned
to its starting state.

## Output

The report is plain text:

```
metric                      tools       json        delta
---------------------- ---------- ---------- ------------
added_files                     6          6           +0
added_lines                   412        385          -27
distinct_wikilinks             34         29           -5
citation_brackets              22         21           -1
orphan_wikilinks                3          4           +1
```

Interpretation tolerances (from the follow-up plan):

| metric             | target                              |
|--------------------|-------------------------------------|
| added_files        | json equal to tools ±1              |
| added_lines        | json ≤ tools, within 20%            |
| distinct_wikilinks | json ≥ 80% of tools                 |
| orphan_wikilinks   | json ≤ tools + 20%                  |
| citation_brackets  | json ≥ tools                        |

If json mode misses any tolerance, the pass-2 json prompt needs
tightening before re-enabling heavy crons against json mode.

## Limitations

- `scribe sync` does more than absorb (project extraction, session
  mining) — those phases run during the comparison too, and the
  snapshot/restore pattern catches the wiki state they touch. Their
  cost shows up in `tools.log`/`json.log` but not in the report
  numbers, which only count files added relative to baseline.
- Orphan detection is heuristic: the script lowercases-and-dashes the
  wikilink target text and looks for a matching basename under the
  baseline wiki dirs. Wikilinks with a custom display text
  (`[[target|display]]`) are matched against `target`. False
  positives are possible when the target's actual file uses a
  different slug convention.
- Running on a populated production KB is slow because every
  `scribe sync` invocation re-walks the project tree. For repeated
  comparisons, point the script at a smaller test KB.
