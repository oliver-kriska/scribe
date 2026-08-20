# Modularization plan for `cmd/scribe`

**Written 2026-08-20.** Measured, not estimated — every number below is
reproducible with the command beside it.

## Where the package actually is

| Metric | Value | How to reproduce |
| --- | ---: | --- |
| production LOC, one package | 46,287 | `find cmd/scribe -name '*.go' ! -name '*_test.go' \| xargs wc -l \| tail -1` |
| test LOC | 35,418 | same with `-name '*_test.go'` |
| statement coverage | 61.9% | `go test -tags sqlite_fts5 -coverprofile=cov.out ./...` |
| functions at 0% | 222 | `go tool cover -func=cov.out \| awk '$3=="0.0%"' \| wc -l` |
| test functions | 1,159 | `grep -rhc '^func Test' cmd/scribe/*_test.go \| paste -sd+ \| bc` |
| `t.Parallel()` call sites | 24 | `grep -rc 't.Parallel()' cmd/scribe/*_test.go \| awk -F: '{s+=$2} END{print s}'` |

`CLAUDE.md` still says to keep one package "until the package breaks 3000
LOC". That threshold was passed roughly fifteen times over. The guidance needs
updating, but **that file is the maintainer's to change** — this document is the
input to that decision, not a substitute for it.

## Why the coverage number is not the useful number

The 222 zero-covered functions are not spread evenly. They cluster where the
code touches the outside world:

```
12  fda.go            macOS Full Disk Access probing + interactive grant
11  doctor.go         health-check sections
 8  ingest.go         inbox draining
 7  sync_sessions.go  session mining
 7  projects.go       manifest CRUD commands
 7  cost_ledger.go    provider cost reconciliation
 6  triage.go / sync.go / stale.go / relations*.go / session_mine_envelope.go
```

`fda.go` cannot be unit-tested at all without a macOS TCC grant, and `doctor.go`
sections are mostly I/O over a real KB. Chasing a global coverage percentage
here would mean writing tests that assert on mocks of the operating system. The
honest target is **not** "raise 61.9%" — it is "make the pure logic inside those
files reachable without the I/O", which is the same work as extracting packages.

## The concrete blocker for `t.Parallel()`

24 parallel call sites across 1,159 tests is not timidity; it is a correct
response to shared mutable state. Scanning every package-level `var` for
assignment inside a function body finds 44 candidates, most of them
`regexp.MustCompile` in a `var (…)` block — immutable in practice (they are
counted because a lazy-init or cache assignment shares the shape). Reproduce:

```sh
cd cmd/scribe
ls *.go | grep -v _test.go | xargs awk \
  '/^var \(/{b=1;next} b&&/^\)/{b=0;next} b&&/^\t[a-zA-Z_]/{print $1;next} /^var [a-zA-Z_]/{print $2}' \
  | sort -u | while read -r v; do
      ls *.go | grep -v _test.go \
        | xargs grep -lqE "^[[:space:]]+$v(\[[^]]*\])?[[:space:]]*(=|\+=)[^=]" && echo "$v"
    done | wc -l
```

The ones that genuinely mutate at runtime are few, and one dominates (writer
counts are that same `grep -l` per name, file-counted):

| Global | Declared | Runtime writers | Risk |
| --- | --- | ---: | --- |
| `runStats map[string]any` | `main.go:24` | **14 files** | unguarded map; concurrent writes are a hard race |
| `globalRoot` | `main.go:20` | 1 | set once from the CLI root flag |
| `version` | `main.go:17` | 0 | set at link time by `-X main.version=…`, never assigned in a body |
| `logLevel` | `logging.go:13` | 1 | set once at startup |
| `ollamaReadyCache` | `llm.go:446` | 1 | memoized probe |
| `promptFallbackOnce` | `claude.go:341` | 1 | `sync.Once`-style latch |
| `runDegradations` | `run_outcome.go` | 2 | mutex-guarded (added 2026-08-20) |

`runStats` is the one worth fixing first, and it has a second, subtler problem:
it is declared nil, so every writer must nil-guard before assigning. That
convention is repeated at ten-plus sites, and `dream.go:133` carries a comment
about the time a bare assignment clobbered it. A convention repeated eleven
times is a missing function.

### Step 0 — `runStats` accessor (do this before any extraction)

Replace direct map access with `setRunStat(k string, v any)` and
`addRunStat(k string, delta int)`, both mutex-guarded and both initializing on
first use. This is mechanical, touches 14 files, changes no behavior, and:

- removes the nil-guard convention entirely (safe by construction),
- makes the map race-free, which is the precondition for `t.Parallel()`,
- gives tests a single `resetRunStats()` seam instead of the
  save/restore/`t.Cleanup` dance currently hand-written in `deep_run_test.go`,
  `dream_hot_test.go`, and elsewhere.

`run_outcome.go` (added 2026-08-20 for the degraded-run outcome) is already
written this way and can serve as the shape to copy.

## Extraction order — leaves first, by measured coupling

Ranked by how few other files a candidate references (fan-out), which is what
determines whether an extraction is a move or a refactor. Fan-out here counts
the other files whose package-level symbols a file mentions, by regex over
top-level `func`/`type`/`var`/`const` names — good enough to rank candidates, not
exact. The globals table above is exact:

```sh
# runtime writers of a global, e.g. runStats
grep -lE 'runStats(\[[^]]+\])? *= *[^=]' cmd/scribe/*.go | grep -v _test | wc -l
```

| Candidate | LOC | fan-out | Notes |
| --- | ---: | ---: | --- |
| `yaml_scalar.go` | 61 | 1 | pure string handling |
| `logging.go` | 104 | 1 | one global, set once |
| `resilience.go` | 105 | 3 | retry/backoff, no KB knowledge |
| `session_transcript.go` | 171 | 3 | parsing only |
| `convert_tier0.go`, `convert_docx.go` | 383 | 4 | format conversion, pure in/out |
| `triage.go` | 516 | 5 | FTS5 scoring; budgeted <1s, no LLM |

**Phase 1 — `internal/textutil`**: `yaml_scalar.go` plus the frontmatter and
scalar helpers. Pure functions, no globals, no I/O. Proves the `internal/` split
works and gives the first package that can run its whole suite in parallel.

**Phase 2 — `internal/convert`**: the tier-0/docx converters. Already shaped as
`([]byte) → ([]byte, error)`, and cleaner than the fan-out table suggests —
neither file references `logMsg`, `logLevel`, or any package-local helper; their
only dependencies are stdlib plus `ledongthuc/pdf` and the html-to-markdown
library. This is close to a pure `git mv`.

**Phase 3 — `internal/triage`**: the FTS5 scoring path. It has a hard contract
already (deterministic, no LLM, <1s) which makes it the best-specified boundary
in the repo. Requires passing a `*sql.DB` in rather than opening one.

Everything else — `sync*.go`, `doctor.go`, `dream*.go` — stays put. Those are
orchestration over the whole KB; splitting them buys import cycles, not clarity.

## What this plan deliberately does not propose

- **No big-bang restructure.** The package is large but coherent, the test suite
  is substantial, and there is one maintainer. A staged extraction that stops
  after Phase 1 still leaves the repo better than it found it; a half-finished
  rewrite does not.
- **No coverage target.** See above: the uncovered mass is OS and I/O
  boundaries, and a number chased for its own sake produces mock-shaped tests.
  Measure coverage *of the extracted packages* instead, where 90%+ is honest and
  achievable.
- **No `internal/` split before Step 0.** Moving code that mutates a shared
  global into another package converts a race into a race across a package
  boundary, which is harder to see and no safer.
