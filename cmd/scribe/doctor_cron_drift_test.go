package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCronPlistDriftRowAllCurrent: every expected job's plist is stamped
// and matches what the current binary would generate — one clean [ok] row,
// no fix line.
func TestCronPlistDriftRowAllCurrent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	jobs := []cronJob{
		{Name: "auto-commit", Command: "/usr/local/bin/scribe each -- commit"},
		{Name: "lint", Command: "/usr/local/bin/scribe each -- lint"},
	}
	mustMkAgentsDir(t)
	for _, job := range jobs {
		mustWritePlist(t, job.Name, stampPlist(renderPlist(job)))
	}

	c := cronPlistDriftRow(jobs)
	if c.Section != "cron" || c.Name != "cron-plists" {
		t.Fatalf("section/name = %s/%s, want cron/cron-plists", c.Section, c.Name)
	}
	if c.Status != statusOK {
		t.Errorf("status = %q, want ok\n  detail: %s", c.Status, c.Detail)
	}
	if c.Detail != "all 2 current" {
		t.Errorf("detail = %q, want %q", c.Detail, "all 2 current")
	}
	if c.Fix != "" {
		t.Errorf("fix = %q, want empty on all-current", c.Fix)
	}
}

// TestCronPlistDriftRowMixed drives the mixed case from issue #54's design:
// one job current, one stale (scribe-authored, content drifted), one
// hand-edited (unstamped), one missing entirely. The aggregated warn row
// must name every affected label and point at the plain fix (some jobs are
// still ok/stale-only-fixable-by-plain-install, so --force must NOT be the
// blanket suggestion here — only stale+missing are actionable without it,
// but hand-edited being present means --force is mentioned per the design:
// "mention --force only when hand-edited/unstamped are present").
func TestCronPlistDriftRowMixed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mustMkAgentsDir(t)

	current := cronJob{Name: "current-job", Command: "/usr/local/bin/scribe each -- commit"}
	staleOld := cronJob{Name: "stale-job", Command: "/opt/homebrew/bin/scribe each -- lint"}
	staleNew := cronJob{Name: "stale-job", Command: "/usr/local/bin/scribe each -- lint"}
	handEdited := cronJob{Name: "handedited-job", Command: "/usr/local/bin/scribe each -- dream"}
	missing := cronJob{Name: "missing-job", Command: "/usr/local/bin/scribe each -- sync"}

	mustWritePlist(t, current.Name, stampPlist(renderPlist(current)))
	mustWritePlist(t, staleOld.Name, stampPlist(renderPlist(staleOld))) // stamped against the OLD command
	mustWritePlist(t, handEdited.Name, renderPlist(handEdited))         // never stamped
	// missing-job: no file written at all.

	jobs := []cronJob{current, staleNew, handEdited, missing}
	c := cronPlistDriftRow(jobs)

	if c.Status != statusWarn {
		t.Fatalf("status = %q, want warn\n  detail: %s", c.Status, c.Detail)
	}
	for _, want := range []string{
		"1 missing (com.scribe.missing-job)",
		"1 stale (com.scribe.stale-job)",
		"1 unstamped/hand-edited (com.scribe.handedited-job)",
	} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q missing substring %q", c.Detail, want)
		}
	}
	if c.Fix != "scribe cron install --force" {
		t.Errorf("fix = %q, want %q (not every non-missing job is hand-edited, so no adoption-line special case)", c.Fix, "scribe cron install --force")
	}
}

// TestCronPlistDriftRowAllUnstamped is the "real machine, first upgrade to
// #54" scenario: every installed plist predates stamping. The fix line
// must call out the one-time --force adoption run explicitly (a plain
// `scribe cron install` silently no-ops on unstamped files, which would
// otherwise look like doctor's advice did nothing).
func TestCronPlistDriftRowAllUnstamped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mustMkAgentsDir(t)

	a := cronJob{Name: "job-a", Command: "/usr/local/bin/scribe each -- commit"}
	b := cronJob{Name: "job-b", Command: "/usr/local/bin/scribe each -- lint"}
	mustWritePlist(t, a.Name, renderPlist(a))
	mustWritePlist(t, b.Name, renderPlist(b))

	c := cronPlistDriftRow([]cronJob{a, b})
	if c.Status != statusWarn {
		t.Fatalf("status = %q, want warn", c.Status)
	}
	want := "scribe cron install --force  (one-time adoption of stamped plists)"
	if c.Fix != want {
		t.Errorf("fix = %q, want %q", c.Fix, want)
	}
}

// TestCronPlistDriftRowStaleAndMissingOnly: no hand-edited jobs at all —
// the fix line must stay the plain, no --force form, since plain `cron
// install` already self-heals both stale and missing without --force.
func TestCronPlistDriftRowStaleAndMissingOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mustMkAgentsDir(t)

	staleOld := cronJob{Name: "stale-job", Command: "/opt/homebrew/bin/scribe each -- lint"}
	staleNew := cronJob{Name: "stale-job", Command: "/usr/local/bin/scribe each -- lint"}
	missing := cronJob{Name: "missing-job", Command: "/usr/local/bin/scribe each -- sync"}
	mustWritePlist(t, staleOld.Name, stampPlist(renderPlist(staleOld)))

	c := cronPlistDriftRow([]cronJob{staleNew, missing})
	if c.Status != statusWarn {
		t.Fatalf("status = %q, want warn", c.Status)
	}
	if c.Fix != "scribe cron install" {
		t.Errorf("fix = %q, want plain %q (no hand-edited jobs present)", c.Fix, "scribe cron install")
	}
}

func mustMkAgentsDir(t *testing.T) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWritePlist(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(plistPath(name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
