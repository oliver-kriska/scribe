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
| A personal KB *and* a team KB on one machine | 1 / N | per-KB |

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
| One weekly consolidation | `scripts/dream-lease.json`, host + contributor keyed, 6h expiry, stealable | Both run `dream` at once. `lock_dir` is machine-local and cannot see across machines. |
| Config trust lock | per-machine snapshot of sensitive keys | A pushed change to source filters, ingestion dirs, capture, the credential gate, or LLM routing applies to every machine silently. |
| Secret-scan commit gate | credential-shaped values held back from commits | Articles carrying a real-looking token commit straight into shared history. |

**Extraction dedup is not on this list.** `scripts/extraction-ledger.json` works
the same whether the flag is set or not — it is keyed on normalized remote URL
plus HEAD SHA, and both the lookup and the semantic merge are unconditional. You
get revision-level dedup on a solo KB too.

One exception worth budgeting for: a machine that has never extracted a given
repository takes the never-extracted branch *before* the ledger is consulted, so
**the first sync on a new machine re-extracts every approved repository once**,
at full model cost, even though a sibling machine already did that revision.
After that first pass the ledger takes over. Preview it with
`scribe sync --dry-run --estimate` before the first real run.

### What turning it on takes away

`team: true` hard-disables **iMessage capture** and **pull integrations** from
the repo config. That is deliberate: they are personal sources, and a pushed
`enabled: true` must not switch them on for every machine reading the repo. The
trusted config snapshot does not restore them either.

Move those blocks into the gitignored `scribe.local.yaml` of the one machine
that should run them. Local config is applied after trust enforcement, so it
always wins.

It also locks sensitive config. Once a machine has trusted a `scribe.yaml`,
later edits to source filters, ingestion dirs, capture, the credential gate or
LLM routing do **not** take effect there — scribe keeps running on the trusted
values and `doctor` reports drift until you run `scribe config diff` and
`scribe config trust` on that machine. If you edit your own config freely, this
is the friction you will notice most.

It also activates the secret gate, which adds a whole-KB audit to
`scribe doctor`. Pre-existing credential-shaped strings in old articles surface
as a new warning — historical, not a regression. New articles containing one are
held back from the commit; watch for `SECRET HELD:` in sync logs.

## Why concurrency is the normal case here

`scribe cron install` writes the **same fixed wall-clock schedule** on every
machine. There is no jitter and no per-machine offset, so two machines fire the
same job in the same minute:

| Job (abridged) | Runs |
|---|---|
| `commit` | hourly |
| `sync` | every 2 hours |
| `sync --sessions` | 3× daily |
| `dream --hot` | daily |
| `lint` | daily |
| the five `lint` mutators | weekly, within one hour of each other |
| `dream` | weekly |
| `pull` | hourly at :17 |

Plan for simultaneous writes as the default, not the exception.

## What is coordinated, and what is not

| Job | Coordinated | How |
|---|---|---|
| `dream`, `dream --hot` | **yes** | committed lease; others see the claim after their pull and skip |
| `sync` project extraction | **yes** | extraction ledger; a revision a teammate already did is skipped |
| `sync --sessions` | no | — |
| `sync` file-inbox drain | no | both machines drain the committed `ingest.inbox_path` after a pull |
| `lint --fix`, `--duplicates`, `--resolve`, `--identities`, `--apply-identities` | no | — |
| `commit` | no | — |
| `capture` | no | convention: one machine only |
| `pull` (bookmark integrations) | no | machine-local cursor in `output/sources/`; convention: one machine only |

Conflicts on scribe-managed shared files are handled by class:

| File | On conflict |
|---|---|
| `wiki/_index.md`, `wiki/_backlinks.json`, `wiki/_digest.md`, `wiki/_hot.md` | either side wins — content regenerates after the pull |
| `wiki/_sessions_log.json`, `wiki/_codex_sessions_log.json` | merged semantically — `processed` unions, later `last_scan` wins — **on `main`; not in a released build yet** |
| `scripts/extraction-ledger.json` | merged semantically |
| `scripts/dream-lease.json` | merged semantically, remote wins the claim |
| `log.md` | union of both sides' lines |
| `scripts/projects.json` | never shared |

### Known limitations

Honest ones, accurate as of 2026-08-20:

- **Some accumulating files are still not merge-aware.**
  `wiki/_unfetched-links.md`, `wiki/_identity-proposals.md`,
  `wiki/_absorb_log.json`, `wiki/_extraction_outcomes.json` and
  `wiki/_contextualized_log.json` are committed, grow from every machine, and
  have no registered conflict handler. The list is not exhaustive.

  An unregistered conflict aborts the rebase, and until it is resolved every
  later pull fails on the same conflict while local commits pile up — so resolve
  one promptly with the [divergence recipe](#divergence-is-normal) rather than
  letting it sit. In practice these files change far less often than the session
  ledgers did, so collisions are occasional rather than daily.

  The two that *were* guaranteed to collide are the session-mining ledgers,
  whose `last_scan` field is rewritten on every `sync --sessions` run. A semantic
  merge for them — and a regenerate-either-side rule for `wiki/_hot.md` — is on
  `main` and lands in the next release. **Until every writer runs a build that
  includes it, expect this conflict and resolve it with the recipe below.** If
  you installed from Homebrew or the shell installer, you do not have it yet.
- **`scribe commit` does not inspect git state before committing.** If a rebase
  is paused or `HEAD` is detached, cron will keep committing onto it and the
  branch will not advance. Never leave a rebase half-finished on a machine whose
  cron is running — see below.
- **The file inbox is shared and undeduplicated.** `ingest.inbox_path`
  (default `raw/inbox`) is inside the committed tree, and every `scribe sync`
  drains it. Two machines that pull the same queued file before either has
  drained it will both convert it, producing duplicate articles. Drop files in
  on the maintenance machine, or accept the occasional duplicate. Note this is
  *not* what `scribe ingest drain` handles — that job works on `output/inbox`,
  which is gitignored and strictly machine-local, so it needs no coordination.
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
   work, verify, `scribe cron install`. On Linux that command only removes macOS
   LaunchAgents — it prints success while cron keeps firing. Comment out the
   scribe block in `crontab -e` instead, and restore it afterwards. A paused rebase plus a live `commit` job
   is the one failure mode that compounds silently.
5. **Credentials never go in a KB.** Provider keys live in
   `~/.config/scribe/config.yaml` or the environment, per machine.

## Divergence is normal

`git status` showing both ahead and behind on a second machine is the expected
steady state, not damage. Reconcile with a rebase, and finish it in one sitting:

```sh
scribe cron uninstall                     # nothing can commit while you work
git branch backup/$(git rev-parse --short HEAD)   # free; do it before deciding
git log --oneline origin/main..HEAD       # what is local-only
git diff --stat origin/main...HEAD        # what it actually changed
git status                                # uncommitted work? see the warning below
```

Rebase, and finish it in one sitting:

```sh
git pull --rebase --autostash
# if it pauses: git rebase --continue, or git rebase --abort
```

If the local commits turn out to be only bookkeeping, resetting is faster than
rebasing:

```sh
git reset --hard origin/main
```

**Read this before you reset.** "Bookkeeping" means commits that touch *only*
`wiki/_index.md`, `wiki/_backlinks.json`, `wiki/_digest.md`, `wiki/_hot.md`,
the lease, the ledger, `log.md`, or a `last_scan` bump. A commit subject
beginning `auto:` is **not** noise — that is where extracted articles land, and
resetting past it destroys model output you already paid for. `reset --hard`
also discards uncommitted tracked changes, which is where articles held back by
the secret gate sit. When in doubt, rebase; the backup branch above is your
undo either way.

Verify before letting cron near it again:

```sh
git symbolic-ref -q HEAD >/dev/null && echo "HEAD attached" || echo "DETACHED — stop"
for d in rebase-merge rebase-apply; do
  [ -d "$(git rev-parse --git-path $d)" ] && echo "REBASE STILL RUNNING — stop"
done
git grep -l '^<<<<<<<' || echo "no conflict markers"
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
