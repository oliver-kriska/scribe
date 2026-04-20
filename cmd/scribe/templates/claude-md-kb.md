## {{.KBName}} Knowledge Base

{{.OwnerName}} maintains a personal knowledge base at `{{.KBDir}}` indexed by qmd. It contains wiki articles, project insights, decisions, patterns, and solutions extracted from projects. The KB is LLM-managed — a `scribe` Go binary runs on cron to auto-extract from git repos, mine Claude Code sessions via ccrider, capture self-sent iMessage URLs, and absorb queued URLs. You don't need to run any of that yourself; it's already scheduled.

**When to search it:** Before making architectural decisions, when encountering a pattern that might exist elsewhere, when the user asks "have I done this before" or "what do I know about X", or when you need context about {{.OwnerName}}'s other projects.

**How to search:** Use the `mcp__plugin_qmd_qmd__query` tool when available (preferred), or `qmd query "<natural language question>"` via Bash. Both work from any directory — qmd collections use absolute paths, so **never `cd` into {{.KBDir}} first**. For exact terms use `qmd search "<keywords>"`. Results include file paths you can then Read for full context. For structural navigation once you're inside the KB, read `{{.KBDir}}/wiki/_index.md`.

**When to search proactively — don't wait for {{.OwnerName}} to ask.** The KB is only valuable if it's consulted before decisions, not after. {{.OwnerName}} has spent research effort on every one of the situations below and the answers are on disk; skipping the search wastes that work. Run `qmd query` at the start of any of these, without asking first:

- **Before recommending a library, tool, or framework** — query `"<name> evaluation verdict"` or `"alternatives to <name>"`. {{.OwnerName}} has already graded tools (`tools/` directory uses `verdict: use | evaluate | skip`). Don't suggest something already rejected.
- **Before proposing an architectural choice** — query `"<problem> decision reasoning"` or `"<pattern> tradeoffs"`. Past decisions live in `decisions/` with full context on what was considered and why. Cite the prior decision ("per [[Decision Title]], you chose X because Y — is that still current?") instead of reinventing it.
- **Before writing code that smells familiar** — query `"<pattern> solution"` or describe the problem in one sentence. `patterns/` and `solutions/` exist specifically for reuse across projects.
- **When {{.OwnerName}} references past work** — phrases like "have I done this before", "what do I know about X", "didn't we decide on X", "how did I solve this last time", "which tool did I use for X", "is there a pattern for this" — these are direct instructions to search. Don't answer from memory; search.
- **When hitting an error message that looks recognisable** — query the error text as a natural-language question. Debug outcomes are logged; the fix may be in `solutions/` or `projects/<name>/learnings.md`.
- **When a session in a new-to-you project starts** — query `"<project name> overview"` and `"<project name> decisions"` before reading any code. `projects/<name>/` contains the orientation you need.
- **Whenever the word "research" comes up — any context.** "research X", "have you researched Y", "this needs research", "let me do some research on Z", "old research on W", "there's research about this" — treat any of these as an instruction to run `qmd query` first. `{{.KBDir}}/research/` is the canonical home for deep dives.

**How to phrase the query.** Natural language beats keywords — the search already expands via vec + hyde. Lead with the concrete noun ("token rate limiter", "azure extraction") rather than the category ("performance", "reliability").

**What "found nothing useful" means.** If a proactive search returns no relevant hits, report the gap: "The KB doesn't cover this yet — if the answer turns out to be reusable, it's a good candidate for a drop file."

**How to contribute from other projects — drop files.** When a session in a non-KB project produces reusable knowledge, write a drop file to `.claude/{{.KBName}}/YYYY-MM-DD-{slug}.md` in the current project root with this frontmatter:

```yaml
---
{{.KBName}}: true
action: create | update | append
title: "Article Title"
type: project | tool | person | decision | pattern | solution | research
domain: {{.DomainsPipe}}
tags: [tag1, tag2]
---
```

`scribe sync` running on cron in the KB will absorb these automatically. For rolling insights that belong to a specific project's memory, add `rolling_target: learnings` or `rolling_target: decisions-log` to the frontmatter. Tell {{.OwnerName}} what you filed and why — don't fabricate drop files for trivial facts.

## Storage boundaries

1. **Knowledge base** (`{{.KBDir}}`) — the long-lived cross-project KB. Reusable patterns, architectural decisions, tool evaluations, research deep dives that apply to more than one project.
2. **Per-project research** (`.claude/research/` in the current project) — single-project research that won't be useful outside this codebase. Format: `YYYY-MM-DD-topic.md`.
3. **Session ephemera** (conversation context only) — task lists, scratch plans, intermediate findings with no value beyond the current session.

**Decision rubric when unsure:**
- Will this matter in a different project? → bucket 1 (drop file)
- Will this matter in *this* project next month? → bucket 2 (`.claude/research/`)
- Will this matter in 10 minutes? → bucket 3 (keep in context)
- Will this never matter again? → don't write it
