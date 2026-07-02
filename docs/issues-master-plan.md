# Open-issues master plan (2026-07-02)

Backlog sweep of all 23 open GitHub issues: triage, priority order, batching, and the
merge/release strategy. Per-issue deep plans live in `docs/issue-NN-*-plan.md`.

Base commit: `49bfd53`.

## Triage

### Implementable now (13 plans, 5 phases)

| Phase | Issues | Theme | Size |
| ----- | ------ | ----- | ---- |
| 1 | #42, #19, #5, residuals (doctor stop-words check; per-KB lock scoping; #28 `--from-sources` if absent) | Small, isolated, low-risk quick wins | S |
| 2 | #21 | Drop authoring CLI + agent skill | M |
| 3 | #22 | Priority lanes for the pending queue | M |
| 4 | #8, #23, #24 | Deeper work: path-keyed manifest, adoption metric, hot-domain dream | L |

> **Sweep finding (2026-07-02): the issue tracker was badly stale against main.**
> Six "open" issues were already fully implemented in the v0.3.0 push (2026-06-30) and
> just never closed: #9 (`7cfcbda`+`8829e27`), #25 (`4c2522e`), #26 (`29edaff` +
> follow-ups `0366938`/`beb06f9`/`d5bb056`), #27 (`2fc3ac1` + earlier), #28+#41
> (`1c73501`). All six closed with evidence comments. Every remaining phase item was
> re-verified as genuinely unimplemented (no code, no commit, no CHANGELOG entry).
> Residuals that survived the audit: `scribe doctor` can't see held stop-word files
> (#25 leftover); the /tmp lock is machine-wide per job-type, not per-KB (#26
> leftover); `projects add --from-sources` may be missing (#28 optional half).

Rationale for the order:

- **Phase 1** items are each isolated (embedded prompt text, a log heartbeat, one
  sanitizer code path, doctor/status output fixes). They de-risk the session and the
  #26-guard (cron install refusing to clobber another KB's plists) closes an active
  data-loss-shaped footgun immediately.
- **Phase 2** follows the maintainer's own roadmap order (#6: drop CLI first — "every
  other feature benefits from cleaner input") and lands the second-KB-test DX fixes
  (#28/#41 explicitly overlap; one implementation, one branch).
- **Phase 3**: #9 is the stated precondition for decomposing the three `nolint:gocognit`
  drivers, and a stub LLM provider also makes every later phase more testable — worth
  landing before the big features. #22 subsumes the two-lane budget in `sync --sessions`.
- **Phase 4**: #8 is "right to do before more team features pile onto basename keys" —
  so it must land before #26. #23/#24 finish the roadmap umbrella.
- **Phase 5**: #26 rewires cron + config; everything else should be stable first, and it
  builds on #8 (identity) and #27 (KB-scoped checks).

### Not implementable in this pass

| Issue | Disposition |
| ----- | ----------- |
| #2 codesigning | **Blocked on Oliver**: needs a paid Apple Developer account ($99/yr). No code until then. |
| #3 branch protection | Conflicts with the recorded Scorecard posture decision (branch-protection = won't-fix for the solo direct-commit workflow). Partial, non-breaking subset possible: a ruleset blocking only force-pushes/deletions on main (does not require PRs or checks). Do that, comment on the issue, leave close/won't-fix to Oliver. |
| #40 OKF export | Explicitly gated on OKF adoption + a concrete consumer — both zero today. Park. (The small `resource:` frontmatter key could ride along later; out of scope now.) |
| #7 TUI, #10 stamps, #11 review-branch, #14 neighbors | Parked by explicit maintainer verdicts recorded in the issues. No action. |
| #12 accepted edge cases | Tracking issue only. No action. |
| #4, #6 umbrellas | Close when their last children (#19; #21–24) land. |

## Merge strategy (the "how do we get this to main" question)

**No PRs.** Standing decision for this repo: solo maintainer, unprotected main,
agent-authored changes commit directly. PRs would create review queues with zero
reviewers and 13 branches' worth of rebase conflicts.

Instead:

1. **One worktree branch per issue-group** (not per file, not one mega-branch).
   Groups: `#28+#41` are one branch; `#27+#26-guard` are one branch; everything else
   is its own branch. Subagents (Sonnet) implement in isolated worktrees.
2. **Sequential merge into local main in phase order**, me resolving conflicts at merge
   time. After every merge: `make check` (test + vet). Phases are parallel *within*,
   sequential *between* — each phase's branches fork from post-previous-phase main, so
   conflict windows stay small.
3. **`make ci` (test+vet+race+lint+vuln) at the end of each phase** before pushing
   `main` to origin. Push per phase, not per issue — CI runs once per phase.
4. **One release at the very end**, not per issue: after the final `make ci` is green
   and Oliver has reviewed the summary, tag `v0.x.y` once → GoReleaser builds → brew
   formula updates. The tag is the only step left to Oliver.
5. Issues are closed via commit-message `Closes #N` footers when the phase is pushed.

Why not "everything directly on main": parallel subagents would trample each other's
working tree. Worktrees give parallelism; sequential local merges give the
single-integration-point simplicity of main-only development.

## Standing constraints for every implementation branch

- Base check first: `git rev-parse HEAD` must match the SHA given in the task; agent
  worktrees have snapshotted stale bases before.
- `make build` only builds (`./bin/scribe`); never run `make install` (replaces the
  live cron binary and kills the chat.db FDA grant).
- Never run `scribe sync` / `scribe dream` outside `go test` (machine-wide /tmp lock
  shared with real cron). Never `scribe init` outside `t.TempDir()` tests.
- No new go.mod deps without written justification in the plan.
- Tests: KB fixtures from embedded templates in `t.TempDir()`; ccrider fixtures under
  `testdata/`; `make test` must pass offline.
