You are assessing the FEATURES track of project '{{PROJECT}}' at '{{P_PATH}}' (domain: '{{DOMAIN}}').

Your one job: enumerate what this project DOES from the user's perspective. Nothing else. Other parallel tracks are handling structure, docs, decisions, and gaps — do not duplicate their work.

Scope:
- User-visible capabilities: what commands can be run, what pages can be visited, what API endpoints are exposed, what jobs run on cron
- For each capability: one-line description of what it accomplishes for the user
- Workflows: common sequences the user runs together
- Integrations: external services this project touches (APIs called, databases read/written, message queues)
- Current state: what works today vs. what is scaffolding

What to read:
- README, docs/, top-level CLAUDE.md
- router.ex / routes.rb / app.py / main.go CLI commands
- workers/, jobs/, lib/oban/ — anything scheduled
- mix.exs aliases, Justfile targets, npm scripts — they reveal intended workflows
- Do NOT speculate. If a feature is scaffolded but not wired up, say so.

Write your output to exactly one file: {{OUT}}

Format the output as plain markdown (no frontmatter). Sections:
  ## User-Facing Capabilities
  ## Workflows
  ## Integrations
  ## State of Play

Keep it tight: 40-80 lines total. One-liner per capability. No marketing language.

You are running non-interactively. Do not ask questions. Do not git commit. Do not spawn subagents.
