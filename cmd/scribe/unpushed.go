package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// unpushedStuckAfter is how old the oldest unpushed commit may get before a
// person is told. Long enough that a laptop offline for an afternoon stays
// quiet; short enough that a machine whose pulls abort (SCRIBE-161: 7 days
// unseen) is flagged the same day.
const unpushedStuckAfter = 6 * time.Hour

// unpushed is the local work in branch..upstream at a point in time.
type unpushed struct {
	Count  int
	Oldest time.Time // author date of the oldest unpushed commit
	Now    time.Time
}

func (u unpushed) stuck() bool {
	return u.Count > 0 && u.Now.Sub(u.Oldest) > unpushedStuckAfter
}

func (u unpushed) String() string {
	return fmt.Sprintf("%d commit(s) not pushed, oldest %s old", u.Count, shortDuration(u.Now.Sub(u.Oldest)))
}

// unpushedWork reads branch..upstream for the branch HEAD is on. ok is
// false when that branch has no upstream (a solo KB, a detached HEAD, or
// not a git repo): there is nothing to push to. Mid-rebase HEAD is
// detached, so the branch comes from the rebase state — that is exactly
// when a machine is stuck.
// Author date, not committer date: a rebase rewrites the committer date on
// every retry, which would keep a stuck commit looking young forever.
func unpushedWork(root string, now time.Time) (unpushed, bool) {
	branch := currentBranchRef(root)
	if branch == "" {
		return unpushed{}, false
	}
	upstream := runCmd(root, "git", "for-each-ref", "--format=%(upstream)", branch)
	if upstream == "" {
		return unpushed{}, false
	}
	res := unpushed{Now: now}
	for _, field := range strings.Fields(runCmd(root, "git", "log", "--format=%at", upstream+".."+branch)) {
		secs, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			continue
		}
		at := time.Unix(secs, 0)
		if res.Count == 0 || at.Before(res.Oldest) {
			res.Oldest = at
		}
		res.Count++
	}
	return res, true
}

// currentBranchRef is HEAD's branch (refs/heads/...), or the branch being
// rebased when a rebase stopped. "" when HEAD is detached for another reason.
func currentBranchRef(root string) string {
	if ref := runCmd(root, "git", "symbolic-ref", "-q", "HEAD"); ref != "" {
		return ref
	}
	for _, state := range []string{"rebase-merge/head-name", "rebase-apply/head-name"} {
		path := runCmd(root, "git", "rev-parse", "--git-path", state)
		if path != "" && !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		if b, err := os.ReadFile(path); err == nil {
			if ref := strings.TrimSpace(string(b)); strings.HasPrefix(ref, "refs/heads/") {
				return ref
			}
		}
	}
	return ""
}

// checkUnpushedWork is the doctor row. A FAIL row also lands in
// NEEDS-ATTENTION.md through the daily `doctor --write-attention` job.
// Old commits alone are not enough for FAIL: after a night asleep, doctor
// and sync start together at wake, and the sync may push them a moment
// later. FAIL needs a sync that ran after the work turned stuck and still
// left it unpushed; until then the row is a warn, which the report skips.
func checkUnpushedWork(root string, now time.Time) []check {
	u, ok := unpushedWork(root, now)
	if !ok {
		return nil
	}
	row := check{Section: "state", Name: "unpushed-work", Status: statusOK, Detail: "nothing older than " + shortDuration(unpushedStuckAfter) + " waits to be pushed"}
	if u.Count > 0 {
		row.Detail = u.String()
	}
	if !u.stuck() {
		return []check{row}
	}
	if !syncRanSince(root, u.Oldest.Add(unpushedStuckAfter)) {
		row.Status = statusWarn
		row.Detail += " — no sync has tried to push since; the next one may"
		return []check{row}
	}
	row.Status = statusFail
	row.Detail += " — teammates do not get this machine's knowledge"
	row.Fix = "see the sync log (`pull` or `push` errors); fix the cause, then `scribe sync` — a conflict may need `git pull --rebase` by hand"
	return []check{row}
}

// syncRanSince reports whether any sync run (plain or --sessions; both pull
// and push) finished after t. Unreadable run records count as "ran", so a
// broken runs dir cannot hide a stuck machine.
func syncRanSince(root string, t time.Time) bool {
	records, err := loadRunRecords(root)
	if err != nil {
		return true
	}
	return records["sync"].After(t) || records["sync --sessions"].After(t)
}

// reportUnpushedWork writes one sync-log line per run and, when the work is
// stuck, a desktop notification at most once a day.
func reportUnpushedWork(root string, now time.Time) {
	u, ok := unpushedWork(root, now)
	if !ok {
		return
	}
	if !u.stuck() {
		logMsg("sync", "unpushed: %d commit(s)", u.Count)
		return
	}
	slog.Warn("unpushed work is stuck: "+u.String(), "script", "sync")
	notifyUnpushedDaily(filepath.Base(root), u, now)
}

// notifyDesktop is a seam for tests. Best effort: Focus or a denied
// notification permission drop it silently, so it is never the only channel.
var notifyDesktop = func(msg string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	// Bounded: sync holds its lock while this runs.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// argv, not string interpolation: no AppleScript quoting of msg needed.
	return exec.CommandContext(ctx, "osascript",
		"-e", "on run argv", "-e", `display notification (item 1 of argv) with title "scribe"`, "-e", "end run",
		msg).Run()
}

func unpushedNotifyStampPath() string {
	return filepath.Join(userConfigDir(), "unpushed-notified")
}

// shortcut: one stamp for all KBs on the machine, so two stuck KBs share one
// notification a day; key the stamp by KB if anyone runs more than one team KB.
func notifyUnpushedDaily(kb string, u unpushed, now time.Time) {
	stamp := unpushedNotifyStampPath()
	if b, err := os.ReadFile(stamp); err == nil {
		if last, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b))); err == nil && now.Sub(last) < 24*time.Hour {
			return
		}
	}
	if err := notifyDesktop(fmt.Sprintf("KB %s: %s. Run `scribe doctor`.", kb, u)); err != nil {
		slog.Warn("unpushed: desktop notification failed", "err", err, "script", "sync")
		return
	}
	err := os.MkdirAll(filepath.Dir(stamp), 0o700)
	if err == nil {
		err = os.WriteFile(stamp, []byte(now.Format(time.RFC3339)+"\n"), 0o600)
	}
	if err != nil {
		slog.Warn("unpushed: notification stamp not written; the next run notifies again", "err", err, "script", "sync")
	}
}
