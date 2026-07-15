---
name: scribe-cli
description: Operate the scribe CLI to keep a knowledge-base pipeline healthy — diagnose with doctor/status, run sync/extraction, manage cron and Full Disk Access, and pick the right command for a maintenance goal. Use when the user wants to run scribe, asks which scribe command does X, says their KB is stale/empty or extraction/sync stopped, or is reading `scribe doctor`/`scribe status` output. For authoring KB content use scribe-kb; for the `scribe lint` content-quality queue use scribe-kb-tidy.
---

# scribe CLI — operations agent skill

`scribe` is a single-binary Go CLI that runs a knowledge-base pipeline:
discover repos → extract knowledge → mine agent sessions → absorb URLs → lint →
reindex with qmd. It normally runs unattended on cron. This skill is for
**driving the CLI by hand** — diagnosing health, running a step manually, and
choosing the right command for a goal.

Two sibling skills own the other halves and this skill routes to them:
- **scribe-kb** — authoring/searching KB *content* (articles, drop files, qmd).
- **scribe-kb-tidy** — working the `scribe lint` content-quality queue (split
  bloated, archive rolling, merge thin/self-named-dirs).

## Golden rule: diagnose read-only before you act

Always start with the two read-only commands. They never write and never call
an LLM, so they are safe to run anytime:

```
scribe doctor      # deps, config, cron, state, run-freshness, errors, ledgers — read-only health
scribe status      # scoreboard: raw-by-density, absorb/contextualize progress, backlog, last sync
```

Read the output first, then run the *one* command it points you at. `doctor`
prints a FAIL/WARN per check and ends with the `status` scoreboard; most WARN
lines already name the command that clears them. See
`references/TROUBLESHOOT.md` for how to read each section.

## Respect the daemon (the lock rule)

scribe runs on the user's cron. Two commands take a **machine-wide lock shared
with that daemon**:

- `scribe sync` — the full pipeline (discover → extract → mine → absorb → reindex)
- `scribe dream` — weekly/`--hot` memory consolidation

Run them manually **only** when you know cron isn't mid-run, run **one at a
time**, and **never background them** — a stray backgrounded `sync`/`dream`
blocks the real cron job. Everything else here is safe to run interactively.

## Goal → command (quick map)

| The user wants to… | Command |
|---|---|
| Check if scribe is healthy | `scribe doctor` |
| See what's pending / backlog | `scribe status` |
| Run the whole pipeline now | `scribe sync` (mind the lock rule) |
| Just mine agent sessions | `scribe sync --sessions` |
| Estimate token cost before syncing | `scribe sync --estimate` |
| Score unprocessed sessions by value | `scribe triage` |
| Enroll a repo for extraction | `scribe projects add <path>` |
| Approve/ignore discovered repos | `scribe projects review` |
| Absorb a local file or URL | `scribe absorb <file>` / `scribe ingest url <url> --absorb` |
| Fix frontmatter errors | `scribe lint --fix` |
| Work the lint content queue | → hand off to the **scribe-kb-tidy** skill |
| Author/search KB content | → hand off to the **scribe-kb** skill |
| Check LLM spend | `scribe cost` |
| Install / check the background jobs | `scribe cron install` / `scribe cron status` |
| Restore chat.db access (macOS) | `scribe fda` |

The full command surface (every subcommand, grouped) is in
`references/COMMANDS.md`. When unsure which command fits, read that first rather
than guessing flags.

## After upgrading the binary

If the user rebuilt/reinstalled scribe (`make install`, `brew upgrade`), on
macOS the chat.db Full Disk Access grant is dropped when the binary is replaced.
Re-run `scribe fda` and confirm with `scribe doctor` (the `deps` section shows
the chat.db/FDA check). Verify the live binary first: `which scribe` and
`scribe --version`.

## Safety rules

- **Read-only first.** `doctor`/`status` before any mutating command. Don't run
  `sync`/`dream` to "see what happens" — respect the lock rule.
- **Let the output pick the command.** Don't invent remediation; most WARNs name
  their fix. If unsure, `scribe <cmd> --help`.
- **Don't fabricate KB content** to make a check pass — that's the daemon's job
  and corrupts provenance. Fix configuration/state, not the knowledge.
- **Scope with `-C <path>` or `SCRIBE_KB`** when the user has more than one KB;
  don't assume the cwd is the intended KB (doctor/status print which KB they hit).
- **Commit is the user's call.** The KB auto-commits on cron; run `scribe commit`
  explicitly only when asked.

## What NOT to do

- **Don't background `scribe sync`/`scribe dream`** or run two at once.
- **Don't run content authoring or lint-queue work here** — route to scribe-kb /
  scribe-kb-tidy so each skill stays one job.
- **Don't edit `scribe.yaml` blind** — for team KBs, sensitive-key changes go
  through `scribe config diff` / `scribe config trust`.

## References

- `references/COMMANDS.md` — the full subcommand map, grouped (core / content /
  quality / maintenance / machine).
- `references/TROUBLESHOOT.md` — symptom → doctor section → fix, for the common
  failure modes (extraction stalled, cron off, FDA lost, ollama down, qmd not
  indexed, lint errors).
- The **scribe-kb** and **scribe-kb-tidy** skills (installed alongside) for
  content authoring and the lint content queue respectively.
