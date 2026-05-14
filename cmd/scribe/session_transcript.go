package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// session_transcript.go is the Phase 4C Go-side ccrider transcript
// fetcher. The previous session-mine path delegated message reads to
// the ccrider MCP tool from inside a `claude -p` invocation. The 4C
// orchestrator pattern pulls transcripts in Go and inlines them into
// the prompt so the LLM has no filesystem or MCP dependency — that's
// what makes the local-Ollama path work.
//
// The shape stays minimal on purpose: a transcript is a list of
// {role, text, ts} tuples in canonical order. Tool-use / tool-result
// turns get joined back together so the rendered transcript reads
// like a conversation, not a JSON-RPC log.

// sessionTurn is one user/assistant turn in a transcript. ToolText
// captures any tool-use or tool-result blob attached to the turn so a
// session-mine prompt can see the verb that was invoked without
// needing the structured payload.
type sessionTurn struct {
	// Role is "user", "assistant", or "system" (the third bucket is
	// rare; we keep it because some early ccrider rows used it).
	Role string
	// Text is the cleaned text the user/assistant exchanged. Tool
	// inputs/outputs come through ToolText.
	Text string
	// ToolText is a flat-text rendering of any tool-use / tool-result
	// payloads attached to the turn. Empty when the turn was pure
	// dialog.
	ToolText string
	// Sequence is the row's sequence column — useful for stable
	// ordering when the ccrider importer wrote multiple rows with
	// the same timestamp.
	Sequence int
}

// fetchSessionTranscript reads every message row for `sessionID` from
// the ccrider DB and returns them in canonical order. Empty slice and
// no error when the session has zero rows (e.g. an enqueued but
// not-yet-imported pending ID).
//
// The function deliberately keeps prepared-statement scope local so
// callers can fan-out cheaply. ccrider's index on session_id makes
// this O(n) in the number of messages even at 10k+ rows.
func fetchSessionTranscript(dbPath, sessionID string) ([]sessionTurn, error) {
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open ccrider db: %w", err)
	}
	defer db.Close()

	//nolint:noctx // CLI top-level
	rows, err := db.Query(`
		SELECT m.type, COALESCE(m.text_content, ''), COALESCE(m.content, ''), COALESCE(m.sequence, 0)
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE s.session_id = ?
		ORDER BY m.sequence ASC, m.id ASC`,
		sessionID)
	if err != nil {
		return nil, fmt.Errorf("query transcript: %w", err)
	}
	defer rows.Close()

	var turns []sessionTurn
	for rows.Next() {
		var typ, textContent, content string
		var seq int
		if err := rows.Scan(&typ, &textContent, &content, &seq); err != nil {
			continue
		}
		t := sessionTurn{Role: roleFromCcriderType(typ), Sequence: seq, Text: strings.TrimSpace(textContent)}
		// `content` is the original JSON-shaped tool payload. We
		// pass it through as ToolText so prompts can decide whether
		// to render it; the orchestrator's renderer truncates each
		// turn so a 100 KB tool result doesn't blow the context.
		if textContent == "" && content != "" {
			t.ToolText = strings.TrimSpace(content)
		}
		if t.Text == "" && t.ToolText == "" {
			continue
		}
		turns = append(turns, t)
	}
	if err := rows.Err(); err != nil {
		return turns, fmt.Errorf("read transcript: %w", err)
	}
	return turns, nil
}

// roleFromCcriderType maps the ccrider `type` column to a normalized
// role string. Anything we don't recognize falls back to "system" so
// downstream renderers don't drop the row silently — but "tool" gets
// its own bucket so the renderer can label tool turns distinctly from
// the assistant's prose (future ccrider versions are expected to split
// these).
func roleFromCcriderType(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "user":
		return "user"
	case "assistant":
		return "assistant"
	case "system":
		return "system"
	case "tool", "tool_use", "tool_result":
		return "tool"
	default:
		return "system"
	}
}

// renderTranscriptForPrompt converts a transcript into a flat text
// block suitable for inlining in an LLM prompt. Each turn becomes one
// labeled paragraph; tool turns get a short prefix marker so the
// session-mine prompt can distinguish dialog from tool noise.
//
// maxChars caps the total render size. When the transcript exceeds
// the cap, the renderer keeps the head and tail and inserts a
// "[truncated ...]" marker in the middle — the head holds the user's
// initial framing and the tail holds the conclusion, which is where
// session-mine extracts most of its signal. Pass 0 to disable.
func renderTranscriptForPrompt(turns []sessionTurn, maxChars int) string {
	var sb strings.Builder
	for _, t := range turns {
		switch t.Role {
		case "user":
			sb.WriteString("## USER\n\n")
		case "assistant":
			sb.WriteString("## ASSISTANT\n\n")
		case "system":
			sb.WriteString("## SYSTEM\n\n")
		case "tool":
			sb.WriteString("## TOOL\n\n")
		}
		if t.Text != "" {
			sb.WriteString(t.Text)
			sb.WriteString("\n")
		}
		if t.ToolText != "" {
			// Trim per-turn tool payloads so a single big edit
			// doesn't push the transcript past the model's
			// context. 1200 chars head + tail keeps the verb
			// and result visible.
			tool := t.ToolText
			if len(tool) > 1200 {
				tool = tool[:600] + "\n…\n" + tool[len(tool)-600:]
			}
			sb.WriteString("\n[tool] ")
			sb.WriteString(tool)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
	rendered := sb.String()
	if maxChars <= 0 || len(rendered) <= maxChars {
		return rendered
	}
	half := maxChars / 2
	if half < 200 {
		half = 200
	}
	if half >= len(rendered) {
		return rendered
	}
	return rendered[:half] + "\n\n[…transcript truncated for prompt budget…]\n\n" + rendered[len(rendered)-half:]
}
