package main

import (
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
