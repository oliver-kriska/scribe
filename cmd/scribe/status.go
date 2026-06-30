package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StatusCmd is a single-shot scoreboard for the KB. Shows what's ingested,
// what's pending, where the pipeline is stuck, and which LLM provider the
// user is on. Deliberately read-only — it does NOT run fetchers, the LLM,
// or qmd. A user who's wondering "what's in my KB and what will sync do?"
// should be able to answer that in <1 second.
//
// Also exposed from `scribe doctor` so doctor acts as a superset.
type StatusCmd struct{}

// ReadOnly marks status as never writing KB state — main() skips the
// run-record append so the scoreboard stays a pure read.
func (s *StatusCmd) ReadOnly() bool { return true }

func (s *StatusCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	return renderStatus(os.Stdout, root)
}

// renderStatus prints the scoreboard. Broken out so `scribe doctor` can
// include it as a section without reinventing the queries. Takes io.Writer
// so tests can capture to a bytes.Buffer.
func renderStatus(w io.Writer, root string) error {
	cfg := loadConfig(root)

	fmt.Fprintln(w, "KB status")
	fmt.Fprintln(w, "─────────")
	fmt.Fprintf(w, "  root: %s\n", root)
	fmt.Fprintln(w)

	// --- raw articles by density ---
	rawDir := filepath.Join(root, "raw", "articles")
	counts := map[string]int{}
	total := 0
	noFrontmatter := 0
	entries, _ := os.ReadDir(rawDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		total++
		data, err := os.ReadFile(filepath.Join(rawDir, e.Name()))
		if err != nil {
			continue
		}
		raw, err := parseFrontmatterRaw(data)
		if err != nil {
			noFrontmatter++
			continue
		}
		d, _ := raw["density"].(string)
		if d == "" {
			counts["unknown"]++
		} else {
			counts[d]++
		}
	}
	fmt.Fprintf(w, "  raw/articles:     %d files\n", total)
	if total > 0 {
		fmt.Fprintf(w, "    density: brief=%d standard=%d dense=%d unknown=%d\n",
			counts["brief"], counts["standard"], counts["dense"], counts["unknown"])
		if noFrontmatter > 0 {
			fmt.Fprintf(w, "    %d file(s) without frontmatter\n", noFrontmatter)
		}
	}

	// --- contextualize + absorb progress ---
	cxLog := loadJSONMap(filepath.Join(root, "wiki", "_contextualized_log.json"))
	absorbLog, _ := loadAbsorbLog(filepath.Join(root, "wiki", "_absorb_log.json"))
	unContext := len(unprocessedForContextualize(root))
	unAbsorb := len(unprocessedForAbsorb(root))
	fmt.Fprintf(w, "  contextualized:   %d done, %d pending\n", len(cxLog), unContext)
	fmt.Fprintf(w, "  absorbed:         %d done, %d pending\n", len(absorbLog), unAbsorb)
	fmt.Fprintln(w)

	// --- backlog: projects + sessions ---
	renderBacklog(w, root, cfg)

	// --- pending approvals (cron-invisible otherwise) ---
	if m, err := loadManifest(root); err == nil {
		if pending := m.pendingProjects(); len(pending) > 0 {
			fmt.Fprintf(w, "  pending approval: %d project(s) — run `scribe projects review`\n", len(pending))
		}
	}

	// --- contextualize provider ---
	cx := cfg.Absorb.Contextualize
	fmt.Fprintf(w, "  contextualize:    provider=%s  model=%s\n", cx.Provider, cx.Model)
	if strings.EqualFold(cx.Provider, "ollama") {
		if err := pingOllamaFast(cx.OllamaURL); err != nil {
			fmt.Fprintf(w, "                    ⚠ ollama unreachable at %s: %v\n", cx.OllamaURL, err)
		} else {
			fmt.Fprintf(w, "                    ✓ ollama up at %s\n", cx.OllamaURL)
		}
	} else {
		fmt.Fprintln(w, "                    tip: set provider=ollama for free local mode")
	}

	// --- proposal files (review queue) ---
	renderProposalQueue(w, root)

	// --- last sync ---
	runsDir := filepath.Join(root, "output", "runs")
	last := lastSyncSummary(runsDir)
	if last != "" {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  last sync:        %s\n", last)
	}

	// --- qmd collection (this KB) ---
	fmt.Fprintln(w)
	if detail, ok := qmdCollectionStatus(root); ok {
		fmt.Fprintf(w, "  qmd collection:   %s\n", detail)
	} else {
		fmt.Fprintln(w, "  qmd collection:   not indexed yet — run `scribe sync` or `qmd update`")
	}

	return nil
}

// renderProposalQueue prints a one-line-per-file review queue so pending
// LLM proposals don't rot on disk. Pulled directly from the known
// proposal markdown paths — counts `###` section headers as the proxy
// for "items awaiting review".
func renderProposalQueue(w io.Writer, root string) {
	type qitem struct {
		label string
		path  string
	}
	items := []qitem{
		{"contradictions:  ", "wiki/_contradictions.md"},
		{"resolutions:     ", "wiki/_resolution-proposals.md"},
		{"identities:      ", "wiki/_identity-proposals.md"},
		{"unfetched-links: ", "wiki/_unfetched-links.md"},
	}
	printed := false
	for _, it := range items {
		abs := filepath.Join(root, it.path)
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		n := countProposalItems(string(data), it.path)
		if n == 0 {
			continue
		}
		if !printed {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "  review queue (hand-review, then clear the file):")
			printed = true
		}
		fmt.Fprintf(w, "    %s%3d pending   (%s)\n", it.label, n, it.path)
	}
}

// countProposalItems counts the per-entry blocks in a proposal file. Uses
// `###` as the proxy for resolve/identity files and `- ` bullets for
// contradictions/unfetched-links which are flat list format.
func countProposalItems(body, path string) int {
	if strings.Contains(path, "_contradictions.md") || strings.Contains(path, "_unfetched-links.md") {
		n := 0
		for line := range strings.SplitSeq(body, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "- ") {
				n++
			}
		}
		return n
	}
	n := 0
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, "### ") {
			n++
		}
	}
	return n
}

// pingOllamaFast does a 2-second GET /api/tags. Separate from the Generate
// path's ensureReady because we don't want the scoreboard to auto-pull a
// model — it just reports.
func pingOllamaFast(baseURL string) error {
	if baseURL == "" {
		baseURL = defaultOllamaURL
	}
	o := &ollamaProvider{baseURL: baseURL, model: ""}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := o.listedModels(ctx)
	return err
}

// lastSyncSummary finds the most recent JSONL entry in output/runs/ whose
// command is "sync" and returns a one-line summary.
func lastSyncSummary(runsDir string) string {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return ""
	}
	// Files are YYYY-MM-DD.jsonl — read the newest.
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
	// Walk lines backward to find the most recent sync entry.
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if strings.Contains(line, `"command":"sync"`) {
			// Parse just enough to pull timestamp + key counters.
			return formatRunLine(line)
		}
	}
	return ""
}

// formatRunLine extracts ts + status + key stats from a JSONL run record.
// We don't pull a full JSON parse because the record has variable shape;
// string scraping a few fields is faster and plenty for the scoreboard.
func formatRunLine(line string) string {
	ts := extractJSONField(line, "timestamp")
	status := extractJSONField(line, "status")
	abs := extractJSONField(line, "absorbed")
	ext := extractJSONField(line, "extracted")
	ses := extractJSONField(line, "sessions")
	return fmt.Sprintf("%s [%s] extracted=%s absorbed=%s sessions=%s",
		ts, status, defaultStr(ext, "0"), defaultStr(abs, "0"), defaultStr(ses, "0"))
}

func extractJSONField(line, key string) string {
	needle := fmt.Sprintf("%q:", key)
	_, after, ok := strings.Cut(line, needle)
	if !ok {
		return ""
	}
	rest := after
	// Value starts with " for strings, digit for numbers.
	if strings.HasPrefix(rest, `"`) {
		end := strings.Index(rest[1:], `"`)
		if end < 0 {
			return ""
		}
		return rest[1 : 1+end]
	}
	// Number or bool — read until , or }.
	end := strings.IndexAny(rest, ",}")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// qmdCollectionStatus reports the qmd collection that indexes THIS KB —
// file count and freshness — or ("", false) when no collection maps to
// this KB (so status prints "not indexed yet"). The old qmdIndexSize
// reported the GLOBAL qmd store size, which on a multi-collection install
// is every KB combined (issue #27 item 5). qmd names a collection after
// the indexed folder's basename; we confirm the match via `collection
// show`'s Path so a same-named collection at a different folder can't be
// misreported as this KB's.
func qmdCollectionStatus(root string) (string, bool) {
	name := filepath.Base(root)
	show, err := runCmdErr(root, "qmd", "collection", "show", name)
	if err != nil {
		return "", false
	}
	if p := qmdField(show, "Path:"); p == "" || !samePath(p, root) {
		return "", false // no collection for this KB (or it points elsewhere)
	}
	detail := name
	// Files + freshness live in `collection list`, not `collection show`.
	if list, lerr := runCmdErr(root, "qmd", "collection", "list"); lerr == nil {
		if files, updated := qmdCollectionFilesUpdated(list, name); files != "" {
			detail += " — " + files + " files"
			if updated != "" {
				detail += ", updated " + updated
			}
		}
	}
	return detail, true
}

// qmdField returns the value after the first "<label> ..." line in qmd's
// human-readable output (e.g. label "Path:"). Empty when absent.
func qmdField(out, label string) string {
	for line := range strings.SplitSeq(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, label); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// qmdCollectionFilesUpdated parses `qmd collection list` for the Files
// and Updated values of the named collection. The block is headed by
// "<name> (qmd://<name>/)" and its fields are indented beneath it until
// the next collection header; scanning stops there so one collection's
// numbers can't bleed into another's.
func qmdCollectionFilesUpdated(list, name string) (files, updated string) {
	header := name + " (qmd://"
	inBlock := false
	for line := range strings.SplitSeq(list, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, header):
			inBlock = true
		case inBlock && strings.Contains(trimmed, "(qmd://"):
			return files, updated // next collection's header — done
		case inBlock && strings.HasPrefix(trimmed, "Files:"):
			files = strings.TrimSpace(strings.TrimPrefix(trimmed, "Files:"))
		case inBlock && strings.HasPrefix(trimmed, "Updated:"):
			updated = strings.TrimSpace(strings.TrimPrefix(trimmed, "Updated:"))
		}
	}
	return files, updated
}

// renderBacklog prints "what's not yet processed" so the user can
// gauge how much work is left to bring the KB to a steady state.
// Three sources of pending work outside raw/articles:
//
//  1. Projects in the manifest whose git SHA has moved since the
//     last extraction (or that have never been extracted). Best-
//     effort — projects with missing directories are silently
//     skipped, matching projectsNeedingExtraction's behavior.
//  2. Sessions in the ccrider DB not yet listed in
//     wiki/_sessions_log.json. Counts only sessions that pass the
//     pre-filter ceiling so mechanical/short sessions don't inflate
//     the backlog.
//  3. Pending drop files staged inside other projects'
//     `.claude/<kb-name>/` directories — these get collected at
//     the start of `scribe sync` but show up here too so the user
//     can spot accumulated drops without running a full sync.
//
// "Pending" means the next sync will actually process it. Work a
// policy gate holds back forever — drop files without an absorb
// opt-in under strictness=high, projects over sync.max_extract_files
// — is reported separately as "held" so the backlog never promises
// work sync won't do. The classification reuses the same rules sync
// itself applies (strictnessHoldsFile, exceedsExtractFileCap), so
// status and the sync-time hold summary can't drift apart.
//
// Each subsection is silenced if the underlying state file is
// missing — a fresh KB shouldn't get a wall of "0 pending" lines.
func renderBacklog(w io.Writer, root string, cfg *ScribeConfig) {
	type backlog struct {
		label    string
		done     int
		todo     int
		held     int
		heldNote string
	}
	var rows []backlog

	// Projects.
	if manifest, err := loadManifest(root); err == nil {
		total := 0
		needing := 0
		capped := 0
		for _, entry := range manifest.Projects {
			if !dirExists(entry.Path) {
				continue
			}
			total++
			var needs bool
			switch {
			case entry.LastSHA == "":
				needs = true
			case hasGit(entry.Path):
				cur := gitSHA(entry.Path)
				needs = cur != "" && cur != entry.LastSHA
			default:
				// Non-git projects: stat-walk fallback, conservative.
				needs = entry.LastExtracted == ""
			}
			if !needs {
				continue
			}
			// Same size gate sync applies: a project over
			// sync.max_extract_files is skipped every run until the user
			// runs `scribe deep`, so it's held, not pending.
			if cfg.Sync.MaxExtractFiles > 0 &&
				exceedsExtractFileCap(cfg, len(gitChangedFiles(entry.Path, entry.LastSHA, extractScanPatterns))) {
				capped++
			} else {
				needing++
			}
		}
		if total > 0 {
			rows = append(rows, backlog{
				label:    "projects (extract):",
				done:     total - needing - capped,
				todo:     needing,
				held:     capped,
				heldNote: ">max_extract_files — run `scribe deep`",
			})
		}
	}

	// Sessions. Scope to projects THIS KB has actually adopted (approved
	// manifest entries), not the whole global ccrider DB — counting the
	// DB minus this KB's mined log made a fresh KB with zero approved
	// projects report the machine's entire session pile as pending
	// (issue #27 item 4). Mirrors the manifest/approval gate sync's
	// preFilterSessions applies, so the backlog can't promise work sync
	// won't do.
	if cfg.CcriderDB != "" && fileExists(cfg.CcriderDB) {
		processed := loadProcessedSessionIDs(filepath.Join(root, "wiki", "_sessions_log.json"))
		processedSet := make(map[string]struct{}, len(processed))
		for _, id := range processed {
			processedSet[id] = struct{}{}
		}
		if pending, ok := countScopedPendingSessions(root, cfg, processedSet); ok {
			rows = append(rows, backlog{label: "sessions (mine):", done: len(processedSet), todo: pending})
		}
	}

	// Drop files staged in other projects, split by the same strictness
	// gate sync's absorb applies: under strictness=high a drop without
	// an opt-in (`absorb: true` or a named domain) never drains, so
	// it's held, not pending.
	if drops := pendingDropFiles(root); len(drops) > 0 {
		held := 0
		for _, f := range drops {
			if strictnessHoldsFile(cfg.Absorb.Strictness, f) {
				held++
			}
		}
		rows = append(rows, backlog{
			label:    "drop files:",
			todo:     len(drops) - held,
			held:     held,
			heldNote: "strictness=high — need opt-in",
		})
	}

	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  backlog (run `scribe sync` to process):")
	for _, r := range rows {
		parts := make([]string, 0, 3)
		if r.done > 0 {
			parts = append(parts, fmt.Sprintf("%d done", r.done))
		}
		if r.held > 0 {
			parts = append(parts, fmt.Sprintf("%d held (%s)", r.held, r.heldNote))
		}
		parts = append(parts, fmt.Sprintf("%d pending", r.todo))
		fmt.Fprintf(w, "    %-22s %s\n", r.label, strings.Join(parts, ", "))
	}
}

// countScopedPendingSessions counts ccrider sessions THIS KB would mine
// but hasn't — scoped to projects with an APPROVED manifest entry, the
// same gate sync's preFilterSessions applies (issue #27 item 4). The old
// whole-DB count made a KB with zero approved projects report the
// machine's entire session pile as pending. processed is the set of
// already-mined session IDs, excluded from the tally. Reads the DB
// read-only; returns (0, false) on any DB/manifest error so the caller
// drops the row rather than printing a wrong number.
func countScopedPendingSessions(root string, cfg *ScribeConfig, processed map[string]struct{}) (int, bool) {
	db, err := sql.Open("sqlite3", cfg.CcriderDB+"?mode=ro")
	if err != nil {
		return 0, false
	}
	defer db.Close()

	manifest, err := loadManifest(root)
	if err != nil {
		return 0, false
	}
	//nolint:noctx // status command is short-lived
	rows, err := db.Query("SELECT session_id, COALESCE(project_path, '') FROM sessions")
	if err != nil {
		return 0, false
	}
	defer rows.Close()

	pending := 0
	for rows.Next() {
		var sid, ppath string
		if err := rows.Scan(&sid, &ppath); err != nil {
			continue
		}
		if ppath == "" {
			continue
		}
		// entryForPath (not a keyed lookup) so a session run inside an
		// approved project's worktree still resolves to that project, and
		// a basename collision can't borrow another project's approval.
		entry := manifest.entryForPath(ppath)
		if entry == nil || !entry.IsApproved() {
			continue
		}
		if _, done := processed[sid]; done {
			continue
		}
		pending++
	}
	if err := rows.Err(); err != nil {
		return 0, false
	}
	return pending, true
}

// pendingDropFiles lists the unprocessed drop files staged inside
// other projects' `.claude/<kb-name>/` directories — the exact set the
// next sync's collect phase would pick up (same enumeration via
// unprocessedDropFiles: approved projects only, main checkout plus
// worktrees, newer than last_drop_processed). Returns paths, not a
// count, so the caller can classify each file against the strictness
// gate.
func pendingDropFiles(root string) []string {
	manifest, err := loadManifest(root)
	if err != nil {
		return nil
	}
	kb := kbName(root)
	var files []string
	for _, entry := range manifest.Projects {
		if !entry.IsApproved() {
			continue
		}
		files = append(files, unprocessedDropFiles(kb, entry)...)
	}
	return files
}
