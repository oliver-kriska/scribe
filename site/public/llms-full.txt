# scribe — your knowledge base, written by your tools

> A single-binary CLI that turns your git repos, Claude Code sessions, and self-sent links into a curated, semantically searchable knowledge base. Cross-project, cron-driven, fully local-capable.

**Version:** v0.2.13 · **License:** MIT · **Repo:** <https://github.com/oliver-kriska/scribe>

---

## What scribe is

scribe is an LLM-written knowledge base pipeline. It mines four input streams — git repos, Claude Code sessions (via ccrider's FTS5 index), URLs you text yourself, and drop files from other projects — then compiles a curated wiki of decisions, patterns, learnings, tool evaluations, and research. The wiki is plain markdown with YAML frontmatter, indexed by `qmd` for semantic search.

scribe is not a RAG pipeline. It keeps raw sources verbatim under `raw/` AND compiles a structural wiki on top — both layers are searchable. Dense sources fan out into multiple entity-first wiki pages via a two-pass absorb (not one summary per source). LLM-generated retrieval-context paragraphs get spliced into every article so embedding models catch implicit entities.

## How it works

1. **Capture** — four input streams, all on cron: git repos, Claude Code sessions via ccrider's FTS5 index, iMessage self-chat URLs, drop files.
2. **Triage** — BM25 keyword density scoring rejects boilerplate sessions before any LLM call. Cheap sessions cost nothing.
3. **Absorb** — two-pass extraction. Pass 1 grounds atomic facts; pass 2 fans dense sources into multiple entity-first wiki pages.
4. **Compile + index** — auto-generated wikilinks, backlinks JSON, retrieval-context paragraphs spliced into every article. `qmd` reindexes.

## Strong points

- **Claude Code becomes context-aware across sessions.** `scribe init` writes a block into `~/.claude/CLAUDE.md` that tells Claude to query your KB via qmd's MCP server before recommending a library, proposing an architecture, or reproducing a pattern.
- **Runs itself on cron.** Hourly auto-commits, 2-hourly project extraction, 3×/day session mining, weekly Dream cycle for consolidation.
- **Knowledge compounds across projects.** One cross-project KB, not siloed per repo.
- **Fully local-capable.** Every pipeline stage — contextualize, atomic facts, pass-2 — can route through Ollama via `pass2_mode: json` + `pass2_provider: ollama` (v0.2.12+). Zero API spend.
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

## Run it locally for $0

```yaml
# scribe.yaml
absorb:
  contextualize:
    provider: ollama
    model: gemma3:4b
  atomic_facts: true
  facts_provider: ollama
  facts_model: gemma3:4b
  pass2_mode: json
  pass2_provider: ollama
  pass2_model: qwen2.5-coder:14b
```

Then `scribe doctor --section localmode` validates the setup before kicking off a sync. scribe auto-pulls models on first use; no manual `ollama pull` needed.

## Search from anywhere

```
qmd query "how did I solve the oban idempotency bug last quarter"
```

`qmd` indexes the KB for semantic search and works from any terminal in any directory. From inside Claude Code: the MCP tool `mcp__plugin_qmd_qmd__query` does the same query against your KB.

## Comparison

| Tool | Session mining | Cron-driven | Density pre-filter | Two-pass absorb | Multi-project | Local-mode |
|---|---|---|---|---|---|---|
| **scribe** | yes (ccrider FTS5) | yes (LaunchAgents) | yes (BM25 triage) | yes (fan-out) | yes | yes (Ollama / llama.cpp) |
| claude-memory-compiler | every session, no filter | opportunistic | no ($115/20min, issue #3) | no | no | no |
| nvk/llm-wiki | no | one-shot `/wiki:assess` | n/a | no | no | no |
| basic-memory | no (issue #669 since Mar) | cron suggested | n/a | no | yes (projects) | no |
| RAG (LangChain, LlamaIndex) | no | indexing only | n/a | chunks only | yes | varies |
| Obsidian / Notion | manual | no | n/a | no | manual | n/a |

## Common questions

**What is scribe?**
A single-binary Go CLI that builds a personal, LLM-written knowledge base from your git repos, Claude Code sessions, and self-sent URLs. The pipeline runs on cron — set up once with `scribe init` + `scribe cron install`.

**How is scribe different from RAG, Obsidian, or claude-memory-compiler?**
RAG stores chunks with no curation layer. Obsidian and Notion expect you to write the notes yourself. claude-memory-compiler runs an LLM call on every Claude Code session — one user burned $115 in 20 minutes (issue #3). Scribe sits between them: it watches your work and writes the notes for you, but uses BM25 keyword density to skip boilerplate sessions before any LLM call, so cheap sessions cost nothing.

**Does scribe require an Anthropic API key?**
No. As of v0.2.12 the entire absorb pipeline (contextualize, atomic facts, pass-2) can route through Ollama with `pass2_mode: json` + `pass2_provider: ollama`. Recommended local models: `gemma3:4b` for contextualize and atomic facts, `qwen2.5-coder:14b` for pass-2.

**What does it cost to run?**
Zero on the local-mode path (Ollama). On the Anthropic-hosted path, contextualize costs roughly $0.0001 per article via Claude Haiku; project extraction and pass-2 use Sonnet at standard prices. The triage pre-filter and density scoring never call an LLM, so most session-mining work is free.

**Does scribe work on Linux?**
Yes. macOS gets LaunchAgents via `scribe cron install`; Linux gets paste-ready crontab lines from the same command. The fsnotify watcher (`scribe watch`) is not cron-friendly on either OS — run it under launchd KeepAlive on macOS or systemd-user on Linux. The iMessage capture step is macOS-only because it reads `chat.db`; everything else is portable.

**Where does scribe store the knowledge base?**
In a plain git repo of markdown files at whatever path you pass to `scribe init`. Push it to your own GitHub, Gitea, or Forgejo — there's no SaaS account, no cloud sync, no vendor lock-in. Open it in Obsidian, VS Code, vim, or mdbook.

**What does the cron schedule look like?**
Hourly KB auto-commit, every 2 hours scan git repos for new decisions and patterns, three times a day mine Claude Code sessions, every 30 minutes drain queued URLs, every 4 hours pull self-iMessaged links, weekly Dream cycle on Sunday for memory consolidation, plus a continuous fsnotify watcher on the ccrider DB for near-real-time session extraction.

---

**License:** MIT
**Source:** <https://github.com/oliver-kriska/scribe>
**Last updated:** 2026-05-14
