You are assessing the DOCS track of project '{{PROJECT}}' at '{{P_PATH}}' (domain: '{{DOMAIN}}').

Your one job: inventory the documentation that already exists inside this project. Nothing else. Other parallel tracks are handling structure, features, decisions, and gaps — do not duplicate their work.

Scope:
- Every README, CLAUDE.md, CONTRIBUTING.md, ARCHITECTURE.md, DESIGN.md, or similar top-level markdown
- .claude/ subdirectories: research/, plans/, analysis/, solutions/, lessons/, inspector/, knowledge/
- docs/ directory contents
- Inline comments worth surfacing are OUT of scope — that is source code, not docs
- Rate each doc: current | stale | historical. A doc is "stale" if its claims contradict the current code or refer to a state of the project that no longer exists.

What to read:
- Glob for *.md across the project (excluding deps/, node_modules/, _build/)
- Read only the top of each file (first 50 lines) to identify its purpose and freshness
- For files in .claude/, read the title + first paragraph only

Write your output to exactly one file: {{OUT}}

Format the output as plain markdown (no frontmatter). Sections:
  ## Top-Level Docs
  ## Planning & Research (.claude/)
  ## Formal Documentation (docs/)
  ## Freshness Verdict — list stale docs explicitly

Each entry format: `- path/to/file.md — purpose (verdict)`

Keep it tight: 40-80 lines total.

You are running non-interactively. Do not ask questions. Do not git commit. Do not spawn subagents.
