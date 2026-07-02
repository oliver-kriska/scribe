package main

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

// adoption.go computes the KB-first adoption metric for issue #23: among
// scoped Claude Code sessions that made at least one "decision" (an
// Edit/Write/MultiEdit tool call), what fraction queried the KB via qmd
// BEFORE that first decision? See docs/issue-23-adoption-metric-plan.md
// for the full design (D1-D8).
//
// # Derivation of the message `type` filter and marker shapes (read this
// # before changing either)
//
// The plan's original draft assumed ccrider stored tool invocations as
// their own message rows with `type IN ('tool', 'tool_use')`. That
// assumption is WRONG for the real ccrider schema, verified by reading
// ccrider's source (github.com/neilberkman/ccrider, checked out
// read-only at /Users/oliverkriska/Projects/ccrider — never modified)
// plus counts-only probes against the real DB (COUNT(*)/GROUP BY/LIKE
// patterns only — no row content was ever extracted, matching the
// "counts-only" methodology the plan's addendum already established):
//
//  1. ccrider/internal/core/db/schema.go declares `messages.type TEXT
//     NOT NULL`. ccrider/pkg/ccsessions/parser.go's parseMessage sets it
//     to the JSONL line's own top-level "type" field verbatim: "user",
//     "assistant", "system", "file-history-snapshot", or
//     "queue-operation" ("summary" lines never become message rows —
//     they set session.Summary instead). There is no "tool"/"tool_use"
//     value anywhere in that vocabulary.
//  2. ccrider/internal/core/importer/importer.go's ImportSession SKIPS
//     inserting any message whose extracted TextContent is empty after
//     trimming (`if trimmed == "" { continue }`). parser.go's switch
//     only ever populates TextContent for "user" and "assistant" lines
//     (text/tool_result-text blocks for user, text blocks for
//     assistant); "system"/"file-history-snapshot"/"queue-operation"
//     always get TextContent="" and are therefore NEVER stored. A
//     real-DB sanity probe confirms this: `SELECT type, COUNT(*) FROM
//     messages GROUP BY type` returns only 'assistant' and 'user' rows;
//     `type IN ('tool','tool_use','tool_result')` returns 0 rows.
//  3. Anthropic's tool_use content blocks
//     (`{"type":"tool_use","id":...,"name":...,"input":{...}}`) live
//     INSIDE an assistant-message's `content` array — never as a
//     separate JSONL line/type of their own. ccrider's ParsedMessage.Content
//     is `raw.Message`, a `json.RawMessage` — the exact, byte-for-byte
//     "message" JSON value from the source JSONL line, written into the
//     `content` TEXT column as `string(msg.Content)` with NO
//     re-serialization (importer.go). So a tool_use block's JSON text is
//     only ever found inside a `type='assistant'` row's `content`
//     column. tool_result blocks, by contrast, live nested inside
//     `type='user'` rows' content arrays (parser.go's userMsgArray
//     struct) — so scanning `type = 'assistant'` structurally excludes
//     tool_result payloads, which is what the plan's original ('tool'
//     vs 'tool_result') distinction was trying to achieve, just via a
//     filter that actually matches ccrider's real schema.
//  4. Compact vs. spaced JSON: a counts-only probe confirms Claude Code
//     writes this content compact — `content LIKE '%"type":"tool_use"%'`
//     matches (79 messages at probe time), `content LIKE '%"type":
//     "tool_use"%'` (one space) matches zero, every time, across both
//     the tool_use marker and a `"name":"Edit"` decision marker. Marker
//     lists below still carry a spaced fallback form per the original
//     plan's defensive reasoning (a future Claude Code version could
//     start pretty-printing), but only the compact form is expected to
//     ever match in practice.
//
// Net effect: the SQL filter is `m.type = 'assistant'`, not `m.type IN
// ('tool', 'tool_use')`. The latter would silently return zero rows
// forever — a metric permanently stuck at 0/0 — against real ccrider
// data.
//
// # Known, accepted limitation (beyond the plan's D4 scan-cap note)
//
// Because ccrider's importer (point 2 above) drops any message with no
// extracted text, an assistant turn that is PURELY a tool call — no
// accompanying prose, which is extremely common in agentic sessions —
// never reaches the `messages` table at all. This metric can only see
// tool calls that happened to share a turn with some narration text.
// That makes the available signal an inherently biased subsample of a
// session's real tool-call history, not the complete record. This is a
// structural property of ccrider's schema (a separate, read-only
// project here) and is not something scribe can fix by changing this
// query — documented so a future reader doesn't mistake a small
// KB-first sample size for a bug in this file.
const adoptionSequenceCap = 300

// adoptionMessageType is the only ccrider `messages.type` value that can
// ever carry a tool_use block — see the file-level comment above.
const adoptionMessageType = "assistant"

// qmdToolMarkers: any MCP server alias ending in "qmd__query" etc. (not
// hardcoded to "mcp__plugin_qmd_qmd__" — a user's local MCP registration
// name can differ), plus the Bash CLI fallback form the handshake block
// documents ("prefer mcp__plugin_qmd_qmd__query ... or `qmd query`/`qmd
// search` via Bash"). Matched via strings.Contains against the raw
// message content, so both an MCP tool_use block's "name" field and a
// Bash tool_use block's input.command string are caught by the same
// substring test.
var qmdToolMarkers = []string{
	"qmd__query", "qmd__get", "qmd__multi_get", "qmd__status",
	"qmd query", "qmd search",
}

// decisionToolMarkers: the standard Anthropic tool_use JSON shape
// (`"name":"Edit"`, with and without the space Go's encoding/json and
// other JSON emitters may or may not insert after the colon — see the
// file-level comment on why only the compact form is expected in
// practice). MultiEdit is Claude Code's older multi-hunk edit tool,
// kept for sessions mined from earlier tool-set versions.
var decisionToolMarkers = []string{
	`"name":"Edit"`, `"name": "Edit"`,
	`"name":"Write"`, `"name": "Write"`,
	`"name":"MultiEdit"`, `"name": "MultiEdit"`,
}

// adoptionWindowResult is one time-window's KB-first ratio.
type adoptionWindowResult struct {
	Days               int
	DecisionSessions   int // sessions with >=1 decision tool call (denominator)
	KBFirstSessions    int // of those, qmd call preceded first decision call (numerator)
	ExcludedNoDecision int // scoped sessions with zero decision tool calls, informational only
}

// Ratio returns the KB-first fraction, 0 when there are no decision
// sessions to divide by (never panics on divide-by-zero).
func (r adoptionWindowResult) Ratio() float64 {
	if r.DecisionSessions == 0 {
		return 0
	}
	return float64(r.KBFirstSessions) / float64(r.DecisionSessions)
}

// classifyToolContent inspects one assistant-type message row's raw
// content (falling back to text_content when content is empty) and
// reports whether it looks like a qmd query/search/get call and/or a
// code-decision call (Edit/Write/MultiEdit). Plain substring matching,
// not JSON parsing — see the file-level comment for why, and for what
// "content" actually holds.
func classifyToolContent(content, textContent string) (isQMD, isDecision bool) {
	haystack := content
	if haystack == "" {
		haystack = textContent
	}
	if haystack == "" {
		return false, false
	}
	isQMD = containsAny(haystack, qmdToolMarkers)
	isDecision = containsAny(haystack, decisionToolMarkers)
	return isQMD, isDecision
}

func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// scopedClaudeSessionIDs returns the ccrider integer session ids in scope
// for the adoption metric (plan D3): provider=claude, updated within
// sinceDays, approved manifest project, not a KB-curation session.
func scopedClaudeSessionIDs(db *sql.DB, root string, sinceDays int) ([]int64, error) {
	manifest, err := loadManifest(root)
	if err != nil {
		return nil, err
	}
	//nolint:noctx // CLI top-level, no context in scope
	rows, err := db.Query(`
		SELECT s.id, COALESCE(s.project_path, '')
		FROM sessions s
		WHERE COALESCE(s.provider, 'claude') = 'claude'
		  AND s.updated_at >= datetime('now', '-' || ? || ' days')`, sinceDays)
	if err != nil {
		return nil, fmt.Errorf("query scoped sessions: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		var projectPath string
		if err := rows.Scan(&id, &projectPath); err != nil {
			continue
		}
		if projectPath == "" || sessionInKB(root, projectPath) {
			continue
		}
		entry := manifest.entryForPath(projectPath)
		if entry == nil || !entry.IsApproved() {
			continue
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// computeAdoptionWindow runs the full scope+scan+aggregate pipeline for one
// window (7 or 30 days).
func computeAdoptionWindow(db *sql.DB, root string, days int) (adoptionWindowResult, error) {
	result := adoptionWindowResult{Days: days}
	ids, err := scopedClaudeSessionIDs(db, root, days)
	if err != nil || len(ids) == 0 {
		return result, err
	}

	idList := make([]string, len(ids))
	for i, id := range ids {
		idList[i] = strconv.FormatInt(id, 10)
	}
	// Integer IDs from our own DB scan — safe to interpolate (no user
	// input reaches this string), same reasoning as buildExcludeClause's
	// sanitized-then-joined session_id list in triage.go. m.type is a
	// fixed literal (adoptionMessageType), not user input either — see
	// the file-level comment for why 'assistant' is the only type that
	// can carry a tool_use block.
	query := fmt.Sprintf(`
		SELECT m.session_id, COALESCE(m.content, ''), COALESCE(m.text_content, '')
		FROM messages m
		WHERE m.session_id IN (%s)
		  AND m.type = '%s'
		  AND COALESCE(m.sequence, 0) <= %d
		ORDER BY m.session_id ASC, m.sequence ASC, m.id ASC`,
		strings.Join(idList, ","), adoptionMessageType, adoptionSequenceCap)

	//nolint:noctx // CLI top-level, no context in scope
	rows, err := db.Query(query)
	if err != nil {
		return result, fmt.Errorf("query tool rows: %w", err)
	}
	defer rows.Close()

	type marker struct{ qmdIdx, decisionIdx int } // -1 = not seen
	seen := map[int64]*marker{}
	idx := map[int64]int{} // running position counter per session

	for rows.Next() {
		var sid int64
		var content, textContent string
		if err := rows.Scan(&sid, &content, &textContent); err != nil {
			continue
		}
		m, ok := seen[sid]
		if !ok {
			m = &marker{qmdIdx: -1, decisionIdx: -1}
			seen[sid] = m
		}
		pos := idx[sid]
		idx[sid] = pos + 1

		isQMD, isDecision := classifyToolContent(content, textContent)
		if isQMD && m.qmdIdx == -1 {
			m.qmdIdx = pos
		}
		if isDecision && m.decisionIdx == -1 {
			m.decisionIdx = pos
		}
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("read tool rows: %w", err)
	}

	for _, m := range seen {
		if m.decisionIdx == -1 {
			result.ExcludedNoDecision++
			continue
		}
		result.DecisionSessions++
		if m.qmdIdx != -1 && m.qmdIdx < m.decisionIdx {
			result.KBFirstSessions++
		}
	}
	// Sessions in scope with zero tool rows at all (never entered `seen`)
	// are implicitly "no decision" too — count them for completeness.
	result.ExcludedNoDecision += len(ids) - len(seen)
	return result, nil
}

// computeAdoptionMetrics runs both windows. Best-effort: DB-open failure
// returns nil, nil (caller skips the runStats fields, matching how other
// sync phases degrade when ccrider is unavailable).
func computeAdoptionMetrics(root string, cfg *ScribeConfig) ([]adoptionWindowResult, error) {
	if cfg.CcriderDB == "" || !fileExists(cfg.CcriderDB) {
		return nil, nil
	}
	db, err := sql.Open("sqlite3", cfg.CcriderDB+"?mode=ro")
	if err != nil {
		// Best-effort, matches how other sync phases degrade when
		// ccrider is unavailable. golangci-lint's nilerr linter doesn't
		// actually flag this pattern (verified: a //nolint:nilerr here
		// reports as unused), so no directive is needed — see the
		// adoption plan Step 2 implementer note.
		return nil, nil
	}
	defer db.Close()

	var results []adoptionWindowResult
	for _, days := range []int{7, 30} {
		r, err := computeAdoptionWindow(db, root, days)
		if err != nil {
			logMsg("sync", "adoption metric (%dd) failed: %v", days, err)
			continue
		}
		results = append(results, r)
	}
	return results, nil
}

// adoptionRunStatsFields flattens window results into the flat-key shape
// runStats/writeRunRecord expects (main.go).
func adoptionRunStatsFields(results []adoptionWindowResult) map[string]any {
	out := map[string]any{}
	for _, r := range results {
		prefix := fmt.Sprintf("adoption_kb_first_%dd", r.Days)
		out[prefix+"_ratio"] = r.Ratio()
		out[prefix+"_num"] = r.KBFirstSessions
		out[prefix+"_den"] = r.DecisionSessions
	}
	return out
}
