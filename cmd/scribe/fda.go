package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// FDACmd handles the macOS Full Disk Access setup flow for `scribe capture`.
// macOS's TCC (Privacy & Security) subsystem does not expose a way for a
// process to grant itself FDA — that would defeat the whole privacy boundary.
// The best we can do from a CLI is:
//
//  1. Probe whether chat.db is already readable (FDA is effectively granted).
//  2. Open the exact System Settings pane the user needs.
//  3. Print the binary path(s) to drag into the list, in a form that
//     survives copy/paste.
//  4. Verify on follow-up invocation so the user knows it stuck.
//
// This command is safe to run repeatedly; it is purely advisory.
type FDACmd struct {
	Verify bool `help:"Only check current FDA state; do not open System Settings." short:"v"`
}

// chatDBPath is the file whose readability determines whether the current
// binary has Full Disk Access. Extracted so tests can point at a tempfile.
var chatDBPath = filepath.Join(os.Getenv("HOME"), "Library", "Messages", "chat.db")

// fdaSystemSettingsURL is the x-apple URL scheme that jumps System Settings
// (macOS 13+) or System Preferences (macOS ≤12) directly to the Full Disk
// Access pane. Both macOS versions honor this URL; `open` does the routing.
const fdaSystemSettingsURL = "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles"

func (c *FDACmd) Run() error {
	if runtime.GOOS != "darwin" {
		fmt.Println("Full Disk Access is macOS-only — not applicable on this platform.")
		return nil
	}
	state := probeFDA()
	// `--verify` is a one-shot for the current binary only. Returning before
	// the multi-binary checks keeps the exit code semantics simple for
	// scripts (exit 0 = this binary has FDA). Trim Targets to just the
	// current inode so the printout doesn't show other binaries as "NOT
	// GRANTED" — they weren't actually checked in this mode.
	if c.Verify {
		state.Targets = filterToCurrent(state.Targets)
		printFDAStatus(state)
		if !state.OK {
			return fmt.Errorf("full disk access not granted for %s", selfBinaryPath())
		}
		return nil
	}
	// For the interactive flow, promote each other-inode target by exec'ing
	// it with `fda --verify`. TCC is per-inode, so the currently-running
	// binary's grant says nothing about the cron binary next door.
	for i, t := range state.Targets {
		if t.Live || t.Path == selfBinaryPath() {
			continue
		}
		if err := exec.Command(t.Path, "fda", "--verify").Run(); err == nil { //nolint:noctx // local scribe self-invocation
			state.Targets[i].Live = true
		}
	}
	state.OK = allTargetsLive(state.Targets)
	printFDAStatus(state)

	if state.OK {
		fmt.Println()
		fmt.Println("All scribe binaries have Full Disk Access. `scribe capture` works in cron and interactively.")
		return nil
	}
	return runFDAFlow(state)
}

// filterToCurrent keeps only the target whose Role == "current". Used by
// `--verify`, which probes only the running binary and should not render
// other inodes as "NOT GRANTED" when they were never tested.
func filterToCurrent(ts []fdaTarget) []fdaTarget {
	for _, t := range ts {
		if t.Role == "current" {
			return []fdaTarget{t}
		}
	}
	return ts
}

// allTargetsLive is true when every listed binary has passed the FDA probe.
// A single ungranted inode means the user still has a broken path.
func allTargetsLive(ts []fdaTarget) bool {
	for _, t := range ts {
		if !t.Live {
			return false
		}
	}
	return len(ts) > 0
}

type fdaState struct {
	OK      bool
	Reason  string
	Targets []fdaTarget
}

// fdaTarget is one on-disk scribe binary the user may need to grant FDA to.
// We enumerate every distinct inode because TCC tracks grants per-binary,
// so a user who granted FDA to the mise-managed copy still sees cron fail
// if the LaunchAgent invokes a different path.
type fdaTarget struct {
	Path string
	Role string // "current" | "cron" | "shadow"
	Live bool   // true once this specific binary has passed a probe
}

// probeFDA opens chat.db and classifies the failure. The Open call is the
// most reliable signal: TCC denials surface as "operation not permitted"
// (EPERM) even though the file exists; that's the FDA-specific pattern we
// match against to avoid false positives on "file missing" and generic I/O.
//
// The probe only speaks for the *currently-running* binary (TCC tracks per
// inode). `OK` therefore means "this invocation can read chat.db"; it does
// not guarantee that a differently-pathed scribe (e.g. the one cron uses)
// has the same grant. fdaTargets() enumerates the others so the caller can
// tell the user which ones still need attention.
func probeFDA() fdaState {
	s := fdaState{Targets: fdaTargets()}
	f, err := os.Open(chatDBPath)
	if err == nil {
		_ = f.Close()
		s.OK = true
		// Mark the currently-running binary as live; other paths stay as
		// "assumed needs grant". There is no way from here to test a
		// different inode's TCC grant without exec'ing that binary.
		for i, t := range s.Targets {
			if t.Role == "current" {
				s.Targets[i].Live = true
			}
		}
		return s
	}
	s.Reason = err.Error()
	return s
}

// fdaTargets returns every distinct scribe binary on disk: the running one,
// the canonical cron path (~/.local/bin/scribe), and any GOBIN shadow.
// Duplicates (same inode) are collapsed.
func fdaTargets() []fdaTarget {
	paths := []struct {
		Role string
		Path string
	}{
		{Role: "current", Path: selfBinaryPath()},
		{Role: "cron", Path: filepath.Join(os.Getenv("HOME"), ".local", "bin", "scribe")},
	}
	if shadow := miseShadowPath(); shadow != "" {
		paths = append(paths, struct {
			Role string
			Path string
		}{Role: "shadow", Path: shadow})
	}

	// Deduplicate by resolved path so symlinks collapse to one target.
	seen := map[string]bool{}
	var out []fdaTarget
	for _, p := range paths {
		if p.Path == "" {
			continue
		}
		if _, err := os.Stat(p.Path); err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(p.Path)
		if err != nil {
			resolved = p.Path
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		out = append(out, fdaTarget{Path: resolved, Role: p.Role})
	}
	return out
}

// selfBinaryPath returns the absolute path of the currently-running scribe
// binary. /proc/self/exe does not exist on macOS, so we fall back to
// os.Executable() which calls _NSGetExecutablePath underneath.
func selfBinaryPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "scribe"
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return exe
	}
	// Resolve symlinks so the path the user pastes into FDA is the real binary
	// TCC tracks (dragging a symlink often fails to register the grant).
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs
	}
	return resolved
}

// miseShadowPath returns the GOBIN-resident copy of the scribe binary if it
// differs from the one that's currently running. Mise (or any GOBIN-setting
// tool) produces a second on-disk scribe; TCC grants are per-inode, so the
// user may need to grant FDA to both copies if they run scribe interactively.
func miseShadowPath() string {
	gobin := strings.TrimSpace(runCmd("", "go", "env", "GOBIN"))
	if gobin == "" {
		return ""
	}
	shadow := filepath.Join(gobin, "scribe")
	if shadow == selfBinaryPath() {
		return ""
	}
	if _, err := os.Stat(shadow); err != nil {
		return ""
	}
	return shadow
}

func printFDAStatus(s fdaState) {
	fmt.Println("Full Disk Access probe:")
	fmt.Printf("  chat.db path: %s\n", chatDBPath)
	fmt.Println("  scribe binaries that need FDA (TCC grants per-inode — add each distinct path):")
	for _, t := range s.Targets {
		marker := " "
		note := ""
		switch t.Role {
		case "current":
			note = "— binary running right now"
		case "cron":
			note = "— invoked by LaunchAgents; MUST be granted for scheduled capture"
		case "shadow":
			note = "— GOBIN copy (from mise); grant if you run scribe interactively"
		}
		if t.Live {
			marker = "✓"
			note = "— GRANTED " + note
		} else {
			note = "— NOT GRANTED " + note
		}
		fmt.Printf("    %s %s  %s\n", marker, t.Path, note)
	}
	if s.OK {
		return
	}
	fmt.Printf("  current-binary status: MISSING — %s\n", s.Reason)
}

// runFDAFlow drives the interactive path: open System Settings, reveal each
// binary in Finder so the user can drag it into the FDA list, then re-probe.
// Launchd-spawned invocations land on `--verify` and skip this branch.
//
// UX note: the "+" / Cmd-Shift-G path-paste flow is unreliable on macOS 14+ —
// it often accepts the Open click but silently does not add a row. Dragging
// the binary from a Finder window is the most dependable method and is
// therefore documented as step 1. Cmd-Shift-G is kept as a fallback.
func runFDAFlow(s fdaState) error {
	fmt.Println()
	fmt.Println("Grant Full Disk Access to every listed scribe binary:")
	fmt.Println()
	fmt.Println("  1. System Settings opens in a moment (Privacy & Security → Full Disk Access).")
	fmt.Println("  2. A Finder window also opens with each scribe binary pre-selected.")
	fmt.Println("     Drag the `scribe` icon directly onto the FDA list — this is the most")
	fmt.Println("     reliable method on macOS 14+ (Sonoma/Sequoia).")
	fmt.Println()
	fmt.Println("     Fallback if drag-and-drop is awkward: click + in the FDA pane, press")
	fmt.Println("     Cmd-Shift-G in the picker, paste one of these paths, Enter, Open:")
	fmt.Println()
	for _, t := range s.Targets {
		if t.Live {
			continue
		}
		fmt.Printf("         %s   (%s)\n", t.Path, t.Role)
	}
	fmt.Println()
	fmt.Println("  3. After the row appears in the FDA list, flip the toggle ON (blue/green).")
	fmt.Println("     Being listed is not enough — the switch must be enabled.")
	fmt.Println("  4. If the row never appears after clicking Open, that confirms the known")
	fmt.Println("     picker bug — close the picker and use drag-and-drop instead.")
	fmt.Println("  5. Return here — the probe retries every 3s for up to 2 minutes.")
	fmt.Println()

	// Best-effort open; if it fails (CI, SSH session) the printed URL still works.
	if err := exec.Command("open", fdaSystemSettingsURL).Start(); err != nil { //nolint:noctx // interactive UI launcher, best-effort
		fmt.Printf("  (could not auto-open System Settings: %v)\n", err)
		fmt.Printf("  Open manually: %s\n\n", fdaSystemSettingsURL)
	}

	// Reveal each ungranted binary in a Finder window (`open -R`) so the user
	// can drag it straight into the FDA list. This sidesteps the Cmd-Shift-G
	// picker entirely — the flakiest part of the grant flow.
	for _, t := range s.Targets {
		if t.Live {
			continue
		}
		if err := exec.Command("open", "-R", t.Path).Start(); err != nil { //nolint:noctx // interactive UI launcher, best-effort
			// Non-fatal: the printed path is still usable manually.
			fmt.Printf("  (could not reveal %s in Finder: %v)\n", t.Path, err)
		}
	}

	// Poll for up to 2 minutes, re-checking every 3 seconds. Each iteration
	// refreshes the live status of each target: the current binary via
	// direct open() and others by exec'ing them. The user can add multiple
	// binaries to FDA in one go and watch each flip to ✓ as it completes.
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		fresh := probeFDA()
		for i, t := range fresh.Targets {
			if t.Live || t.Path == selfBinaryPath() {
				continue
			}
			if err := exec.Command(t.Path, "fda", "--verify").Run(); err == nil { //nolint:noctx // local scribe self-invocation
				fresh.Targets[i].Live = true
			}
		}
		if allTargetsLive(fresh.Targets) {
			fmt.Println()
			fmt.Println("  ✓ All scribe binaries have Full Disk Access.")
			fmt.Println("  Reload the LaunchAgent so launchd picks up the new grant:")
			fmt.Println("      scribe cron uninstall && scribe cron install")
			return nil
		}
	}
	return fmt.Errorf("full disk access still missing after 2 minutes — re-run `scribe fda` when you've completed the steps above")
}
