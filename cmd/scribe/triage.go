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

	_ "github.com/mattn/go-sqlite3"
)

type TriageCmd struct {
	Top          int    `help:"Number of top sessions to show." default:"20"`
	All          bool   `help:"Show all scored sessions."`
	Project      string `help:"Filter by project name." short:"p"`
	Sort         string `help:"Sort order: score (default) or date (newest first)." default:"score" enum:"score,date"`
	MessageLimit int    `help:"Only include sessions with at most N messages (0=no limit)." name:"message-limit" default:"0"`
	MinMessages  int    `help:"Only include sessions with at least N messages." name:"min-messages" default:"0"`
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

	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
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
	return nil
}

func (t *TriageCmd) runScoring(db *sql.DB, _ string, excludeIDs []string) error {
	excludeClause := buildExcludeClause(excludeIDs)
	projectClause := buildProjectClause(t.Project)
	homeProjects := filepath.Join(os.Getenv("HOME"), "Projects") + "/"

	root, _ := kbDir()
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
		%s %s %s
	ORDER BY %s
	LIMIT %d`,
		ctes, homeProjects, scoreExpr, selectCols, anyHitExpr,
		excludeClause, projectClause, t.messageLimitClause(), t.orderClause(), t.Top)

	rows, err := db.Query(query) //nolint:noctx,gosec // G701: scribe-owned SQL; CLI top-level
	if err != nil {
		return fmt.Errorf("scoring query: %w", err)
	}
	defer rows.Close()

	type result struct {
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
	}

	var results []result
	for rows.Next() {
		var r result
		var date, summary sql.NullString
		var hours sql.NullFloat64
		err := rows.Scan(&r.SessionID, &r.Project, &r.Msgs, &r.Score,
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

	if t.Interactive {
		// Build fzf input: "session_id\tscore\tproject\tmsgs\tdate\tsummary"
		// fzf displays columns 2.., preview runs `ccrider show {1}` against col 1.
		if _, err := exec.LookPath("fzf"); err != nil {
			return fmt.Errorf("fzf not installed — install via `brew install fzf`")
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
