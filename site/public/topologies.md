# Topologies — who writes to your KB, and what that costs

`scribe` is single-user by design, but a KB is a plain git repository, so more
than one machine can write to it. This page covers every supported arrangement,
what coordination each one needs, and what is still on you.

For the exact commands, see the [setup runbook](https://getscribe.dev/setup.md).
This page is the reference behind it.

## The only question that matters

Not how many people. **How many machines write to one KB.**

A solo developer with a desktop and a laptop has exactly the same coordination
problem as a five-person team, because the thing being coordinated is concurrent
git writes, not humans. `scribe.yaml` spells that switch `team: true`, which is
a misleading name — read it as *multi-writer*.

| Your situation | Writers per KB | `team: true` |
|---|---|---|
| One machine, one KB | 1 | no |
| Desktop + laptop, both yours | 2 | **yes** |
| A team, one machine each | N | **yes** |
| A team where some people have two machines | N | **yes** |
| macOS and Linux writers mixed | N | **yes** |
| A personal KB *and* a team KB on one machine | 1 each | per-KB |

Only the last row is a different problem — see
[A second KB on one machine](https://getscribe.dev/setup.md#a-second-kb-on-one-machine).
Everything else is the same recipe.

## Single writer

Nothing to coordinate. `scribe init`, `scribe cron install`, done. A git remote
is optional and serves as backup, not as a merge layer.

Leave `team: true` unset. Turning it on costs you iMessage capture and pull
integrations from `scribe.yaml` (see below) for no benefit.

## More than one writer

Follow [One KB on several machines](https://getscribe.dev/setup.md#one-kb-on-several-machines)
for a solo multi-device setup, or
[Team owner](https://getscribe.dev/setup.md#team-owner-create-and-publish) and
[Team member](https://getscribe.dev/setup.md#team-member-clone-and-onboard) for a
shared KB. The mechanics are identical; only who runs step 1 differs.

Three guarantees switch on with `team: true`, and each fails silently when it
is missing:

| Guarantee | Mechanism | Without it |
|---|---|---|
| Per-machine project manifest | `scripts/projects.json` gitignored | It stays committed while holding machine-local absolute paths, extracted SHAs and approval decisions. Every `sync --discover` rewrites it with the other machine's view. |
| Extraction dedup | `scripts/extraction-ledger.json`, keyed on normalized remote + HEAD SHA | Both machines extract the same revision, pay the model bill twice, and write duplicate articles. |
| One weekly consolidation | `scripts/dream-lease.json`, host + contributor keyed, 6h expiry, stealable | Both run `dream` at once. `lock_dir` is machine-local and cannot see across machines. |

### What turning it on takes away

`team: true` hard-disables **iMessage capture** and **pull integrations** from
the repo config. That is deliberate: they are personal sources, and a pushed
`enabled: true` must not switch them on for every machine reading the repo. The
trusted config snapshot does not restore them either.

Move those blocks into the gitignored `scribe.local.yaml` of the one machine
that should run them. Local config is applied after trust enforcement, so it
always wins.

It also activates the secret gate, which adds a whole-KB audit to
`scribe doctor`. Pre-existing credential-shaped strings in old articles surface
as a new warning — historical, not a regression. New articles containing one are
held back from the commit; watch for `SECRET HELD:` in sync logs.

## Why concurrency is the normal case here

`scribe cron install` writes the **same fixed wall-clock schedule** on every
machine. There is no jitter and no per-machine offset, so two machines fire the
same job in the same minute:

| Job | Runs |
|---|---|
| `ingest drain` | every 30 minutes |
| `commit` | hourly |
| `sync` | every 2 hours |
| `sync --sessions` | 3× daily |
| `dream --hot` | daily |
| `lint` | daily |
| the five `lint` mutators | weekly, within one hour of each other |
| `dream` | weekly |

Plan for simultaneous writes as the default, not the exception.

## What is coordinated, and what is not

| Job | Coordinated | How |
|---|---|---|
| `dream`, `dream --hot` | **yes** | committed lease; others see the claim after their pull and skip |
| `sync` project extraction | **yes** | extraction ledger; a revision a teammate already did is skipped |
| `sync --sessions` | no | — |
| `ingest drain` | no | — |
| `lint --fix`, `--duplicates`, `--resolve`, `--identities`, `--apply-identities` | no | — |
| `commit` | no | — |
| `capture` | no | convention: one machine only |

Conflicts on scribe-managed shared files are handled by class:

| File | On conflict |
|---|---|
| `wiki/_index.md`, `wiki/_backlinks.json`, `wiki/_digest.md` | either side wins — content regenerates after the pull |
| `scripts/extraction-ledger.json` | merged semantically |
| `scripts/dream-lease.json` | merged semantically, remote wins the claim |
| `log.md` | union of both sides' lines |
| `scripts/projects.json` | never shared |

### Known limitations

Honest ones, current as of this writing:

- **Some accumulating files are not yet merge-aware.**
  `wiki/_sessions_log.json`, `wiki/_unfetched-links.md`,
  `wiki/_identity-proposals.md` and `wiki/_hot.md` are committed and grow from
  every machine, but have no registered conflict handler. Concurrent writes can
  produce a hard conflict that must be resolved by hand once.
  `wiki/_sessions_log.json` is the most exposed, because `sync --sessions` fires
  at the same times on every machine and each machine adds different session IDs.
- **`scribe commit` does not inspect git state before committing.** If a rebase
  is paused or `HEAD` is detached, cron will keep committing onto it and the
  branch will not advance. Never leave a rebase half-finished on a machine whose
  cron is running — see below.
- **`capture` is coordinated by convention only.** Nothing stops two machines
  running it against the same KB.

## Operating rules for a multi-writer KB

1. **One machine owns maintenance.** Throttle the uncoordinated mutators
   everywhere else with `each.cadence` in that machine's gitignored
   `scribe.local.yaml`. The
   [runbook](https://getscribe.dev/setup.md#one-kb-on-several-machines) has the
   block to paste. Leave `dream` enabled everywhere — the lease makes it safe,
   and the weekly cycle still runs when your main machine is asleep.
2. **One machine owns capture.** On macOS, Messages is synced across devices, so
   every Mac signed into the same account sees identical messages. Capturing on
   two double-ingests every URL. The per-machine Full Disk Access grant is a
   useful forcing function — simply never run `scribe fda` on the others.
3. **Match git's pull behaviour to cron's.** scribe always pulls with
   `--rebase --autostash`; a hand-run `git pull` does not, and fails on a
   diverged clone. On every writer:

   ```sh
   git config pull.rebase true
   git config rebase.autoStash true
   ```

4. **Stop cron before any manual git surgery.** `scribe cron uninstall`, do the
   work, verify, `scribe cron install`. A paused rebase plus a live `commit` job
   is the one failure mode that compounds silently.
5. **Credentials never go in a KB.** Provider keys live in
   `~/.config/scribe/config.yaml` or the environment, per machine.

## Divergence is normal

`git status` showing both ahead and behind on a second machine is the expected
steady state, not damage. Reconcile with a rebase, and finish it in one sitting:

```sh
scribe cron uninstall                     # nothing can commit while you work
git log --oneline origin/main..HEAD       # what is local-only
git diff --stat origin/main...HEAD        # what it actually changed
```

If the local commits are only scheduled noise, discard them:

```sh
git reset --hard origin/main
```

Otherwise rebase behind a backup branch:

```sh
git branch backup/$(git rev-parse --short HEAD)
git pull --rebase --autostash
# if it pauses: git rebase --continue, or git rebase --abort
```

Verify before letting cron near it again:

```sh
git symbolic-ref -q HEAD >/dev/null && echo "HEAD attached" || echo "DETACHED — stop"
[ -d "$(git rev-parse --git-path rebase-merge)" ] && echo "REBASE STILL RUNNING — stop"
grep -rl '<<<<<<< ' wiki raw 2>/dev/null | head
git push && scribe doctor
scribe cron install
```

## Two readings that look like failures and are not

- **`scripts/projects.json — 0 projects` on a new machine.** Discovery only
  enrolls a path that exists on disk. A fresh machine has the session history but
  not the repositories. Clone the repositories you will work on there, re-run
  `scribe sync --discover`, then `scribe projects review`. Session mining stays
  at `0 pending` until projects exist.
- **A lower qmd file count than your other machine.** Derived artifacts are
  gitignored and do not clone. Compare a clone against
  `git ls-files '*.md' | wc -l`, never against the other machine's total.

Freshness warnings reading `never run` clear themselves once cron has fired once.
