// sync_sessions_test.go — priority-lane scheduling for the pending-session
// queue (issue #22): lane classification, floor-reservation admission,
// aging promotion, size-pool partitioning, and merge/sort semantics. See
// docs/issue-22-priority-lanes-plan.md §4.4 for the test plan this file
// implements.
//
// Real mining is deliberately NOT exercised end-to-end here: it shells out
// to the real `ccrider sync` binary and re-execs the current binary as
// `scribe triage --json` (via
// os.Executable()), neither of which behaves safely or deterministically
// under `go test` in this environment (ccrider is on PATH and would touch
// the real ~/.config/ccrider state; the test binary is not `scribe` and
// can't answer `triage --json`). admitForPool — the function mineSessions
// delegates all scheduling logic to — is exercised directly instead,
// including with the real preFilterSessions scope-gate closure the normal
// pool actually uses, to cover the same "pending + live-triage entries
// merge, classify, and admit in the right order" surface without those
// unsafe dependencies.
package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDryRunTriageArgs(t *testing.T) {
	tests := []struct {
		name  string
		large bool
		want  []string
	}{
		{name: "normal", want: []string{"triage", "--top", "3", "--sort", "score", "--message-limit", "300"}},
		{name: "large", large: true, want: []string{"triage", "--top", "3", "--sort", "score", "--min-messages", "301"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dryRunTriageArgs(3, "score", tt.large)
			if !equalStrings(got, tt.want) {
				t.Fatalf("dryRunTriageArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMineSessionsDryRunDoesNotSyncCcrider(t *testing.T) {
	home := t.TempDir()
	binDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ccrider-called")
	fakeCcrider := filepath.Join(binDir, "ccrider")
	if err := os.WriteFile(fakeCcrider, []byte("#!/bin/sh\n: > \"$CCRIDER_MARKER\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeScribe := filepath.Join(binDir, "scribe")
	if err := os.WriteFile(fakeScribe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	originalExecutable := sessionExecutable
	sessionExecutable = func() (string, error) { return fakeScribe, nil }
	t.Cleanup(func() { sessionExecutable = originalExecutable })
	t.Setenv("HOME", home)
	t.Setenv("CCRIDER_MARKER", marker)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cmd := SyncCmd{DryRun: true, SessionsMax: 1, SessionSort: "score", SkipLarge: true}
	if _, err := cmd.mineSessions(t.TempDir()); err != nil {
		t.Fatalf("mineSessions() error = %v", err)
	}
	if fileExists(marker) {
		t.Fatal("dry-run invoked ccrider sync")
	}
}

// idsOf extracts IDs in slice order, for order-sensitive assertions.
func idsOf(entries []pendingEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

func TestSplitBudgetByLane(t *testing.T) {
	cases := []struct {
		budget     int
		ratio      float64
		wantHot    int
		wantNormal int
	}{
		{budget: 0, ratio: 0.3, wantHot: 0, wantNormal: 0},
		{budget: 1, ratio: 0.3, wantHot: 1, wantNormal: 0}, // rounds down to 0 for Normal
		{budget: 3, ratio: 0.3, wantHot: 2, wantNormal: 1},
		{budget: 10, ratio: 0.3, wantHot: 7, wantNormal: 3},
		{budget: 2, ratio: 0.5, wantHot: 1, wantNormal: 1},
	}
	for _, tc := range cases {
		t.Run("", func(t *testing.T) {
			hot, normal := splitBudgetByLane(tc.budget, tc.ratio)
			if hot != tc.wantHot || normal != tc.wantNormal {
				t.Errorf("splitBudgetByLane(%d, %.1f) = (%d, %d), want (%d, %d)",
					tc.budget, tc.ratio, hot, normal, tc.wantHot, tc.wantNormal)
			}
		})
	}
}

func TestAdmitByLaneFloor(t *testing.T) {
	mk := func(prefix string, n int) []pendingEntry {
		out := make([]pendingEntry, n)
		for i := range n {
			out[i] = pendingEntry{ID: prefix + strconv.Itoa(i)}
		}
		return out
	}

	t.Run("enough candidates in both lanes: floor respected exactly", func(t *testing.T) {
		hot := mk("h", 5)
		normal := mk("n", 4)
		got := admitByLaneFloor(hot, normal, 5, 0.4) // hotSlots=3, normalSlots=2
		want := []string{"h0", "h1", "h2", "n0", "n1"}
		if !equalStrings(idsOf(got), want) {
			t.Errorf("got %v, want %v", idsOf(got), want)
		}
	})

	t.Run("hot lane empty: 100% backfilled from normal", func(t *testing.T) {
		normal := mk("n", 5)
		got := admitByLaneFloor(nil, normal, 3, 0.3)
		want := []string{"n0", "n1", "n2"}
		if !equalStrings(idsOf(got), want) {
			t.Errorf("got %v, want %v", idsOf(got), want)
		}
	})

	t.Run("normal lane empty: 100% from hot", func(t *testing.T) {
		hot := mk("h", 5)
		got := admitByLaneFloor(hot, nil, 3, 0.3)
		want := []string{"h0", "h1", "h2"}
		if !equalStrings(idsOf(got), want) {
			t.Errorf("got %v, want %v", idsOf(got), want)
		}
	})

	t.Run("both lanes together short of budget: returns everything, no panic", func(t *testing.T) {
		hot := mk("h", 2)
		normal := mk("n", 1)
		got := admitByLaneFloor(hot, normal, 10, 0.3)
		want := []string{"h0", "h1", "n0"}
		if !equalStrings(idsOf(got), want) {
			t.Errorf("got %v, want %v", idsOf(got), want)
		}
	})

	t.Run("budget=0 returns empty", func(t *testing.T) {
		got := admitByLaneFloor(mk("h", 1), mk("n", 1), 0, 0.3)
		if len(got) != 0 {
			t.Errorf("got %v, want empty", idsOf(got))
		}
	})
}

func TestClassifyLanes(t *testing.T) {
	cfg := PriorityLanesConfig{HotThreshold: 90, AgeDays: 7}
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		e       pendingEntry
		wantHot bool
	}{
		{"score above threshold", pendingEntry{ID: "a", Score: 95, HasScore: true}, true},
		{"score below threshold, fresh", pendingEntry{ID: "b", Score: 50, HasScore: true}, false},
		{
			"aged normal-score entry promotes to hot", pendingEntry{
				ID: "c", Score: 50, HasScore: true,
				HasEnqueuedAt: true, EnqueuedAt: now.Add(-10 * 24 * time.Hour),
			}, true,
		},
		{"legacy unknown age forces hot regardless of score", pendingEntry{ID: "d", Score: 10, HasScore: true, LegacyUnknownAge: true}, true},
		{
			"recent enqueued, below threshold stays normal", pendingEntry{
				ID: "e", Score: 50, HasScore: true,
				HasEnqueuedAt: true, EnqueuedAt: now.Add(-1 * time.Hour),
			}, false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hot, normal := classifyLanes([]pendingEntry{tc.e}, cfg, now)
			gotHot := len(hot) == 1
			if gotHot != tc.wantHot {
				t.Errorf("classifyLanes(%+v) hot=%v, want hot=%v (hot=%v normal=%v)", tc.e, gotHot, tc.wantHot, hot, normal)
			}
		})
	}
}

func TestSortWithinLane(t *testing.T) {
	t.Run("same score: legacy-unknown oldest, real timestamp, fresh (no signal) newest", func(t *testing.T) {
		entries := []pendingEntry{
			{ID: "fresh", Score: 50, HasScore: true}, // no age signal -> "now"
			{ID: "real", Score: 50, HasScore: true, HasEnqueuedAt: true, EnqueuedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			{ID: "legacy", Score: 50, HasScore: true, LegacyUnknownAge: true},
		}
		sortWithinLane(entries)
		want := []string{"legacy", "real", "fresh"}
		if got := idsOf(entries); !equalStrings(got, want) {
			t.Errorf("order = %v, want %v", got, want)
		}
	})

	t.Run("different scores: higher first regardless of age", func(t *testing.T) {
		entries := []pendingEntry{
			{ID: "low-but-legacy", Score: 10, HasScore: true, LegacyUnknownAge: true},
			{ID: "high-fresh", Score: 90, HasScore: true},
		}
		sortWithinLane(entries)
		want := []string{"high-fresh", "low-but-legacy"}
		if got := idsOf(entries); !equalStrings(got, want) {
			t.Errorf("order = %v, want %v", got, want)
		}
	})
}

func TestMergeCandidates(t *testing.T) {
	t.Run("collision: pending entry's fields win", func(t *testing.T) {
		pending := []pendingEntry{
			{ID: "dup", Score: 40, HasScore: true, HasEnqueuedAt: true, EnqueuedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		}
		triaged := []pendingEntry{{ID: "dup", Score: 99, HasScore: true}} // fresh triage rediscovery
		got := mergeCandidates(pending, triaged)
		if len(got) != 1 {
			t.Fatalf("got %d entries, want 1: %+v", len(got), got)
		}
		if got[0].Score != 40 || !got[0].HasEnqueuedAt {
			t.Errorf("collision entry = %+v, want pending's Score=40 HasEnqueuedAt=true to survive", got[0])
		}
	})

	t.Run("no collision: both preserved, pending first", func(t *testing.T) {
		pending := []pendingEntry{{ID: "p1", Score: 10, HasScore: true}}
		triaged := []pendingEntry{{ID: "t1", Score: 20, HasScore: true}}
		got := mergeCandidates(pending, triaged)
		want := []string{"p1", "t1"}
		if got := idsOf(got); !equalStrings(got, want) {
			t.Errorf("order = %v, want %v", got, want)
		}
	})
}

func TestBackfillMsgCounts(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	insertFixtureSession(t, db, "known-count", "/p/alpha", 42, "2026-06-01T10:00:00", "2026-06-01T12:00:00", "s")

	t.Run("unknown MsgCount gets backfilled from ccrider", func(t *testing.T) {
		entries := []pendingEntry{{ID: "known-count", MsgCount: -1}}
		got := backfillMsgCounts(dbPath, entries)
		if got[0].MsgCount != 42 {
			t.Errorf("MsgCount = %d, want 42", got[0].MsgCount)
		}
	})

	t.Run("already-known MsgCount is left untouched", func(t *testing.T) {
		// "not-in-db" has no fixture row; if backfillMsgCounts queried it
		// anyway, queryMessageCount would fail (ok=false) and leave
		// MsgCount unchanged regardless — the real assertion is that the
		// pre-set MsgCount survives without needing a lookup at all.
		entries := []pendingEntry{{ID: "not-in-db", MsgCount: 17}}
		got := backfillMsgCounts(dbPath, entries)
		if got[0].MsgCount != 17 {
			t.Errorf("MsgCount = %d, want 17 (untouched)", got[0].MsgCount)
		}
	})

	t.Run("DB-open failure fails open: MsgCount stays -1", func(t *testing.T) {
		badPath := filepath.Join(t.TempDir(), "does-not-exist.db")
		entries := []pendingEntry{{ID: "x", MsgCount: -1}, {ID: "y", MsgCount: -1}}
		got := backfillMsgCounts(badPath, entries)
		for _, e := range got {
			if e.MsgCount != -1 {
				t.Errorf("entry %s MsgCount = %d, want -1 (fail open)", e.ID, e.MsgCount)
			}
		}
	})
}

func TestPartitionBySize(t *testing.T) {
	entries := []pendingEntry{
		{ID: "at-300", MsgCount: 300},
		{ID: "over-300", MsgCount: 301},
		{ID: "unknown", MsgCount: -1},
	}
	normal, large := partitionBySize(entries)
	if got := idsOf(normal); !equalStrings(got, []string{"at-300", "unknown"}) {
		t.Errorf("normal = %v, want [at-300 unknown]", got)
	}
	if got := idsOf(large); !equalStrings(got, []string{"over-300"}) {
		t.Errorf("large = %v, want [over-300]", got)
	}
}

// TestAdmitForPool_NoStarvation is the invariant test: a queue dominated
// 18:2 by Hot-scoring entries must not starve the 2 Normal entries
// forever. Simulates consecutive sync runs by removing admitted IDs
// between calls (mirroring the real drain-then-requeue cycle) and asserts
// both Normal entries clear within a small, bounded number of runs — the
// direct regression guard for the starvation class of bug the
// floor-reservation design exists to prevent.
func TestAdmitForPool_NoStarvation(t *testing.T) {
	cfg := PriorityLanesConfig{HotThreshold: 90, AgeDays: 7}
	now := time.Now()

	var pool []pendingEntry
	for i := range 18 {
		pool = append(pool, pendingEntry{
			ID: "hot-" + strconv.Itoa(i), Score: 95, HasScore: true, MsgCount: -1,
			HasEnqueuedAt: true, EnqueuedAt: now.Add(-time.Duration(i) * time.Minute),
		})
	}
	for i := range 2 {
		pool = append(pool, pendingEntry{
			ID: "normal-" + strconv.Itoa(i), Score: 40, HasScore: true, MsgCount: -1,
			HasEnqueuedAt: true, EnqueuedAt: now.Add(-time.Duration(i) * time.Minute),
		})
	}

	noopFilter := func(ids []string) []string { return ids }
	const budget = 3
	const maxRuns = 10

	admittedNormal := map[string]bool{}
	runsToFullyAdmitNormal := -1
	for run := 1; run <= maxRuns && len(pool) > 0; run++ {
		admitted := admitForPool(pool, nil, budget, cfg, noopFilter)
		admittedSet := make(map[string]bool, len(admitted))
		for _, id := range admitted {
			admittedSet[id] = true
			if strings.HasPrefix(id, "normal-") {
				admittedNormal[id] = true
			}
		}
		var remaining []pendingEntry
		for _, e := range pool {
			if !admittedSet[e.ID] {
				remaining = append(remaining, e)
			}
		}
		pool = remaining
		if len(admittedNormal) == 2 && runsToFullyAdmitNormal == -1 {
			runsToFullyAdmitNormal = run
		}
	}

	if len(admittedNormal) != 2 {
		t.Fatalf("both Normal entries never admitted across %d runs (got %v)", maxRuns, admittedNormal)
	}
	if runsToFullyAdmitNormal > 2 {
		t.Errorf("Normal entries took %d runs to fully admit, want <=2 — floor-reservation should guarantee steady throughput even against an 18:2 Hot-dominated queue", runsToFullyAdmitNormal)
	}
}

// TestAdmitForPool_DegradesToScoreDesc confirms that with real defaults
// (90/7) and a queue where nothing is Hot or aged, admission ordering
// degrades to pure score-descending — the new priority-lane code path
// must not silently reorder things when nothing unusual is present.
// (Renamed from the research doc's "no subscriptions" framing — this
// plan's lane axis is score, not domain subscriptions; see
// docs/issue-22-priority-lanes-plan.md §1.)
func TestAdmitForPool_DegradesToScoreDesc(t *testing.T) {
	var cfg PriorityLanesConfig
	applyPriorityLanesDefaults(&cfg) // 90/7

	pending := []pendingEntry{
		{ID: "p-30", Score: 30, HasScore: true, MsgCount: -1},
		{ID: "p-70", Score: 70, HasScore: true, MsgCount: -1},
	}
	triaged := []pendingEntry{
		{ID: "t-50", Score: 50, HasScore: true, MsgCount: -1},
	}
	noopFilter := func(ids []string) []string { return ids }

	got := admitForPool(pending, triaged, 10, cfg, noopFilter)
	want := []string{"p-70", "t-50", "p-30"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v (pure score-desc order)", got, want)
	}
}

// TestAdmitForPool_WithPreFilterSessions covers the seam mineSessions'
// normal pool actually drives: pending-queue entries merged with
// live-triage entries, admitted by priority lane, then run through the
// real preFilterSessions scope gate (ccrider-backed, no LLM/subprocess
// involved) — the same closure §3.5 wires into admitForPool. This is the
// scoped-down replacement for a full mineSessions() integration test (see
// the file-level doc comment for why mineSessions itself is unsafe to
// exercise under `go test` in this environment).
func TestAdmitForPool_WithPreFilterSessions(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	root := sessionsTestKB(t, dbPath)

	const proj = "/home/dev/projects/alpha"
	seed := func(sessionID string, userMsgs, charsPerMsg int) {
		rowid := insertFixtureSession(t, db, sessionID, proj, userMsgs+1,
			"2026-06-01T10:00:00", "2026-06-01T12:00:00", "s")
		for range userMsgs {
			insertFixtureMessage(t, db, rowid, "user", strings.Repeat("x", charsPerMsg), false)
		}
	}
	seed("hot-rich", 3, 800)    // passes the mechanical-content gate
	seed("normal-rich", 3, 800) // passes the mechanical-content gate
	seed("hot-thin", 2, 400)    // fails the gate (thin) — must be dropped

	cfg := PriorityLanesConfig{HotThreshold: 90, AgeDays: 7}
	pending := []pendingEntry{
		{ID: "hot-rich", Score: 95, HasScore: true, MsgCount: -1},
		{ID: "hot-thin", Score: 99, HasScore: true, MsgCount: -1},
	}
	triaged := []pendingEntry{
		{ID: "normal-rich", Score: 40, HasScore: true, MsgCount: -1},
	}

	got := admitForPool(pending, triaged, 5, cfg, func(ids []string) []string {
		kept, _ := preFilterSessions(root, dbPath, ids)
		return kept
	})

	want := []string{"hot-rich", "normal-rich"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v (hot-thin must be dropped by the mechanical-content gate)", got, want)
	}
}
