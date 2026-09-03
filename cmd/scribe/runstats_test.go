package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// TestRunStatsHelpersAreGoroutineSafe pins the fix for the parallel
// extraction crash: runExtractEnvelope ran per project inside an errgroup
// and did check-then-init plus read-modify-write on the bare runStats map,
// which is a runtime fatal ("concurrent map writes") under two envelope
// projects. `make race` runs this with the detector on.
func TestRunStatsHelpersAreGoroutineSafe(t *testing.T) {
	orig := runStats
	t.Cleanup(func() { runStats = orig })
	runStats = nil

	const workers, perWorker = 16, 250
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for range perWorker {
				setRunStatIfAbsent("mode", "envelope-extract")
				setRunStatIfAbsent("project", w)
				addRunStat("envelope_actions_applied", 2)
				addRunStat("envelope_actions_errored", 1)
				setRunStat("last_worker", w)
				mergeRunStats(map[string]any{"merged": true})
				_ = snapshotRunStats()
			}
		})
	}
	wg.Wait()

	got := snapshotRunStats()
	if got["envelope_actions_applied"] != workers*perWorker*2 {
		t.Errorf("applied = %v, want %d", got["envelope_actions_applied"], workers*perWorker*2)
	}
	if got["envelope_actions_errored"] != workers*perWorker {
		t.Errorf("errored = %v, want %d", got["envelope_actions_errored"], workers*perWorker)
	}
	if got["mode"] != "envelope-extract" || got["merged"] != true {
		t.Errorf("labels lost: %v", got)
	}
	// The snapshot is a copy: mutating it must not touch the live map.
	got["mode"] = "tampered"
	if snapshotRunStats()["mode"] != "envelope-extract" {
		t.Error("snapshotRunStats must return a copy")
	}
}

// TestNoDirectRunStatsWrites is the source-level guard: every write to
// the telemetry map goes through the helpers in main.go, so a future
// parallel call site cannot reintroduce the race by accident.
func TestNoDirectRunStatsWrites(t *testing.T) {
	direct := regexp.MustCompile(`(^|[^A-Za-z0-9_.])runStats(\[[^\]]*\]\s*=|\s*=\s)`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "main.go" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, ln := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(ln), "//") {
				continue
			}
			if direct.MatchString(ln) {
				t.Errorf("%s:%d writes runStats directly — use setRunStat/addRunStat/mergeRunStats: %s", f, i+1, strings.TrimSpace(ln))
			}
		}
	}
}
