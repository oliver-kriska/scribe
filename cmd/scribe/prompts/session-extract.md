You are working in {{KB_DIR}}. Read CLAUDE.md for schema and writing standards.

Extract knowledge from these specific Claude Code sessions (pre-scored as knowledge-rich by FTS5 triage):

Session IDs: {{SESSION_ID_LIST}}

Related sessions from the same project (already extracted or being extracted in parallel — reference them via [[wikilinks]] or session:id rather than duplicating their content):
{{RELATED_SESSIONS}}

For each session:
1. Use ccrider MCP tool get_session_messages to read the session content
2. Identify extractable knowledge: decisions with reasoning, architecture patterns, research findings, tool evaluations, lessons learned
3. Write or update wiki articles in {{KB_DIR}} following frontmatter conventions
4. Set sources to include session:{session_id}
5. For learnings and decisions, append to rolling memory files (projects/{name}/learnings.md, decisions-log.md)
6. Update wiki/_sessions_log.json — add each processed session ID to the 'processed' object with timestamp
7. Rebuild _index.md and _backlinks.json

Extract the non-obvious knowledge — decisions with reasoning, patterns that apply across projects, research findings with data. Not conversation summaries or routine code changes.
Do NOT git commit.
You are running non-interactively. Never ask questions — decide and act.
