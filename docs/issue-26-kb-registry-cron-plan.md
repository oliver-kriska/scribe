# Issue #26 — KB-agnostic scheduler + KB registry

## STATUS: already implemented and released (v0.3.0, 2026-06-30). Do not re-implement.

This document was commissioned as a greenfield implementation plan for GitHub
issue #26. Investigation of `main` before writing any implementation steps
found that #26 (and its sibling #27) are **already fully implemented, tested,
and shipped** — the GitHub issue is simply stale (never closed after the
release). Building a "from scratch" implementation on top of this would
duplicate or regress working, tested code.

This document instead: (1) records the evidence that the feature is done, (2)
maps every one of the original design points to the exact code that
implements it, so a reader doesn't have to re-derive that, and (3) describes
the **one genuinely open residual item** plus the housekeeping (`gh issue
close #26`) that should follow. If an implementer agent lands here expecting
a build task, the correct action is: skip to [Residual work](#residual-work),
do that (small, optional), and close the issue.

---

## 1. Problem & context

The original problem (from the GitHub issue, opened 2026-06-12): `scribe cron
install` was per-KB, but LaunchAgent labels were `com.scribe.<name>` with no
KB discriminator — installing from a second KB silently overwrote the first
KB's plists (old `cron.go:189` in the pre-#26 code). Direction from the
maintainer: cron becomes machine-level; agents run `scribe`, and `scribe`
itself reads a registry of KBs and decides what to run in each from that KB's
own config.

### Evidence this is done

- `git log --oneline --grep="#26"` on `main` shows the base implementation
  and three explicit follow-ups, all already merged:
  - `29edaff feat(cron): KB-agnostic scheduler + KB registry (issue #26)`
  - `5d35749 fix(registry): kb_dir must stay in the rotation when kbs: is non-empty`
  - `0366938 feat(each): per-KB scheduler cadence gating (#26 follow-up 1)`
  - `beb06f9 feat(budget): machine-level output-token ceiling across KBs (#26 follow-up 2)`
  - `d5bb056 feat(watch): multi-KB watcher serving every registered KB (#26 follow-up 3)`
- `CHANGELOG.md`, `## [0.3.0] — 2026-06-30`, section "### Added — KB registry
  + KB-agnostic scheduler (#26)" documents the feature as shipped to users.
- Dedicated test files exist and pass as part of `make test`: `registry_test.go`
  (148 lines), `each_cadence_test.go` (150 lines), `budget_machine_test.go` (77
  lines), `watch_multikb_test.go` (101 lines), `doctor_cron_scope_test.go` (29
  lines) — roughly 500+ lines of purpose-built coverage.
- Issue #27 ("doctor/status: KB-scope the global-state checks"), listed in the
  original brief as a hard prerequisite landing before #26, is **also fully
  done** — commit `2fc3ac1`'s message states outright: *"Three remaining items
  (2,3,6 landed earlier)"*, confirming all six items of #27 are shipped, not
  just items 1/4/5 as assumed when this plan was commissioned.

---

## 2. Design decisions — as actually implemented

Every open question in the original brief was in fact settled. This section
documents the choice **that shipped**, with the file:line where it lives, so
nobody re-litigates it.

### (a) `kbs:` registry generalizes `kb_dir`, single-KB installs migrate with zero changes

- `cmd/scribe/config.go:765-773` — `userConfig` struct: `KBDir string
  \`yaml:"kb_dir"\`` stays; `KBs []string \`yaml:"kbs,omitempty"\`` added.
- `cmd/scribe/registry.go:24-45` — `registeredKBs()` unions `kb_dir` (always
  first) with the `kbs:` list, deduped by absolute path, filtered to valid KB
  roots (`isKBRoot`). A KB with no `kbs:` entries at all still gets exactly
  `[kb_dir]` — this is the zero-change migration path.
- `cmd/scribe/registry.go:49-59` (`kbRegistered`) and `:65-108`
  (`registerKB`/`unregisterKB`) implement idempotent add/remove that preserve
  the rest of the YAML file (comments, `kb_dir`) via text-level insertion
  (`appendKBEntry` / `removeKBEntry`, `registry.go:114-155`).
- Rejected alternative (implicit in the shipped design, confirmed by
  `registry.go:17-23` doc comment): registry entries do **not** replace
  `kb_dir` — dropping `kb_dir` from the rotation when `kbs:` becomes non-empty
  was explicitly identified and fixed as a bug (`5d35749`).

### (b) `com.scribe.*` agents become KB-agnostic; command surface is `scribe each -- <job>`

- `cmd/scribe/cron.go:60-167` (`scribeJobs`) — every scheduled job's `Command`
  is built via the `each := func(sub string) string { return binary + " each
  -- " + sub }` closure (`cron.go:62`), e.g. `scribe each -- sync --max 2`.
  Only the long-running `watch` agent runs the bare binary (`cron.go:160-165`)
  because it's inherently multi-KB internally (see (issue watcher) below).
- Chosen surface: **`scribe each -- <subcommand>`**, not a hypothetical
  `sync --all-kbs` flag threaded through every command. Rationale (implicit
  in the implementation): keeps per-command flag surfaces untouched;
  KB-iteration is a single cross-cutting wrapper (`cmd/scribe/each.go`)
  instead of being bolted onto N commands individually.
- Plist rewrite on upgrade: `CronInstallCmd.Run` (`cron.go:498-583`) always
  regenerates and reloads every plist (subject to `--force` for existing
  files, `cron.go:551-554`); no separate "upgrade" mode was needed because
  install is idempotent and detects legacy state itself (next point).
- Legacy-install migration: `otherKBServedByAgents` (`cron.go:219-235`) reads
  existing on-disk plists and recovers the `cd "<root>"` a pre-#26 single-KB
  plist embedded (`plistKBRoot`, `cron.go:204-217`). `CronInstallCmd.Run`
  (`cron.go:526-543`) registers *both* the new KB and, if found, the legacy
  KB, before overwriting the plists — so migrating never silently drops a KB
  from the schedule. `doctor`'s `cronScopeCheck` (`doctor.go:751-772`) is the
  "notice" mechanism: it detects a legacy plist pointing elsewhere and prints
  `Fix: scribe cron install   # migrate to the KB-agnostic (scribe each) scheduler`.

### (c) Per-KB behavior from `scribe.yaml`/`scribe.local.yaml`

- Capture self-chat gating: `capture.go:96-98` — `CaptureCmd.Run` errors
  immediately (`"no self-chat handle configured..."`) when
  `resolveSelfChatHandles(cfg.Capture)` (`capture.go:344-354`) is empty. Under
  `scribe each`, this becomes a per-KB failure that's logged and skipped —
  see (e) below — so a KB without capture configured simply no-ops on that
  job every tick without operator action.
- Dream weekly cadence: the shared cron trigger fires `scribe each -- dream`
  once (Sun 2am, `cron.go:97-102`); each KB's dream then runs through the same
  advisory lock (`dream.go:53`, `lockPathFor(cfg.LockDir, "dream")`) and, for
  team KBs, a committed leader-lease (`dream_lease.go`, referenced at
  `dream.go:64`, "Team KBs coordinate the weekly dream through a committed
  lease") so only one machine actually runs the consolidation on a shared KB.
- Team KBs pull-before-sync: `sync.go:184-211` (`pullPhase`) — gated by
  `cfg.Team` and `sync.always_pull_before_sync` via `pullBeforeSyncEnabled`
  (`sync.go:191`), runs before extraction on every `scribe sync` invocation
  regardless of whether it was invoked directly or via `scribe each`.

### (d) Per-KB cadence gating via existing run records; no per-KB plist schedules

- `cmd/scribe/each.go:27-29` (`EachConfig.Cadence map[string]string`, `yaml:"cadence"` under `each:` in `scribe.yaml`, wired at `config.go:151`).
- `each.go:109-128` (`cadenceSkipReason`) reads `output/runs/*.jsonl` via
  `loadRunRecords(kb)` and skips a KB's job for this tick when its last
  successful run is younger than the configured interval. **Fails open**: no
  cadence configured, unreadable records, or "never run" all mean "run it"
  (`each.go:111-123` — this is a documented, deliberate choice, not an
  oversight).
- Key resolution: `eachJobKeys` (`each.go:135-155`) derives a specific key
  (command + first `--flag`, e.g. `"sync --sessions"`) and a bare-command
  fallback key (`"sync"`), so one `cadence: {sync: 2h}` entry paces every mode
  of that command family (`cadenceInterval`, `each.go:160-177`).
- Durations accept a day suffix (`parseCadenceDuration`, `each.go:179-192`,
  `"7d"`/`"1.5d"`) because Go's `time.ParseDuration` has no day unit and a
  weekly cadence as `"168h"` reads poorly.
- Confirms: **no per-KB plist schedules exist** — every registered KB is
  offered every job on every tick; cadence is the only throttle, and it lives
  in each KB's own `scribe.yaml`, matching the "per-KB scribe.yaml with
  defaults" option from the original brief.

### (e) Failure isolation — one KB erroring never blocks the rest

- `each.go:71-102` (`EachCmd.Run`) loops `registeredKBs()` sequentially; a
  child failure increments `failed` and logs `"FAILED: %v (continuing)"`
  (`each.go:92`) but the loop does not `return` — the function always returns
  `nil` (`each.go:101`, comment: *"failure isolation: a per-KB error never
  fails the tick"*), so launchd never sees a non-zero exit and never disables
  the agent.
- Locks: **not per-KB** — `lockfile.go:49` keys a lock purely by job name
  (`"scribe-" + name + ".lock"`) inside `cfg.LockDir`, which defaults to the
  machine-wide `/tmp` (`config.go:588`, `LockDir: "/tmp"`). Because
  `each.go`'s loop runs KBs **sequentially within one process** (not
  concurrently), this does not cause data races, but it does mean a slow KB's
  `sync` lock can make a *subsequent, overlapping* `scribe each -- sync` cron
  tick (fired before the first finished) bail out entirely — including for
  KBs the first tick already finished. See [Residual work](#residual-work).

### (f) Machine-level output-token budget ceiling

- `config.go:799-806` — new `userConfig.DailyOutputTokenCeiling int64
  \`yaml:"daily_output_token_ceiling,omitempty"\`` lives in the **global**
  `~/.config/scribe/config.yaml`, distinct from the pre-existing per-KB
  `SyncConfig.DailyOutputTokenCeiling` (`config.go:511-522`, in each KB's
  `scribe.yaml`).
- `budget.go:99-121` (`checkBudget`) checks **both**: the per-KB `limit` first
  (existing behavior), then `machineOutputTokenCeiling()` (`budget.go:125-127`)
  against `getMachineDailyMeteredOutputTokens()` (`budget.go:157-177`), which
  sums `readDailyMeteredOutputTokens` (`budget.go:184-211`) across **every**
  `registeredKBs()` entry's `output/costs/<day>.jsonl`. Either ceiling being
  zero disables that half independently; `SCRIBE_BYPASS_BUDGET=1` bypasses
  both (`budget.go:107-109`).
- Local providers (`ollama`, `llamacpp`) are exempt from both ceilings
  (`budget.go:51-61`, `isLocalProvider`) — matches the pre-existing per-KB
  semantics, just extended machine-wide.
- Caching: a second, separate 30s cache (`machineBudgetState`, `budget.go:155`)
  keeps the cross-KB fan-out off the hot path, independent from the existing
  per-KB `budgetState` cache so the two sums are never confused.

### (g) `cron install`/`status`/`uninstall` UX becomes KB-independent; registry surface is `scribe kb add/list/remove`

- `cmd/scribe/kb.go` — `KbCmd` with `Add`/`List`/`Remove` subcommands, wired
  into the CLI root at `cmd/scribe/main.go:72` (`Kb KbCmd \`cmd:"" group:"system"...\``).
  This is the chosen surface — a dedicated `scribe kb` command group, not a
  flag folded into `cron install`.
  - `KbAddCmd.Run` (`kb.go:20-41`) defaults to the current KB via `kbDir()`
    when no path is given, then calls `registerKB`.
  - `KbListCmd.Run` (`kb.go:45-60`) prints the registry with a `(default)`
    marker on whichever entry matches `kb_dir`.
  - `KbRemoveCmd.Run` (`kb.go:66-78`) calls `unregisterKB`; `kb_dir` itself is
    left untouched by design (`registry.go:90-92`) — it's the default, not a
    registry membership, so "remove" can't accidentally orphan bare commands.
- `CronStatusCmd.Run` (`cron.go:589-614`) no longer needs a KB context: it
  prints the full registry first, then per-agent LaunchAgent state — the same
  output regardless of which KB's directory you run it from.
- `CronUninstallCmd.Run` (`cron.go:622-642`) unloads/removes the fixed
  machine-level job set; also KB-independent.
- `CronInstallCmd.Run` (`cron.go:498-583`) auto-registers the current KB
  (unless it's a throwaway/temp path — `isThrowawayPath` guard, `cron.go:527`)
  before writing plists, so "install" and "register" are the same action from
  the user's point of view — no separate `scribe kb add` step is required on
  first install.

---

## 3. Implementation steps

None — the feature is implemented. If this document is being read by an
agent tasked with "implementing issue #26", the correct step is to run the
verification in [§4](#4-test-plan-verification) and then do the
[Residual work](#residual-work) below (which is small and optional), not to
write new registry/each/budget/watch code.

---

## 4. Test plan (verification)

To confirm the above is actually in the working tree (not just described),
an implementer should run, from the repo root, no network access required:

```sh
make test          # go test ./... -tags sqlite_fts5 — includes all files below
go test ./cmd/scribe/... -tags sqlite_fts5 -run 'TestRegistry|TestEach|TestBudgetMachine|TestWatchMultiKB|TestDoctorCronScope' -v
make check          # test + vet
```

Existing fixtures already cover the acceptance criteria that a from-scratch
plan would have specified:

- `registry_test.go` — dedup, `kb_dir`-always-present, invalid-path skipping,
  idempotent add/remove, comment-preserving YAML edits.
- `each_cadence_test.go` — fail-open on missing/unparseable cadence, specific
  vs. bare-command key precedence, day-suffix duration parsing.
- `budget_machine_test.go` — machine ceiling summed across multiple KB cost
  ledgers, local-provider exemption, bypass env var.
- `watch_multikb_test.go` — a session is only dropped from the queue once
  *every* registered KB has processed it (`processedByAllKBs`).
- `doctor_cron_scope_test.go` — legacy single-KB plist detection vs.
  registered-KB "ok" state.

No new test fixtures are needed unless the residual lock item below is
picked up.

---

## 5. Risks & edge cases — as already handled

| Risk | Handling | Citation |
|---|---|---|
| Single-KB installs must migrate with zero user action | `kb_dir` always included in `registeredKBs()` even with empty `kbs:` | `registry.go:24-45` |
| One KB erroring must not block others | loop continues past a child failure, always returns `nil` | `each.go:71-102` |
| A KB directory that was deleted but still registered | `registeredKBs()` filters through `isKBRoot(abs)`, silently drops non-existent/non-KB paths every call | `registry.go:38-40` |
| Legacy pre-#26 single-KB plist still installed | detected via embedded `cd "<root>"`, both KBs re-registered before overwrite, `doctor` surfaces a fix hint | `cron.go:219-235`, `:526-543`; `doctor.go:751-772` |
| Cron install from a throwaway/temp KB must never bind the machine schedule | `isThrowawayPath` guard skips auto-registration; `writeGlobalState` chokepoint has no bind override for `cron install` | `cron.go:527`, `:556-564` |
| Budget ceiling double-counting or missing a KB | `registeredKBs()` is the single dedup source used by both the per-tick scheduler and the machine budget sum | `budget.go:170-171` |
| Machine-wide lock granularity (the one open item) | currently one lock per job-type name in a shared `/tmp` dir, not per-KB — see below | `lockfile.go:49`, `config.go:588` |

---

## 6. Interactions with other open issues

- **#27** (KB-scope doctor/status global-state checks) — also fully shipped
  (see §1). No interaction risk remains; do not re-plan.
- **#8** (path-keyed manifest identity) — genuinely still open, unrelated to
  the registry/scheduler work; the manifest identity problem (basename
  collisions) is orthogonal to which KBs a machine iterates.
- Any other issue in a backlog phase list that references "#26 guard" as a
  prerequisite (e.g. a short-term clobber guard) should be re-read: the
  guard was explicitly superseded by the real registry per the original
  issue text ("Short-term guard... can land independently... cheap") — if
  such a guard was never separately built, it's now moot, since `cron
  install` no longer clobbers at all (`cron.go:521-543`).

---

## Residual work

One small, optional item was found, not required to close #26 but worth a
follow-up if the maintainer wants it:

**Per-KB lock scoping.** `lockPathFor` (`lockfile.go:49`) keys locks by job
name only, inside `cfg.LockDir` which defaults machine-wide to `/tmp`
(`config.go:588`). Under `scribe each`, this is safe (KBs run sequentially
in one process, so no two `sync` calls are ever concurrent), but it means an
overlapping cron tick (e.g. a slow `sync-projects` run still processing KB 3
of 5 when the next 2-hour tick fires) causes the *entire* new tick to skip —
including KBs the first tick already finished — rather than just skipping
the busy KB. Two options, either landable independently:

1. Key the lock by `(job name, KB root)` instead of just job name, so
   `LockDir` stays global but two different KBs' `sync` runs never contend.
   Smallest change: `lockPathFor(lockDir, name, kbRoot)` and update the ~4
   call sites (`capture.go:65`, `commit.go:31`, `dream.go:53`, `sync.go:89`,
   `projects.go:87`).
2. Leave it as-is and document the trade-off (a single "is sync already
   running anywhere" gate) as intentional — it already fails safe (skip, not
   corrupt) and each.go's failure/skip counters already surface it in logs.

**Recommendation:** option 2 (document, don't change) unless the maintainer
has actually observed cron ticks being starved by this in practice — the
current behavior is safe, just occasionally conservative, and per-KB lock
keys would need their own test fixture to avoid silently breaking the
existing "prevent two concurrent syncs of the same KB" guarantee.

## Size estimate

- Verification only (recommended): **S** (~30 min, `make test` + `make check`,
  then `gh issue close 26` and `gh issue close 27` with a comment pointing at
  this document and the CHANGELOG entry).
- If the residual lock item is picked up: **S** (~1-2 hours, ~40-60 LOC across
  `lockfile.go` + 5 call sites + one new test in a `lockfile_test.go`).
