# Codex project discovery — implementation plan

Status: **draft, waits behind 100%-Ollama work**
Filed: 2026-05-14
Owner: Oliver
Parent research: [[2026-05-14-codex-project-discovery]] (`.claude/research/`)

Goal: extend `scribe sync --discover` and the project manifest to find
projects from **OpenAI Codex CLI** sessions, alongside the existing
discovery of Claude Code projects. Discovery only — mining Codex
session content is a separate scope.

Sequencing: do not start until the [[100-percent-ollama-plan]] is at
least through Phase 4A.5 (top-level `LLMConfig`). The Codex work
touches `config.go`, `sync.go`, `doctor.go`, and `manifest.go` — the
same files the LLM-config refactor touches. Stacking the changes
risks merge churn for no benefit.

---

## Why this is small and worth doing

Already established in the research note. Two summary points:

1. **Codex layout is unambiguous.** Unlike Claude's
   `decodeClaudePath` greedy-rebuild against ambiguous dashes, Codex
   records the project `cwd` as a verbatim JSON string in the first
   line of every rollout. One `json.Unmarshal` per file, done.
2. **Discovery does not need session mining.** The manifest only
   cares about *which projects exist*. Reading the `session_meta`
   header per rollout (~1 KB) is enough; the rest of the JSONL is
   ignored at discovery time. Mining (ccrider-style FTS5 over Codex
   transcripts) is a separate stream we are explicitly deferring.

---

## Phased plan

### Phase C1 — Discovery MVP (half a session)

The minimum that lets `scribe sync --discover` find Codex projects.

**Config**:
- `ScribeConfig.CodexSessionsDir string` — default
  `~/.codex/sessions`. Mirrors `ClaudeProjectsDir` exactly. Add to
  `defaultConfig()`, `applyDefaults()`, the init template, and the
  YAML doc block.

**New file `cmd/scribe/codex.go`**:

```go
type codexSessionMeta struct {
    Cwd        string `json:"cwd"`
    ID         string `json:"id"`
    Originator string `json:"originator"`
    Source     string `json:"source"`
    Git        struct {
        RepositoryURL string `json:"repository_url"`
        Branch        string `json:"branch"`
        CommitHash    string `json:"commit_hash"`
    } `json:"git"`
}

// readCodexSessionMeta opens a rollout-*.jsonl, reads only its first
// line (the `session_meta` event), and returns the cwd + git payload.
// Returns (nil, nil) when the file is empty or the first event is
// not session_meta — discovery treats that as "not a Codex session,
// skip without warning".
func readCodexSessionMeta(path string) (*codexSessionMeta, error)

// walkCodexSessions enumerates rollout files under root and calls fn
// with each unique cwd once (dedup happens inside). Walks lazily by
// year so a stale ~/.codex/sessions/2024/ tree doesn't slow the scan.
func walkCodexSessions(root string, fn func(meta *codexSessionMeta, sessionPath string)) error
```

Implementation notes:

- Use `bufio.Scanner` with a 1 MB buffer ceiling — `base_instructions.text`
  in the session_meta payload inlines Codex's ~8 KB system prompt and
  the default 64 KB scanner buffer is enough today but worth being
  explicit about.
- Walk order: descending by date partition so the most-recent project
  paths win when two rollouts disagree about the same cwd (rare but
  possible after a directory rename).
- Skip the `archived_sessions/` sibling directory — Codex moved those
  out of view; surprising the user by un-archiving them is bad UX.

**Sync wiring** (`sync.go:295`):

After the existing Claude scan, run a second pass:

```go
codexCount, err := s.discoverCodex(root, manifest, cfg)
if err != nil {
    logMsg("sync", "codex discovery failed (continuing): %v", err)
}
discovered += codexCount
```

`discoverCodex` reuses the existing `manifest.isIgnored`,
`hasSignificantContent`, `manifest.resolveDomain`, and
`ensureRepoYAML` helpers — same shape as the Claude branch. Dedup
on `cwd` against `manifest.Projects` so a project that has both
Claude and Codex sessions stays one manifest entry.

**Manifest schema** (`manifest.go`):

Add one field to `ProjectEntry`:

```go
DiscoveredFrom string `json:"discovered_from,omitempty"` // "claude" | "codex" | "both"
```

Migration: missing field reads as `""`; treat as `"claude"` for
back-compat (every existing entry was found through the Claude
scanner). The discovery pass updates the field when a project is
seen from a second source.

**Doctor probe** (`doctor.go:175`):

New `codex_sessions_dir` row alongside `claude_projects_dir`. Status:
- OK: dir exists, contains ≥1 rollout
- WARN: dir missing (Codex not installed) or zero rollouts
- never FAIL — Codex is optional

**Tests**:

- Fixture rollout: a tiny `testdata/codex/rollout-fixture.jsonl`
  containing one `session_meta` line for a known cwd. The cwd points
  at a `t.TempDir()`-created project to keep tests hermetic.
- Unit: `readCodexSessionMeta` happy path, empty file, malformed first
  line, first line is wrong event type.
- Unit: `walkCodexSessions` finds two rollouts with the same cwd and
  yields the cwd once.
- Integration: end-to-end `discover` against a temp `~/.codex` that
  contains one rollout in two different cwds and one duplicate.
  Asserts manifest gets two projects and `DiscoveredFrom = "codex"`.

**Acceptance**:
- `scribe sync --discover --dry-run` lists Codex-only projects on
  Oliver's machine with the correct `cwd` and domain.
- `scribe doctor` shows the new row.
- Running discovery twice doesn't create duplicate manifest entries.

**Files touched** (delta):
- new: `cmd/scribe/codex.go` (~120 lines)
- new: `cmd/scribe/codex_test.go` (~150 lines)
- new: `cmd/scribe/testdata/codex/rollout-fixture.jsonl`
- edit: `cmd/scribe/config.go` (+ `CodexSessionsDir`, defaults, init
  template block, applyDefaults home-expansion)
- edit: `cmd/scribe/sync.go` (+ `discoverCodex`, call site in
  `discover`)
- edit: `cmd/scribe/manifest.go` (+ `DiscoveredFrom`)
- edit: `cmd/scribe/doctor.go` (+ row)
- edit: `cmd/scribe/init.go` (surface the new config row in the
  init summary)

**Estimate**: half a session including tests.

---

### Phase C2 — Provenance polish (small, optional)

Once C1 lands, decide whether the provenance bookkeeping is worth
expanding. Three small things:

1. **Lift git context from the rollout into `.repo.yaml`.** Today
   `ensureRepoYAML` (`sync.go:355`) shells out to `gitRemoteURL` and
   `gitBranch` against the working tree. When the project came in via
   a Codex rollout, prefer the payload's
   `git.repository_url`/`git.branch`/`git.commit_hash` — they were
   captured at session start and don't require the working tree to
   still be on the original branch.

2. **Originator stats**. `payload.originator` and `payload.source`
   distinguish Codex Desktop / CLI / VSCode / etc. Surface a count
   in `scribe doctor`'s codex row (`"4 Desktop + 2 CLI sessions
   across 3 projects"`). Useful for the "which surface drives my
   work" question.

3. **First-seen / last-seen per project**. The rollout filename
   encodes the start timestamp. Stash `LastCodexSession time.Time`
   on `ProjectEntry` so manifest dumps are sortable by recent
   activity. Tiny.

**Files touched**: same set as C1, no new files.

**Estimate**: half a session if we do all three.

---

### Phase C3 — Codex session mining (large, explicitly deferred)

Out of scope for this plan. Captured here only so future-Oliver
remembers what's *not* covered:

- A `ccrider`-equivalent for Codex (FTS5 indexer over Codex JSONL
  events) — or extend `ccrider` itself to accept a second source.
- A `scribe sync --sessions` branch that triages and mines Codex
  sessions the same way it does Claude ones.
- Per-event schema work for Codex's `tool_call` / `message` shapes,
  which differ from Claude's.

The pieces above need to coordinate with whichever direction the
`100-percent-ollama-plan.md` Phase 4C (session mining via envelope)
lands first. Doing Codex mining as a sibling envelope path is the
clean integration — `EnvelopeV2 + MetaAction` already covers writes
to `_sessions_log.json` and rolling memory, regardless of which agent
the session came from.

**Pre-requisite to even start C3**: 100%-Ollama Phase 4C ships and
proves the envelope shape works for Claude sessions. If we do Codex
mining before that lands, we'd reinvent the schema and have to
re-do it.

---

## Edge cases to handle in C1

Pre-emptively, not as TODOs:

| Case | Behaviour |
|---|---|
| `~/.codex/sessions/` missing entirely | warn in doctor, return 0 from discoverCodex, do not error |
| Empty rollout file | skip (readCodexSessionMeta returns nil) |
| Rollout's first line is not `session_meta` | skip with a debug-level log (never seen, but cheap to handle) |
| Two rollouts disagree about `cwd` for the same `id` | impossible — `id` is per-rollout. Dedup is on `cwd` not `id` |
| `cwd` points at a path that no longer exists | skip (same `dirExists` filter Claude branch already uses, `sync.go:314`) |
| `cwd` is inside a git worktree (`.git/worktrees/...`) | accept; `hasSignificantContent` filters most worktrees with no content of their own |
| Codex Desktop and Codex CLI both used the same project | one manifest entry, `originator` mix recorded for doctor stats |
| `--ephemeral` sessions | invisible by design — Codex never writes a rollout, scribe never sees it |
| `~/.codex/archived_sessions/` | skipped (do not walk) |
| Symlinked sessions dir | follow once via `os.Stat` resolution; refuse if it loops |

---

## Risks

- **Scanner buffer**. `base_instructions.text` in `session_meta` is
  the full Codex system prompt — ~8 KB on this machine, but Codex
  may grow it. Explicit `Buffer(make([]byte, 64*1024), 1024*1024)`
  call defends against future bloat. Test fixture should include a
  realistic-size prompt to lock the budget.
- **Performance on large session histories**. A heavy user might
  accumulate thousands of rollouts. `discover` walks them once per
  sync run; each file is ~1 KB read for the first line. Even at
  10,000 sessions this is ~10 MB of disk, sub-second. Don't
  preoptimize.
- **Schema drift**. `session_meta` is a versionless event today. If
  Codex changes the field name (`cwd` → `working_dir`, etc.),
  discovery silently stops finding Codex projects. Mitigation: a
  `scribe doctor` row that probes one rollout and asserts the cwd
  field parses non-empty. Easy to add as part of C1.

---

## Out of scope (deliberate)

- Codex session mining (`C3` above — deferred behind 100%-Ollama 4C).
- Codex `memories/`, `rules/`, `skills/` directories — different
  problem (cross-agent rule/skill sharing).
- Auto-installing Codex CLI for users who don't have it. We probe and
  inform; we don't manage their install.
- Reading Codex's SQLite (`state_5.sqlite`, `logs_1.sqlite`,
  `session_index.jsonl`). These are Desktop-private and version-
  bumping. Walking rollouts directly is robust.
- Cursor / Aider / other agent layouts. The C1 code generalizes
  naturally (one `<agent>_sessions_dir` + a `discover<Agent>` helper
  per agent) but we don't stub for hypothetical agents.

---

## Ship checklist

```
[ ] config.CodexSessionsDir + defaults + applyDefaults + init template row
[ ] cmd/scribe/codex.go (readCodexSessionMeta, walkCodexSessions, discoverCodex helpers)
[ ] testdata/codex/rollout-fixture.jsonl
[ ] codex_test.go covers: happy path, empty file, malformed first line, dedup, walk order
[ ] sync.go:discover calls discoverCodex after Claude pass
[ ] manifest.ProjectEntry.DiscoveredFrom + back-compat ("" reads as "claude")
[ ] doctor.go codex_sessions_dir row + missing-dir is WARN not FAIL
[ ] init summary line shows the codex dir status
[ ] integration test: temp ~/.codex with two cwds + a duplicate
[ ] dogfood on Oliver's machine: scribe sync --discover --dry-run lists Codex-only projects
[ ] update CLAUDE.md "Key external surfaces" table with the codex sessions row
[ ] CHANGELOG.md entry under the next version
```

## References

- `.claude/research/2026-05-14-codex-project-discovery.md` — full
  research note with the verbatim `session_meta` example from this
  machine and the comparison table against Claude's layout
- `cmd/scribe/sync.go:295` `discover` — Claude scan we extend
- `cmd/scribe/manifest.go:176` `decodeClaudePath` — the greedy-rebuild
  helper Codex discovery deliberately does NOT need
- `cmd/scribe/doctor.go:175` — Claude row Codex row sits next to
- openai/codex GitHub issue #20864 — confirms Desktop's index files
  exist but we don't depend on them
