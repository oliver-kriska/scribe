You are working in {{KB_DIR}}. Extract knowledge from directory '{{REL_DIR}}' in project '{{PROJECT}}' at '{{P_PATH}}' (domain: '{{DOMAIN}}').

Step 1: Read {{KB_DIR}}/CLAUDE.md for schema and frontmatter conventions.
Step 2: Read these files: {{FILE_LIST}}
Step 3: For each piece of extractable knowledge (decisions, patterns, tools, research findings, evaluations, analyses), write or update wiki articles in {{KB_DIR}} following CLAUDE.md frontmatter conventions. Set domain: '{{DOMAIN}}'. Set sources to the original file paths.
Step 3.5: Cross-reference check — search existing wiki articles (using Grep) for overlapping topics. If this knowledge extends or contradicts an existing article from another domain, update that article with a cross-reference.
Step 4: For learnings and decisions, append entries to rolling memory files (projects/{name}/learnings.md, projects/{name}/decisions-log.md).
Step 5: Check line counts. Split any article over 150 lines. Project articles go in projects/{lowercase-name}/.
Step 6: Rebuild {{KB_DIR}}/wiki/_index.md and {{KB_DIR}}/wiki/_backlinks.json.

Extract the non-obvious knowledge — research findings, design decisions with reasoning, evaluation results, quantitative data, patterns. Not file summaries.
Do NOT git commit. Do NOT use subagents.
You are running non-interactively. Never ask questions — decide and act.
