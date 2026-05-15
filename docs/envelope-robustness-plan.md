# Envelope robustness + absorb-log resilience

Status: planned (2026-05-15). Triggered by two failures observed during
the 0.2.14–0.2.16 100%-Ollama rollout against scriptorium.

## The single root cause

Both bugs the user hit are the same defect: **the envelope executor
lets a model write to scribe-generated `_`-prefixed artifacts.**

Evidence:

- `wiki/_absorb_log.json` (48.9 KB) parses as a valid JSON object
  followed by extra appended data with a *foreign schema*:
  `…s-42.md", "status": "absorbed", "reason": "Tweet content was never
  captured…", "rule": 7}`. No Go writer emits that shape — every Go
  caller goes through `saveAbsorbLog` (atomic tmp+rename, `{name:{at,
  sha}}` schema). The fragment is an **LLM-fabricated record** the
  dream/extract envelope `append`-ed onto the file.
- `appendToFile` only errors when the target is *missing*
  (`wiki_actions.go:522`). `_absorb_log.json` exists, so the bogus
  append succeeded and corrupted it.
- The dream run's other 6 errors (`update_frontmatter
  "wiki/_backlinks.json": no frontmatter delimiter`, `append
  "wiki/_stale_candidates.md": append target missing`) are the *same
  model behavior* — it just happened to pick targets that failed
  loudly instead of corrupting silently.
- Consequence: `loadAbsorbLog` (`absorb_log.go:87`) now returns
  `parse absorb log: invalid character '{' after top-level value`,
  which aborts the **entire absorb phase every sync run** until the
  file is hand-repaired. So one bad envelope action took out the
  absorb pipeline for days.

`validateActionPath` (`wiki_actions.go:476`) accepts any path rooted
in a `wikiDirs` entry. Every generated artifact (`_index.md`,
`_backlinks.json`, `_absorb_log.json`, `_hot.md`, `_staleness.jsonl`,
`_contextualized_log.json`, `_sections.json`, `_relations_*.jsonl`,
`_stale_candidates.md`, …) lives under `wiki/`, so they all pass.

## Fix — four layers, executor first

### Layer 1 (the real guard): reject `_`-prefixed targets in `validateActionPath`

`wiki_actions.go`, in `validateActionPath`, after the wiki-dirs check:

```go
base := filepath.Base(cleaned)
if strings.HasPrefix(base, "_") {
    return "", fmt.Errorf("path %q targets a scribe-generated artifact (underscore-prefixed); models must not write these", rel)
}
```

Rationale: every `_`-prefixed file in the KB is derived — regenerated
by `scribe index` / `backlinks` / `stale build` / `sections build` /
`contextualize` / the absorb log. A model has *zero* legitimate reason
to author one. This is deterministic and doesn't depend on prompt
obedience. It converts the silent corruption into a recorded
`res.Errors` entry the run record already surfaces.

Edge: `_index.md` is sometimes referenced in prose. We're blocking
*writes*, not links — no impact on `related:` / `[[_index]]` body
text.

### Layer 2: promote `append`-to-missing → `create`

`wiki_actions.go`, `case "append"`. When `appendToFile` fails because
the target is missing, the model's *intent* ("this content belongs in
this file") is still satisfiable — just write it as a new file:

```go
case "append":
    if opts.DryRun { … }
    if err := appendToFile(abs, a.Content); err != nil {
        if errors.Is(err, fs.ErrNotExist) || isAppendTargetMissing(err) {
            // Model picked the wrong op for a new file. Honor intent.
            if werr := writeFileAtomic(abs, []byte(a.Content), 0o644); werr != nil {
                res.Errors = append(res.Errors, fmt.Sprintf("action[%d] append→create %q: %v", i, a.Path, werr))
                continue
            }
            logMsg("envelope", "action[%d] append target missing, promoted to create: %s", i, a.Path)
            res.Applied = append(res.Applied, a.Path)
            continue
        }
        res.Errors = append(res.Errors, …)
        continue
    }
```

`appendToFile` should wrap with `%w` (it already does) so
`errors.Is(err, fs.ErrNotExist)` works. Layer 1 runs first, so this
promotion can never resurrect a `_`-prefixed write.

### Layer 3: absorb-log resilience + one-time repair

Two parts.

**3a — salvage on parse failure.** `loadAbsorbLog` (`absorb_log.go`)
currently hard-fails on corrupt JSON, which kills absorb. Change it to
attempt recovery:

1. Try `json.Unmarshal` as today.
2. On failure, find the first balanced top-level `}` and re-parse the
   prefix (the original valid object). The corruption pattern is
   always *trailing* garbage appended after a complete object, so
   prefix-truncation recovers the full pre-corruption log.
3. If prefix-parse succeeds: log
   `logMsg("absorb", "warn: _absorb_log.json had trailing garbage (%d bytes), recovered %d entries — rewriting clean", …)`,
   immediately `saveAbsorbLog` the recovered map (atomic; heals the
   file), continue.
4. If even the prefix is unparseable: log a loud warning, return an
   empty log (absorb re-runs idempotently — wasteful but correct, and
   the absorb-decision dedupe in `checkAbsorbDecision` prevents
   duplicate articles).

Never abort the absorb phase for a corrupt log again.

**3b — heal the live scriptorium file once.** After 3a ships, the next
`scribe sync` self-heals scriptorium's `_absorb_log.json`
automatically (recover-prefix → rewrite clean). No manual step, no
data loss — the recovered prefix is the complete pre-corruption log;
only the fabricated `{status,reason,rule}` fragment is dropped.

### Layer 4 (defense in depth): prompt rule

Add to every envelope prompt that can emit actions (`dream-ollama.md`,
`dream-anthropic.md`, `extract-ollama.md`, `extract-anthropic.md`,
`assess-*.md`, `deep-extract-*.md`, `session-mine-*.md`,
`absorb-pass2-json.md`):

```
- NEVER target a file whose name starts with "_" (e.g. _index.md,
  _backlinks.json, _absorb_log.json). Those are generated by scribe
  and regenerated automatically — writing them corrupts the KB.
- Use "create" for a new file. Use "append" ONLY for a file you were
  shown exists.
```

Layer 1 is the actual guarantee; Layer 4 reduces wasted generations
(rejected actions still cost tokens) and improves local-model output.

## Tests

- `validateActionPath`: table case — every `_`-prefixed path under
  each `wikiDirs` entry rejected; non-underscore paths still accepted;
  `_`-prefixed in a *subdir* (`projects/_x.md`) rejected
  (basename-check, not prefix-of-rel).
- append→create promotion: missing target writes the file and lands
  in `res.Applied`; existing target still appends; `_`-prefixed
  missing target still rejected (Layer 1 wins).
- `loadAbsorbLog`: (a) clean file → parsed; (b) valid-object +
  trailing-garbage fixture → recovered entry count + file rewritten
  clean; (c) total garbage → empty log + no error; (d) missing file →
  empty log (unchanged).
- Regression fixture: a trimmed copy of the real corrupt
  `_absorb_log.json` shape under `testdata/`.

## Scope / ship

- One file mostly (`wiki_actions.go`), plus `absorb_log.go`, plus 8
  prompt edits, plus tests. ~250 LOC.
- No config surface, no schema change, backward compatible.
- Ship as **0.2.18** (0.2.17 = Codex handshake, already in CHANGELOG).
- CHANGELOG framing: "Fix — envelope executor could corrupt
  scribe-generated artifacts; absorb phase no longer abortable by one
  bad action."

## Out of scope

- File-locking `_absorb_log.json` against concurrent writers — the
  corruption source was the envelope, not a race; Layer 1 removes it.
  Revisit only if corruption recurs from a *different* writer.
- A general "generated artifact registry" — the `_` prefix convention
  is already the de-facto contract (`config.go:1652` lists them); a
  registry is over-engineering for one boolean check.
