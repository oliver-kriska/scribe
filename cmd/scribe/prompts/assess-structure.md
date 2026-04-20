You are assessing the STRUCTURE track of project '{{PROJECT}}' at '{{P_PATH}}' (domain: '{{DOMAIN}}').

Your one job: produce a structural map of this project. Nothing else. Other parallel tracks are handling features, docs, decisions, and gaps — do not duplicate their work.

Scope:
- Top-level directory layout: what each directory holds
- Entry points: main binaries, CLI commands, server endpoints, worker registries
- Module boundaries: how the code is organized into logical units
- Persistence: databases, schema locations, migration directories
- External dependencies: config files, services, keys this project expects to exist
- Deployment target: where this runs (Fly, self-host, desktop, CLI, etc.)

What to read:
- Root of {{P_PATH}}: README, mix.exs / package.json / go.mod / Cargo.toml / pyproject.toml, Justfile / Makefile, Dockerfile, .env.example
- config/ or equivalent
- lib/ or src/ — read filenames only via Glob, not contents, unless a file is obviously an entry point (main.go, application.ex, app.tsx, etc.)
- Do NOT read individual source files beyond entry points. Your job is the map, not the territory.

Write your output to exactly one file: {{OUT}}

Format the output as plain markdown (no frontmatter). Sections:
  ## Directory Layout
  ## Entry Points
  ## Module Boundaries
  ## Persistence
  ## External Dependencies
  ## Deployment

Keep it tight: 40-80 lines total. Facts only. No prose padding.

You are running non-interactively. Do not ask questions. Do not git commit. Do not spawn subagents.
