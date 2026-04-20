You are working in {{KB_DIR}}. Extract knowledge from project '{{PROJECT}}' at '{{P_PATH}}' (domain: '{{DOMAIN}}').

Step 1: Read {{KB_DIR}}/CLAUDE.md for schema and frontmatter conventions.
{{DROP_INSTRUCTION}}Step 2: {{STEP2}}
Step 3: Read the knowledge-dense .md files in the project (docs/, decisions/, research/, plans/, analysis/, .claude/ subdirs — including .claude/research/, .claude/plans/, .claude/analysis/, .claude/solutions/, .claude/inspector/, .claude/lessons/). Use Glob to find them, then Read. Skip source code, test files, build artifacts, and evals/.
Step 3.5: Cross-reference check — before writing new articles, search existing wiki articles (using Grep) for overlapping topics. If project knowledge extends or contradicts an existing article from another domain, update that article with a cross-reference. This is the core value of the KB.
Step 4: For each piece of extractable knowledge (decisions, patterns, tools, research findings, evaluations, analyses), write or update wiki articles in {{KB_DIR}} following CLAUDE.md frontmatter conventions. Set domain: '{{DOMAIN}}'. Set sources to the original file paths. Score the `confidence:` field using the Confidence Rubric in CLAUDE.md: `high` for assertive language backed by committed code / deployed behavior / primary source; `medium` for evaluated-but-not-committed mixed-register claims; `low` for hedging ("maybe", "probably"), speculation, or single-opinion sources. Default to `medium` when unsure. Do not inflate — dream uses `confidence:` as the arbiter when contradictions surface across domains, so a falsely `high` article corrupts later resolution.
Step 4.5: For learnings and decisions discovered during extraction, append entries to rolling memory files:
  - projects/{name}/learnings.md — insights and lessons learned
  - projects/{name}/decisions-log.md — decisions with context
  Create these files if they don't exist. Use frontmatter with 'rolling: true'. Entry format: '## YYYY-MM-DD | One-line summary' followed by Context/Source paragraphs, separated by '---'. Newest entries go at the top (after frontmatter). Only add genuinely new findings — not summaries of existing articles.
Step 5: After all writes, check line counts on every article you modified. Split any article over 150 lines into focused sub-articles. For files with 'rolling: true' in frontmatter, archive oldest entries (from bottom) to {name}-archive-YYYY.md instead of splitting. Project articles go in projects/{lowercase-name}/overview.md, never projects/Name.md.
Step 6: Rebuild {{KB_DIR}}/wiki/_index.md and {{KB_DIR}}/wiki/_backlinks.json.
{{FILELIST}}

Do NOT git commit. Do NOT use subagents. Keep it focused: extract the non-obvious knowledge, not file summaries.
You are running non-interactively. Never ask questions — decide and act.
