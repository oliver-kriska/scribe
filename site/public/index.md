# scribe — your knowledge base, written by your tools

> Memory your AI agents read before they decide — not a second brain you maintain and never reopen. A single-binary CLI that turns your git repos, Claude Code and Codex sessions, and self-sent links into a curated, semantically searchable knowledge base. Auto-discovers projects from both Claude Code and Codex, and both agents get the same query-KB + write-drop-files handshake. Cross-project, cron-driven, runs 100% on local Ollama with zero API spend.

**License:** MIT · **Repo:** <https://github.com/oliver-kriska/scribe> · **Maintainer's KB:** 7,472 articles, zero typed by hand

---

## What scribe is

scribe is an LLM-written knowledge base pipeline. It mines four input streams — git repos, agent sessions (Claude Code via ccrider's FTS5 index, Codex CLI rollouts), URLs you text yourself, and drop files from other projects — then compiles a curated wiki of decisions, patterns, learnings, tool evaluations, and research. The wiki is plain markdown with YAML frontmatter, indexed by `qmd` for semantic search.

scribe is not a RAG pipeline. It keeps raw sources verbatim under `raw/` AND compiles a structural wiki on top — both layers are searchable. Dense sources fan out into multiple entity-first wiki pages via a two-pass absorb (not one summary per source). LLM-generated retrieval-context paragraphs get spliced into every article so embedding models catch implicit entities.

## How it works

1. **Capture** — four input streams, all on cron: git repos, Claude Code & Codex sessions, iMessage self-chat URLs, drop files.
2. **Triage** — BM25 keyword density scoring rejects boilerplate sessions before any LLM call. Cheap sessions cost nothing.
3. **Absorb** — two-pass extraction. Pass 1 grounds atomic facts; pass 2 fans dense sources into multiple entity-first wiki pages.
4. **Compile + index** — auto-generated wikilinks, backlinks JSON, retrieval-context paragraphs spliced into every article. `qmd` reindexes.

## Hands-off — the autonomous loop

After `scribe init` + `scribe cron install`, five things happen on their own. New work flows in, the KB grows, your private git remote stays current, and the next Claude Code or Codex session in any project queries what scribe just wrote.

1. **Discovery — scribe finds every project you've already touched (Claude Code + Codex).** Walks `~/.claude/projects/` (the directory Claude Code creates the first time you open any codebase) and also walks `~/.codex/sessions/` — reading the `session_meta` event at the head of each rollout to extract a verbatim `cwd`. Cross-agent projects collapse to a single manifest entry tagged `discovered_from: both`. Every git repo you've opened in either CLI becomes a tracked project automatically; the discovery pass on the every-2-hours cron tick keeps the manifest fresh.

2. **The agent handshake — your tools write the notes for you (Claude Code + Codex).** `scribe init` appends a parameterised block to **both** `~/.claude/CLAUDE.md` and `~/.codex/AGENTS.md`. That block tells every Claude Code *and* Codex session, in every project, to (a) query your KB before recommending a library or making a decision (Claude via the qmd MCP server, Codex via shell `qmd query`), and (b) when a session produces reusable knowledge, write a drop file to `.claude/<your-kb-name>/YYYY-MM-DD-{slug}.md` in the current project — the same shared drop location for both agents — with structured frontmatter (`type`, `domain`, `tags`, `action: create | update | append`).

3. **Cron sweeps — drop files + research notes flow into the absorb pipeline.** Every 2 hours `scribe sync` visits each tracked project and globs `.claude/<your-kb-name>/*.md` (Claude-written drop files) and `.claude/research/**/*.md` (your manual research deep-dives). New files are staged into the KB's `output/drops-<project>/` directory and fed through the same two-pass absorb. Each project's `last_drop_processed` and `last_research_processed` timestamps are tracked so the next sweep picks up only what's new.

4. **Auto-publish — your private KB repo commits and pushes itself.** `scribe commit` runs hourly. It stages every change, builds a structured commit message, and runs `git push` to your origin (your own GitHub, Gitea, or Forgejo — there's no scribe-hosted backend). On non-fast-forward, it runs `git pull --rebase` and retries once. Force-push is never attempted. Every invocation appends a JSON record to `output/runs/YYYY-MM-DD.jsonl` for `scribe doctor` to audit.

5. **Weekly cleanup — Dream cycle prunes, merges, breaks down (100% Ollama).** Sundays at 02:00, `scribe dream` runs a 4-phase consolidation pass: ORIENT (read-only inventory), SIGNAL (gather contradictions and stale articles), CONSOLIDATE (merge duplicates, promote rough notes to full articles, break dense ones into per-entity sub-pages), PRUNE + INDEX (drop superseded content, refresh `_index.md`, `_hot.md`, `_backlinks.json`). Three other weekly passes handle conflict resolution, identity clustering, and high-confidence alias auto-application.

   **Dream runs 100% on Ollama.** The hour-long monolithic `claude -p` path is replaced with a Go orchestrator that walks the orient packet itself, inlines it into one bounded prompt, and parses one `EnvelopeV2` JSON document back. With `llm.provider: ollama` the entire weekly cycle runs locally — verified end-to-end on a 7,472-article KB in ~70s on `gemma3:12b` at `num_ctx=16384`. The legacy monolithic path is still available via `dream.mode: monolithic`.

The loop closes here: every absorb tick reindexes `qmd`; the next time you open Claude Code (via the MCP `mcp__plugin_qmd_qmd__query` tool) or Codex (via shell `qmd query`) in any project, the agent finds whatever scribe just wrote. When the same problem comes up in a different repo, the agent pulls the prior solution before suggesting code — and if that session produces a new lesson, it writes a drop file, and the cycle repeats.

## Strong points

- **Your agents become context-aware across sessions.** `scribe init` writes a handshake block into **both** `~/.claude/CLAUDE.md` and `~/.codex/AGENTS.md` that tells Claude Code and Codex to query your KB before recommending a library, proposing an architecture, or reproducing a pattern.
- **Runs itself on cron.** Hourly auto-commits, 2-hourly project extraction, 3×/day session mining, weekly Dream cycle for consolidation.
- **Knowledge compounds across projects.** One cross-project KB, not siloed per repo.
- **Fully local-capable — 100% Ollama.** Every LLM op — per-project extraction, absorb (contextualize, atomic facts, pass-2), dream, assess, deep, session-mine, relations migrate — runs end-to-end against a local Ollama server. There is no remaining `claude -p` callsite in the normal sync flow. A single line in `scribe.yaml` flips the whole pipeline: `llm.provider: ollama`. Zero API spend.
- **Plain markdown you own.** A git repo of plain markdown files. Push to your own GitHub, Gitea, or Forgejo. Open in Obsidian, VS Code, vim, or mdbook. No vendor lock-in.
- **Typed graph, not just tags.** Articles connect via typed edges: `supersedes`, `contradicts`, `specializes`, `derived_from`, `extends`. `scribe relations migrate` uses an LLM to classify existing `related:` links into the typed schema.

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

## Run it locally for $0 — 100% Ollama

A single line in `scribe.yaml` flips the entire pipeline — per-project extraction, absorb, dream, assess, deep, session-mine, relations migrate — onto a local Ollama server:

```yaml
# scribe.yaml
llm:
  provider: ollama
  model: gemma3:12b             # cross-op default
  ollama_url: http://localhost:11434
  num_ctx: 16384               # keeps dense-article tails intact
```

Per-op overrides (e.g. pin `contextualize` and `pass2` on the MoE `qwen3:30b-a3b` for higher quality, or use `gemma4` as a lighter fallback) still work — set the per-op `provider`/`model` explicitly and they win over the top-level block. Then `scribe doctor --section localmode` validates the setup before kicking off a sync. scribe auto-pulls models on first use; no manual `ollama pull` needed.

**No cost asterisk.** Every LLM step that fires during a normal `scribe sync` — per-project extraction, the two-pass absorb, and the weekly dream/assess/deep/session-mine passes — runs through bounded JSON-envelope subtasks against your local Ollama server. There is no remaining `claude -p` callsite, so `ollama ps` shows the work and `ollama` does it. Verified end-to-end against a 7,472-article KB on `gemma3:12b` — a full sync makes zero Anthropic calls.

## Search from anywhere

```
qmd query "how did I solve the oban idempotency bug last quarter"
```

`qmd` indexes the KB for semantic search and works from any terminal in any directory. From inside Claude Code: the MCP tool `mcp__plugin_qmd_qmd__query` does the same query against your KB.

## Comparison

| Tool | Session mining | Cron-driven | Density pre-filter | Two-pass absorb | Multi-project | Local-mode |
|---|---|---|---|---|---|---|
| **scribe** | yes (Claude ccrider + Codex) | yes (LaunchAgents) | yes (BM25 triage) | yes (fan-out) | yes | yes (Ollama / llama.cpp) |
| claude-memory-compiler | every session, no filter | opportunistic | no ($115/20min, issue #3) | no | no | no |
| nvk/llm-wiki | no | one-shot `/wiki:assess` | n/a | no | no | no |
| basic-memory | no (issue #669 since Mar) | cron suggested | n/a | no | yes (projects) | no |
| RAG (LangChain, LlamaIndex) | no | indexing only | n/a | chunks only | yes | varies |
| Obsidian / Notion | manual | no | n/a | no | manual | n/a |

## Common questions

**What is scribe?**
A single-binary Go CLI that builds a personal, LLM-written knowledge base from your git repos, Claude Code and Codex sessions, and self-sent URLs. The pipeline runs on cron — set up once with `scribe init` + `scribe cron install`.

**How is scribe different from RAG, Obsidian, or claude-memory-compiler?**
RAG stores chunks with no curation layer. Obsidian and Notion expect you to write the notes yourself. claude-memory-compiler runs an LLM call on every Claude Code session — one user burned $115 in 20 minutes (issue #3). Scribe sits between them: it watches your work and writes the notes for you, but uses BM25 keyword density to skip boilerplate sessions before any LLM call, so cheap sessions cost nothing.

**Is this just another "second brain"?**
No — and that's the point. A second brain is notes *you* read, connect, and think in; many people build one, stop reopening it, and end up with a graveyard of notes plus a monthly token bill. scribe is the opposite: memory your *agent* reads before it decides. It stores the reasoning behind a choice — "you solved this then, this way; you picked this library because; the research result was X, sources Y" — not summaries that lower the quality of material you wanted to read in full. And it's cheap to query: most lookups are plain-text matches over markdown, with vectors only when the question is fuzzy. It pays off most where the expensive half of the job is rebuilding context rather than deciding — which is exactly developer work.

**Does scribe require an Anthropic API key?**
No. Every LLM op in scribe — per-project extraction, absorb (contextualize, atomic facts, pass-2), dream, assess, deep, session-mine, relations migrate — runs end-to-end against a local Ollama server. There is no remaining `claude -p` callsite in a normal `scribe sync`. A single line in `scribe.yaml` flips the whole pipeline: `llm.provider: ollama`. Per-op overrides still work if you want to keep some passes on Anthropic.

**What does it cost to run?**
Zero on the local-mode path (Ollama) for the entire pipeline — every `claude -p` callsite in a normal `scribe sync`, including per-project extraction and the weekly Dream cycle, runs locally. On the Anthropic-hosted path, contextualize costs roughly $0.0001 per article via Claude Haiku; project extraction, pass-2, and dream use Sonnet at standard prices. The triage pre-filter and density scoring never call an LLM, so most session-mining work is free regardless of backend.

**Does scribe work on Linux?**
Yes. macOS gets LaunchAgents via `scribe cron install`; Linux gets paste-ready crontab lines from the same command. The fsnotify watcher (`scribe watch`) is not cron-friendly on either OS — run it under launchd KeepAlive on macOS or systemd-user on Linux. The iMessage capture step is macOS-only because it reads `chat.db`; everything else is portable.

**Where does scribe store the knowledge base?**
In a plain git repo of markdown files at whatever path you pass to `scribe init`. Push it to your own GitHub, Gitea, or Forgejo — there's no SaaS account, no cloud sync, no vendor lock-in. Open it in Obsidian, VS Code, vim, or mdbook.

**What does the cron schedule look like?**
Hourly KB auto-commit, every 2 hours scan git repos for new decisions and patterns, three times a day mine Claude Code sessions via ccrider — and Codex CLI sessions in that same pass when opted in — every 30 minutes drain queued URLs, every 4 hours pull self-iMessaged links, weekly Dream cycle on Sunday for memory consolidation, plus a continuous fsnotify watcher on the ccrider DB for near-real-time session extraction.

---

**License:** MIT
**Source:** <https://github.com/oliver-kriska/scribe>
**Last updated:** 2026-06-04
