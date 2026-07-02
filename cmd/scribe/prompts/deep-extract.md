You are working in {{KB_DIR}}. Extract knowledge from directory '{{REL_DIR}}' in project '{{PROJECT}}' at '{{P_PATH}}' (domain: '{{DOMAIN}}').

Step 1: Read {{KB_DIR}}/CLAUDE.md for schema and frontmatter conventions.
Step 2: Read these files: {{FILE_LIST}}
Step 3: For each piece of extractable knowledge (decisions, patterns, tools, research findings, evaluations, analyses), write or update wiki articles in {{KB_DIR}} following CLAUDE.md frontmatter conventions. Set domain: '{{DOMAIN}}'. Set sources to the original file paths.
Step 3.5: Research before creating — for EACH candidate new article, Grep {{KB_DIR}}/wiki/_index.md for the candidate title's distinctive words and Grep the content dirs for 2-3 topic keywords. On a match (any domain), UPDATE the existing article — append a section, cross-reference, extend `sources:` — and reuse its exact `title:` in wikilinks. Create only when the search comes back empty; a near-duplicate page splits future updates and corrupts contradiction resolution.
Step 4: For learnings and decisions, append entries to rolling memory files (projects/{name}/learnings.md, projects/{name}/decisions-log.md).
Step 5: Check line counts. Split any article over 150 lines. Project articles go in projects/{lowercase-name}/.
Step 6: Rebuild {{KB_DIR}}/wiki/_index.md and {{KB_DIR}}/wiki/_backlinks.json.

Extract the non-obvious knowledge — research findings, design decisions with reasoning, evaluation results, quantitative data, patterns. Not file summaries. When the source documents a debugging or decision process, also capture what was tried and rejected (and why) and the conditions under which the chosen approach breaks — not just the fix or verdict; a reader avoiding a known dead end is often more valuable than the fix itself. This is a guideline, not a required section — skip it when the source has no failed attempts or known failure conditions to report.
Do NOT git commit. Do NOT use subagents.
You are running non-interactively. Never ask questions — decide and act.
