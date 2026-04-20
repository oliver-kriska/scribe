You are working in {{KB_DIR}}. Read CLAUDE.md for schema and writing standards.

Extract knowledge from this LARGE Claude Code session ({{MESSAGE_COUNT}} messages):

Session ID: {{SESSION_ID}}

Related sessions from the same project (reference these via [[wikilinks]] or session:id rather than duplicating their content):
{{RELATED_SESSIONS}}

This session is too large to read in full. Follow this strategy:
1. First, use generate_session_anchor to get the session summary and key points
2. Use get_session_messages to read the FIRST 50 messages (early context and goals)
3. Use get_session_messages to read the LAST 50 messages (conclusions and outcomes)
4. Based on the anchor summary, identify decision points or turning points and read those specific message ranges
5. Focus on extracting: decisions with reasoning, architecture patterns, research findings with data, tool evaluations with verdicts, lessons learned from mistakes

Be selective. Large sessions contain mostly routine code changes. Extract only high-signal knowledge — decisions, patterns, research findings. Skip: routine file edits, test fixes, formatting changes, debugging steps without insights.

Write or update wiki articles in {{KB_DIR}} following frontmatter conventions.
Set sources to include session:{{SESSION_ID}}.
For learnings and decisions, append to rolling memory files (projects/{name}/learnings.md, decisions-log.md).
Update wiki/_sessions_log.json — add the processed session ID to the 'processed' object with timestamp, project, and summary.
Rebuild _index.md and _backlinks.json.

Do NOT git commit.
You are running non-interactively. Never ask questions — decide and act.
