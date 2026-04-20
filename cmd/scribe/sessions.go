package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

// SessionsCmd groups session-log debug and repair operations.
// These replace the ad-hoc Python one-liners we used to reach for
// when the sync pipeline behaved unexpectedly.
type SessionsCmd struct {
	Stats   SessionsStatsCmd   `cmd:"" help:"Show _sessions_log.json counts and breakdown."`
	Inspect SessionsInspectCmd `cmd:"" help:"Dump filter verdict + stats for one session ID."`
	Unskip  SessionsUnskipCmd  `cmd:"" help:"Re-evaluate skipped entries and remove ones that now pass the pre-filter."`
	Filter  SessionsFilterCmd  `cmd:"" help:"Preview pre-filter behavior on top triaged sessions."`
}

// --- shared helpers ---

// openSessionsLog loads wiki/_sessions_log.json as a typed view.
type sessionsLogView struct {
	path      string
	raw       map[string]any
	processed map[string]any // session_id -> entry
}

func loadSessionsLogView(root string) (*sessionsLogView, error) {
	path := filepath.Join(root, "wiki", "_sessions_log.json")
	raw := loadJSONMap(path)
	proc, _ := raw["processed"].(map[string]any)
	if proc == nil {
		proc = make(map[string]any)
	}
	return &sessionsLogView{path: path, raw: raw, processed: proc}, nil
}

func (v *sessionsLogView) save() error {
	v.raw["processed"] = v.processed
	return saveJSONMap(v.path, v.raw)
}

// entryIsSkipped returns true if the processed entry has skipped:true.
func entryIsSkipped(e any) bool {
	m, ok := e.(map[string]any)
	if !ok {
		return false
	}
	s, _ := m["skipped"].(bool)
	return s
}

func entryProject(e any) string {
	m, ok := e.(map[string]any)
	if !ok {
		return ""
	}
	p, _ := m["project"].(string)
	return p
}

func entryExtracted(e any) string {
	m, ok := e.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["extracted"].(string)
	return s
}

// --- sessions stats ---

type SessionsStatsCmd struct {
	JSON bool `help:"Output JSON instead of a table."`
}

func (c *SessionsStatsCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	view, err := loadSessionsLogView(root)
	if err != nil {
		return err
	}

	total := len(view.processed)
	var extracted, skipped int
	byProject := make(map[string]int)
	byDate := make(map[string]int)
	var firstExtract, lastExtract string

	for _, e := range view.processed {
		if entryIsSkipped(e) {
			skipped++
			continue
		}
		extracted++
		if p := entryProject(e); p != "" {
			byProject[p]++
		}
		if ts := entryExtracted(e); ts != "" {
			if len(ts) >= 10 {
				byDate[ts[:10]]++
			}
			if firstExtract == "" || ts < firstExtract {
				firstExtract = ts
			}
			if ts > lastExtract {
				lastExtract = ts
			}
		}
	}

	lastScan, _ := view.raw["last_scan"].(string)

	if c.JSON {
		out := map[string]any{
			"total":         total,
			"extracted":     extracted,
			"skipped":       skipped,
			"by_project":    byProject,
			"by_date":       byDate,
			"first_extract": firstExtract,
			"last_extract":  lastExtract,
			"last_scan":     lastScan,
		}
		data, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(data))
		return nil
	}

	fmt.Printf("Sessions log: %s\n", relPath(root, view.path))
	fmt.Printf("  total     : %d\n", total)
	fmt.Printf("  extracted : %d\n", extracted)
	fmt.Printf("  skipped   : %d\n", skipped)
	fmt.Printf("  last scan : %s\n", lastScan)
	if firstExtract != "" {
		fmt.Printf("  first     : %s\n", firstExtract)
		fmt.Printf("  last      : %s\n", lastExtract)
	}

	if len(byProject) > 0 {
		fmt.Println("\nBy project:")
		keys := sortedCountKeys(byProject)
		for _, k := range keys {
			fmt.Printf("  %-30s %4d\n", k, byProject[k])
		}
	}

	if len(byDate) > 0 {
		fmt.Println("\nRecent days (last 14):")
		dates := make([]string, 0, len(byDate))
		for d := range byDate {
			dates = append(dates, d)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(dates)))
		if len(dates) > 14 {
			dates = dates[:14]
		}
		for _, d := range dates {
			fmt.Printf("  %s  %4d\n", d, byDate[d])
		}
	}
	return nil
}

func sortedCountKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

// --- sessions inspect ---

type SessionsInspectCmd struct {
	ID   string `arg:"" help:"Session UUID."`
	JSON bool   `help:"Output JSON."`
}

func (c *SessionsInspectCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	cfg := loadConfig(root)

	// Log state
	view, err := loadSessionsLogView(root)
	if err != nil {
		return err
	}
	logEntry, inLog := view.processed[c.ID]

	// DB state
	db, err := sql.Open("sqlite3", cfg.CcriderDB+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open ccrider db: %w", err)
	}
	defer db.Close()

	// Basic session row
	type dbRow struct {
		MessageCount int
		Project      string
		CreatedAt    string
		UpdatedAt    string
		Summary      string
	}
	var row dbRow
	//nolint:noctx // CLI top-level, no context in scope
	err = db.QueryRow(`
		SELECT COALESCE(s.message_count, 0),
		       COALESCE(s.project_path, ''),
		       COALESCE(s.created_at, ''),
		       COALESCE(s.updated_at, ''),
		       COALESCE(s.llm_summary, '')
		FROM sessions s
		WHERE s.session_id = ?`, c.ID).Scan(&row.MessageCount, &row.Project, &row.CreatedAt, &row.UpdatedAt, &row.Summary)
	inDB := err == nil

	stats := querySessionStats(db, c.ID)
	verdict := stats.filterVerdict()
	if verdict == "" {
		verdict = "keep"
	}

	if c.JSON {
		out := map[string]any{
			"session_id":     c.ID,
			"in_db":          inDB,
			"in_log":         inLog,
			"log_entry":      logEntry,
			"log_skipped":    entryIsSkipped(logEntry),
			"message_count":  row.MessageCount,
			"project_path":   row.Project,
			"created_at":     row.CreatedAt,
			"updated_at":     row.UpdatedAt,
			"summary":        row.Summary,
			"user_msgs":      stats.UserMsgs,
			"total_chars":    stats.TotalChars,
			"filter_verdict": verdict,
		}
		data, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(data))
		return nil
	}

	fmt.Printf("Session: %s\n", c.ID)
	fmt.Println(strings.Repeat("-", 60))
	if !inDB {
		fmt.Println("  ⚠ not found in ccrider DB")
	} else {
		fmt.Printf("  project        : %s\n", row.Project)
		fmt.Printf("  message_count  : %d\n", row.MessageCount)
		fmt.Printf("  created_at     : %s\n", row.CreatedAt)
		fmt.Printf("  updated_at     : %s\n", row.UpdatedAt)
		if row.Summary != "" {
			s := row.Summary
			if len(s) > 200 {
				s = s[:200] + "..."
			}
			fmt.Printf("  summary        : %s\n", s)
		}
	}

	fmt.Println()
	fmt.Println("Pre-filter stats:")
	fmt.Printf("  user_msgs      : %d\n", stats.UserMsgs)
	fmt.Printf("  total_chars    : %d\n", stats.TotalChars)
	fmt.Printf("  verdict        : %s\n", verdict)

	fmt.Println()
	fmt.Println("Sessions log:")
	if !inLog {
		fmt.Println("  (not in log — unprocessed)")
	} else if entryIsSkipped(logEntry) {
		fmt.Println("  ⚠ marked skipped")
		if m, ok := logEntry.(map[string]any); ok {
			if r, _ := m["reason"].(string); r != "" {
				fmt.Printf("  reason         : %s\n", r)
			}
			if t, _ := m["extracted"].(string); t != "" {
				fmt.Printf("  skipped_at     : %s\n", t)
			}
		}
	} else {
		fmt.Println("  ✓ extracted")
		if m, ok := logEntry.(map[string]any); ok {
			if p, _ := m["project"].(string); p != "" {
				fmt.Printf("  project        : %s\n", p)
			}
			if t, _ := m["extracted"].(string); t != "" {
				fmt.Printf("  extracted_at   : %s\n", t)
			}
			if s, _ := m["summary"].(string); s != "" {
				if len(s) > 200 {
					s = s[:200] + "..."
				}
				fmt.Printf("  summary        : %s\n", s)
			}
		}
	}
	return nil
}

// --- sessions unskip ---

type SessionsUnskipCmd struct {
	DryRun bool `help:"Show what would change without writing." short:"n"`
	All    bool `help:"Remove ALL skipped entries regardless of current filter verdict."`
}

func (c *SessionsUnskipCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	cfg := loadConfig(root)

	view, err := loadSessionsLogView(root)
	if err != nil {
		return err
	}

	// Collect skipped IDs.
	var skippedIDs []string
	for id, entry := range view.processed {
		if entryIsSkipped(entry) {
			skippedIDs = append(skippedIDs, id)
		}
	}
	sort.Strings(skippedIDs)

	if len(skippedIDs) == 0 {
		fmt.Println("No skipped entries in sessions log.")
		return nil
	}

	fmt.Printf("Found %d skipped entries.\n", len(skippedIDs))

	// Re-evaluate against current pre-filter.
	db, err := sql.Open("sqlite3", cfg.CcriderDB+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open ccrider db: %w", err)
	}
	defer db.Close()

	var toRemove []string
	keptEmpty := 0
	keptThin := 0
	notInDB := 0
	for _, id := range skippedIDs {
		if c.All {
			toRemove = append(toRemove, id)
			continue
		}
		stats := querySessionStats(db, id)
		if !stats.Found {
			notInDB++
			continue
		}
		switch stats.filterVerdict() {
		case "":
			toRemove = append(toRemove, id)
		case "empty":
			keptEmpty++
		case "thin":
			keptThin++
		}
	}

	fmt.Printf("  would unskip : %d (pass current filter)\n", len(toRemove))
	fmt.Printf("  keep as empty: %d\n", keptEmpty)
	fmt.Printf("  keep as thin : %d\n", keptThin)
	if notInDB > 0 {
		fmt.Printf("  not in DB    : %d\n", notInDB)
	}

	if c.DryRun {
		if len(toRemove) > 0 {
			fmt.Println("\nWould unskip:")
			for _, id := range toRemove {
				fmt.Printf("  %s\n", id)
			}
		}
		return nil
	}

	if len(toRemove) == 0 {
		fmt.Println("\nNothing to do.")
		return nil
	}

	for _, id := range toRemove {
		delete(view.processed, id)
	}
	if err := view.save(); err != nil {
		return fmt.Errorf("save sessions log: %w", err)
	}
	fmt.Printf("\nReset %d wrongly-skipped sessions for reprocessing.\n", len(toRemove))
	return nil
}

// --- sessions filter preview ---

type SessionsFilterCmd struct {
	Top          int `help:"Number of top triaged sessions to test." default:"50"`
	MessageLimit int `help:"Max messages per session (matches sync behavior)." name:"message-limit" default:"300"`
}

func (c *SessionsFilterCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	cfg := loadConfig(root)

	// Use the sync helper to run triage via the scribe binary itself.
	syncCmd := &SyncCmd{SessionsMax: c.Top, SessionSort: "score"}
	ids := syncCmd.triageSessionIDs(c.Top, "--message-limit", fmt.Sprintf("%d", c.MessageLimit))
	if len(ids) == 0 {
		fmt.Println("Triage returned no sessions.")
		return nil
	}

	db, err := sql.Open("sqlite3", cfg.CcriderDB+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open ccrider db: %w", err)
	}
	defer db.Close()

	fmt.Printf("Testing top %d triaged sessions (message-limit=%d):\n\n", len(ids), c.MessageLimit)
	fmt.Printf("%-36s  %6s  %8s  %-8s\n", "SESSION_ID", "USER", "CHARS", "VERDICT")
	fmt.Println(strings.Repeat("-", 66))

	counts := map[string]int{"keep": 0, "empty": 0, "thin": 0}
	for _, id := range ids {
		stats := querySessionStats(db, id)
		v := stats.filterVerdict()
		tag := v
		if tag == "" {
			tag = "keep"
		}
		counts[tag]++
		fmt.Printf("%-36s  %6d  %8d  %-8s\n", id, stats.UserMsgs, stats.TotalChars, tag)
	}
	fmt.Println()
	fmt.Printf("Summary: keep=%d  empty=%d  thin=%d\n", counts["keep"], counts["empty"], counts["thin"])
	return nil
}
