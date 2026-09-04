package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	_ "github.com/mattn/go-sqlite3"
)

type TriageCmd struct {
	Top          int    `help:"Number of top sessions to show." default:"20"`
	All          bool   `help:"Show all scored sessions."`
	Project      string `help:"Filter by project name." short:"p"`
	Sort         string `help:"Sort order: score (default) or date (newest first)." default:"score" enum:"score,date"`
	MessageLimit int    `help:"Only include sessions with at most N messages (0=no limit)." name:"message-limit" default:"0"`
	MinMessages  int    `help:"Only include sessions with at least N messages." name:"min-messages" default:"0"`
	InScope      bool   `name:"in-scope" help:"Only sessions this KB could actually mine (drops ignored, too-shallow, out-of-sources and unapproved projects). Used by sync so undrainable sessions cannot occupy admission slots."`
	IDs          bool   `name:"ids" help:"Output session IDs only (for piping)."`
	Stats        bool   `help:"Show score distribution stats."`
	JSON         bool   `help:"Output JSON."`
	Interactive  bool   `help:"Pipe results into fzf with ccrider show preview, output the selected session ID."`
}

func (t *TriageCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}

	cfg := loadConfig(root)
	dbPath := cfg.CcriderDB
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return fmt.Errorf("ccrider database not found at %s", dbPath)
	}

	db, err := openSQLiteRO(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	// Build exclusion list from processed sessions
	sessionsLog := filepath.Join(root, "wiki", "_sessions_log.json")
	excludeIDs := loadProcessedSessionIDs(sessionsLog)

	if t.All {
		t.Top = 99999
	}

	if t.Stats {
		return t.runStats(db, excludeIDs)
	}

	return t.runScoring(db, root, excludeIDs)
}

func (t *TriageCmd) runStats(db *sql.DB, excludeIDs []string) error {
	excludeClause := buildExcludeClause(excludeIDs)
	projectClause := buildProjectClause(t.Project)

	// The stats-mode scoring uses a single MATCH across all non-code categories;
	// build it from the live triage config so customized keywords apply here too.
	root, _ := kbDir()
	cfg := loadConfig(root)
	keywords, _ := cfg.Triage.Resolve()
	statsMatch := buildStatsMatchClause(keywords)

	// FTS5 MATCH syntax isn't parameterizable; inputs are scribe-owned config
	// (triage keywords) and CLI flags (excludeIDs, project) — no untrusted input.
	query := fmt.Sprintf(`
	WITH scored AS (
		SELECT s.session_id, s.message_count,
			COUNT(*) as hits
		FROM messages_fts f
		JOIN messages m ON m.id = f.rowid
		JOIN sessions s ON s.id = m.session_id
		WHERE messages_fts MATCH '%s'
			AND s.message_count > 5
			AND s.summary NOT LIKE 'You are working in%%'
			%s %s %s
		GROUP BY s.session_id
	)
	SELECT
		CASE
			WHEN hits >= 50 THEN '1. 50+  (goldmine)'
			WHEN hits >= 20 THEN '2. 20-49 (rich)'
			WHEN hits >= 10 THEN '3. 10-19 (moderate)'
			WHEN hits >= 5  THEN '4. 5-9   (some)'
			ELSE                  '5. 1-4   (low)'
		END as bucket,
		COUNT(*) as sessions,
		SUM(message_count) as total_msgs,
		ROUND(AVG(hits), 1) as avg_hits
	FROM scored
	GROUP BY 1
	ORDER BY 1`, statsMatch, excludeClause, projectClause, t.messageLimitClause())

	rows, err := db.Query(query) //nolint:noctx // triage is a CLI top-level, no ctx in scope
	if err != nil {
		return fmt.Errorf("stats query: %w", err)
	}
	defer rows.Close()

	fmt.Printf("%-25s %8s %10s %8s\n", "Bucket", "Sessions", "Messages", "Avg Hits")
	fmt.Println(strings.Repeat("-", 55))
	for rows.Next() {
		var bucket string
		var sessions, totalMsgs int
		var avgHits float64
		if err := rows.Scan(&bucket, &sessions, &totalMsgs, &avgHits); err != nil {
			return fmt.Errorf("scan stats row: %w", err)
		}
		fmt.Printf("%-25s %8d %10d %8.1f\n", bucket, sessions, totalMsgs, avgHits)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate stats rows: %w", err)
	}
	return nil
}

// triageResult is one row of `scribe triage --json` output. Shared with
// sync_sessions.go's triageSessionsScored, which decodes the same JSON
// shape when pulling live-triage candidates into the priority-lane
// admission path (issue #22) — one struct, one set of JSON tags, no
// drift between the two call sites.
type triageResult struct {
	SessionID string  `json:"session_id"`
	Project   string  `json:"project"`
	Msgs      int     `json:"msgs"`
	Score     int     `json:"total_score"`
	Dec       int     `json:"dec"`
	Arch      int     `json:"arch"`
	Res       int     `json:"res"`
	Learn     int     `json:"learn"`
	Eval      int     `json:"eval"`
	Deep      int     `json:"deep"`
	Date      string  `json:"date"`
	Hours     float64 `json:"hours"`
	Summary   string  `json:"summary"`

	// rawPath is s.project_path verbatim, unstripped and unexported so it
	// never reaches --json output. --in-scope needs the real path: the
	// `project` column above has the ~/Projects prefix removed for display.
	rawPath string
}

// inScopeOverfetch is how many rows --in-scope reads per row it intends to
// return. The predicate runs in Go (path depth, ignore list, sources
// globs, manifest approval — none of it expressible in this query), so the
// blockers have to be read before they can be dropped.
//
// 20x rather than a smaller constant because the blockers are not randomly
// distributed: a mount root that collapses many repos into one shallow
// path tends to score HIGH (long, dense sessions) and so clusters at the
// exact top of the ordering the miner reads. On the KB this was written
// for, 10 of the top 11 were undrainable. The cap keeps the worst case
// bounded on a large ccrider DB.
const (
	inScopeOverfetch    = 20
	inScopeOverfetchCap = 2000
)

// scanLimit is the SQL LIMIT: t.Top normally, over-fetched under
// --in-scope so keepInScope can drop blockers and still fill t.Top.
func (t *TriageCmd) scanLimit() int {
	if !t.InScope {
		return t.Top
	}
	n := t.Top * inScopeOverfetch
	if n > inScopeOverfetchCap {
		n = inScopeOverfetchCap
	}
	// Floor applied AFTER the cap on purpose: the cap bounds the
	// over-fetch, never the request. `--all` sets Top to 99999, and
	// returning 2000 rows for it would silently truncate.
	if n < t.Top {
		n = t.Top
	}
	return n
}

// keepInScope drops sessions the miner could never process and trims back
// to t.Top. Without --in-scope it is a no-op, so the human-facing `scribe
// triage` still shows everything (seeing the blockers ranked first is how
// you diagnose a stalled queue).
//
// The predicate is sessionDropReason — the same one preFilterSessions
// applies after admission. Sharing it is the point: when triage ranked a
// session the pre-filter then dropped unmarked, that session kept its slot
// forever and admissible work behind it never ran.
func (t *TriageCmd) keepInScope(root string, cfg *ScribeConfig, results []triageResult) []triageResult {
	if !t.InScope {
		return results
	}
	// Fails open on a manifest read error, matching preFilterSessions:
	// mining a session that should have been skipped is recoverable,
	// mining nothing is not.
	manifest, _ := loadManifest(root)
	kept := make([]triageResult, 0, min(len(results), t.Top))
	for _, r := range results {
		if sessionDropReason(cfg, manifest, root, r.rawPath) != "" {
			continue
		}
		kept = append(kept, r)
		if len(kept) == t.Top {
			break
		}
	}
	return kept
}

func (t *TriageCmd) runScoring(db *sql.DB, _ string, excludeIDs []string) error {
	excludeClause := buildExcludeClause(excludeIDs)
	projectClause := buildProjectClause(t.Project)
	homeProjects := filepath.Join(os.Getenv("HOME"), "Projects") + "/"

	root, _ := kbDir()
	kbExcludeClause := buildKBExcludeClause(root)
	cfg := loadConfig(root)
	keywords, weights := cfg.Triage.Resolve()

	// Emit one CTE per triage category so scribe.yaml can tune keywords and
	// weights per-KB. `code_pattern` is special-cased: it queries the
	// messages_fts_code virtual table, not messages_fts.
	ctes, scoreExpr, selectCols, anyHitExpr := buildTriageSQL(keywords, weights)

	// FTS5 MATCH syntax isn't parameterizable; all inputs are scribe-owned.
	query := fmt.Sprintf(`
	WITH
%s
	SELECT
		s.session_id,
		REPLACE(s.project_path, '%s', '') as project,
		s.project_path as raw_project_path,
		s.message_count as msgs,
		%s as total_score,
		%s,
		date(s.updated_at) as date,
		ROUND(MAX((JULIANDAY(SUBSTR(s.updated_at,1,23)) - JULIANDAY(SUBSTR(s.created_at,1,23))) * 24, 0), 1) as hours,
		SUBSTR(COALESCE(s.llm_summary, s.summary, ''), 1, 80) as summary
	FROM sessions s
	LEFT JOIN decision_hits d ON d.sid = s.id
	LEFT JOIN architecture_hits a ON a.sid = s.id
	LEFT JOIN research_hits r ON r.sid = s.id
	LEFT JOIN learning_hits l ON l.sid = s.id
	LEFT JOIN evaluation_hits e ON e.sid = s.id
	LEFT JOIN deep_work_hits dp ON dp.sid = s.id
	LEFT JOIN code_pattern_hits cd ON cd.sid = s.id
	WHERE s.message_count > 5
		AND s.summary NOT LIKE 'You are working in%%'
		AND (%s) > 0
		%s %s %s %s
	ORDER BY %s
	LIMIT %d`,
		ctes, homeProjects, scoreExpr, selectCols, anyHitExpr,
		excludeClause, kbExcludeClause, projectClause, t.messageLimitClause(), t.orderClause(), t.scanLimit())

	rows, err := db.Query(query) //nolint:noctx // CLI top-level, no ctx in scope
	if err != nil {
		return fmt.Errorf("scoring query: %w", err)
	}
	defer rows.Close()

	var results []triageResult
	for rows.Next() {
		var r triageResult
		var date, summary sql.NullString
		var hours sql.NullFloat64
		err := rows.Scan(&r.SessionID, &r.Project, &r.rawPath, &r.Msgs, &r.Score,
			&r.Dec, &r.Arch, &r.Res, &r.Learn, &r.Eval, &r.Deep,
			&date, &hours, &summary)
		if err != nil {
			continue
		}
		r.Date = date.String
		r.Hours = hours.Float64
		r.Summary = summary.String
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate scoring rows: %w", err)
	}
	results = t.keepInScope(root, cfg, results)

	if t.Interactive {
		// Build fzf input: "session_id\tscore\tproject\tmsgs\tdate\tsummary"
		// fzf displays columns 2.., preview runs `ccrider show {1}` against col 1.
		if _, err := exec.LookPath("fzf"); err != nil {
			return errors.New("fzf not installed — install via `brew install fzf`")
		}
		var buf strings.Builder
		for _, r := range results {
			summary := strings.ReplaceAll(r.Summary, "\t", " ")
			fmt.Fprintf(&buf, "%s\t%d\t%s\t%d\t%s\t%s\n",
				r.SessionID, r.Score, r.Project, r.Msgs, r.Date, summary)
		}
		cmd := exec.Command("fzf", //nolint:noctx // interactive fzf
			"--delimiter=\t",
			"--with-nth=2..",
			"--preview=ccrider show {1} 2>/dev/null || echo 'install ccrider to see session preview'",
			"--preview-window=right:60%",
			"--header=SCORE  PROJECT              MSGS  DATE        SUMMARY",
		)
		cmd.Stdin = strings.NewReader(buf.String())
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			// Exit 130 = user canceled with Ctrl-C, treat as clean exit.
			exitErr := &exec.ExitError{}
			if errors.As(err, &exitErr) {
				return nil
			}
			return fmt.Errorf("fzf: %w", err)
		}
		// Print just the selected session ID.
		if line := strings.TrimSpace(string(out)); line != "" {
			if idx := strings.Index(line, "\t"); idx > 0 {
				fmt.Println(line[:idx])
			} else {
				fmt.Println(line)
			}
		}
		return nil
	}

	if t.IDs {
		for _, r := range results {
			fmt.Println(r.SessionID)
		}
		return nil
	}

	if t.JSON {
		data, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	// Pretty table
	fmt.Printf("Session Knowledge Triage (top %d unprocessed)\n", t.Top)
	fmt.Println("================================================")
	fmt.Println()
	fmt.Printf("%-38s %-20s %4s %5s %5s %3s %3s %3s %3s %3s %3s %-10s %s\n",
		"SESSION_ID", "PROJECT", "MSGS", "SCORE", "HOURS", "DEC", "ARC", "RES", "LRN", "EVL", "DEP", "DATE", "SUMMARY")
	fmt.Println(strings.Repeat("-", 150))
	for _, r := range results {
		project := r.Project
		if len(project) > 20 {
			project = project[:17] + "..."
		}
		summary := r.Summary
		if len(summary) > 50 {
			summary = summary[:47] + "..."
		}
		fmt.Printf("%-38s %-20s %4d %5d %5.1f %3d %3d %3d %3d %3d %3d %-10s %s\n",
			r.SessionID, project, r.Msgs, r.Score, r.Hours,
			r.Dec, r.Arch, r.Res, r.Learn, r.Eval, r.Deep,
			r.Date, summary)
	}
	return nil
}

// messageLimitClause returns SQL WHERE clause for message count filtering.
func (t *TriageCmd) messageLimitClause() string {
	var parts []string
	if t.MessageLimit > 0 {
		parts = append(parts, fmt.Sprintf("AND s.message_count <= %d", t.MessageLimit))
	}
	if t.MinMessages > 0 {
		parts = append(parts, fmt.Sprintf("AND s.message_count >= %d", t.MinMessages))
	}
	return strings.Join(parts, " ")
}

// orderClause returns the SQL ORDER BY expression based on the sort flag.
func (t *TriageCmd) orderClause() string {
	if t.Sort == "date" {
		return "s.updated_at DESC"
	}
	return "total_score DESC"
}

func loadProcessedSessionIDs(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var log struct {
		Processed map[string]any `json:"processed"`
	}
	if err := json.Unmarshal(data, &log); err != nil {
		return nil
	}
	ids := make([]string, 0, len(log.Processed))
	for id := range log.Processed {
		ids = append(ids, id)
	}
	return ids
}

func buildExcludeClause(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	quoted := make([]string, len(ids))
	for i, id := range ids {
		// Sanitize to prevent injection
		clean := strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
				return r
			}
			return -1
		}, id)
		quoted[i] = "'" + clean + "'"
	}
	return "AND s.session_id NOT IN (" + strings.Join(quoted, ",") + ")"
}

// buildKBExcludeClause excludes sessions whose working directory is the KB
// root or nested inside it. Sessions spent curating the wiki are full of the
// KB's own content; mining them re-emits that content as "new" articles —
// the session-side twin of the KB-extracts-itself loop. Empty root yields no
// clause. project_path comparisons use substr (not LIKE) so `_`/`%` in a real
// path can't act as wildcards; the literal is single-quote escaped.
func buildKBExcludeClause(root string) string {
	if root == "" {
		return ""
	}
	r := strings.ReplaceAll(filepath.Clean(root), "'", "''")
	// utf8 rune count: SQLite substr() counts characters, so the prefix
	// length must match the character length of "<root>/".
	n := utf8.RuneCountInString(filepath.Clean(root)) + 1
	return fmt.Sprintf("AND s.project_path != '%s' AND substr(s.project_path, 1, %d) != '%s/'", r, n, r)
}

// triageCategoryAlias maps a category name to the short SQL alias used in
// the scoring CTEs. Column names in the result set come from these aliases,
// and the scanner below depends on the exact order.
var triageCategoryAliases = map[string]string{
	"decision":     "d",
	"architecture": "a",
	"research":     "r",
	"learning":     "l",
	"evaluation":   "e",
	"deep_work":    "dp",
	"code_pattern": "cd",
}

// buildTriageSQL assembles the CTE block, weighted score expression, SELECT
// column list, and the "any hit" WHERE predicate from the resolved keywords
// and weights. Category order follows triageCategoryOrder so the result-set
// column order stays stable across config changes.
//
// `code_pattern` queries messages_fts_code (a separate FTS5 table scoped to
// code chunks); all other categories query messages_fts.
func buildTriageSQL(keywords map[string]string, weights map[string]int) (ctes, scoreExpr, selectCols, anyHitExpr string) {
	cteParts := make([]string, 0, len(triageCategoryOrder))
	scoreParts := make([]string, 0, len(triageCategoryOrder))
	selectParts := make([]string, 0, len(triageCategoryOrder))
	anyParts := make([]string, 0, len(triageCategoryOrder))
	displayCols := map[string]string{
		"decision":     "dec",
		"architecture": "arch",
		"research":     "res",
		"learning":     "learn",
		"evaluation":   "eval",
		"deep_work":    "deep",
		"code_pattern": "code",
	}
	for _, cat := range triageCategoryOrder {
		kw := ftsEscape(keywords[cat])
		alias := triageCategoryAliases[cat]
		weight := weights[cat]
		table := "messages_fts"
		if cat == "code_pattern" {
			table = "messages_fts_code"
		}
		cte := fmt.Sprintf("\t%s_hits AS (\n\t\tSELECT m.session_id as sid, COUNT(*) as hits\n\t\tFROM %s f JOIN messages m ON m.id = f.rowid\n\t\tWHERE %s MATCH '%s'\n\t\tGROUP BY m.session_id\n\t)", cat, table, table, kw)
		cteParts = append(cteParts, cte)
		scoreParts = append(scoreParts, fmt.Sprintf("COALESCE(%s.hits,0)*%d", alias, weight))
		if cat != "code_pattern" {
			selectParts = append(selectParts, fmt.Sprintf("COALESCE(%s.hits,0) as %s", alias, displayCols[cat]))
		}
		anyParts = append(anyParts, fmt.Sprintf("COALESCE(%s.hits,0)", alias))
	}
	ctes = strings.Join(cteParts, ",\n")
	scoreExpr = strings.Join(scoreParts, " + ")
	selectCols = strings.Join(selectParts, ",\n\t\t")
	anyHitExpr = strings.Join(anyParts, " + ")
	return
}

// buildStatsMatchClause flattens the non-code categories into one FTS5 MATCH
// expression for the stats view. Used only by runStats. The simple `OR` join
// preserves the same high-level "is this session rich at all" signal the
// original hand-authored string expressed.
func buildStatsMatchClause(keywords map[string]string) string {
	var parts []string
	for _, cat := range triageCategoryOrder {
		if cat == "code_pattern" {
			continue
		}
		if kw := strings.TrimSpace(keywords[cat]); kw != "" {
			parts = append(parts, "("+kw+")")
		}
	}
	return ftsEscape(strings.Join(parts, " OR "))
}

// scoreText is the pure, in-process triage scorer used by the Codex
// session-mining path (C3). ccrider sessions are scored via FTS5 BM25
// inside SQLite; Codex rollouts are not in any DB, so this scores the
// rendered transcript text directly against the same resolved
// keyword/weight config (TriageConfig.Resolve) the FTS5 path uses —
// one scoring definition, two call sites.
//
// Model: for each category, if ANY of its keywords appears in the
// text (case-insensitive substring), add that category's weight once.
// This deliberately mirrors triage's "(kw) OR (kw) ..." presence test
// plus per-category weight — it is a threshold GATE (MinScore), not a
// BM25 ranker, so reproducing term-frequency math here would add
// complexity without changing the keep/skip decision.
func scoreText(keywords map[string]string, weights map[string]int, text string) int {
	if strings.TrimSpace(text) == "" {
		return 0
	}
	lower := strings.ToLower(text)
	score := 0
	for _, cat := range triageCategoryOrder {
		for _, term := range triageKeywordTerms(keywords[cat]) {
			if strings.Contains(lower, term) {
				score += weights[cat]
				break // category counts once
			}
		}
	}
	return score
}

// triageKeywordTerms splits an FTS5-shaped keyword string
// (`a OR b OR "two words"`) into plain lowercased match terms with
// surrounding quotes stripped. Empty/blank terms are dropped.
func triageKeywordTerms(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, raw := range strings.Split(s, " OR ") {
		t := strings.ToLower(strings.TrimSpace(raw))
		t = strings.Trim(t, `"`)
		t = strings.TrimSpace(t)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ftsEscape doubles single-quotes so a user-authored keyword list that happens
// to contain `'` doesn't break out of the surrounding string literal when
// interpolated into the query. FTS5 itself has no string-escape issues with
// most punctuation, but the Go-side Sprintf does.
func ftsEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func buildProjectClause(project string) string {
	if project == "" {
		return ""
	}
	// Sanitize to prevent injection
	clean := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '/' || r == '.' {
			return r
		}
		return -1
	}, project)
	return "AND s.project_path LIKE '%" + clean + "%'"
}
