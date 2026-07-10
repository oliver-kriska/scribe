# scribe — your knowledge base, written by your tools

> scribe is the compiled, LLM-written knowledge base — the "LLM Wiki" pattern as a single-binary CLI: plain markdown in git, no vector DB, no RAG. It's memory your AI agents read before they decide, not a second brain you maintain and never reopen. A single-binary CLI that turns your git repos, Claude Code and Codex sessions, and self-sent links into a curated, semantically searchable knowledge base. Auto-discovers projects from both Claude Code and Codex, and both agents get the same query-KB + write-drop-files handshake. Cross-project, cron-driven, runs 100% on local Ollama with zero API spend. In team mode, a small team points every machine at one git-backed KB — with a trust layer, a deterministic secret-scan commit gate, and per-git-remote approval keeping secrets, private client repos, and hostile config changes out of shared history.

**License:** MIT · **Repo:** <https://github.com/oliver-kriska/scribe> · **Maintainer's KB:** 7,472 articles, zero typed by hand

---

## What scribe is

scribe is an LLM-written knowledge base pipeline. It mines four input streams — git repos, agent sessions (Claude Code via ccrider's FTS5 index, Codex CLI rollouts), URLs you text yourself, and drop files from other projects — then compiles a curated wiki of decisions, patterns, learnings, tool evaluations, and research. The wiki is plain markdown with YAML frontmatter, indexed by `qmd` for semantic search.

scribe is not a RAG pipeline. It keeps raw sources verbatim under `raw/` AND compiles a structural wiki on top — both layers are searchable. Dense sources fan out into multiple entity-first wiki pages via a two-pass absorb (not one summary per source). LLM-generated retrieval-context paragraphs get spliced into every article so embedding models catch implicit entities.

## How it works

Three stages, one pipeline:

1. **Capture** — four input streams, all on cron: git repos, Claude Code & Codex sessions, iMessage self-chat URLs, drop files.
2. **Triage** — BM25 keyword density scoring rejects boilerplate sessions before any LLM call. Cheap sessions cost nothing, so the inference bill scales with signal, not session count.
3. **Absorb** — two-pass extraction. Pass 1 grounds atomic facts; pass 2 fans dense sources into multiple entity-first wiki pages.
4. **Compile + index** — auto-generated wikilinks, backlinks JSON, retrieval-context paragraphs spliced into every article. `qmd` reindexes, and the next agent session reads what scribe just wrote (`CLAUDE.md` / `AGENTS.md`).

## What makes scribe different

- **Your agents become context-aware across sessions.** `scribe init` writes a handshake block into **both** `~/.claude/CLAUDE.md` and `~/.codex/AGENTS.md` that tells Claude Code and Codex to query your KB before recommending a library, proposing an architecture, or reproducing a pattern.
- **Runs itself on cron.** Hourly auto-commits, 2-hourly project extraction, 3×/day session mining, a weekly Dream cycle for consolidation with a daily hot-domain pass in between. launchd on macOS, systemd/crontab on Linux.
- **Knowledge compounds across projects.** One cross-project KB, not siloed per repo. Solve the oban idempotency bug in project A on Monday; the agent finds your fix on Friday when the same shape comes up in project B.
- **Fully local-capable — 100% Ollama.** Every LLM op — per-project extraction, absorb (contextualize, atomic facts, pass-2), dream, assess, deep, session-mine, relations migrate — runs end-to-end against a local Ollama server. There is no remaining `claude -p` callsite in the normal sync flow. A single line in `scribe.yaml` flips the whole pipeline: `llm.provider: ollama`. Zero API spend.
- **Plain markdown you own.** A git repo of plain markdown files with YAML frontmatter. Push to your own GitHub, Gitea, or Forgejo. Open in Obsidian, VS Code, vim, or mdbook. No SaaS, no vendor lock-in.
- **Typed graph, not just tags.** Articles connect via a closed 10-kind typed-edge schema: `supersedes`, `contradicts`, `derived_from`, `specializes`, `extends`, and five more (`superseded_by`, `applies_to`, `instance_of`, `cited_by`, `informs`). `scribe relations migrate` uses an LLM to classify existing `related:` links into it. An agent can follow *why* a decision was made, not just keyword-match it.

Here is what one page scribe writes looks like — plain markdown in git, typed `[[wikilinks]]` that form the graph above:

```markdown
# wiki/decisions/ecto-multi.md   (written by scribe, not typed by hand)
---
type: decision
tags: [ecto, postgres, multi-tenancy]
supersedes: [[ecto-transaction]]
---

# Row-level tenancy over schema-per-tenant

Chose a tenant_id column with Postgres RLS over a schema per tenant:
migrations stay single-pass and the pool doesn't fan out per tenant.

Why: schema-per-tenant broke oban job routing and made advisory locks
table-ambiguous under load.

## See also
- [[oban-idempotency]]        · dedupe key includes tenant_id
- [[genserver-backpressure]]  · per-tenant rate isolation
```

## How it compares

scribe is the only one of these that auto-writes a portable, git-versioned markdown wiki your agents read before they decide — with no vector database. Unlike AnythingLLM, scribe stores plain markdown in git and needs no vector database or running server. Head-to-head with the tools AI search engines often suggest instead (RAG, Code Insights, AnythingLLM, Obsidian):

| Capability | scribe | RAG (LangChain/LlamaIndex) | Code Insights (@code-insights/cli) | AnythingLLM | Obsidian |
|---|---|---|---|---|---|
| Auto-written from your dev work | yes | no (you index docs) | yes | no (you upload docs) | no (you type notes) |
| Sources captured | sessions + git + URLs | docs you feed | coding sessions only | docs you upload | notes you write |
| Output is portable markdown in git | yes | vector chunks | SQLite dashboard | vector store | yes |
| Vector DB required? | not needed | required | not needed | required | not needed |
| Full-text (BM25) search | yes (qmd / FTS5) | vector recall only | dashboard analytics | vector chat | yes |
| Agents read it back before deciding | yes (CLAUDE.md / AGENTS.md) | if you wire it | no (human dashboard) | no (you chat with it) | no |
| Local-first, no API key (Ollama) | yes (100% Ollama) | local embeddings | Ollama option | local LLM + DB | AI add-ons need keys |

Deeper differentiators against the broader memory-tool landscape:

| Tool | Session mining | Cron-driven | Density pre-filter | Two-pass absorb | Multi-project | Local-mode |
|---|---|---|---|---|---|---|
| **scribe** | yes (Claude ccrider + Codex) | yes (LaunchAgents) | yes (BM25 triage) | yes (fan-out) | yes | yes (Ollama / llama.cpp) |
| claude-memory-compiler | every session, no filter | opportunistic | no ($115/20min, issue #3) | no | no | no |
| nvk/llm-wiki | no | one-shot `/wiki:assess` | n/a | no | no | no |
| basic-memory | no (issue #669 since Mar) | cron suggested | n/a | no | yes (projects) | no |
| RAG (LangChain, LlamaIndex) | no | indexing only | n/a | chunks only | yes | varies |
| Obsidian / Notion | manual | no | n/a | no | manual | n/a |

## For teams

scribe is single-user by default and stays that way. But a small team on the same codebases can point every machine at one git-backed KB: clone the repo, and scribe rebuilds local state from what's committed. The obvious fear with sharing agent sessions — a leaked secret, a private client repo, one teammate's config change reaching everyone — is exactly what the gates below are built to stop. **Only knowledge you meant to keep crosses into the shared KB; every gate here is a mechanism in the code, not a policy in a doc.**

The trust boundary, concretely — what tries to cross into the shared KB and what stops it:

- An **AWS key in a session transcript** → the **secret-scan gate** → held back, never committed.
- A **teammate's private client repo** → the **remote allowlist** → never ingested without an approve.
- A **pushed config change that repoints the LLM** → the **trust layer** → reverted to the last trusted snapshot.
- **The reasoning behind a fix** → **curated extract** → crosses into the shared KB, provenance kept.

The mechanisms:

- **Shared config is untrusted by default.** A trust layer pins the sensitive surface of a shared `scribe.yaml`: provider, model, ingest paths, and the secret scanner itself. A pushed change that would repoint inference to a new endpoint or drain a new directory into the KB reverts to the last trusted snapshot until a human approves it.
- **Secrets never reach shared history.** scribe mines session transcripts, which routinely carry API keys and tokens. In team mode a deterministic secret scanner runs in the commit gate and holds flagged credentials back before anything lands in shared git history. No LLM, no network: a regex pass on the commit path.
- **Extraction is paid for once, not per laptop.** Every `scribe sync` pulls, merges, and reindexes before it extracts, and a committed ledger keeps two machines from mining the same git revision twice. Your inference bill scales with commits, not with the number of laptops pointed at the KB.
- **A teammate's unrelated repo never leaks in.** Discovered projects start pending. `allowed_remotes` and source filters gate discovery by git-remote identity, and `scribe projects {list,approve,ignore,review}` controls what enters the pipeline, so a side project or a client checkout never lands in the shared KB without an approve.
- **Curate privately, promote deliberately.** `scribe promote <article> --to team-kb` copies a page from your personal KB into the shared one, provenance recorded. Derived and coordination files are refused as sources, so the team KB fills with what you meant to publish, not your working scratch.
- **One machine consolidates, no server.** The weekly Dream consolidation rewrites, merges, and prunes the whole wiki, so exactly one machine should run it. A committed leader lease in the repo elects that machine: no etcd, no lock server, and two laptops never race to rewrite the same wiki at 02:00 Sunday.

## Inference & cost — local, hosted, or Anthropic

Run the whole pipeline on local Ollama for $0, point it at a hosted OpenAI-compatible API (Together, Groq, Fireworks, Hugging Face) when the laptop is running hot, or keep `claude -p` for the Anthropic path. The three coexist; pick the trade per op in `scribe.yaml`, and nothing reaches a paid provider without explicit config. The API key never lives in `scribe.yaml` — only the name of the environment variable that holds it. `scribe cost` then reconciles every token across providers and KBs, to the cent against the provider's own dashboard.

Real numbers, same week, same machine, names anonymized:

- **$0 — local · Ollama.** Per-project extraction, two-pass absorb, the weekly Dream cycle, assess, deep, and session-mine all run against a local Ollama server. `ollama ps` shows the work; there is no `claude -p` callsite in a normal local `scribe sync`.
- **$0.34 — hosted · Together.** Part of the week's work routed to a hosted OpenAI-compatible endpoint (Qwen3-235B). It handled a smaller volume than the Anthropic path, so read it as what that path billed, not a like-for-like race.
- **$103.57 — Anthropic · 7 days.** The Anthropic path (Sonnet + Haiku) for the same period. The all-provider total for the week is $103.91, once the $0.34 Together run is added.

```
scribe cost — last 7 days — 2 KBs (personal-kb, team-kb)

By provider:
  provider    calls   in-tokens    out-tokens    usd
  anthropic     452     251,719     1,341,118   $103.57
  together      195   1,261,197       146,728     $0.34
  ollama      2,684  14,711,415     1,932,708       —

By KB:
  team-kb     1,732   7,020,329     2,211,137   $102.96
  personal-kb 1,599   9,204,002     1,209,417     $0.95

usd = provider-billed spend incl. cache-write & cache-read; the in-tokens
column is uncached input only, so usd exceeds an in/out list estimate. One
API key usually bills every KB on a machine, so `scribe cost` aggregates
them by default and reconciles to the provider dashboard.
```

Configure the whole pipeline with one `llm` block. Local mode:

```yaml
# scribe.yaml — local (Ollama, $0)
llm:
  provider: ollama
  model: gemma3:12b             # cross-op default
  ollama_url: http://localhost:11434
  num_ctx: 16384                # keeps dense-article tails intact
```

Hosted mode (key is read from the named env var, never stored in the file):

```yaml
# scribe.yaml — hosted (OpenAI-compatible)
llm:
  provider: together
  model: Qwen/Qwen3-235B-A22B-Instruct-2507-tput
  base_url: https://api.together.xyz/v1
  api_key_env: TOGETHER_API_KEY
```

Anthropic mode (per-op model routing):

```yaml
# scribe.yaml — Anthropic
llm:
  provider: anthropic
  model: sonnet
absorb:
  pass1_model: haiku
  pass2_model: sonnet
sync:
  daily_output_token_ceiling: 200000   # runaway-spend backstop
```

Per-op overrides always win over the top-level block. `scribe doctor --section localmode` validates the setup before a sync; scribe auto-pulls Ollama models on first use.

## Who it's for

Built for developers who use AI tools every day. The expensive half of the job isn't deciding — it's rebuilding the context you already had. scribe automates that half: if your Claude Code or Codex history is already full of decisions, fixes, and library evaluations, it keeps them from evaporating between sessions.

- **Heavy AI user — you live in Claude Code and Codex.** Your agents keep re-deriving the same answers because each session starts from zero. scribe gives them durable memory: a handshake into `CLAUDE.md` + `AGENTS.md`, 3×/day session mining via ccrider + Codex rollouts, and drop files written back from any project.
- **Multi-project dev — you solve the same problem twice.** One cross-project KB means Friday's repo can pull Monday's fix. Auto-discovery across every git repo you've opened, entity-first fan-out with no buried summaries, and typed edges (`supersedes`, `contradicts`, `specializes`) keep the graph honest as patterns evolve.
- **Local & private — you want zero API spend.** Run the entire pipeline locally on Ollama. Plain markdown on your own git remote (GitHub, Gitea, Forgejo). No SaaS, no cloud sync, no vendor lock-in. Open it in Obsidian, VS Code, vim, or mdbook.

## Search from anywhere

```
qmd query "how did I solve the oban idempotency bug last quarter"
```

`qmd` indexes the KB for BM25 keyword search and semantic vector search, and works from any terminal in any directory — no reindex per tool, no format conversion. From inside Claude Code, the MCP tool `mcp__plugin_qmd_qmd__query` runs the same query; from Codex, shell `qmd query` does. Because it's plain markdown, you can also `grep` it.

## The command surface

45 subcommands, one binary. The ones you'll actually type:

```
scribe init                 # bootstrap a KB, wire the agent handshake
scribe sync                 # discover → extract → absorb → reindex
scribe sync --sessions      # mine Claude Code + Codex transcripts
scribe sync --estimate      # token estimate, zero LLM calls
scribe doctor               # validate setup, cron, git remote, Ollama
scribe commit               # stage + push the KB to your private remote
scribe dream                # weekly consolidation (Ollama-driven)
scribe capture              # drain queued URLs / iMessage links
scribe relations migrate    # classify `related:` into typed edges
scribe cron install / uninstall / status
```

Run `scribe --help` to see all 45. `scribe cron install` puts the boring ones on a schedule so you never type them again.

## In practice

Two real loops from the maintainer's normal use — concrete, not marketing:

- **Cross-project memory.** Evaluated a Phoenix translation library for one app; months later, started a different Phoenix project with the same problem. scribe had already absorbed the verdict from the prior session (skip DB-backed Gettext, weighed against `.po` files and managed services). When Claude Code opened the new repo and asked the KB, the "skip" verdict surfaced first with the reasoning attached — no re-research. Surfaced via `qmd query "phoenix translation library"` → `tools/kanta.md · verdict: skip`.
- **Solved twice, written once.** Fixed an Oban idempotency bug in project A; months later the same shape appeared in project B. The fix (an idempotency-key strategy for an external-call worker) was captured automatically when the post-fix session was mined. When the race reappeared, the agent grepped the KB before guessing and proposed the exact prior pattern. The second fix took fifteen minutes instead of an afternoon.

## The autonomous loop — hands-off

After `scribe init` + `scribe cron install`, five things happen on their own. New work flows in, the KB grows, your private git remote stays current, and the next Claude Code or Codex session in any project queries what scribe just wrote.

1. **Discovery — scribe finds every project you've already touched (Claude Code + Codex).** Walks `~/.claude/projects/` and `~/.codex/sessions/` — reading the `session_meta` event at the head of each rollout to extract a verbatim `cwd`. Cross-agent projects collapse to a single manifest entry tagged `discovered_from: both`. Every git repo you've opened in either CLI becomes a tracked project automatically.

2. **The agent handshake — your tools write the notes for you (Claude Code + Codex).** `scribe init` appends a parameterised block to **both** `~/.claude/CLAUDE.md` and `~/.codex/AGENTS.md`. That block tells every Claude Code *and* Codex session to (a) query your KB before recommending a library or making a decision (Claude via the qmd MCP server, Codex via shell `qmd query`), and (b) write reusable knowledge back as a drop file to `.claude/<your-kb-name>/YYYY-MM-DD-{slug}.md` with structured frontmatter.

3. **Cron sweeps — drop files + research notes flow into the absorb pipeline.** Every 2 hours `scribe sync` visits each tracked project and globs `.claude/<your-kb-name>/*.md` and `.claude/research/**/*.md`. New files are fed through the same two-pass absorb; per-project timestamps track what's new.

4. **Auto-publish — your private KB repo commits and pushes itself.** `scribe commit` runs hourly: stages every change, builds a structured commit message, and `git push`es to your origin (GitHub, Gitea, or Forgejo — no scribe-hosted backend). On non-fast-forward, it runs `git pull --rebase` and retries once; force-push is never attempted.

5. **Weekly cleanup — Dream cycle prunes, merges, breaks down (100% Ollama).** Sundays at 02:00, `scribe dream` runs a 4-phase consolidation (ORIENT → SIGNAL → CONSOLIDATE → PRUNE + INDEX). A Go orchestrator inlines the orient packet into one bounded prompt and parses one `EnvelopeV2` JSON document back — verified end-to-end on a 7,472-article KB in ~70s on `gemma3:12b`.

The loop closes here: every absorb tick reindexes `qmd`; the next time you open Claude Code or Codex in any project, the agent finds whatever scribe just wrote — and if that session produces a new lesson, it writes a drop file, and the cycle repeats.

## Install

```sh
brew tap oliver-kriska/scribe
brew install oliver-kriska/scribe/scribe
scribe init --path ~/my-kb
cd ~/my-kb
scribe cron install
scribe doctor
```

Or via shell installer: `curl -fsSL https://raw.githubusercontent.com/oliver-kriska/scribe/main/install.sh | bash`

## Common questions

**What is scribe?**
A single-binary Go CLI that builds a personal, LLM-written knowledge base from your git repos, Claude Code and Codex sessions, and self-sent URLs. The pipeline runs on cron — set up once with `scribe init` + `scribe cron install`.

**How is scribe different from RAG, Obsidian, or claude-memory-compiler?**
RAG stores chunks with no curation layer. Obsidian and Notion expect you to write the notes yourself. claude-memory-compiler runs an LLM call on every Claude Code session — one user burned $115 in 20 minutes (issue #3). Scribe sits between them: it watches your work and writes the notes for you, but uses BM25 keyword density to skip boilerplate sessions before any LLM call, so cheap sessions cost nothing.

**Is this just another "second brain"?**
No — and that's the point. A second brain is notes *you* read, connect, and think in; many people build one, stop reopening it, and end up with a graveyard of notes plus a monthly token bill. scribe is the opposite: memory your *agent* reads before it decides. It stores the reasoning behind a choice — not summaries that lower the quality of material you wanted to read in full. It pays off most where the expensive half of the job is rebuilding context rather than deciding — which is exactly developer work.

**Does scribe require an Anthropic API key?**
No. Every LLM op in scribe — per-project extraction, absorb (contextualize, atomic facts, pass-2), dream, assess, deep, session-mine, relations migrate — runs end-to-end against a local Ollama server. There is no remaining `claude -p` callsite in a normal `scribe sync`. A single line in `scribe.yaml` flips the whole pipeline: `llm.provider: ollama`. Per-op overrides still work if you want to keep some passes on Anthropic or a hosted API.

**What does it cost to run?**
Zero on the local-mode path (Ollama) for the entire pipeline. On a hosted OpenAI-compatible API, a full week of work ran ~$0.34 (Together); on the Anthropic path the same week was ~$103.57 ($103.91 across all three paths). The triage pre-filter and density scoring never call an LLM, so most session-mining work is free regardless of backend. `scribe cost` reconciles every token to the cent across providers and KBs.

**Can a team share one KB safely?**
Yes. A small team points every machine at one git-backed KB. A trust layer treats shared `scribe.yaml` as untrusted by default (config that repoints inference or widens ingest paths reverts until approved); a deterministic secret-scan gate holds credentials back before they hit shared git history; `allowed_remotes` keeps a teammate's unrelated or client repo out; `scribe promote` moves curated pages over with provenance; and a committed leader lease elects the single machine that runs the weekly consolidation — no server, no etcd.

**Does scribe work on Linux?**
Yes. macOS gets LaunchAgents via `scribe cron install`; Linux gets paste-ready crontab lines from the same command. The iMessage capture step is macOS-only because it reads `chat.db`; everything else is portable.

**Where does scribe store the knowledge base?**
In a plain git repo of markdown files at whatever path you pass to `scribe init`. Push it to your own GitHub, Gitea, or Forgejo — there's no SaaS account, no cloud sync, no vendor lock-in. Open it in Obsidian, VS Code, vim, or mdbook.

**What does the cron schedule look like?**
Hourly KB auto-commit, every 2 hours scan git repos for new decisions and patterns, three times a day mine Claude Code sessions via ccrider — and Codex CLI sessions in that same pass when opted in — every 30 minutes drain queued URLs, every 4 hours pull self-iMessaged links, a weekly Dream cycle on Sunday with a daily hot-domain pass in between, plus a continuous fsnotify watcher on the ccrider DB for near-real-time session extraction.

**Is scribe an alternative to RAG for a personal knowledge base?**
Yes. scribe is a compiled knowledge base, not a retrieval pipeline — it writes curated markdown articles into a git repo instead of chunking documents into a vector database, so there are no embeddings to maintain and no vector DB to run. Most lookups are plain-text BM25 matches, and the curated wiki stays small enough for an agent to read whole.

**How is scribe different from Code Insights, AnythingLLM, or Obsidian?**
Code Insights turns your AI coding sessions into an analytics dashboard in a local SQLite database; scribe turns them — plus your git repos and self-sent URLs — into a portable markdown wiki in git that your agents read back before they decide. AnythingLLM is a RAG chat app that needs a vector database and documents you upload; scribe needs neither. Obsidian is a manual notes tool you type into yourself — scribe writes the notes for you.

**Is scribe an AnythingLLM alternative?**
Yes. scribe is an AnythingLLM alternative for people who want an LLM wiki instead of a RAG server: it's a compiled knowledge base — plain markdown in git, no vector database, no server to run — where AnythingLLM is a RAG chat app built around a vector store and documents you upload. scribe auto-captures knowledge from your Claude Code and Codex sessions into portable markdown your agents read back before they decide.

**Does scribe build a knowledge base from my Claude Code and Codex sessions automatically?**
Yes. On cron, scribe mines your Claude Code sessions via ccrider's FTS5 index and your Codex CLI rollouts, scores each session with BM25 keyword density to skip boilerplate before any LLM call, then runs a two-pass absorb that fans dense sessions out into entity-first wiki articles.

**Is scribe local-first, and does it work without an API key?**
Yes. The entire pipeline can run 100% locally against an Ollama server with no Anthropic API key — a single line in `scribe.yaml` flips every LLM op to local. Your knowledge base is a plain git repo of markdown on your own machine, with no SaaS account and no cloud sync.

**Does scribe have full-text (BM25) search, and does it run on cron?**
Yes to both. The knowledge base is indexed by `qmd` for BM25 keyword search and semantic vector search, and because it's plain markdown you can also `grep` it from any terminal or query it from inside your agent. The whole pipeline runs unattended on macOS LaunchAgents or Linux cron.

---

**License:** MIT
**Source:** <https://github.com/oliver-kriska/scribe>
**Last updated:** 2026-07-10
