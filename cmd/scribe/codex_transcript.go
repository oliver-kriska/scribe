package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// codex_transcript.go is the C3 Codex-side analog of
// session_transcript.go's ccrider fetcher. It turns a Codex CLI
// rollout (~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl) into the same
// []sessionTurn shape the envelope session-mine path already consumes,
// so everything downstream (renderTranscriptForPrompt,
// runSessionEnvelopeOnce, applyWikiActions) is reused unchanged — the
// only Codex-specific surface is this one function.
//
// SCHEMA (verified 2026-05-15 against a real 176-event scribe rollout;
// fixture pinned at testdata/codex/rollout-transcript.jsonl). Codex
// writes the OpenAI Responses-API item schema. Every line is the
// codexRolloutEnvelope {timestamp,type,payload}. Two parallel streams
// exist:
//
//   - response_item — the canonical model I/O stream. THE ONLY stream
//     we consume.
//   - event_msg     — the UI/telemetry stream. Every content-bearing
//     event_msg (user_message, agent_message) DUPLICATES a
//     response_item/message; the rest is noise (token_count is the
//     single most frequent event). Consuming both double-counts every
//     turn — the trap the original C3 plan's guessed table fell into.
//
// response_item payload sub-types (payload.type):
//
//	message              role=user|assistant, content:[{type:input_text|
//	                     output_text, text}] → sessionTurn.Text
//	function_call        {name, arguments(JSON str), call_id} → ToolText
//	function_call_output {call_id, output} → ToolText
//	reasoning            content:null, encrypted_content only —
//	                     UNRECOVERABLE, skipped
//
// Skipped entirely: every event_msg, turn_context, session_meta, and
// the leading synthetic <environment_context> user turn Codex injects
// (cwd/shell/date — pure harness noise that would pollute triage).

// codexResponseItem is the flattened union of the response_item
// payload sub-shapes we care about. One unmarshal, switch on Type;
// absent fields stay zero (encoding/json ignores what isn't present).
type codexResponseItem struct {
	Type      string             `json:"type"`
	Role      string             `json:"role"`
	Content   []codexContentPart `json:"content"`
	Name      string             `json:"name"`
	Arguments string             `json:"arguments"`
	CallID    string             `json:"call_id"`
	Output    string             `json:"output"`
}

type codexContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// fetchCodexTranscript reads a rollout JSONL and returns its turns in
// append order. Robustness mirrors readCodexSessionMeta: a single
// malformed line is skipped (not fatal), an empty/zero-turn rollout
// yields an empty slice and no error so the caller can skip cheaply.
//
// A bufio.Reader (not bufio.Scanner) is used deliberately: a
// function_call_output line can legitimately exceed the 1 MB
// scanner ceiling on a big file read or grep, and Scanner would abort
// the whole rest of the transcript on bufio.ErrTooLong. ReadString
// has no line-length limit.
func fetchCodexTranscript(rolloutPath string) ([]sessionTurn, error) {
	f, err := os.Open(rolloutPath)
	if err != nil {
		return nil, fmt.Errorf("open codex rollout: %w", err)
	}
	defer f.Close()

	var turns []sessionTurn
	r := bufio.NewReader(f)
	lineNo := -1
	for {
		line, rerr := r.ReadString('\n')
		if s := strings.TrimSpace(line); s != "" {
			lineNo++
			if t, ok := codexLineToTurn(s, lineNo); ok {
				turns = append(turns, t)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return turns, fmt.Errorf("read codex rollout: %w", rerr)
		}
	}
	return turns, nil
}

// codexLineToTurn maps one rollout line to a sessionTurn. Returns
// (_, false) for every line that is not a content-bearing
// response_item (event_msg, turn_context, session_meta, reasoning,
// the synthetic environment_context user turn, or a malformed line).
func codexLineToTurn(line string, seq int) (sessionTurn, bool) {
	var env codexRolloutEnvelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		return sessionTurn{}, false // malformed → skip, non-fatal
	}
	if env.Type != "response_item" {
		return sessionTurn{}, false // event_msg / turn_context / session_meta
	}
	var item codexResponseItem
	if err := json.Unmarshal(env.Payload, &item); err != nil {
		return sessionTurn{}, false
	}
	switch item.Type {
	case "message":
		text := codexMessageText(item.Content)
		if text == "" {
			return sessionTurn{}, false
		}
		if isEnvironmentContext(text) {
			return sessionTurn{}, false // synthetic harness turn
		}
		role := item.Role
		if role != "user" && role != "assistant" {
			role = "system"
		}
		return sessionTurn{Role: role, Text: text, Sequence: seq}, true
	case "function_call":
		tool := strings.TrimSpace(item.Name + "(" + item.Arguments + ")")
		if item.CallID != "" {
			tool = "call " + item.CallID + ": " + tool
		}
		return sessionTurn{Role: "tool", ToolText: tool, Sequence: seq}, true
	case "function_call_output":
		out := strings.TrimSpace(item.Output)
		if out == "" {
			return sessionTurn{}, false
		}
		if item.CallID != "" {
			out = "result " + item.CallID + ": " + out
		}
		return sessionTurn{Role: "tool", ToolText: out, Sequence: seq}, true
	default:
		// reasoning (encrypted/unrecoverable) and any future
		// response_item sub-type we don't model: skip rather than
		// emit a garbage turn.
		return sessionTurn{}, false
	}
}

// codexMessageText concatenates the text of every input_text /
// output_text content part. Other part types (images, refusals) carry
// no mineable prose and are ignored.
func codexMessageText(parts []codexContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "input_text" || p.Type == "output_text" {
			if p.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

// isEnvironmentContext reports whether a user message is the synthetic
// <environment_context> block Codex injects each turn (cwd/shell/date).
// It is harness scaffolding, not user intent, and would skew triage
// scoring and pollute the mined transcript.
func isEnvironmentContext(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, "<environment_context>") &&
		strings.Contains(t, "</environment_context>")
}
