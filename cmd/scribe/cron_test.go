package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRenderCrontab confirms the collapsing rules and the one-line-per-slot
// fallback match what we document in README. Each case is one representative
// LaunchAgent schedule from scribeJobs.
func TestRenderCrontab(t *testing.T) {
	cases := []struct {
		name string
		job  cronJob
		want []string
	}{
		{
			name: "hourly_at_7",
			job: cronJob{
				Command:  "scribe commit",
				Schedule: schedSpec{Calendar: hourlyAt(7)},
			},
			// Collapses to `7 */1 * * *`.
			want: []string{"7 */1 * * * scribe commit"},
		},
		{
			name: "every_2h_at_23",
			job: cronJob{
				Command:  "scribe sync --max 2",
				Schedule: schedSpec{Calendar: everyNHoursAt(2, 23)},
			},
			want: []string{"23 */2 * * * scribe sync --max 2"},
		},
		{
			name: "every_30_minutes",
			job: cronJob{
				Command:  "scribe ingest drain",
				Schedule: schedSpec{Calendar: everyNMinutes(30)},
			},
			want: []string{"*/30 * * * * scribe ingest drain"},
		},
		{
			name: "three_fixed_times",
			job: cronJob{
				Command: "scribe sync --sessions",
				Schedule: schedSpec{Calendar: []calTime{
					{Hour: 3, Minute: 0, Weekday: -1},
					{Hour: 12, Minute: 0, Weekday: -1},
					{Hour: 18, Minute: 0, Weekday: -1},
				}},
			},
			// Three distinct times — no collapse; sorted lexicographically.
			want: []string{
				"0 12 * * * scribe sync --sessions",
				"0 18 * * * scribe sync --sessions",
				"0 3 * * * scribe sync --sessions",
			},
		},
		{
			name: "weekly_sun_2am",
			job: cronJob{
				Command:  "scribe dream",
				Schedule: schedSpec{Calendar: []calTime{{Hour: 2, Minute: 0, Weekday: 0}}},
			},
			want: []string{"0 2 * * 0 scribe dream"},
		},
		{
			name: "keepalive_no_cron",
			job:  cronJob{KeepAlive: true, Command: "scribe watch"},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderCrontab(tc.job)
			if len(got) != len(tc.want) {
				t.Fatalf("len: got %d (%v), want %d (%v)", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if strings.TrimSpace(got[i]) != tc.want[i] {
					t.Errorf("[%d] got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestScribeJobsIncludesLintDuplicates guards the weekly structural
// content-duplicate scan being part of the installed schedule.
func TestScribeJobsIncludesLintDuplicates(t *testing.T) {
	var found bool
	for _, j := range scribeJobs("scribe") {
		if j.Name == "lint-duplicates" {
			found = true
			if !strings.Contains(j.Command, "lint --duplicates") {
				t.Errorf("lint-duplicates command = %q, want it to run `lint --duplicates`", j.Command)
			}
			if !strings.Contains(j.Command, "each --") {
				t.Errorf("job command should be KB-agnostic (each --): %q", j.Command)
			}
		}
	}
	if !found {
		t.Error("scribeJobs is missing the lint-duplicates weekly job")
	}
}

// TestPlistKBRoot pins the `cd "<root>" && ` extraction used to detect
// LEGACY single-KB plists (pre-#26) during migration. KB-agnostic plists
// have no cd, so they must yield "".
func TestPlistKBRoot(t *testing.T) {
	legacy := renderPlist(cronJob{Name: "auto-commit", Command: `cd "/Users/u/Projects/my-kb" && /usr/local/bin/scribe commit`})
	if got := plistKBRoot(legacy); got != "/Users/u/Projects/my-kb" {
		t.Errorf("plistKBRoot(legacy) = %q, want /Users/u/Projects/my-kb", got)
	}
	agnostic := renderPlist(scribeJobs("/usr/local/bin/scribe")[0])
	if got := plistKBRoot(agnostic); got != "" {
		t.Errorf("plistKBRoot(KB-agnostic) = %q, want empty (no cd)", got)
	}
	if got := plistKBRoot("<plist>no cd prefix here</plist>"); got != "" {
		t.Errorf("plistKBRoot(no marker) = %q, want empty", got)
	}
}

// TestOtherKBServedByAgents covers the cron-install clobber guard:
// existing com.scribe.* plists pointing at a different KB root must be
// detected; same-root plists and absent plists must not trip it.
func TestOtherKBServedByAgents(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	agents := filepath.Join(fakeHome, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := otherKBServedByAgents("/Users/u/Projects/kb-a"); got != "" {
		t.Errorf("no plists on disk: got %q, want empty", got)
	}

	// A LEGACY single-KB plist (embeds `cd "<root>"`) serving kb-a.
	name := scribeJobs("/usr/local/bin/scribe")[0].Name
	legacy := renderPlist(cronJob{Name: name, Command: `cd "/Users/u/Projects/kb-a" && /usr/local/bin/scribe commit`})
	if err := os.WriteFile(plistPath(name), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := otherKBServedByAgents("/Users/u/Projects/kb-a"); got != "" {
		t.Errorf("same root: got %q, want empty", got)
	}
	if got := otherKBServedByAgents("/Users/u/Projects/kb-b"); got != "/Users/u/Projects/kb-a" {
		t.Errorf("legacy plist for kb-a: got %q, want /Users/u/Projects/kb-a", got)
	}

	// A KB-agnostic plist (no cd) must NOT be flagged as serving another KB.
	agnostic := renderPlist(scribeJobs("/usr/local/bin/scribe")[0])
	if err := os.WriteFile(plistPath(name), []byte(agnostic), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := otherKBServedByAgents("/Users/u/Projects/kb-b"); got != "" {
		t.Errorf("KB-agnostic plist must not be detected as foreign: got %q, want empty", got)
	}
}

// TestCronInstallRefusesThrowawayKB: installing LaunchAgents from a
// temp-path KB would point this machine's whole schedule at a directory
// that vanishes on reboot — the writeGlobalState chokepoint must refuse
// before any plist lands or launchctl runs.
func TestCronInstallRefusesThrowawayKB(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("LaunchAgent install is darwin-only")
	}
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("SCRIBE_KB", t.TempDir()) // under os.TempDir() → throwaway

	c := &CronInstallCmd{}
	err := c.Run()
	if err == nil || !strings.Contains(err.Error(), "throwaway") {
		t.Fatalf("cron install from throwaway KB: want refusal, got %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(fakeHome, "Library", "LaunchAgents"))
	if len(entries) != 0 {
		t.Errorf("refusal still wrote %d plist(s)", len(entries))
	}
}
