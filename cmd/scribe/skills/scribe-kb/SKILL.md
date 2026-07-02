---
name: scribe-kb
description: Read, write, and search a scribe-managed knowledge base (markdown vault with frontmatter conventions, wikilinks, and qmd hybrid search). Use when the user's project has a scribe.yaml (KB root) or a .claude/<kb_name>/ drop-file directory (consumer side), or when they mention scribe, scriptorium, qmd, drop files, or "my KB". Covers frontmatter schema, wikilink syntax, drop-file pattern, search via qmd, and directory taxonomy.
---

# scribe knowledge base — agent skill

A scribe-managed KB is a local-first markdown vault. Articles live in topic directories under a KB root, every article carries YAML frontmatter, links between articles are `[[Wikilinks]]`, and search runs through qmd (BM25 + vector hybrid). A daemon `scribe` runs on cron to extract from project repos, mine Claude Code sessions, capture iMessage URLs, absorb queued URLs, lint frontmatter, and reindex qmd.

This skill teaches a Claude Code / Codex / OpenCode session how to interact with such a KB without breaking the conventions the daemon expects.

## When to use this skill

Trigger on any of:

- **You're working in the KB root** — there's a `scribe.yaml` in the project root, or directories named `wiki/`, `decisions/`, `solutions/`, `patterns/`, `research/`, `ideas/`, `tools/`, `people/`, `projects/`, `raw/articles/`.
- **You're working in a non-KB project that drops files into the KB** — there's a `.claude/<some_kb_name>/` directory with markdown drop files, or the user's CLAUDE.md mentions a personal KB and a drop-file convention.
- The user asks about: their KB, scriptorium, qmd, drop files, "what do I know about X", "have I done this before", "save this for later", or any of the article types above.

## Operations cheat sheet

| What | Command (qmd / shell) |
|---|---|
| Find articles by topic | `qmd query "<natural-language question>"` (or use the qmd MCP tool when present) |
| Exact-keyword search | `qmd search "<keywords>"` |
| Read a specific article | Read tool with the absolute path |
| Read a section | `scribe sections get <article> <id>` (Phase 5A) |
| Write a new wiki article | Edit/Write tool — see references/FRONTMATTER.md |
| File a drop from another project | `scribe drop --title ... --type ... --domain ...` — see references/DROP_FILES.md |
| Validate frontmatter before commit | `scribe validate <file>` or `scribe lint --changed` |
| List sections in an article | `scribe sections list <article>` |

## Workflow: file a drop file from another project

When you produce reusable knowledge in a non-KB project that should travel to the user's KB, write a drop file:

1. **Pick a path:** `.claude/<kb_name>/YYYY-MM-DD-{slug}.md` in the current project root. `<kb_name>` is the value the user has used in their CLAUDE.md (often `scriptorium`).
2. **Run `scribe drop`** with the required flags (`--title`, `--type`, `--domain`, `--body`) — or hand-write the frontmatter per references/DROP_FILES.md (`scriptorium: true`, `action`, `title`, `type`, `domain`, `tags`) if `scribe` isn't installed.
3. **Write the body** — what's reusable, what would teach a future session this insight in 30 seconds.
4. **Tell the user** what you filed and why. Don't fabricate drop files for trivial facts.

`scribe sync` running on the user's cron will absorb the drop file automatically — you don't need to do anything else.

## Workflow: write a new wiki article

When the user asks you to write inside the KB itself (not a drop file):

1. **Pick the directory** by article type (see references/STRUCTURE.md). A reusable solution → `solutions/`. A long-lived decision → `decisions/`. A research deep-dive → `research/`. A pattern that applies across projects → `patterns/`.
2. **Write valid frontmatter** — required keys: `title, type, created, updated, domain, confidence, tags, related, sources`. See references/FRONTMATTER.md.
3. **Cross-link** with `[[Article Title]]` to existing articles. Search first with qmd; don't invent neighbors.
4. **Commit** with a clear message. The user's pre-commit hook runs `scribe lint`.

## Workflow: search before recommending

Before recommending a tool, library, decision, or pattern, search the KB:

```
qmd query "<the thing> verdict"
qmd query "<the thing> alternatives"
qmd query "<the problem> decision reasoning"
```

Past tool evaluations live in `tools/` with `verdict: use | evaluate | skip`. Past decisions live in `decisions/` with full context. Don't suggest something already rejected.

## What NOT to do

- **Don't write directly to the KB from a non-KB project.** Use a drop file. The user's daemon owns absorbs.
- **Don't invent frontmatter fields** outside the closed set. Lint will reject typos.
- **Don't bypass `scribe lint`** when the user asks you to commit. The pre-commit hook is the canonical validator.
- **Don't `cd` into the KB root before searching with qmd** — qmd uses absolute paths and works from any directory. CD-ing breaks the user's flow.
- **Don't write rolling-file contents into article shapes.** `learnings.md`, `decisions-log.md` are append-only logs; ordinary articles are paragraph-shaped.

## References

- [FRONTMATTER.md](references/FRONTMATTER.md) — required and optional frontmatter keys per article type
- [WIKILINKS.md](references/WIKILINKS.md) — link syntax (basic, aliased, section, block-anchor)
- [STRUCTURE.md](references/STRUCTURE.md) — directory taxonomy and what belongs where
- [DROP_FILES.md](references/DROP_FILES.md) — the cross-project drop-file pattern in detail
- [QUERY.md](references/QUERY.md) — qmd query patterns: lex, vec, hyde, when to use which
- [COMPAT.md](references/COMPAT.md) — Obsidian / Logseq vault compatibility notes
