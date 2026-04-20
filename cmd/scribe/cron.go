package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
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
func scribeJobs(root, binary string) []cronJob {
	cd := fmt.Sprintf("cd %q && ", root)
	logDir := "/tmp"
	return []cronJob{
		{
			Name:     "auto-commit",
			Desc:     "Hourly KB auto-commit",
			Command:  cd + binary + " commit",
			LogFile:  filepath.Join(logDir, "scribe-autocommit.log"),
			Schedule: schedSpec{Calendar: hourlyAt(7)},
		},
		{
			Name:     "sync-projects",
			Desc:     "Project extraction every 2h",
			Command:  cd + binary + " sync --max 2",
			LogFile:  filepath.Join(logDir, "scribe-sync.log"),
			Schedule: schedSpec{Calendar: everyNHoursAt(2, 23)},
		},
		{
			Name:    "sync-sessions",
			Desc:    "Session mining at 3:00, 12:00, 18:00",
			Command: cd + binary + " sync --sessions --sessions-max 3 --skip-large",
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
			Command:  cd + binary + " lint",
			LogFile:  filepath.Join(logDir, "scribe-lint.log"),
			Schedule: schedSpec{Calendar: []calTime{{Hour: 12, Minute: 30, Weekday: -1}}},
		},
		{
			Name:     "dream",
			Desc:     "Weekly Dream cycle (Sun 2am)",
			Command:  cd + binary + " dream",
			LogFile:  filepath.Join(logDir, "scribe-dream.log"),
			Schedule: schedSpec{Calendar: []calTime{{Hour: 2, Minute: 0, Weekday: 0}}},
		},
		{
			Name:     "lint-fix",
			Desc:     "Weekly frontmatter auto-repair (Sat 1am — before dream)",
			Command:  cd + binary + " lint --fix",
			LogFile:  filepath.Join(logDir, "scribe-lint-fix.log"),
			Schedule: schedSpec{Calendar: []calTime{{Hour: 1, Minute: 0, Weekday: 6}}},
		},
		{
			Name:     "capture-imessage",
			Desc:     "iMessage capture every 4h",
			Command:  cd + binary + " capture --fetch",
			LogFile:  filepath.Join(logDir, "scribe-imessage.log"),
			Schedule: schedSpec{Calendar: everyNHoursAt(4, 43)},
		},
		{
			Name:     "ingest-drain",
			Desc:     "Drain queued URLs into raw/articles/ every 30min",
			Command:  cd + binary + " ingest drain",
			LogFile:  filepath.Join(logDir, "scribe-ingest.log"),
			Schedule: schedSpec{Calendar: everyNMinutes(30)},
		},
		{
			Name:     "capture-refetch",
			Desc:     "Retry stub links daily at 06:30 (parks failures in _unfetched-links.md)",
			Command:  cd + binary + " capture --refetch",
			LogFile:  filepath.Join(logDir, "scribe-capture-refetch.log"),
			Schedule: schedSpec{Calendar: []calTime{{Hour: 6, Minute: 30, Weekday: -1}}},
		},
		{
			Name:     "lint-resolve",
			Desc:     "Weekly conflict-resolution pass (Sat 1:30am — after lint-fix, before dream)",
			Command:  cd + binary + " lint --resolve",
			LogFile:  filepath.Join(logDir, "scribe-lint-resolve.log"),
			Schedule: schedSpec{Calendar: []calTime{{Hour: 1, Minute: 30, Weekday: 6}}},
		},
		{
			Name:     "lint-identities",
			Desc:     "Weekly identity-clustering pass (Sat 1:45am)",
			Command:  cd + binary + " lint --identities",
			LogFile:  filepath.Join(logDir, "scribe-lint-identities.log"),
			Schedule: schedSpec{Calendar: []calTime{{Hour: 1, Minute: 45, Weekday: 6}}},
		},
		{
			Name:     "apply-identities",
			Desc:     "Auto-apply high-confidence identity aliases (Sat 1:55am — after lint-identities)",
			Command:  cd + binary + " lint --apply-identities",
			LogFile:  filepath.Join(logDir, "scribe-apply-identities.log"),
			Schedule: schedSpec{Calendar: []calTime{{Hour: 1, Minute: 55, Weekday: 6}}},
		},
		{
			Name:      "watch",
			Desc:      "fsnotify watcher for ccrider DB (Codex + Claude near-real-time)",
			Command:   cd + binary + " watch",
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

// guiDomain returns launchctl domain target for the current user.
func guiDomain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

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
			minute = fmt.Sprintf("%d", ct.Minute)
		}
		if ct.Hour >= 0 {
			hr = fmt.Sprintf("%d", ct.Hour)
		}
		if ct.Weekday >= 0 {
			wd = fmt.Sprintf("%d", ct.Weekday)
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
	out, _ := exec.Command("launchctl", "print", domain+"/"+label).CombinedOutput() //nolint:noctx // fast status probe, no cancellation needed
	s := string(out)
	if len(s) > 0 && !strings.Contains(s, "Could not find") && !strings.Contains(s, "could not find") {
		state = "loaded"
	}
	return state
}

// resolveScribeBinary finds the installed scribe binary.
//
// Prefer ~/.local/bin/scribe (the canonical install target from the Justfile)
// over a PATH lookup. Relying on exec.LookPath means whichever directory comes
// first in PATH wins — and a stray old build in ~/bin/scribe would silently
// shadow the fresh one from `just build`, baking the wrong path into the
// installed LaunchAgent plists. If the canonical binary isn't on disk yet
// (e.g. first install from a freshly cloned repo before `just build`), fall
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
	DryRun bool `help:"Print generated plists without writing or loading them."`
	Force  bool `help:"Overwrite existing plists."`
}

func (c *CronInstallCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	binary := resolveScribeBinary()
	jobs := scribeJobs(root, binary)

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
			fmt.Println(renderPlist(job))
		}
		return nil
	}

	domain := guiDomain()
	for _, job := range jobs {
		plist := renderPlist(job)
		path := plistPath(job.Name)
		label := plistLabel(job.Name)

		if _, err := os.Stat(path); err == nil && !c.Force {
			fmt.Printf("skip %s (exists; use --force to overwrite)\n", label)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("mkdir LaunchAgents: %w", err)
		}
		if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		fmt.Printf("wrote %s\n", path)

		// Boot out first (ignore errors) then bootstrap.
		_ = exec.Command("launchctl", "bootout", domain+"/"+label).Run()                  //nolint:noctx // launchctl IPC, fast
		out, err := exec.Command("launchctl", "bootstrap", domain, path).CombinedOutput() //nolint:noctx // launchctl IPC, fast
		if err != nil {
			fmt.Printf("  bootstrap failed: %s\n  %s\n", err, strings.TrimSpace(string(out)))
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
	root, err := kbDir()
	if err != nil {
		return err
	}
	jobs := scribeJobs(root, resolveScribeBinary())
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
	root, err := kbDir()
	if err != nil {
		return err
	}
	jobs := scribeJobs(root, resolveScribeBinary())
	domain := guiDomain()

	for _, job := range jobs {
		label := plistLabel(job.Name)
		path := plistPath(job.Name)
		_ = exec.Command("launchctl", "bootout", domain+"/"+label).Run() //nolint:noctx // launchctl IPC, fast
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
