package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DoctorCmd is a read-only health check for a scribe KB checkout. It audits
// dependencies, config, LaunchAgents, state files, and run freshness, then
// prints each check with an exact remediation command. Doctor never mutates
// anything: it diagnoses and points, the user runs the fixes.
//
// Exit code is non-zero only on hard failures (FAIL). Freshness drift is a
// warning, not a failure — cron might simply not have fired yet because the
// Mac was asleep.
type DoctorCmd struct {
	JSON        bool          `help:"Emit structured JSON instead of text."`
	Section     string        `help:"Run only one section: deps | config | cron | state | freshness | errors." enum:"deps,config,cron,state,freshness,errors," default:""`
	ErrorWindow time.Duration `help:"How far back to scan run records for errors." default:"24h"`
}

type checkStatus string

const (
	statusOK   checkStatus = "ok"
	statusWarn checkStatus = "warn"
	statusFail checkStatus = "FAIL"
)

// check is one line of doctor's report. Each check carries enough context to
// render itself in either text or JSON mode without the formatter needing to
// know about the underlying probe.
type check struct {
	Section string      `json:"section"`
	Name    string      `json:"name"`
	Status  checkStatus `json:"status"`
	Detail  string      `json:"detail,omitempty"`
	Fix     string      `json:"fix,omitempty"`
}

func (c *DoctorCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return fmt.Errorf("not inside a scribe KB checkout: %w", err)
	}
	cfg := loadConfig(root)

	sectionOrder := []string{"deps", "config", "cron", "state", "freshness", "errors"}
	var all []check
	for _, name := range sectionOrder {
		if c.Section != "" && c.Section != name {
			continue
		}
		switch name {
		case "deps":
			all = append(all, checkDeps()...)
		case "config":
			all = append(all, checkConfig(root, cfg)...)
		case "cron":
			all = append(all, checkCron(root)...)
		case "state":
			all = append(all, checkState(root)...)
		case "freshness":
			all = append(all, checkFreshness(root, time.Now())...)
		case "errors":
			all = append(all, checkRecentErrors(root, time.Now(), c.ErrorWindow)...)
		}
	}

	if c.JSON {
		printChecksJSON(all, root)
	} else {
		printChecksText(all, root)
		// Append the status scoreboard in text mode (skipped under --section
		// filters so section-targeted runs stay focused; skipped under --json
		// so the JSON shape stays stable).
		if c.Section == "" {
			fmt.Println()
			_ = renderStatus(os.Stdout, root)
		}
	}

	fails := 0
	for _, ck := range all {
		if ck.Status == statusFail {
			fails++
		}
	}
	if fails > 0 {
		return fmt.Errorf("%d check(s) failed — see output above", fails)
	}
	return nil
}

// ---- Dependencies ----

func checkDeps() []check {
	var out []check
	for _, d := range scribeDeps {
		path, err := exec.LookPath(d.Binary)
		switch {
		case err == nil:
			out = append(out, check{Section: "deps", Name: d.Name, Status: statusOK, Detail: path})
		case d.Required:
			out = append(out, check{Section: "deps", Name: d.Name, Status: statusFail, Detail: "not found in PATH", Fix: d.Fix})
		default:
			out = append(out, check{Section: "deps", Name: d.Name, Status: statusWarn, Detail: "not found (optional)", Fix: d.Fix})
		}
	}

	// Full Disk Access probe — capture-imessage needs chat.db readable.
	// On modern macOS (10.15+) TCC tracks the *binary being executed* per
	// inode+cdhash, not the parent Terminal. So the fix is always "grant FDA
	// to the scribe binary itself", which `scribe fda` drives interactively.
	chatDB := filepath.Join(os.Getenv("HOME"), "Library", "Messages", "chat.db")
	if f, err := os.Open(chatDB); err == nil {
		_ = f.Close()
		out = append(out, check{Section: "deps", Name: "chat.db (FDA)", Status: statusOK, Detail: "readable"})
	} else {
		out = append(out, check{
			Section: "deps", Name: "chat.db (FDA)", Status: statusFail,
			Detail: "unreadable — `scribe capture` will fail",
			Fix:    "run `scribe fda` (grants Full Disk Access to the scribe binary)",
		})
	}

	// Warn about the Homebrew-Cellar / cdhash tax: the FDA grant is keyed to
	// the exact binary inode. Every `brew upgrade scribe` replaces the
	// Cellar-versioned binary, which invalidates the prior grant and makes
	// capture silently start failing until the user re-runs `scribe fda`.
	// This stays a warn (not a fail) because the binary may already be
	// granted — we only want to pre-empt the next upgrade surprise.
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil && strings.Contains(resolved, "/Cellar/scribe/") {
			out = append(out, check{
				Section: "deps", Name: "FDA (brew upgrade)", Status: statusWarn,
				Detail: "running from " + resolved + " — the TCC grant is tied to this exact binary and will be invalidated by the next `brew upgrade scribe`",
				Fix:    "re-run `scribe fda` after every upgrade (until signed builds ship)",
			})
		}
	}
	return out
}

// ---- Config ----

func checkConfig(root string, cfg *ScribeConfig) []check {
	var out []check

	cfgPath := filepath.Join(root, "scribe.yaml")
	if fileExists(cfgPath) {
		out = append(out, check{Section: "config", Name: "scribe.yaml", Status: statusOK, Detail: relPath(root, cfgPath)})
	} else {
		out = append(out, check{Section: "config", Name: "scribe.yaml", Status: statusWarn, Detail: "missing — using defaults", Fix: "scribe init"})
	}

	if dirExists(cfg.ClaudeProjectsDir) {
		out = append(out, check{Section: "config", Name: "claude_projects_dir", Status: statusOK, Detail: cfg.ClaudeProjectsDir})
	} else {
		out = append(out, check{
			Section: "config", Name: "claude_projects_dir", Status: statusFail,
			Detail: cfg.ClaudeProjectsDir + " does not exist",
			Fix:    "edit scribe.yaml or install Claude Code",
		})
	}

	if fileExists(cfg.CcriderDB) {
		out = append(out, check{Section: "config", Name: "ccrider_db", Status: statusOK, Detail: cfg.CcriderDB})
	} else {
		out = append(out, check{
			Section: "config", Name: "ccrider_db", Status: statusFail,
			Detail: cfg.CcriderDB + " does not exist",
			Fix:    "run ccrider once to initialize the database",
		})
	}

	claudeMD := filepath.Join(os.Getenv("HOME"), ".claude", "CLAUDE.md")
	data, err := os.ReadFile(claudeMD)
	switch {
	case err != nil:
		out = append(out, check{Section: "config", Name: "~/.claude/CLAUDE.md", Status: statusWarn, Detail: "not found", Fix: "scribe init"})
	case strings.Contains(string(data), claudeMDMarkerBegin) && strings.Contains(string(data), claudeMDMarkerEnd):
		out = append(out, check{Section: "config", Name: "~/.claude/CLAUDE.md block", Status: statusOK, Detail: "installed"})
	default:
		out = append(out, check{Section: "config", Name: "~/.claude/CLAUDE.md block", Status: statusWarn, Detail: "scribe block not found", Fix: "scribe init"})
	}

	return out
}

// ---- Cron ----

func checkCron(root string) []check {
	var out []check
	binary := resolveScribeBinary()
	jobs := scribeJobs(root, binary)
	domain := guiDomain()
	for _, job := range jobs {
		label := plistLabel(job.Name)
		path := plistPath(job.Name)
		state := probeLaunchAgent(domain, label, path)
		switch state {
		case "loaded":
			out = append(out, check{Section: "cron", Name: label, Status: statusOK, Detail: "loaded"})
		case "present":
			out = append(out, check{
				Section: "cron", Name: label, Status: statusFail,
				Detail: "plist on disk but not loaded into " + domain,
				Fix:    "scribe cron install",
			})
		default: // "missing"
			out = append(out, check{
				Section: "cron", Name: label, Status: statusFail,
				Detail: "plist missing (" + path + ")",
				Fix:    "scribe cron install",
			})
		}
	}
	return out
}

// ---- State files ----

func checkState(root string) []check {
	var out []check

	if m, err := loadManifest(root); err == nil {
		out = append(out, check{
			Section: "state", Name: "scripts/projects.json", Status: statusOK,
			Detail: fmt.Sprintf("%d projects", len(m.Projects)),
		})
	} else {
		out = append(out, check{
			Section: "state", Name: "scripts/projects.json", Status: statusFail,
			Detail: err.Error(),
			Fix:    "restore from git or rerun `scribe sync` to rebuild",
		})
	}

	statePath := filepath.Join(root, "scripts", "imessage-state.json")
	if _, err := loadCaptureState(statePath); err == nil {
		out = append(out, check{Section: "state", Name: "scripts/imessage-state.json", Status: statusOK, Detail: "parsed"})
	} else {
		out = append(out, check{
			Section: "state", Name: "scripts/imessage-state.json", Status: statusFail,
			Detail: err.Error(),
			Fix:    "delete and rerun `scribe capture` to regenerate",
		})
	}

	// Generic JSON parse-check for the wiki-side state files.
	jsonFiles := []struct {
		rel string
		fix string
	}{
		{"wiki/_sessions_log.json", "run `scribe sync --sessions` to rebuild"},
		{"wiki/_backlinks.json", "run `scribe backlinks` to rebuild"},
	}
	for _, jf := range jsonFiles {
		out = append(out, checkJSONFile(root, jf.rel, jf.fix))
	}

	// Markdown files: exist + non-empty is enough.
	mdFiles := []struct {
		rel string
		fix string
	}{
		{"wiki/_index.md", "run `scribe index` to rebuild"},
		{"log.md", "append-only; restore from git"},
	}
	for _, mf := range mdFiles {
		abs := filepath.Join(root, mf.rel)
		info, err := os.Stat(abs)
		switch {
		case err != nil:
			out = append(out, check{Section: "state", Name: mf.rel, Status: statusFail, Detail: "missing", Fix: mf.fix})
		case info.Size() == 0:
			out = append(out, check{Section: "state", Name: mf.rel, Status: statusWarn, Detail: "empty file", Fix: mf.fix})
		default:
			out = append(out, check{Section: "state", Name: mf.rel, Status: statusOK, Detail: humanSize(info.Size())})
		}
	}

	return out
}

// checkJSONFile reads a file and attempts to json.Unmarshal it. Missing or
// corrupt files become FAILs with the caller-provided fix hint.
func checkJSONFile(root, rel, fix string) check {
	abs := filepath.Join(root, rel)
	data, err := os.ReadFile(abs)
	if err != nil {
		return check{Section: "state", Name: rel, Status: statusFail, Detail: "cannot read: " + err.Error(), Fix: fix}
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return check{Section: "state", Name: rel, Status: statusFail, Detail: "invalid JSON: " + err.Error(), Fix: fix}
	}
	return check{Section: "state", Name: rel, Status: statusOK, Detail: "parsed"}
}

// ---- Freshness ----

// freshnessSpec maps a monitored command to its max allowable gap since the
// last successful run. A nil/zero LastOk always yields a WARN — we prefer a
// loud "never ran" to silent drift.
type freshnessSpec struct {
	Command string // command path as written by writeRunRecord (e.g. "sync", "ingest drain")
	ArgFlag string // optional args-substring required (e.g. "--sessions") to distinguish modes of the same command
	Label   string
	MaxGap  time.Duration
	Fix     string
}

var freshnessSpecs = []freshnessSpec{
	{Command: "sync", Label: "sync (projects)", MaxGap: 6 * time.Hour, Fix: "scribe sync"},
	{Command: "sync", ArgFlag: "--sessions", Label: "sync (sessions)", MaxGap: 36 * time.Hour, Fix: "scribe sync --sessions"},
	{Command: "lint", Label: "lint", MaxGap: 48 * time.Hour, Fix: "scribe lint"},
	{Command: "dream", Label: "dream", MaxGap: 10 * 24 * time.Hour, Fix: "scribe dream"},
	{Command: "capture", Label: "capture", MaxGap: 12 * time.Hour, Fix: "scribe capture --fetch"},
	{Command: "commit", Label: "commit", MaxGap: 6 * time.Hour, Fix: "scribe commit"},
	{Command: "ingest drain", Label: "ingest drain", MaxGap: 3 * time.Hour, Fix: "scribe ingest drain"},
}

// runRecord mirrors the JSONL schema writeRunRecord emits in main.go.
type runRecord struct {
	Command   string   `json:"command"`
	Status    string   `json:"status"`
	Timestamp string   `json:"timestamp"`
	Args      []string `json:"args"`
}

// loadRunRecords scans output/runs/*.jsonl and returns, for each command key,
// the newest "ok" timestamp. Keys are either the bare command path ("sync")
// or "<command> <flag>" ("sync --sessions") so the freshness specs can
// distinguish the two modes that share the same command path.
//
// A missing runs directory is not an error — doctor must work on fresh
// checkouts before any scribe commands have been logged.
func loadRunRecords(root string) (map[string]time.Time, error) {
	runsDir := filepath.Join(root, "output", "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]time.Time{}, nil
		}
		return nil, err
	}
	result := map[string]time.Time{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(runsDir, e.Name()))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			var r runRecord
			if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
				continue
			}
			if r.Status != "ok" || r.Command == "" {
				continue
			}
			ts, err := time.Parse(time.RFC3339, r.Timestamp)
			if err != nil {
				continue
			}
			if prev, ok := result[r.Command]; !ok || ts.After(prev) {
				result[r.Command] = ts
			}
			// Flag-specific keys so modes like `sync --sessions` track separately.
			for _, arg := range r.Args {
				if strings.HasPrefix(arg, "--") {
					key := r.Command + " " + arg
					if prev, ok := result[key]; !ok || ts.After(prev) {
						result[key] = ts
					}
				}
			}
		}
		_ = f.Close()
	}
	return result, nil
}

// classifyFreshness compares a last-ok timestamp to a threshold and returns
// the status word plus a display detail. Extracted as a pure function so the
// thresholds are unit-testable without setting up a fake filesystem.
func classifyFreshness(lastOk time.Time, now time.Time, gap time.Duration) (checkStatus, string) {
	if lastOk.IsZero() {
		return statusWarn, "never run (no record in output/runs/)"
	}
	age := now.Sub(lastOk)
	if age > gap {
		return statusWarn, fmt.Sprintf("last ok %s ago — expected ≤ %s", shortDuration(age), shortDuration(gap))
	}
	return statusOK, "last ok " + shortDuration(age) + " ago"
}

// runError captures the latest error timestamp + message for one command key.
// loadRunErrors populates these so checkRecentErrors can surface the most
// recent failure per command within the configured window.
type runError struct {
	When time.Time
	Msg  string
	Args []string
}

// loadRunErrors scans output/runs/*.jsonl and returns the newest `status:"error"`
// record per command key within `since`. A command is keyed by its base command
// name (e.g. "sync", "capture") so a cron running the same command every hour
// folds into one error line, not dozens.
func loadRunErrors(root string, since time.Time) (map[string]runError, error) {
	runsDir := filepath.Join(root, "output", "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]runError{}, nil
		}
		return nil, err
	}
	result := map[string]runError{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(runsDir, e.Name()))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			var r struct {
				Command   string   `json:"command"`
				Status    string   `json:"status"`
				Timestamp string   `json:"timestamp"`
				Error     string   `json:"error"`
				Args      []string `json:"args"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
				continue
			}
			if r.Status != "error" || r.Command == "" {
				continue
			}
			ts, err := time.Parse(time.RFC3339, r.Timestamp)
			if err != nil || ts.Before(since) {
				continue
			}
			if prev, ok := result[r.Command]; !ok || ts.After(prev.When) {
				result[r.Command] = runError{When: ts, Msg: r.Error, Args: r.Args}
			}
		}
		_ = f.Close()
	}
	return result, nil
}

// checkRecentErrors reports the newest error-per-command inside the window.
// Errors are warnings by default because a single transient failure shouldn't
// fail the doctor exit code — but repeated failures (e.g. every scheduled
// triage run over a whole day) still get surfaced instead of being masked by
// the latest successful run, which was the original doctor blind spot.
func checkRecentErrors(root string, now time.Time, window time.Duration) []check {
	since := now.Add(-window)
	errs, err := loadRunErrors(root, since)
	if err != nil {
		return []check{{
			Section: "errors", Name: "output/runs", Status: statusFail,
			Detail: err.Error(),
			Fix:    "check filesystem permissions on output/runs/",
		}}
	}
	if len(errs) == 0 {
		return []check{{
			Section: "errors", Name: "recent runs", Status: statusOK,
			Detail: fmt.Sprintf("no errors in last %s", shortDuration(window)),
		}}
	}
	// Stable order: sort command keys.
	keys := make([]string, 0, len(errs))
	for k := range errs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []check
	for _, k := range keys {
		e := errs[k]
		age := shortDuration(now.Sub(e.When))
		detail := fmt.Sprintf("last error %s ago: %s", age, truncateError(e.Msg))
		out = append(out, check{
			Section: "errors", Name: k, Status: statusWarn,
			Detail: detail,
			Fix:    fixHintForError(k, e.Msg),
		})
	}
	return out
}

// fixHintForError turns a known error signature into a runnable command the
// user can paste. Falls back to "read the jsonl" when the pattern doesn't
// match — that's still useful, just less directed.
func fixHintForError(command, msg string) string {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "operation not permitted") && command == "capture":
		return "run: scribe fda  (grants Full Disk Access interactively)"
	case strings.Contains(lower, "no such module: fts5"):
		return "rebuild with sqlite_fts5 tag: make install (or reinstall via brew)"
	case strings.Contains(lower, "handle") && strings.Contains(lower, "not found in chat.db"):
		return "fix capture.self_chat_handle in scribe.yaml"
	case strings.Contains(lower, "no self-chat handle configured"):
		return "set capture.self_chat_handle in scribe.yaml or SCRIBE_SELF_CHAT_ID"
	case strings.Contains(lower, "rate limit"):
		return "wait out Anthropic rate-limit; scribe sync resumes automatically next run"
	}
	return "inspect output/runs/"
}

// truncateError keeps the error section readable — full messages can run past
// 200 chars and break the aligned text layout in printChecksText.
func truncateError(msg string) string {
	msg = strings.ReplaceAll(msg, "\n", " ")
	const limit = 140
	if len(msg) > limit {
		return msg[:limit] + "…"
	}
	if msg == "" {
		return "(no message)"
	}
	return msg
}

func checkFreshness(root string, now time.Time) []check {
	records, err := loadRunRecords(root)
	if err != nil {
		return []check{{
			Section: "freshness", Name: "output/runs", Status: statusFail,
			Detail: err.Error(),
			Fix:    "check filesystem permissions on output/runs/",
		}}
	}
	var out []check
	for _, spec := range freshnessSpecs {
		key := spec.Command
		if spec.ArgFlag != "" {
			key = spec.Command + " " + spec.ArgFlag
		}
		lastOk := records[key]
		status, detail := classifyFreshness(lastOk, now, spec.MaxGap)
		ck := check{Section: "freshness", Name: spec.Label, Status: status, Detail: detail}
		if status != statusOK {
			ck.Fix = spec.Fix
		}
		out = append(out, ck)
	}
	return out
}

// ---- Formatting helpers ----

func shortDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		days := int(d / (24 * time.Hour))
		h := int((d % (24 * time.Hour)).Hours())
		if h == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, h)
	}
}

func humanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%dB", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(bytes)/float64(div), "KMGT"[exp])
}

func printChecksText(all []check, root string) {
	fmt.Printf("KB root: %s\n\n", root)

	sectionOrder := []string{}
	bySection := map[string][]check{}
	for _, ck := range all {
		if _, ok := bySection[ck.Section]; !ok {
			sectionOrder = append(sectionOrder, ck.Section)
		}
		bySection[ck.Section] = append(bySection[ck.Section], ck)
	}

	titles := map[string]string{
		"deps":      "Dependencies:",
		"config":    "Config:",
		"cron":      "Cron (LaunchAgents):",
		"state":     "State files:",
		"freshness": "Freshness (from output/runs/):",
		"errors":    "Recent run errors:",
	}

	ok, warn, fail := 0, 0, 0
	for _, sec := range sectionOrder {
		fmt.Println(titles[sec])
		maxName := 0
		for _, ck := range bySection[sec] {
			if len(ck.Name) > maxName {
				maxName = len(ck.Name)
			}
		}
		for _, ck := range bySection[sec] {
			marker := "[ok]  "
			switch ck.Status {
			case statusWarn:
				marker = "[warn]"
				warn++
			case statusFail:
				marker = "[FAIL]"
				fail++
			default:
				ok++
			}
			fmt.Printf("  %s  %-*s  %s\n", marker, maxName, ck.Name, ck.Detail)
			if ck.Fix != "" {
				fmt.Printf("          fix: %s\n", ck.Fix)
			}
		}
		fmt.Println()
	}

	fmt.Printf("Summary: %d ok, %d warn, %d FAIL\n", ok, warn, fail)
}

func printChecksJSON(all []check, root string) {
	ok, warn, fail := 0, 0, 0
	for _, ck := range all {
		switch ck.Status {
		case statusOK:
			ok++
		case statusWarn:
			warn++
		case statusFail:
			fail++
		}
	}
	if all == nil {
		all = []check{}
	}
	payload := map[string]any{
		"kb_root": root,
		"checks":  all,
		"summary": map[string]int{"ok": ok, "warn": warn, "fail": fail},
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}
