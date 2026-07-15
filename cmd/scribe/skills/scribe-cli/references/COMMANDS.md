# scribe command map

Every subcommand, grouped by job. Run `scribe <cmd> --help` for the full flag
set of any command. Commands marked **read-only** never write KB state or call
an LLM — safe to run anytime. Commands marked **lock** take the machine-wide
lock shared with cron — run one at a time, never backgrounded.

## Core

| Command | What it does |
|---|---|
| `scribe sync` | **lock** — full pipeline: discover → extract → mine sessions → absorb → reindex → commit |
| `scribe sync --sessions` | Mine agent sessions only (Claude Code via ccrider + Codex CLI when enabled) |
| `scribe sync --estimate` | Token estimate for pending work — no LLM calls |
| `scribe sync --max-absorb N` | One-shot override of `absorb.max_per_run` |
| `scribe status` | **read-only** — scoreboard: raw-by-density, absorb/contextualize progress, backlog, last sync, qmd/ollama health |
| `scribe doctor` | **read-only** — health check: deps, config, localmode, convert, cron, state, freshness, errors, contradictions, stale, vault. Ends with the status scoreboard |
| `scribe doctor --section <name>` | Run one section only (deps \| config \| cron \| state \| freshness \| errors \| …) |
| `scribe commit` | Auto-commit and push pending KB changes |
| `scribe watch` | Long-running watcher on the ccrider DB for near-real-time session extraction (launchd-friendly) |

## Content (author / ingest)

| Command | What it does |
|---|---|
| `scribe drop --title … --type … --domain …` | Author a validated drop file for the current project's KB handoff dir (the scribe-kb path) |
| `scribe write` | CLI write surface: create an article or append to rolling memory |
| `scribe absorb <file>` | Absorb a local file (md/txt/html) end-to-end: ingest → contextualize → absorb |
| `scribe ingest url <url> [--absorb]` | Queue a URL (fast) or ingest+absorb it in one shot |
| `scribe ingest drain` | Process queued URLs from `output/inbox/` into `raw/articles/` |
| `scribe ingest file <path>` | Convert + ingest a local PDF/DOCX/EPUB/HTML/TXT/MD |
| `scribe capture` | Pull URLs you iMessaged to yourself into `raw/articles/` (macOS; needs Full Disk Access) |
| `scribe contextualize --scope raw\|wiki\|all` | Insert LLM retrieval-context paragraphs for better qmd search |

## Projects (extraction sources)

| Command | What it does |
|---|---|
| `scribe projects list` | List discovered projects with status |
| `scribe projects review` | Interactively approve/ignore each pending project |
| `scribe projects approve [--all]` | Approve pending project(s) for the pipeline |
| `scribe projects add <path> [--local] [--domain d]` | Enroll a repo by hand (no session history needed) |
| `scribe projects add --from-sources` | Bulk-enroll every `sources.include`-listed repo |
| `scribe projects ignore <names>…` | Remove project(s) and block re-discovery |
| `scribe scan <path>` / `scribe deep <project>` / `scribe assess <project>` | Pre-scan / deep-extract / one-shot parallel deep assessment |

## Quality (lint, ledgers, structure)

| Command | What it does |
|---|---|
| `scribe lint` | **read-only** — frontmatter + size + orphan checks, grouped by class; ends with a "To fix, run:" footer. `-v` per-file, `-q` errors-only |
| `scribe lint --fix` | Repair mechanical frontmatter (duplicate keys, list/date formatting, invalid domains, defaults, `*.md.md`) |
| `scribe lint --contradictions` | LLM pass for factual disagreements across articles |
| `scribe link` | Link orphan articles to contextual hosts via See Also sections |
| `scribe tier {compute,list,set,write}` | `index_tier` hint (stub\|brief\|standard\|deep\|reference); `tier write --missing-only` backfills |
| `scribe sections {build,list,get}` | H1/H2/H3 section sidecars for articles |
| `scribe relations {get,set,rm,graph,check,migrate}` | Typed edges between articles |
| `scribe contradictions {build,list,show,resolve}` | Contradiction ledger |
| `scribe stale {build,list,show}` | Staleness ledger (date + source signals) |
| `scribe view {<name>,--list}` | Declarative views over wiki frontmatter |

**Content-quality warnings** (bloated / thin / rolling-overgrown / self-named
dir) that `scribe lint` flags but `--fix` can't repair → hand off to the
**scribe-kb-tidy** skill.

## Consolidation & cost

| Command | What it does |
|---|---|
| `scribe dream` | **lock** — weekly 4-phase memory consolidation |
| `scribe dream --hot` | **lock** — daily mini-consolidation of the busiest domain (auto-gates) |
| `scribe triage` | **read-only** — score unprocessed sessions by knowledge density |
| `scribe cost` | Summarize LLM calls (tokens, wallclock, USD); aggregates registered KBs, scope with `--kb` |

## Machine / setup

| Command | What it does |
|---|---|
| `scribe init` | Scaffold a fresh KB (writes scribe.yaml, templates, agent handshake blocks) |
| `scribe cron {install,status,uninstall}` | Manage the macOS LaunchAgents that run the pipeline |
| `scribe fda` | macOS Full Disk Access probe + interactive grant (needed for chat.db capture) |
| `scribe install-tools` | Bootstrap optional tools (uv + marker-pdf) for full PDF/DOCX/PPTX/XLSX/EPUB ingestion |
| `scribe skill {install,list}` | Install/list the embedded agent skills (scribe-kb, scribe-kb-tidy, scribe-cli) |
| `scribe config {diff,trust,update}` | Team KBs: review/approve sensitive `scribe.yaml` changes; append docs for new options |
| `scribe kb {add,list,remove}` | Machine-level KB registry the scheduler iterates |
| `scribe each -- <subcommand>` | Run a subcommand in every registered KB (cron uses this) |
| `scribe promote --to <kb> <article>` | Copy an article into another KB with provenance |

## Internal plumbing (usually invoked by sync/dream)

`backlinks`, `index`, `orphans`, `validate`, `hook`, `hot`, `sessions`,
`scan`, `deep` — runnable standalone for debugging, but normally called under
the hood. Prefer the higher-level command unless you're specifically debugging.
