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

// unpushedWork reads branch..upstream for every local branch that tracks
// one. ok is false when none does (a solo KB, or not a git repo): there is
// nothing to push to. It reads refs, not HEAD, so a rebase left in progress
// (detached HEAD) still counts — that is exactly when a machine is stuck.
// Author date, not committer date: a rebase rewrites the committer date on
// every retry, which would keep a stuck commit looking young forever.
func unpushedWork(root string, now time.Time) (unpushed, bool) {
	res := unpushed{Now: now}
	tracked := false
	refs := runCmd(root, "git", "for-each-ref", "--format=%(refname) %(upstream)", "refs/heads")
	for _, line := range strings.Split(refs, "\n") {
		branch, upstream, _ := strings.Cut(line, " ")
		if upstream == "" {
			continue
		}
		tracked = true
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
	}
	return res, tracked
}

// checkUnpushedWork is the doctor row. A FAIL row also lands in
// NEEDS-ATTENTION.md through the daily `doctor --write-attention` job.
func checkUnpushedWork(root string, now time.Time) []check {
	u, ok := unpushedWork(root, now)
	if !ok {
		return nil
	}
	row := check{Section: "state", Name: "unpushed-work", Status: statusOK, Detail: "nothing older than " + shortDuration(unpushedStuckAfter) + " waits to be pushed"}
	if u.Count > 0 {
		row.Detail = u.String()
	}
	if u.stuck() {
		row.Status = statusFail
		row.Detail += " — teammates do not get this machine's knowledge"
		row.Fix = "see the sync log (`pull` or `push` errors); fix the cause, then `scribe sync` — a conflict may need `git pull --rebase` by hand"
	}
	return []check{row}
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
