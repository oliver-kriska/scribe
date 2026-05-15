# Release reminder (warn-only) — and why there's no automation

Refreshing getscribe.dev is a **manual** act: you invoke the
`getscribe-site-sync` skill when you decide the page should be updated. There
is deliberately no hook, cron, or headless `claude -p` that deploys the public
site on its own. Auto-deploying a marketing site from a git tag is hidden magic
with real blast radius (a bad generation goes live unattended) — not worth it
for a page that changes a few times a year.

## The only optional piece: a warn-only hook

`scripts/hooks/reference-transaction` (marker `GSS-WARN-v1`) does exactly one
thing: when a new `refs/tags/vX.Y.Z` is created, it prints a short reminder to
stderr that getscribe.dev may be stale and that you can run the skill when
ready. It never deploys, never spawns a process, never calls an LLM. Git has
no `post-tag` hook; `reference-transaction` (git ≥ 2.28) is the cleanest signal
for "a tag was just created" and — unlike `pre-commit`/`pre-push`, which this
repo already customises — it is otherwise unused here, so installing it touches
nothing else.

Install (idempotent, chain-safe):

```sh
bash "$(git rev-parse --show-toplevel)/.claude/skills/getscribe-site-sync/scripts/install-release-hook.sh"
```

If a foreign `reference-transaction` hook already exists, the installer
preserves it (`reference-transaction.pre-gss`) and installs a small dispatcher
that runs the original first (stdin tee'd to both) then the warn hook. Re-run
the installer after a fresh clone — `.git/hooks/` is not version-controlled.
The skill files themselves are committed (`.claude/skills/` is un-ignored), so
the logic travels with the repo; only the local reminder wiring doesn't.

To remove the reminder entirely, just delete `.git/hooks/reference-transaction`
(or the `.pre-gss` original back, if it was chained).

## What still has to be done by hand

Everything: reviewing the CHANGELOG delta, deciding whether the marketing copy
actually needs to change, running the skill's audit/deploy/verify, and
committing. That is the point — the skill makes the manual run fast and safe;
it does not make it automatic.
