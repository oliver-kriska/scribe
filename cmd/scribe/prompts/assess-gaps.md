You are assessing the GAPS track of project '{{PROJECT}}' at '{{P_PATH}}' (domain: '{{DOMAIN}}').

Your one job: identify what is missing, broken, or flagged as unfinished. Nothing else. Other parallel tracks are handling structure, features, docs, and decisions — do not duplicate their work.

Scope:
- TODO / FIXME / HACK / XXX comments in source code
- Tasks listed in .claude/plans/, .claude/research/, or TODO.md
- Known bugs surfaced in README, CHANGELOG, or docs
- Tests that are skipped or marked pending
- Scaffolded code that was never wired up
- Dead or orphaned modules
- Recent commits marked "wip", "draft", "stub"
- Tooling that is documented but not installed/configured (e.g. CI mentioned but no workflow file)

How to hunt:
- `git -C {{P_PATH}} grep -n "TODO\|FIXME\|HACK\|XXX" -- ':(exclude)deps' ':(exclude)node_modules' ':(exclude)_build'` | head -60
- Read .claude/plans/*.md and TODO*.md if they exist
- Check CHANGELOG.md and README for "known issues" sections
- Look at test runners: @tag :skip (Elixir), .skip (Jest), pytest.mark.skip, etc.

Write your output to exactly one file: {{OUT}}

Format the output as plain markdown (no frontmatter). Sections:
  ## TODO / FIXME Inventory
  ## Planned But Unfinished
  ## Known Bugs
  ## Dead Code / Orphans
  ## Missing Tooling

Each entry: one line, file:line when applicable.

Distinguish signal from noise. A 2-year-old TODO in a rarely-touched file matters less than a TODO in the hot path. Do not include every TODO — pick the ones a new collaborator actually needs to know about. Cap at ~20 per section.

Keep it tight: 40-80 lines total.

You are running non-interactively. Do not ask questions. Do not git commit. Do not spawn subagents.
