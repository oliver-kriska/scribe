package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// CronCmd manages macOS LaunchAgents for scribe's scheduled KB jobs.
//
// Why LaunchAgents instead of crontab: macOS cron runs under launchd without
// a user Aqua session, so it cannot read the login keychain. Claude Code's
// OAuth subscription stores tokens in the login keychain, so any `claude -p`
// invocation from cron fails with "Not logged in". LaunchAgents loaded into
// the gui/<uid> domain run inside the user's login session and have keychain
// access. This command installs/removes/status those plists.
type CronCmd struct {
	Install   CronInstallCmd   `cmd:"" help:"Install LaunchAgent plists for scribe KB jobs."`
	Status    CronStatusCmd    `cmd:"" help:"Show status of scribe LaunchAgents."`
	Uninstall CronUninstallCmd `cmd:"" help:"Unload and remove scribe LaunchAgents."`
}

// cronJob describes a scheduled scribe task.
type cronJob struct {
	Name      string // short name; label becomes com.scribe.<name>
	Desc      string
	Command   string // shell command run via `zsh -lc`
	LogFile   string
	Schedule  schedSpec
	KeepAlive bool // long-running: boot at load, auto-restart on crash
}

// schedSpec is a launchd schedule: either interval or calendar.
type schedSpec struct {
	Calendar []calTime
}

type calTime struct {
	Hour    int // -1 means omit
	Minute  int // -1 means omit
	Weekday int // -1 means omit (0 = Sunday)
}

// scribeJobs returns the default KB cron schedule. Consumers (install,
// status, uninstall, doctor) all share this definition so there is one
// source of truth for what scribe runs on cron.
// scribeJobs returns the machine's cron schedule. Commands are KB-agnostic
// (issue #26): each scheduled job runs `scribe each -- <sub>`, which
// iterates the KB registry (kbs: in the user config) and runs <sub> in each
// registered KB with per-KB failure isolation. One agent set therefore
// serves every KB — installing from a second KB no longer clobbers the
// first. `watch` is a single long-running agent, but it too serves every
// registered KB: it watches the shared ccrider DB and feeds the machine-
// global pending queue, deduping against all KBs' processed logs (watch.go).
func scribeJobs(binary string) []cronJob {
	logDir := "/tmp"
	each := func(sub string) string { return binary + " each -- " + sub }
	return []cronJob{
		{
			Name:     "auto-commit",
			Desc:     "Hourly KB auto-commit",
			Command:  each("commit"),
			LogFile:  filepath.Join(logDir, "scribe-autocommit.log"),
			Schedule: schedSpec{Calendar: hourlyAt(7)},
		},
		{
			Name:     "sync-projects",
			Desc:     "Project extraction every 2h",
			Command:  each("sync --max 2"),
			LogFile:  filepath.Join(logDir, "scribe-sync.log"),
			Schedule: schedSpec{Calendar: everyNHoursAt(2, 23)},
		},
		{
			Name:    "sync-sessions",
			Desc:    "Session mining at 3:00, 12:00, 18:00",
			Command: each("sync --sessions --sessions-max 3 --skip-large"),
			LogFile: filepath.Join(logDir, "scribe-sync.log"),
			Schedule: schedSpec{Calendar: []calTime{
				{Hour: 3, Minute: 0, Weekday: -1},
				{Hour: 12, Minute: 0, Weekday: -1},
				{Hour: 18, Minute: 0, Weekday: -1},
			}},
		},
		{
			Name:     "lint",
			Desc:     "Daily structural lint at 12:30pm",
			Command:  each("lint"),
			LogFile:  filepath.Join(logDir, "scribe-lint.log"),
			Schedule: schedSpec{Calendar: []calTime{{Hour: 12, Minute: 30, Weekday: -1}}},
		},
		{
			Name:     "dream",
			Desc:     "Weekly Dream cycle (Sun 2am)",
			Command:  each("dream"),
			LogFile:  filepath.Join(logDir, "scribe-dream.log"),
			Schedule: schedSpec{Calendar: []calTime{{Hour: 2, Minute: 0, Weekday: 0}}},
		},
		{
			Name:     "dream-hot",
			Desc:     "Daily hot-domain mini consolidation (self-gating)",
			Command:  each("dream --hot"),
			LogFile:  filepath.Join(logDir, "scribe-dream-hot.log"),
			Schedule: schedSpec{Calendar: []calTime{{Hour: 3, Minute: 10, Weekday: -1}}},
		},
		{
			Name:     "lint-fix",
			Desc:     "Weekly frontmatter auto-repair (Sat 1am — before dream)",
			Command:  each("lint --fix"),
			LogFile:  filepath.Join(logDir, "scribe-lint-fix.log"),
			Schedule: schedSpec{Calendar: []calTime{{Hour: 1, Minute: 0, Weekday: 6}}},
		},
		{
			Name:     "lint-duplicates",
			Desc:     "Weekly content-duplicate scan → wiki/_duplicates.md (Sat 1:15am — after lint-fix)",
			Command:  each("lint --duplicates"),
			LogFile:  filepath.Join(logDir, "scribe-lint-duplicates.log"),
			Schedule: schedSpec{Calendar: []calTime{{Hour: 1, Minute: 15, Weekday: 6}}},
		},
		{
			Name:     "capture-imessage",
			Desc:     "iMessage capture every 4h",
			Command:  each("capture --fetch"),
			LogFile:  filepath.Join(logDir, "scribe-imessage.log"),
			Schedule: schedSpec{Calendar: everyNHoursAt(4, 43)},
		},
		{
			Name:     "ingest-drain",
			Desc:     "Drain queued URLs into raw/articles/ every 30min",
			Command:  each("ingest drain"),
			LogFile:  filepath.Join(logDir, "scribe-ingest.log"),
			Schedule: schedSpec{Calendar: everyNMinutes(30)},
		},
		{
			Name:     "capture-refetch",
			Desc:     "Retry stub links daily at 06:30 (parks failures in _unfetched-links.md)",
			Command:  each("capture --refetch"),
			LogFile:  filepath.Join(logDir, "scribe-capture-refetch.log"),
			Schedule: schedSpec{Calendar: []calTime{{Hour: 6, Minute: 30, Weekday: -1}}},
		},
		{
			Name:     "lint-resolve",
			Desc:     "Weekly conflict-resolution pass (Sat 1:30am — after lint-fix, before dream)",
			Command:  each("lint --resolve"),
			LogFile:  filepath.Join(logDir, "scribe-lint-resolve.log"),
			Schedule: schedSpec{Calendar: []calTime{{Hour: 1, Minute: 30, Weekday: 6}}},
		},
		{
			Name:     "lint-identities",
			Desc:     "Weekly identity-clustering pass (Sat 1:45am)",
			Command:  each("lint --identities"),
			LogFile:  filepath.Join(logDir, "scribe-lint-identities.log"),
			Schedule: schedSpec{Calendar: []calTime{{Hour: 1, Minute: 45, Weekday: 6}}},
		},
		{
			Name:     "apply-identities",
			Desc:     "Auto-apply high-confidence identity aliases (Sat 1:55am — after lint-identities)",
			Command:  each("lint --apply-identities"),
			LogFile:  filepath.Join(logDir, "scribe-apply-identities.log"),
			Schedule: schedSpec{Calendar: []calTime{{Hour: 1, Minute: 55, Weekday: 6}}},
		},
		{
			Name:      "watch",
			Desc:      "fsnotify watcher for ccrider DB (Codex + Claude near-real-time)",
			Command:   binary + " watch",
			LogFile:   filepath.Join(logDir, "scribe-watch.log"),
			KeepAlive: true,
		},
	}
}

func hourlyAt(minute int) []calTime {
	out := make([]calTime, 24)
	for h := range 24 {
		out[h] = calTime{Hour: h, Minute: minute, Weekday: -1}
	}
	return out
}

func everyNHoursAt(n, minute int) []calTime {
	var out []calTime
	for h := 0; h < 24; h += n {
		out = append(out, calTime{Hour: h, Minute: minute, Weekday: -1})
	}
	return out
}

// everyNMinutes returns calendar entries at every Nth minute across the day.
// launchd StartCalendarInterval matches any entry, so a 30-minute cadence is
// 48 entries (one per half-hour slot).
func everyNMinutes(n int) []calTime {
	var out []calTime
	for h := range 24 {
		for m := 0; m < 60; m += n {
			out = append(out, calTime{Hour: h, Minute: m, Weekday: -1})
		}
	}
	return out
}

func plistLabel(name string) string { return "com.scribe." + name }

func plistPath(name string) string {
	return filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", plistLabel(name)+".plist")
}

// plistKBRoot extracts the KB root from a rendered scribe plist: every
// job command starts with `cd "<root>" && `, and xmlEscape leaves
// double quotes intact, so the root is recoverable verbatim.
func plistKBRoot(plist string) string {
	_, after, ok := strings.Cut(plist, `cd "`)
	if !ok {
		return ""
	}
	root, _, ok := strings.Cut(after, `"`)
	if !ok {
		return ""
	}
	return root
}

// otherKBServedByAgents returns the KB root that this machine's existing
// com.scribe.* LaunchAgents serve when it differs from root ("" when no
// agents exist, they already serve root, or they're the KB-agnostic
// registry agents with no embedded cd). Only legacy single-KB plists embed
// a `cd "<root>"`, so this now reports a pre-#26 install awaiting migration.
func otherKBServedByAgents(root string) string {
	for _, job := range scribeJobs("scribe") {
		data, err := os.ReadFile(plistPath(job.Name))
		if err != nil {
			continue
		}
		if other := plistKBRoot(string(data)); other != "" && !samePath(other, root) {
			return other
		}
	}
	return ""
}

// renderPlist generates launchd plist XML for a cron job.
func renderPlist(job cronJob) string {
	var sb strings.Builder
	fmt.Fprintln(&sb, `<?xml version="1.0" encoding="UTF-8"?>`)
	fmt.Fprintln(&sb, `<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`)
	fmt.Fprintln(&sb, `<plist version="1.0">`)
	fmt.Fprintln(&sb, `<dict>`)
	fmt.Fprintf(&sb, "  <key>Label</key><string>%s</string>\n", plistLabel(job.Name))
	fmt.Fprintln(&sb, `  <key>ProgramArguments</key>`)
	fmt.Fprintln(&sb, `  <array>`)
	fmt.Fprintln(&sb, `    <string>/bin/zsh</string>`)
	fmt.Fprintln(&sb, `    <string>-lc</string>`)
	fmt.Fprintf(&sb, "    <string>%s</string>\n", xmlEscape(job.Command))
	fmt.Fprintln(&sb, `  </array>`)

	if job.KeepAlive {
		fmt.Fprintln(&sb, `  <key>KeepAlive</key><true/>`)
		fmt.Fprintln(&sb, `  <key>RunAtLoad</key><true/>`)
		// Small throttle so a crash loop doesn't spin at 100% CPU.
		fmt.Fprintln(&sb, `  <key>ThrottleInterval</key><integer>30</integer>`)
	} else {
		if len(job.Schedule.Calendar) == 1 {
			fmt.Fprintln(&sb, `  <key>StartCalendarInterval</key>`)
			fmt.Fprintln(&sb, `  <dict>`)
			writeCalEntries(&sb, job.Schedule.Calendar[0], "    ")
			fmt.Fprintln(&sb, `  </dict>`)
		} else if len(job.Schedule.Calendar) > 1 {
			fmt.Fprintln(&sb, `  <key>StartCalendarInterval</key>`)
			fmt.Fprintln(&sb, `  <array>`)
			for _, ct := range job.Schedule.Calendar {
				fmt.Fprintln(&sb, `    <dict>`)
				writeCalEntries(&sb, ct, "      ")
				fmt.Fprintln(&sb, `    </dict>`)
			}
			fmt.Fprintln(&sb, `  </array>`)
		}
		fmt.Fprintln(&sb, `  <key>RunAtLoad</key><false/>`)
	}

	fmt.Fprintf(&sb, "  <key>StandardOutPath</key><string>%s</string>\n", xmlEscape(job.LogFile))
	fmt.Fprintf(&sb, "  <key>StandardErrorPath</key><string>%s</string>\n", xmlEscape(job.LogFile))
	fmt.Fprintln(&sb, `</dict>`)
	fmt.Fprintln(&sb, `</plist>`)
	return sb.String()
}

func writeCalEntries(sb *strings.Builder, ct calTime, indent string) {
	if ct.Hour >= 0 {
		fmt.Fprintf(sb, "%s<key>Hour</key><integer>%d</integer>\n", indent, ct.Hour)
	}
	if ct.Minute >= 0 {
		fmt.Fprintf(sb, "%s<key>Minute</key><integer>%d</integer>\n", indent, ct.Minute)
	}
	if ct.Weekday >= 0 {
		fmt.Fprintf(sb, "%s<key>Weekday</key><integer>%d</integer>\n", indent, ct.Weekday)
	}
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// ---- content stamping (issue #54) ----
//
// `cron install` embeds a `<!-- scribe:digest:sha256:<hex> -->` comment on
// its own line right after the XML declaration of every plist it writes.
// The digest covers the WHOLE plist with that one line normalized to a
// fixed placeholder, so verifying a stamp never has to hash itself, and
// "does this file match what the current binary would generate right now"
// reduces to a single byte-for-byte comparison after both sides go through
// the same normalization (see plistStampState). XML comments in the prolog
// (between the `<?xml ...?>` declaration and the root element) are
// standard XML — Apple's own DOCTYPE line lives in exactly that position —
// so this needed no live launchctl load to trust; verified here by string
// inspection only, per the hard rule against touching real LaunchAgents.
const plistDigestPlaceholder = "<!-- scribe:digest: -->"

// plistDigestLineRe matches the stamp comment scribe writes, capturing
// whatever value follows `sha256:` — even a corrupted one — so a
// hand-edited digest is caught as a mismatch instead of silently falling
// through to "no stamp at all".
var plistDigestLineRe = regexp.MustCompile(`(?m)^<!-- scribe:digest:sha256:(.*) -->\n?`)

// extractPlistDigest returns the hex value embedded in content's stamp
// comment, or ok=false when content has no scribe digest line at all.
func extractPlistDigest(content string) (digest string, ok bool) {
	m := plistDigestLineRe.FindStringSubmatch(content)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// normalizePlistDigest returns content with its digest comment line (if
// any) replaced by the fixed placeholder, or the placeholder inserted at
// the same position stampPlist uses — right after the XML declaration —
// when content has no stamp yet. A stamped plist and a freshly rendered
// (unstamped) one normalize to identical bytes iff they are otherwise
// identical; that equality is the whole basis for stale/ok detection.
func normalizePlistDigest(content string) string {
	if plistDigestLineRe.MatchString(content) {
		return plistDigestLineRe.ReplaceAllString(content, plistDigestPlaceholder+"\n")
	}
	return insertAfterFirstLine(content, plistDigestPlaceholder+"\n")
}

// insertAfterFirstLine inserts line right after content's first line (the
// `<?xml version="1.0" ...?>` declaration for a rendered plist).
func insertAfterFirstLine(content, line string) string {
	idx := strings.Index(content, "\n")
	if idx < 0 {
		return content + "\n" + line
	}
	return content[:idx+1] + line + content[idx+1:]
}

// stampPlist takes a freshly rendered, unstamped plist (renderPlist's
// output) and returns it with a digest comment inserted right after the
// XML declaration. The digest is sha256 of the plist with that same line
// normalized to plistDigestPlaceholder — see normalizePlistDigest.
func stampPlist(content string) string {
	normalized := normalizePlistDigest(content)
	sum := sha256.Sum256([]byte(normalized))
	digestLine := fmt.Sprintf("<!-- scribe:digest:sha256:%x -->\n", sum)
	return strings.Replace(normalized, plistDigestPlaceholder+"\n", digestLine, 1)
}

// plistState classifies an installed plist against the content the
// current binary would generate for the same job right now.
type plistState int

const (
	okState         plistState = iota // scribe-stamped, content matches expected
	staleState                        // scribe-stamped, content differs — safe to rewrite
	handEditedState                   // stamp missing or doesn't match its own content — never auto-touched
)

// plistStampState classifies installed (a plist read off disk) against
// expected (the UNSTAMPED plist renderPlist would generate right now for
// the same job — current binary path, log paths, schedule). "authored" —
// installed's embedded digest actually matches installed's own content —
// is what separates a legitimately stale scribe-written plist (safe to
// rewrite without --force) from a hand-edited or third-party one (never
// auto-touched). Missing files are handled by the caller: this only
// classifies content that exists.
func plistStampState(installed, expected string) plistState {
	digest, ok := extractPlistDigest(installed)
	if !ok {
		return handEditedState
	}
	normalizedInstalled := normalizePlistDigest(installed)
	sum := sha256.Sum256([]byte(normalizedInstalled))
	if hex.EncodeToString(sum[:]) != digest {
		return handEditedState
	}
	if normalizedInstalled != normalizePlistDigest(expected) {
		return staleState
	}
	return okState
}

// installAction is what `cron install` should do for one job's existing
// on-disk plist (or lack of one). Factored out from CronInstallCmd.Run so
// the decision — which never touches disk, launchctl, or the KB-binding
// writeGlobalState chokepoint — is directly table-testable on its own,
// independent of the throwaway-KB write refusal that makes exercising the
// actual write path difficult from t.TempDir()-rooted tests.
type installAction int

const (
	actionWrite          installAction = iota // no existing file, or --force: (over)write unconditionally
	actionRefresh                             // stamped + un-edited but stale: rewrite without --force
	actionSkipUpToDate                        // stamped + un-edited + matches expected: nothing to do
	actionSkipHandEdited                      // stamp missing or invalid: needs --force
)

// cronInstallDecision decides what to do for one job given existing (the
// current file content on disk, meaningful only when exists is true), raw
// (this job's current UNSTAMPED renderPlist output — "expected"), and
// whether --force was passed.
func cronInstallDecision(existing string, exists bool, raw string, force bool) installAction {
	if !exists || force {
		return actionWrite
	}
	switch plistStampState(existing, raw) {
	case okState:
		return actionSkipUpToDate
	case staleState:
		return actionRefresh
	default: // handEditedState
		return actionSkipHandEdited
	}
}

// anyScribeAgentInstalled reports whether at least one com.scribe.*.plist
// exists in the LaunchAgents dir — the signal `cron install --if-installed`
// uses to distinguish "user already opted into cron on this machine"
// (proceed and self-heal) from "fresh install, never opted in" (silent
// no-op). Pure file read: never launchctl, never mutation.
func anyScribeAgentInstalled() bool {
	agentsDir := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "com.scribe.") && strings.HasSuffix(e.Name(), ".plist") {
			return true
		}
	}
	return false
}

// guiDomain returns launchctl domain target for the current user.
func guiDomain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

// runLaunchctl is a package variable (pointing at realRunLaunchctl) purely
// so cron install/uninstall tests can swap in a stub instead of shelling
// out to a real launchctl — which would load/unload actual LaunchAgents on
// whatever machine runs `go test`. Production code never reassigns it;
// same pattern as runClaude in claude.go.
var runLaunchctl = realRunLaunchctl

func realRunLaunchctl(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput() //nolint:noctx // launchctl IPC, fast
	return string(out), err
}

// renderCrontab emits one crontab(5) line per scheduled invocation for `job`.
// Returns nil if the job has no Calendar slots (e.g. a KeepAlive watcher) —
// those should run under systemd/supervisor, not cron.
//
// Each Calendar slot with fully-specified hour+minute produces `M H * * *`;
// a weekday-pinned slot (used for the weekly dream cycle) produces
// `M H * * W`. We deliberately do not collapse the 48 "every 30 minutes"
// entries into `*/30 * * * *` — the LaunchAgent form is the source of truth
// and "paste these exact lines" is easier to trust than "I promise this is
// equivalent".
func renderCrontab(job cronJob) []string {
	if len(job.Schedule.Calendar) == 0 {
		return nil
	}
	// Try to collapse "every N minutes at every hour" into one */N line.
	if ok, step := everyMinutesStep(job.Schedule.Calendar); ok {
		return []string{fmt.Sprintf("*/%d * * * * %s", step, job.Command)}
	}
	// Try "every N hours at a fixed minute" — collapse into `M */N * * *`.
	if ok, step, minute := everyHoursStep(job.Schedule.Calendar); ok {
		return []string{fmt.Sprintf("%d */%d * * * %s", minute, step, job.Command)}
	}
	lines := make([]string, 0, len(job.Schedule.Calendar))
	for _, ct := range job.Schedule.Calendar {
		minute := "*"
		hr := "*"
		wd := "*"
		if ct.Minute >= 0 {
			minute = strconv.Itoa(ct.Minute)
		}
		if ct.Hour >= 0 {
			hr = strconv.Itoa(ct.Hour)
		}
		if ct.Weekday >= 0 {
			wd = strconv.Itoa(ct.Weekday)
		}
		lines = append(lines, fmt.Sprintf("%s %s * * %s %s", minute, hr, wd, job.Command))
	}
	sort.Strings(lines)
	return lines
}

// everyMinutesStep detects the "every N minutes across all 24 hours" pattern
// produced by everyNMinutes. Returns the step (e.g. 30) and true only when
// the calendar is exactly 24 × (60/N) entries with a regular stride.
func everyMinutesStep(cal []calTime) (bool, int) {
	if len(cal) < 24 || len(cal)%24 != 0 {
		return false, 0
	}
	step := 60 / (len(cal) / 24)
	if step < 1 || step > 30 || 60%step != 0 {
		return false, 0
	}
	// Confirm pattern: for each hour 0..23 we should see minutes 0,step,2*step,...
	want := map[int]map[int]bool{}
	for h := range 24 {
		want[h] = map[int]bool{}
		for m := 0; m < 60; m += step {
			want[h][m] = true
		}
	}
	for _, c := range cal {
		if c.Hour < 0 || c.Minute < 0 || c.Weekday >= 0 {
			return false, 0
		}
		if !want[c.Hour][c.Minute] {
			return false, 0
		}
		delete(want[c.Hour], c.Minute)
	}
	for _, set := range want {
		if len(set) != 0 {
			return false, 0
		}
	}
	return true, step
}

// everyHoursStep detects the "every N hours at a fixed minute" pattern (e.g.
// 4,8,12,16,20,0 all at :43). Returns step, minute, true on match.
func everyHoursStep(cal []calTime) (bool, int, int) {
	if len(cal) < 2 {
		return false, 0, 0
	}
	minute := cal[0].Minute
	var hours []int
	for _, c := range cal {
		if c.Weekday >= 0 || c.Minute != minute || c.Hour < 0 {
			return false, 0, 0
		}
		hours = append(hours, c.Hour)
	}
	sort.Ints(hours)
	step := hours[1] - hours[0]
	if step <= 0 || 24%step != 0 || len(hours) != 24/step {
		return false, 0, 0
	}
	for i := 1; i < len(hours); i++ {
		if hours[i]-hours[i-1] != step {
			return false, 0, 0
		}
	}
	return true, step, minute
}

// printLinuxCronInstructions walks the scribe job list and emits a
// ready-to-paste crontab block plus a note about the long-running watcher
// job, which needs a supervisor (systemd user service, tmux, etc.) rather
// than cron.
func printLinuxCronInstructions(jobs []cronJob, _ bool) error {
	fmt.Println("Linux cron install (scribe does not ship LaunchAgents on non-Darwin)")
	fmt.Println()
	fmt.Println("1. Add these lines to your crontab (`crontab -e`):")
	fmt.Println()
	fmt.Println("# ---- scribe ----")
	fmt.Println("SHELL=/bin/bash")
	fmt.Println("PATH=" + os.Getenv("PATH"))
	fmt.Println()
	for _, job := range jobs {
		lines := renderCrontab(job)
		if len(lines) == 0 {
			continue
		}
		fmt.Printf("# %s\n", job.Desc)
		for _, ln := range lines {
			fmt.Println(ln)
		}
		fmt.Println()
	}
	fmt.Println("# ---- end scribe ----")
	fmt.Println()

	fmt.Println("2. Long-running jobs (not cron-friendly):")
	for _, job := range jobs {
		if !job.KeepAlive {
			continue
		}
		fmt.Printf("   - %s: %s\n", job.Name, job.Desc)
		fmt.Printf("     command: %s\n", job.Command)
		fmt.Println("     run under systemd, supervisord, or a persistent tmux session.")
	}

	fmt.Println()
	fmt.Println("3. Verify with: scribe doctor")
	fmt.Println("   (the freshness + errors sections use output/runs/*.jsonl and don't")
	fmt.Println("    depend on how the jobs are scheduled.)")
	return nil
}

// probeLaunchAgent classifies a LaunchAgent by its on-disk plist and whether
// launchctl can describe it. Returns "missing" (no plist), "present" (plist
// exists but not loaded into the domain), or "loaded" (launchctl print found
// it). Used by both `cron status` and `doctor`.
func probeLaunchAgent(domain, label, plistPath string) string {
	state := "missing"
	if _, err := os.Stat(plistPath); err == nil {
		state = "present"
	}
	out, _ := runLaunchctl("print", domain+"/"+label)
	if len(out) > 0 && !strings.Contains(out, "Could not find") && !strings.Contains(out, "could not find") {
		state = "loaded"
	}
	return state
}

// resolveScribeBinary finds the installed scribe binary.
//
// Prefer ~/.local/bin/scribe (the canonical deploy target of `make install`)
// over a PATH lookup. Relying on exec.LookPath means whichever directory comes
// first in PATH wins — and a stray old build in ~/bin/scribe would silently
// shadow the fresh one from `make install`, baking the wrong path into the
// installed LaunchAgent plists. If the canonical binary isn't on disk yet
// (e.g. first install from a freshly cloned repo before `make install`), fall
// back to PATH so install still works.
func resolveScribeBinary() string {
	canonical := filepath.Join(os.Getenv("HOME"), ".local", "bin", "scribe")
	if _, err := os.Stat(canonical); err == nil {
		return canonical
	}
	if p, err := exec.LookPath("scribe"); err == nil {
		return p
	}
	return canonical
}

// ---- install ----

type CronInstallCmd struct {
	DryRun      bool `help:"Print generated plists without writing or loading them."`
	Force       bool `help:"Overwrite existing plists (including hand-edited/unstamped ones)."`
	IfInstalled bool `help:"No-op unless scribe cron is already installed on this machine (brew post_install self-heal on upgrade)." name:"if-installed"`
}

func (c *CronInstallCmd) Run() error {
	// --if-installed must be checked before kbDir(): it exists so brew's
	// post_install (running from an arbitrary directory, not a KB
	// checkout — see .goreleaser.yml) can call this unconditionally on
	// every install AND upgrade without erroring on a fresh machine that
	// never opted into cron. Pure file read, no KB context needed.
	if c.IfInstalled && !anyScribeAgentInstalled() {
		fmt.Println("cron not installed on this machine — nothing to refresh (use 'scribe cron install' to opt in)")
		return nil
	}

	root, err := kbDir()
	if err != nil {
		return err
	}
	binary := resolveScribeBinary()
	jobs := scribeJobs(binary)

	// On non-macOS systems, print crontab lines the user can paste. launchd
	// plists would be meaningless and systemd user-timers are involved enough
	// that we prefer explicit manual install over writing unit files the user
	// may not expect.
	if runtime.GOOS != "darwin" {
		return printLinuxCronInstructions(jobs, c.DryRun)
	}

	if c.DryRun {
		for _, job := range jobs {
			fmt.Printf("=== %s (%s) ===\n", plistLabel(job.Name), job.Desc)
			fmt.Println(stampPlist(renderPlist(job)))
		}
		return nil
	}

	// Agents are KB-agnostic now (issue #26): they run `scribe each`,
	// which iterates the registry. So installing just means "register this
	// KB and ensure the machine-level agents exist" — no clobber is
	// possible. (Throwaway temp KBs are skipped here; the writeGlobalState
	// chokepoint below refuses to bind the machine to them.)
	if !isThrowawayPath(root) {
		if added, err := registerKB(root); err != nil {
			return err
		} else if added {
			fmt.Printf("registered %s in the KB registry (%s)\n", root, userConfigPath())
		}
		// Legacy single-KB plists (pre-#26) embed `cd "<otherKB>"` and serve
		// only that KB. Re-installing the KB-agnostic agents overwrites them
		// — but first register that KB too, so migration doesn't silently
		// drop it from the schedule.
		if other := otherKBServedByAgents(root); other != "" {
			if added, err := registerKB(other); err == nil && added {
				fmt.Printf("migrating legacy agents: also registered %s\n", other)
			}
			fmt.Printf("replacing legacy single-KB agents (served %s) with KB-agnostic ones\n", other)
		}
	}

	domain := guiDomain()
	for _, job := range jobs {
		raw := renderPlist(job)
		path := plistPath(job.Name)
		label := plistLabel(job.Name)

		existing, statErr := os.ReadFile(path)
		action := cronInstallDecision(string(existing), statErr == nil, raw, c.Force)

		switch action {
		case actionSkipUpToDate:
			fmt.Printf("skip %s (up to date)\n", label)
			continue
		case actionSkipHandEdited:
			fmt.Printf("skip %s (unstamped or hand-edited; use --force to overwrite)\n", label)
			continue
		}

		plist := stampPlist(raw)

		// LaunchAgent plists are machine-global state binding this
		// machine's schedule to a KB root — route the write through the
		// shared chokepoint (init.go) so it inherits the throwaway-path
		// refusal. cron install has no bind override on purpose: a /tmp
		// KB must never own the schedule (it vanishes on reboot and the
		// jobs would burn tokens against a dead path).
		if err := writeGlobalState(root, false, path, []byte(plist), 0o644); err != nil {
			return err
		}
		if action == actionRefresh {
			fmt.Printf("refresh %s (content changed)\n", label)
		} else {
			fmt.Printf("wrote %s\n", path)
		}

		// Boot out first (ignore errors) then bootstrap.
		_, _ = runLaunchctl("bootout", domain+"/"+label)
		out, err := runLaunchctl("bootstrap", domain, path)
		if err != nil {
			fmt.Printf("  bootstrap failed: %s\n  %s\n", err, strings.TrimSpace(out))
		} else {
			fmt.Printf("  loaded into %s\n", domain)
		}
	}

	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Verify with: scribe cron status")
	fmt.Println("  2. Remove old cron entries: crontab -e (delete scribe lines)")
	fmt.Println("  3. Monitor logs in /tmp/scribe-*.log")
	return nil
}

// ---- status ----

type CronStatusCmd struct{}

func (c *CronStatusCmd) Run() error {
	// KB-agnostic: agents serve the whole registry, so status no longer
	// needs a KB context. Show the registry first, then per-agent state.
	if kbs := registeredKBs(); len(kbs) > 0 {
		fmt.Println("Registered KBs (scribe each iterates these):")
		for _, kb := range kbs {
			fmt.Printf("  - %s\n", kb)
		}
		fmt.Println()
	} else {
		fmt.Printf("No registered KBs — add one with `scribe kb add <path>` (or set kb_dir in %s)\n\n", userConfigPath())
	}

	jobs := scribeJobs(resolveScribeBinary())
	domain := guiDomain()

	fmt.Printf("%-28s %-10s %s\n", "LABEL", "STATE", "PLIST")
	fmt.Println(strings.Repeat("-", 80))
	for _, job := range jobs {
		label := plistLabel(job.Name)
		path := plistPath(job.Name)
		state := probeLaunchAgent(domain, label, path)
		fmt.Printf("%-28s %-10s %s\n", label, state, path)
	}
	return nil
}

// ---- uninstall ----

type CronUninstallCmd struct {
	KeepFiles bool `help:"Unload but keep plist files on disk."`
}

func (c *CronUninstallCmd) Run() error {
	// KB-agnostic: removing the machine-level agents needs no KB context.
	jobs := scribeJobs(resolveScribeBinary())
	domain := guiDomain()

	for _, job := range jobs {
		label := plistLabel(job.Name)
		path := plistPath(job.Name)
		_, _ = runLaunchctl("bootout", domain+"/"+label)
		if c.KeepFiles {
			fmt.Printf("unloaded %s (plist kept)\n", label)
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fmt.Printf("remove failed %s: %s\n", path, err)
			continue
		}
		fmt.Printf("removed %s\n", label)
	}
	return nil
}
