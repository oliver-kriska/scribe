You are assessing the DECISIONS track of project '{{PROJECT}}' at '{{P_PATH}}' (domain: '{{DOMAIN}}').

Your one job: extract the architectural and technical decisions visible in this project. Nothing else. Other parallel tracks are handling structure, features, docs, and gaps — do not duplicate their work.

Scope:
- Technology choices: which framework, database, queue, deploy target — and WHY when the reason is recoverable from commits, docs, or plan files
- Patterns adopted: file-over-app, CQRS, event sourcing, feature flags, whatever shows up
- Explicit trade-offs mentioned in docs or commits
- Rejected alternatives (when documented)
- Decisions that look locked-in vs. decisions that are marked provisional

Where to look (in order):
1. .claude/research/ and .claude/plans/ — these often contain reasoning
2. docs/decisions/, docs/adr/, ARCHITECTURE.md
3. CLAUDE.md
4. git log for commits with keywords: decided, chose, switched, migrate, rewrite, replace
5. Top of main config files (config.exs, config/runtime.exs, application.ex)

You MAY run: `git -C {{P_PATH}} log --oneline --grep="decided\|chose\|switched" -i | head -30`
You MAY run: `git -C {{P_PATH}} log --oneline -n 50` for recent context.

Write your output to exactly one file: {{OUT}}

Format the output as plain markdown (no frontmatter). Sections:
  ## Locked-In Choices
  ## Provisional / Reconsidering
  ## Rejected Alternatives
  ## Decision Sources — list where each decision was recovered from

Each entry: one sentence on WHAT, one sentence on WHY, one sentence on WHERE the reasoning lives.

Attribution over assertion: "project chose X because Y" not "X is the best choice". No peacock words.

Keep it tight: 40-80 lines total.

You are running non-interactively. Do not ask questions. Do not git commit. Do not spawn subagents.
