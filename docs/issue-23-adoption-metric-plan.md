# Issue #23 — KB-first adoption metric (roadmap-01)

Part of the roadmap umbrella #6, third in the suggested order. Phase 4 of
`docs/issues-master-plan.md` (with #8, #24). This plan is self-sufficient —
written for an implementer with no prior context on this repo or issue.

## 1. Problem & context

The whole point of scribe's Claude/Codex handshake blocks
(`~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md` — written by `scribe init`, see
`CLAUDE.md` "Key external surfaces" table) is that agent sessions query the
KB via `qmd` *before* making decisions, not after. There is currently no
measurement of whether that actually happens. Issue #23 asks for:

> Measure whether agent sessions actually query the KB before making
> decisions. Needs ccrider-side signal extraction: detect qmd query/search
> tool calls early in sessions vs decisions made without them, and surface
> the ratio in scribe status or the weekly digest.

Relevant existing code:

- `cmd/scribe/session_transcript.go:50-94` (`fetchSessionTranscript`) is the
  only place in the codebase that currently reads ccrider's `messages.content`
  column (the raw JSON-shaped tool payload) and `messages.sequence` column.
  It has **no test file** — nothing exercises it, and as shown below the test
  fixture doesn't even have those columns yet.
- `cmd/scribe/testdata/ccrider_schema.sql:19-24` — the `messages` table in the
  test fixture only has `id, session_id, type, text_content`. It is missing
  `content` and `sequence`, which the real ccrider DB has (per
  `session_transcript.go:58-59`'s query). This must be fixed before the new
  metric (or `fetchSessionTranscript`) can be tested at all.
- `cmd/scribe/sync_sessions.go:47-61` (`querySessionStats`) and
  `:128-180` (`preFilterSessions`) show the established pattern for reading
  ccrider session/message stats: open `sql.Open("sqlite3", dbPath+"?mode=ro")`,
  query with `//nolint:noctx // CLI top-level, no context in scope`.
- `cmd/scribe/status.go:379-540` (`renderBacklog`, `countScopedPendingSessions`)
  is the existing precedent for "scope sessions to THIS KB's approved
  projects" — it uses `manifest.entryForPath(path).IsApproved()`, not the
  broader `projectScopeAllowed` used by the sync-time mining lane. This plan
  reuses that narrower, already-established scoping rule (see Design
  Decisions §D3).
- `cmd/scribe/status.go:207-241` (`lastSyncSummary`, `formatRunLine`,
  `extractJSONField`) is the existing "read the last `sync` run record from
  `output/runs/*.jsonl`" pattern. This plan extends it rather than adding a
  second way to read run records.
- `cmd/scribe/digest.go:58-79` (`digestLocalNotes`) is the existing
  precedent for *per-machine, non-committed* signals: "these exist (and
  differ) per machine — they must never enter the committed digest, or its
  bytes stop converging across members." The adoption ratio is computed from
  the **local** ccrider DB (never committed to git), so per this file's own
  documented rule it belongs here, not in `buildDigest`'s committed markdown
  (see Design Decision §D6 — this corrects the task brief's "leaning" note).
- `cmd/scribe/main.go:22-24,228-277` (`runStats`, `writeRunRecord`) is the
  mechanism every command uses to attach extra fields to its JSONL run
  record. `cmd/scribe/sync.go:172-177` shows `SyncCmd.Run()` assigning
  `runStats` right before returning — this plan adds fields to that same map.
- `cmd/scribe/watch.go:164-185` is the precedent for provider filtering
  (`COALESCE(s.provider, 'claude')`) and SQLite relative-date windows
  (`datetime('now', '-' || ? || ' minutes')`, parameterized).
- `cmd/scribe/manifest.go:64,188,265,352` — `ProjectEntry.IsApproved()`,
  `Manifest.isIgnored`, `sessionInKB(root, projectPath)`,
  `Manifest.entryForPath(path)`.

## 2. Design decisions

### D1 — Metric definition

**KB-first ratio** = among scoped sessions (see D3) that contain **at least
one "decision" tool call** (Edit / Write / MultiEdit), the fraction where a
**qmd tool call happened before the first decision tool call**.

Chosen over the alternative phrasing in the task brief ("within the first N
messages") because it directly answers the issue's actual question — "did
the agent query the KB *before making decisions*" — rather than a weaker
proxy that would count a session as compliant even if it queried the KB only
after already editing files, as long as the query happened to land within an
arbitrary early-message window. Sessions with **zero** decision tool calls
(pure research/Q&A sessions) are excluded from both numerator and
denominator — "did you check the KB before deciding" doesn't apply to a
session where nothing was decided — but their count is still surfaced
separately (`ExcludedNoDecision`) so the output is legible.

A message-count cap is still used, but only as a **performance/scan bound**,
not as part of the metric (D4).

### D2 — What "qmd tool call" and "decision tool call" mean at the SQL level

`content` (per `session_transcript.go:78` comment) is "the original
JSON-shaped tool payload" — i.e. it is Anthropic's own `tool_use` content
block, serialized, because ccrider mines Claude Code's session JSONL files
which are themselves the documented Anthropic Messages API format:
`{"type":"tool_use","id":"toolu_...","name":"<ToolName>","input":{...}}`.
No code in this repo currently JSON-decodes `content` — `session_transcript.go`
always treats it as opaque text (`renderTranscriptForPrompt` truncates
`ToolText` as a plain string, never parses it). This plan follows that same
convention: **plain substring matching on the raw `content` text**, not JSON
parsing. This is simpler, matches the existing codebase precedent, and
naturally catches both (a) an MCP tool's literal name field, and (b) a
`Bash` tool's `input.command` argument containing a `qmd query`/`qmd search`
CLI invocation — both appear as substrings in the same raw JSON-shaped text.

Marker sets (package-level vars in the new `adoption.go`, all matched via
`strings.Contains`, case-sensitive — these are canonical tool names / a CLI
invocation a human would type verbatim, not natural language):

```go
// qmdToolMarkers: any MCP server alias ending in "qmd__query" etc. (not
// hardcoded to "mcp__plugin_qmd_qmd__" — a user's local MCP registration
// name can differ), plus the Bash CLI fallback form the handshake block
// documents ("prefer mcp__plugin_qmd_qmd__query ... or `qmd query`/`qmd
// search` via Bash").
var qmdToolMarkers = []string{
	"qmd__query", "qmd__get", "qmd__multi_get", "qmd__status",
	"qmd query", "qmd search",
}

// decisionToolMarkers: the standard Anthropic tool_use JSON shape
// (`"name":"Edit"`, with and without the space Go's encoding/json and other
// JSON emitters may or may not insert after the colon). MultiEdit is
// Claude Code's older multi-hunk edit tool, kept for sessions mined from
// earlier tool-set versions.
var decisionToolMarkers = []string{
	`"name":"Edit"`, `"name": "Edit"`,
	`"name":"Write"`, `"name": "Write"`,
	`"name":"MultiEdit"`, `"name": "MultiEdit"`,
}
```

**Known unknown, flagged not guessed:** whether ccrider's `content` column
actually preserves this raw shape verbatim (vs. re-summarizing it) has not
been verified against the real DB — the planning pass for this issue
deliberately did not query `~/.config/ccrider/sessions.db` to avoid pulling
private session content into a written plan file. **Implementation step 0**
(§3) requires a one-time manual, read-only, terminal-only check before
writing `classifyToolContent`'s tests. If the assumption is wrong, only the
marker lists need adjusting — the surrounding SQL/scope/aggregation code is
unaffected.

`m.type` filter: query `type IN ('tool', 'tool_use')`, **deliberately
excluding `'tool_result'`**. `roleFromCcriderType`
(`session_transcript.go:102-115`) maps all three type strings to one "tool"
role, meaning downstream code has never needed to distinguish invocation
from result — but this metric does: a `tool_result` row's content is a
tool's *output*, and output text can accidentally contain a marker
substring (e.g. a grep result whose matched line literally contains
`"name":"Edit"`, or a qmd result snippet that mentions the word "query").
Excluding `tool_result` reduces (does not eliminate — see Risks) that
false-positive surface.

### D3 — Scope: which sessions count

Reuse the exact scoping rule `countScopedPendingSessions`
(`status.go:497-540`) already applies, not the broader `projectScopeAllowed`
(`sync_sessions.go:75-83`) used by the sync-time mining lane:

1. `COALESCE(s.provider, 'claude') = 'claude'` — **Codex sessions are
   explicitly out of scope for v1.** Codex's tool-call vocabulary
   (`apply_patch`, `shell` function calls, per `cmd/scribe/codex*.go`) is
   different from Claude Code's, and the marker sets above would silently
   misclassify Codex sessions rather than correctly detect their equivalent
   actions. Extending to Codex is a clearly separable follow-up, not part of
   this issue.
2. `project_path` must resolve to an **approved** manifest entry:
   `manifest.entryForPath(path) != nil && entry.IsApproved()`.
3. `!sessionInKB(root, projectPath)` — sessions spent curating the KB itself
   are about the KB, not a normal dev session; `sessionInKB` already exists
   for exactly this exclusion (used by `preFilterSessions`,
   `sync_sessions.go:157`).
4. `s.updated_at` falls inside the window (7d or 30d, see D5).

Rationale for reusing `countScopedPendingSessions`'s narrower rule rather
than `projectScopeAllowed`'s (which also layers `sources`/`ignore` checks):
this metric lives beside `renderBacklog` in the same file and should not
introduce a second, subtly different definition of "sessions this KB owns."

### D4 — Scan bound (performance, not metric semantics)

Cap the per-session message scan at **`m.sequence <= 300`**. 300 is not an
arbitrary new number — it is the codebase's own existing "normal session"
ceiling: `SyncCmd.SkipLarge` help text says "Skip large sessions (>300
messages)" (`sync.go:34`), and `SessionsFilterCmd.MessageLimit` defaults to
`300` (`sessions.go:430`). Reusing it means this plan doesn't invent a new
threshold with its own tuning story.

Sequence is assumed to be **per-session monotonic**, based on
`fetchSessionTranscript`'s `ORDER BY m.sequence ASC, m.id ASC` with no
partitioning (`session_transcript.go:63`) — if it were a global counter
across all sessions, that ordering wouldn't make sense as "canonical order"
for one session's transcript. `m.id ASC` is kept as the secondary sort (and,
in the Go aggregation, ties are broken by *iteration order*, not by
comparing raw sequence numbers — see §3 step 4) so degenerate fixtures where
every row has `sequence=0` still produce a correct chronological order.

**Known limitation, explicitly accepted:** a session whose first Edit/Write
happens after row 300 will be bucketed as "no decision found" (excluded)
even though it does eventually decide something. This mirrors the existing
`SkipLarge` precedent (>300-message sessions are already treated specially
elsewhere in this codebase) rather than inventing a new edge case.

### D5 — Time windows

Both **7 days** and **30 days**, computed every run — matches the task
brief exactly, no config knob needed for the window (see D8). Window
filtering uses `s.updated_at >= datetime('now', '-' || ? || ' days')`,
parameterized exactly like `watch.go:180`'s `'-' || ? || ' minutes'` pattern.

### D6 — Where it surfaces: `scribe status` **and** `scribe digest`, but the
digest placement differs from the task brief's phrasing

- **`scribe status`**: new line(s) in `renderStatus` (`status.go:38`),
  placed right after the existing "last sync" block (`status.go:117-123`).
  Read-only, sourced from the cached run-record data (D7) — consistent with
  `StatusCmd`'s own doc comment: "Deliberately read-only — it does NOT run
  fetchers, the LLM, or qmd... should be able to answer that in <1 second."
- **`scribe digest`**: as a **per-machine stdout note** via
  `digestLocalNotes` (`digest.go:58-79`), **not** inside the committed
  `wiki/_digest.md` markdown that `buildDigest` (`digest.go:94-155`)
  generates. `digest.go`'s own top-of-file comment is explicit about why:
  the committed digest must be "derived ONLY from committed shared state...
  so every member's regeneration converges on the same content given the
  same history," and per-machine state "prints to stdout only, never into
  the committed file — embedding it would make the digest a function of
  per-machine state and the regenerated bytes ping-pong between members'
  commits." The adoption ratio is computed from each machine's **local**
  ccrider DB (session history is never pushed to the KB repo), so by this
  file's own stated rule it cannot go into `buildDigest`'s output. This is a
  deliberate correction of the task brief's "leaning: `scribe status` +
  weekly digest" — the *feature* (digest) is right, the *mechanism*
  (committed markdown vs. stdout note) must follow the file's existing
  architecture.

### D7 — Computation cost: compute once per `sync`, cache in the run record,
never recompute in `status`/`digest`

Settled per the task brief's explicit prompt to settle this. Computed inside
`SyncCmd.Run()` (`sync.go`) on **every** non-dry-run invocation (not gated
behind `--sessions`), because:

- The metric is a pure read of ccrider DB state independent of whether this
  run mines/extracts anything — no reason to tie it to the rarer
  `--sessions` runs.
- Per `docs/issues-master-plan.md`'s own freshness table
  (`doctor.go:1023-1031`), plain `sync` runs more often (`MaxGap: 6h`) than
  `sync --sessions` (`MaxGap: 36h`) — computing on every plain sync keeps the
  number fresh.
- It is read-only against the ccrider DB (`?mode=ro`), bounded by the 7/30
  day window (D5) and the 300-message scan cap (D4), so it stays cheap
  relative to sync's overall runtime (sync already has no strict time
  budget, unlike triage/hook/doctor per `CLAUDE.md`'s "No LLM calls in the
  hot path" rule — this isn't an LLM call and isn't one of those three
  budgeted commands).

Result is written into `runStats` (`sync.go:172-177`, alongside `discovered`/
`extracted`/`sessions`/`absorbed`) and picked up by `writeRunRecord`
(`main.go:228-277`) into the daily `output/runs/YYYY-MM-DD.jsonl` file.
`scribe status` and `scribe digest` both read the **most recent `sync`
record** — never touching ccrider directly — via a new shared helper
extracted from `lastSyncSummary` (see §3 step 6). This is the same "cache
the computed number in the run record, read-only commands only read it back"
pattern `status.go` already uses for `extracted`/`absorbed`/`sessions`.

### D8 — No new `scribe.yaml` config surface

Window (7/30 days), scan cap (300), and marker lists are hardcoded package
constants/vars in the new `adoption.go`, not `scribe.yaml` keys. Rejected
alternative: a `cfg.Adoption` config block mirroring `TriageConfig`
(`config.go:399-402`). Reasoning: this is a first-cut heuristic metric with
no proven need for per-KB tuning yet; adding config surface for numbers
nobody has asked to change yet is exactly the kind of premature knob
`CLAUDE.md`'s "New dependencies need justification" spirit argues against
(same reasoning, applied to config surface instead of go.mod). If real usage
shows the thresholds need tuning, that's a small, easy follow-up — turning
already-hardcoded constants into config fields is strictly additive.

## 3. Implementation steps

### Step 0 (manual, before writing code) — verify the `content` shape assumption

In a terminal (not in this repo, not committed anywhere), read-only:

```sh
sqlite3 ~/.config/ccrider/sessions.db \
  "SELECT type, substr(content,1,300) FROM messages WHERE type IN ('tool','tool_use') AND content IS NOT NULL AND content != '' LIMIT 5"
```

Confirm: (a) `content` contains a `"name":"<ToolName>"`-shaped substring
somewhere, and (b) which literal string(s) `type` actually takes for tool
invocation rows (`'tool'` vs `'tool_use'` — `roleFromCcriderType` accepts
both, but production may only ever emit one). Do not paste sample output
into any commit, comment, or code — only note the key names/type values
observed and adjust §3 step 2's marker lists / type filter accordingly if
they differ from D2's assumption.

### Step 1 — extend the test fixture schema

File: `cmd/scribe/testdata/ccrider_schema.sql`. Add two columns to the
`messages` table (`:19-24`), additive and backward-compatible (existing
`INSERT INTO messages (session_id, type, text_content) VALUES (...)` calls
in `sessions_test.go:50-51` keep working unchanged — new columns default to
NULL, and every query touching them already uses `COALESCE`):

```sql
CREATE TABLE messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   INTEGER REFERENCES sessions(id),
    type         TEXT,
    text_content TEXT,
    content      TEXT,
    sequence     INTEGER DEFAULT 0
);
```

Update the file's top comment to note it now also covers
`session_transcript.go` and the new adoption-metric code, not just
`sessions.go`/`sync_sessions.go`/`hook.go`/`triage.go`.

Also add a `sessions.provider` note is already present; no change needed
there.

### Step 2 — new file `cmd/scribe/adoption.go`

```go
package main

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

const adoptionSequenceCap = 300

var qmdToolMarkers = []string{ /* as in D2 */ }
var decisionToolMarkers = []string{ /* as in D2 */ }

// adoptionWindowResult is one time-window's KB-first ratio.
type adoptionWindowResult struct {
	Days                int
	DecisionSessions    int // sessions with >=1 decision tool call (denominator)
	KBFirstSessions     int // of those, qmd call preceded first decision call (numerator)
	ExcludedNoDecision  int // scoped sessions with zero decision tool calls, informational only
}

func (r adoptionWindowResult) Ratio() float64 {
	if r.DecisionSessions == 0 {
		return 0
	}
	return float64(r.KBFirstSessions) / float64(r.DecisionSessions)
}

// classifyToolContent inspects one tool-type message row's raw content
// (falling back to text_content when content is empty) and reports whether
// it looks like a qmd query/search/get call and/or a code-decision call
// (Edit/Write/MultiEdit). See adoption plan D2 for why this is plain
// substring matching, not JSON parsing.
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
// for the adoption metric (D3): provider=claude, updated within sinceDays,
// approved manifest project, not a KB-curation session.
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
	// sanitized-then-joined session_id list in triage.go.
	query := fmt.Sprintf(`
		SELECT m.session_id, COALESCE(m.content, ''), COALESCE(m.text_content, '')
		FROM messages m
		WHERE m.session_id IN (%s)
		  AND m.type IN ('tool', 'tool_use')
		  AND COALESCE(m.sequence, 0) <= %d
		ORDER BY m.session_id ASC, m.sequence ASC, m.id ASC`,
		strings.Join(idList, ","), adoptionSequenceCap)

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
		return nil, nil //nolint:nilerr // best-effort, matches other sync phases
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
// runStats/writeRunRecord expects (main.go:22-24, 251-261).
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
```

Notes for the implementer:
- `//nolint:nilerr` — check this project's `.golangci.yml` actually has that
  linter enabled before assuming the directive is needed; if not, drop it
  (don't add an unnecessary nolint the linter would flag as unused).
- Every SQL call here follows the exact `sql.Open(..., "?mode=ro")` +
  `//nolint:noctx // CLI top-level, no context in scope` convention already
  used in `sync_sessions.go` and `sessions.go` — match it exactly rather than
  introducing `context.Context` plumbing that nothing else in this call path
  uses.

### Step 3 — wire into `SyncCmd.Run()`

File: `cmd/scribe/sync.go`. Add the call right before the existing
`runStats = map[string]any{...}` assignment (`sync.go:172-177`), guarded the
same way other post-processing steps are (`!s.DryRun`):

```go
adoptionFields := map[string]any{}
if !s.DryRun {
	if results, err := computeAdoptionMetrics(root, cfg); err == nil {
		adoptionFields = adoptionRunStatsFields(results)
	}
}

runStats = map[string]any{
	"discovered": counters.discovered,
	"extracted":  counters.extracted,
	"sessions":   counters.sessionsScanned,
	"absorbed":   counters.absorbed,
}
maps.Copy(runStats, adoptionFields) // "maps" import already needed — check main.go's import; add to sync.go if absent
```

(`main.go:6` already imports `"maps"` for the same `maps.Copy` pattern into
`runStats` in `writeRunRecord` — mirror that import in `sync.go` rather than
writing a manual copy loop.)

### Step 4 — shared "read latest adoption stats from run records" helper

File: `cmd/scribe/status.go`. Refactor `lastSyncSummary`
(`status.go:209-241`) to extract its "find the newest `output/runs/*.jsonl`
file, scan backward for the last line containing `"command":"sync"`" logic
into a standalone helper, then add a second reader on top of it:

```go
// newestSyncRunLine returns the most recent JSONL line in runsDir whose
// command is "sync", or "" if none exists. Extracted from lastSyncSummary
// so status and digest can both read cached sync-time computations (e.g.
// the adoption metric, D7) without re-querying ccrider.
func newestSyncRunLine(runsDir string) string {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return ""
	}
	var newest os.DirEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if newest == nil || e.Name() > newest.Name() {
			newest = e
		}
	}
	if newest == nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(runsDir, newest.Name()))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], `"command":"sync"`) {
			return lines[i]
		}
	}
	return ""
}

func lastSyncSummary(runsDir string) string {
	line := newestSyncRunLine(runsDir)
	if line == "" {
		return ""
	}
	return formatRunLine(line)
}

// adoptionSnapshot is one window's cached ratio, read back from the run
// record `sync` wrote (D7 — computed once at sync time, never recomputed
// here).
type adoptionSnapshot struct {
	Days        int
	Ratio       float64
	Numerator   int
	Denominator int
}

// loadLatestAdoptionStats reads the adoption_kb_first_* fields from the most
// recent sync run record. ok=false when no sync has run this feature yet
// (fresh KB, or a KB whose last sync predates this feature / had no
// ccrider DB available) — callers must render nothing in that case rather
// than a misleading zero.
func loadLatestAdoptionStats(root string) ([]adoptionSnapshot, bool) {
	runsDir := filepath.Join(root, "output", "runs")
	line := newestSyncRunLine(runsDir)
	if line == "" {
		return nil, false
	}
	var out []adoptionSnapshot
	for _, days := range []int{7, 30} {
		prefix := fmt.Sprintf("adoption_kb_first_%dd", days)
		den := extractJSONField(line, prefix+"_den")
		if den == "" {
			continue // this window wasn't computed on that run (e.g. old record)
		}
		ratio := extractJSONField(line, prefix+"_ratio")
		num := extractJSONField(line, prefix+"_num")
		snap := adoptionSnapshot{Days: days}
		fmt.Sscanf(ratio, "%f", &snap.Ratio)
		fmt.Sscanf(num, "%d", &snap.Numerator)
		fmt.Sscanf(den, "%d", &snap.Denominator)
		out = append(out, snap)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}
```

`extractJSONField` (`status.go:256-277`) already handles numeric (and
float) values via its "read until `,` or `}`" branch — no changes needed
there.

### Step 5 — render in `scribe status`

File: `cmd/scribe/status.go`, inside `renderStatus`
(`status.go:38-134`), right after the "last sync" block (`:117-123`):

```go
if snaps, ok := loadLatestAdoptionStats(root); ok {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  KB-first adoption (queried KB before first edit):")
	for _, s := range snaps {
		fmt.Fprintf(w, "    %2dd: %5.0f%%  (%d/%d decision sessions)\n",
			s.Days, s.Ratio*100, s.Numerator, s.Denominator)
	}
}
```

Prints nothing when `ok` is false — matches the existing `if last != ""`
guard style around it (`:120-123`).

### Step 6 — render in `scribe digest`

File: `cmd/scribe/digest.go`, inside `digestLocalNotes`
(`:58-79`), add one more note block after the staleness-ledger note:

```go
if snaps, ok := loadLatestAdoptionStats(root); ok {
	for _, s := range snaps {
		notes = append(notes, fmt.Sprintf(
			"KB-first adoption (%dd): %.0f%% (%d/%d decision sessions) — `scribe status` for detail",
			s.Days, s.Ratio*100, s.Numerator, s.Denominator))
	}
}
```

No change to `buildDigest`/`DigestCmd.Run` — the note flows through the
existing `for _, n := range digestLocalNotes(root) { fmt.Printf("note (this
machine): %s\n", n) }` loop already in `DigestCmd.Run` (`digest.go:52-54`).

## 4. Test plan

All fixtures under `cmd/scribe/testdata/`; never touch
`~/.config/ccrider/sessions.db`. `make test` must pass offline.

### 4.1 Fixture helper — `cmd/scribe/adoption_test.go`

Add a helper (this file, not `sessions_test.go`, since it's specific to the
new tool-row shape and keeps `sessions_test.go` unchanged aside from the
schema columns already being additive):

```go
// insertFixtureToolMessage inserts a tool-type messages row with content
// and sequence set (the columns newCcriderDB's fixture didn't previously
// exercise — see adoption plan §Step 1).
func insertFixtureToolMessage(t *testing.T, db *sql.DB, sessionRowID int64, msgType, content string, sequence int) {
	t.Helper()
	//nolint:noctx // test fixture
	_, err := db.Exec(`INSERT INTO messages (session_id, type, content, sequence) VALUES (?, ?, ?, ?)`,
		sessionRowID, msgType, content, sequence)
	if err != nil {
		t.Fatal(err)
	}
}
```

### 4.2 Unit tests — `classifyToolContent`

Table-driven, no DB needed:
- qmd MCP tool use: `{"type":"tool_use","name":"mcp__plugin_qmd_qmd__query","input":{...}}` → `isQMD=true, isDecision=false`.
- qmd MCP under a different local alias: `{"name":"mcp__qmd__query",...}` → `isQMD=true` (proves the marker isn't over-anchored to `plugin_qmd_qmd`).
- Bash qmd CLI form: `{"name":"Bash","input":{"command":"qmd query \"auth flow\""}}` → `isQMD=true`.
- Edit/Write/MultiEdit: three cases, each → `isDecision=true, isQMD=false`.
- Unrelated tool (`Grep`, `Read`) → both false.
- Empty content, non-empty textContent containing a marker → still detected (fallback path).
- Both empty → both false, no panic.
- A `tool_result` payload whose output text happens to contain `"name":"Edit"` (simulating a grep match) — this test exists to **document** the known false-positive risk from D2, not to assert it's prevented; assert the function still finds it (proving why the `type` filter, not this function, is what excludes `tool_result` rows).

### 4.3 `scopedClaudeSessionIDs` — integration test against fixture DB

Reuse `newCcriderDB` (`sessions_test.go:15-31`) + `insertFixtureSession`
(`:34-44`) + `writeStatusManifest` (`status_sessions_test.go:71-85`).
Cases, mirroring `TestCountScopedPendingSessions`
(`status_sessions_test.go:14-47`) and `TestCountScopedPendingSessions_ZeroApproved`
(`:51-69`):
- Approved-project session within window → included.
- Pending-project session → excluded.
- Unknown-project (no manifest entry) session → excluded.
- `provider='codex'` session in an approved project, otherwise identical →
  excluded (proves the v1 Claude-only scope).
- Session whose `project_path` equals the KB root itself → excluded (via
  `sessionInKB`).
- Session older than the window (`updated_at` before the cutoff) → excluded
  from the 7-day call but included in the 30-day call.

### 4.4 `computeAdoptionWindow` — end-to-end scenarios

Build sessions with `insertFixtureToolMessage` rows at controlled
`sequence` values:
- qmd call at sequence 2, Edit at sequence 5 → counts toward `KBFirstSessions` and `DecisionSessions`.
- Edit at sequence 2, qmd call at sequence 5 (edited first, queried after) →
  counts toward `DecisionSessions` but NOT `KBFirstSessions`.
- Only qmd calls, no Edit/Write/MultiEdit ever → counts toward
  `ExcludedNoDecision`, not `DecisionSessions`.
- Only Edit, no qmd call at all → counts toward `DecisionSessions`, not
  `KBFirstSessions`.
- Scoped session with zero tool-type rows at all → counts toward
  `ExcludedNoDecision`.
- Edit at sequence 301 (beyond `adoptionSequenceCap`) with a qmd call at
  sequence 1 → session is bucketed as `ExcludedNoDecision` (the accepted
  D4 limitation) — assert this explicitly so a future cap change is a
  visible, intentional test diff, not a silent behavior change.
- `Ratio()` on a zero-`DecisionSessions` result returns `0`, no
  divide-by-zero panic.
- Two sessions tied on the same `sequence` number for qmd vs. decision
  markers (both `sequence=3`, qmd row inserted before Edit row) → resolved
  by insertion/iteration order (`m.id ASC` tiebreak), not sequence equality
  — assert the earlier-inserted row wins per D4's tie-breaking note.

### 4.5 `computeAdoptionMetrics` — degrade gracefully

- `cfg.CcriderDB` empty or pointing at a nonexistent path → returns
  `(nil, nil)`, no panic, no error surfaced to the caller (matches other
  best-effort sync phases).

### 4.6 `loadLatestAdoptionStats` / `newestSyncRunLine` — status/digest side

- No `output/runs/` directory at all → `ok=false`.
- A `sync` run record present but written by a pre-feature scribe version
  (no `adoption_kb_first_*` keys) → `ok=false` (not a zero-value snapshot —
  proves "don't claim data that isn't there").
- A `sync` run record with both windows present → both parsed correctly,
  `Ratio` as a float (e.g. `0.625`), rendered as `62%` (verify the `%5.0f%%`
  format math, including the `*100` step).
- `renderStatus` (via the existing `captureStatusOutput`-style test harness
  already used in `status_test.go`) — snapshot-style assert that the new
  block appears/doesn't appear based on `loadLatestAdoptionStats`'s `ok`.
- `digestLocalNotes` — assert the new note string appears in the returned
  slice, and (separately, reusing existing `buildDigest` tests in
  `digest_test.go`) assert the committed digest markdown **does not**
  contain the adoption numbers — this is the concrete regression test for
  D6's "never in the committed file" rule.

### 4.7 Session-mining infra sanity (side effect of Step 1)

Not required by this issue, but since Step 1 extends the fixture with
`content`/`sequence` columns that `fetchSessionTranscript`
(`session_transcript.go`) already queries but has never been tested against,
add one small smoke test — `TestFetchSessionTranscript_ToolRows` — inserting
a mixed user/assistant/tool-type transcript and asserting
`fetchSessionTranscript` returns it in `sequence` order with `ToolText`
populated for tool rows. This closes a real, pre-existing test gap the
planning pass for this issue surfaced, at near-zero incremental cost once
the fixture already has the columns.

## 5. Risks & edge cases

- **`content` shape assumption unverified** (D2). Mitigated by the Step 0
  manual check and by the fact that only two marker-list variables need
  updating if wrong — no SQL/scope/aggregation code depends on the exact
  shape.
- **`tool_result` false positives are not fully eliminated**, only reduced,
  by excluding `type='tool_result'` rows — if ccrider ever tags an
  invocation-and-result pair with the same generic `type='tool'` value (no
  distinct `tool_use`/`tool_result` strings in production, per the Step 0
  unknown), this filter does nothing and a tool's *output* text could still
  trigger a marker match. This is a precision risk that inflates the ratio
  (false "KB-first"), not a crash risk. Documented as an accepted v1
  limitation; a future fix (if Step 0 reveals this happens) is to also
  exclude rows whose content looks like a result (e.g. starts with `{"type":
  "tool_result"` if that shape is actually distinguishable) — deferred until
  the real shape is known.
- **ccrider schema drift across versions.** This plan assumes today's
  columns (`type`, `text_content`, `content`, `sequence`) and the
  `roleFromCcriderType` type-string vocabulary. If a future ccrider version
  renames/restructures these, `computeAdoptionWindow`'s query will either
  error (caught by the existing `if err != nil { logMsg(...); continue }`
  guard in `computeAdoptionMetrics`, degrading to "no adoption data this
  run" rather than crashing sync) or silently return zero rows (degrades to
  ratio=0/0 → `Ratio()` returns 0, `DecisionSessions=0` — status/digest
  would print `0/0` sessions, which is honest but not obviously "this broke"
  — worth a follow-up doctor check if this becomes a real problem, out of
  scope here).
- **Sessions with no tool calls recorded at all** (e.g. a ccrider import
  that only captured dialog turns for some session, or a session pre-dating
  ccrider's tool-row support) — handled: such a session yields zero rows
  from the tool-scan query, falls into `ExcludedNoDecision` via the
  `len(ids) - len(seen)` accounting in `computeAdoptionWindow`, not a crash
  or a miscounted denominator.
- **Performance on a 2500+ session DB.** Pass 1
  (`scopedClaudeSessionIDs`) scans the `sessions` table filtered by
  `updated_at`/`provider` — cheap even unindexed at a few thousand rows.
  Pass 2's `session_id IN (...)` batch query against `messages` is the risk:
  if ccrider's production `messages` table has no index on `session_id`
  (unknown — the fixture schema declares no indexes at all), this becomes a
  full-table scan filtered by an IN-list, which could be slow on a
  message-heavy DB. Because this runs once per `sync` (D7), not per
  `status`/`digest` call, an occasional multi-second cost is acceptable and
  won't violate the sub-second budgets that actually matter
  (`triage`/`hook`/`doctor`, per `CLAUDE.md`). If `sync`'s own freshness gate
  (`doctor.go:1024`, 6h `MaxGap`) starts flagging slow runs because of this,
  the mitigation is narrowing the day window or adding a coarser pre-filter
  — not a redesign.
- **`--dry-run` sync never populates the run record's adoption fields**
  (guarded off in Step 3) — `status`/`digest` then keep showing the last
  *real* sync's numbers, which is correct (dry-run shouldn't produce new
  cached state) but worth a one-line note in the PR/commit description so
  it isn't mistaken for a bug during review.

## 6. Interactions with other open issues

- **#22** (priority lanes / two-lane session budget in `sync --sessions`,
  Phase 3, precedes this plan in the master ordering) also touches session
  scanning in `sync_sessions.go`. This plan's new code lives in a separate
  file (`adoption.go`) and a separate code path (it runs unconditionally in
  `SyncCmd.Run()`, not inside the `--sessions` mining lane #22 will likely
  restructure) — no direct function-level overlap expected. The one soft
  dependency: if #22 changes `preFilterSessions`/`sessionInKB`/
  `projectScopeAllowed` signatures, `scopedClaudeSessionIDs` (which calls
  `sessionInKB` and `manifest.entryForPath`/`IsApproved` directly, not
  through those higher-level functions) should be unaffected, but the
  implementer should `grep -n "sessionInKB\|entryForPath"` after #22 lands
  to confirm no signature changed.
- **#8** (path-keyed manifest, same Phase 4, may land before or after this
  issue within the phase). If #8 changes how `manifest.entryForPath` or
  `ProjectEntry` identity works, `scopedClaudeSessionIDs`'s two calls to
  those APIs (Step 2) are the only touch points to re-check — everything
  else in this plan is independent of manifest internals.
- **#6 umbrella**: this issue is one of its listed children; closing #23
  (via commit-message `Closes #23` per the master plan's merge strategy)
  moves the umbrella one step closer to closeable once #19/#21-24 are all
  done.

## 7. Size estimate

**M.** Roughly 300-350 LOC for `adoption.go` (including comments — the
functions themselves are ~180 LOC), ~250-300 LOC of new tests across
`adoption_test.go` (new file) plus additions to `status_test.go`/
`digest_test.go`, a ~10-line schema fixture edit, a ~15-line `sync.go`
edit, and a ~70-line refactor+addition in `status.go` (extracting
`newestSyncRunLine`, adding `loadLatestAdoptionStats` and the render call)
plus a ~10-line addition in `digest.go`. Net new/changed: roughly
500-600 LOC. No new go.mod dependencies, no new `scribe.yaml` config keys.

---

## Addendum (2026-07-02, main session): marker-format probe results

Counts-only, read-only probe of the real ccrider DB (no content extracted):

- `messages.content` DOES preserve tool_use blocks: 59,375 messages across
  1,940/2,876 sessions contain the substring `tool_use`.
- BUT the serialization is NOT compact Anthropic JSON: `"type":"tool_use"`
  matches only 79 messages and `"type": "tool_use"` matches 0.
- `mcp__plugin_qmd` appears in only 17 messages and `qmd query` in 86 — an
  MCP-tool-name-only matcher would drastically undercount. The matcher must be
  tolerant (multiple marker forms), and the qmd-call base rate is low enough
  that the metric will be meaningful (not saturated).

Resolution path for the implementer: do NOT inspect private DB content.
ccrider's source is checked out at `/Users/oliverkriska/Projects/ccrider` —
read its message-ingest/storage code to pin the exact `content` serialization
(and what `text_content` holds), derive the marker list from that, then
validate with counts-only queries like the above.
