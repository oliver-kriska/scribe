package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStrictnessHoldsFile pins the shared hold-classification rule used by
// both sync's absorb gate and the status backlog: strictness=high holds a
// file unless its frontmatter opts in via `absorb: true` or a named domain.
func TestStrictnessHoldsFile(t *testing.T) {
	cases := []struct {
		name       string
		strictness string
		content    string
		held       bool
	}{
		{"high absorb opt-in", "high", "---\nabsorb: true\n---\nbody\n", false},
		{"high named domain", "high", "---\ndomain: widgets\n---\nbody\n", false},
		{"high general domain", "high", "---\ndomain: general\n---\nbody\n", true},
		{"high empty domain", "high", "---\ndomain: \"\"\n---\nbody\n", true},
		{"high absorb false", "high", "---\nabsorb: false\n---\nbody\n", true},
		{"high no frontmatter", "high", "just a body\n", true},
		{"high malformed frontmatter", "high", "---\n: [unbalanced\n", true},
		{"medium general domain", "medium", "---\ndomain: general\n---\nbody\n", false},
		{"low no frontmatter", "low", "just a body\n", false},
	}
	dir := t.TempDir()
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("f%d.md", i))
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := strictnessHoldsFile(tc.strictness, path); got != tc.held {
				t.Errorf("strictnessHoldsFile(%q, %s) = %v, want %v", tc.strictness, tc.name, got, tc.held)
			}
		})
	}
}

// statusBacklogFixture builds a minimal KB plus external project dirs for
// renderBacklog tests. Drop files land in one non-git "dropper" project
// (marked extracted so it doesn't pollute the projects row); mdProjects
// are never-extracted projects holding N markdown files each so the
// max_extract_files gate has something to measure.
type statusBacklogFixture struct {
	strictness string
	maxExtract int
	drops      map[string]string // drop filename → content
	mdProjects map[string]int    // project name → number of .md files
}

func (f statusBacklogFixture) build(t *testing.T) (root string, cfg *ScribeConfig) {
	t.Helper()
	root = t.TempDir()

	yaml := fmt.Sprintf("kb_name: testkb\nabsorb:\n  strictness: %s\nsync:\n  max_extract_files: %d\n",
		f.strictness, f.maxExtract)
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	projects := map[string]map[string]string{}

	if len(f.drops) > 0 {
		pdir := t.TempDir()
		dropDir := filepath.Join(pdir, ".claude", "testkb")
		if err := os.MkdirAll(dropDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range f.drops {
			if err := os.WriteFile(filepath.Join(dropDir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		// last_sha + last_extracted set, non-git dir → counts as done in
		// the projects row, so drop assertions stay isolated.
		projects["dropper"] = map[string]string{
			"path":           pdir,
			"domain":         "general",
			"last_sha":       "abc123",
			"last_extracted": "2026-01-01T00:00:00Z",
		}
	}

	for name, n := range f.mdProjects {
		pdir := t.TempDir()
		for i := range n {
			file := filepath.Join(pdir, fmt.Sprintf("doc%d.md", i))
			if err := os.WriteFile(file, []byte("# doc\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		// last_sha empty → "never extracted" → needs extraction without
		// any git subprocess (gitChangedFiles falls back to findFiles).
		projects[name] = map[string]string{"path": pdir, "domain": "general"}
	}

	scriptsDir := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]any{"projects": projects})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scriptsDir, "projects.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg = loadConfig(root)
	cfg.CcriderDB = "" // never touch a real sessions DB from tests
	return root, cfg
}

// TestRenderBacklogHeldSplit covers the held-by-policy vs genuinely-pending
// split in the status backlog (issue #17): drop files held by the
// strictness gate and projects held by the max_extract_files cap must not
// read as work the next sync will do.
func TestRenderBacklogHeldSplit(t *testing.T) {
	optIn := "---\nabsorb: true\n---\nbody\n"
	namedDomain := "---\ndomain: widgets\n---\nbody\n"
	generalDomain := "---\ndomain: general\n---\nbody\n"
	noFrontmatter := "just a body\n"

	cases := []struct {
		name    string
		fixture statusBacklogFixture
		want    []string
	}{
		{
			name: "strictness high splits drops into held and pending",
			fixture: statusBacklogFixture{
				strictness: "high",
				maxExtract: 100,
				drops: map[string]string{
					"a.md": optIn,
					"b.md": namedDomain,
					"c.md": generalDomain,
					"d.md": noFrontmatter,
				},
			},
			want: []string{
				"drop files: 2 held (strictness=high — need opt-in), 2 pending",
			},
		},
		{
			name: "strictness medium leaves all drops pending",
			fixture: statusBacklogFixture{
				strictness: "medium",
				maxExtract: 100,
				drops: map[string]string{
					"a.md": optIn,
					"c.md": generalDomain,
					"d.md": noFrontmatter,
				},
			},
			want: []string{
				"drop files: 3 pending",
			},
		},
		{
			name: "project over max_extract_files is held not pending",
			fixture: statusBacklogFixture{
				strictness: "medium",
				maxExtract: 2,
				mdProjects: map[string]int{"big": 3, "small": 1},
			},
			want: []string{
				"projects (extract): 1 held (>max_extract_files — run `scribe deep`), 1 pending",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, cfg := tc.fixture.build(t)
			var buf bytes.Buffer
			renderBacklog(&buf, root, cfg)
			lines := normalizeLines(buf.String())
			for _, want := range tc.want {
				if !containsLine(lines, want) {
					t.Errorf("backlog missing line %q\ngot:\n%s", want, buf.String())
				}
			}
		})
	}
}

// TestRenderBacklogSessionQueueRow covers the priority-lane queue summary
// row (issue #22): printed only when the hook/watch queue has entries,
// classified into hot/normal/aged the same way pendingQueueSummary does,
// and folded into the SAME "backlog (run `scribe sync`...)" block as the
// other rows rather than a second header.
func TestRenderBacklogSessionQueueRow(t *testing.T) {
	writeQueue := func(t *testing.T, content string) {
		t.Helper()
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		scribeDir := filepath.Join(xdg, "scribe")
		if err := os.MkdirAll(scribeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scribeDir, "pending-sessions.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("no queue file: no row, no header forced", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		fixture := statusBacklogFixture{strictness: "medium", maxExtract: 100}
		root, cfg := fixture.build(t)
		var buf bytes.Buffer
		renderBacklog(&buf, root, cfg)
		if buf.Len() != 0 {
			t.Errorf("expected no backlog output at all, got:\n%s", buf.String())
		}
	})

	t.Run("queue entries print under the shared backlog header", func(t *testing.T) {
		now := time.Now().UTC()
		fresh := now.Add(-1 * time.Hour).Format(time.RFC3339)
		aged := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339) // > default AgeDays=7
		content := "s-hot\t95\t50\t" + fresh + "\n" +
			"s-normal\t40\t50\t" + fresh + "\n" +
			"s-aged\t40\t50\t" + aged + "\n"
		writeQueue(t, content)

		fixture := statusBacklogFixture{strictness: "medium", maxExtract: 100}
		root, cfg := fixture.build(t)
		var buf bytes.Buffer
		renderBacklog(&buf, root, cfg)
		lines := normalizeLines(buf.String())
		if !containsLine(lines, "backlog (run `scribe sync` to process):") {
			t.Errorf("missing shared backlog header:\n%s", buf.String())
		}
		want := "session queue (hooked): 2 hot, 1 normal (1 aged→hot)"
		if !containsLine(lines, want) {
			t.Errorf("missing line %q\ngot:\n%s", want, buf.String())
		}
	})

	t.Run("queue-only backlog (no other rows) still prints the header", func(t *testing.T) {
		writeQueue(t, "s-hot\t95\t50\t"+time.Now().UTC().Format(time.RFC3339)+"\n")
		fixture := statusBacklogFixture{strictness: "medium", maxExtract: 100}
		root, cfg := fixture.build(t)
		var buf bytes.Buffer
		renderBacklog(&buf, root, cfg)
		lines := normalizeLines(buf.String())
		if !containsLine(lines, "backlog (run `scribe sync` to process):") {
			t.Errorf("missing shared backlog header when only the queue row has content:\n%s", buf.String())
		}
		if !containsLine(lines, "session queue (hooked): 1 hot, 0 normal") {
			t.Errorf("missing queue row:\n%s", buf.String())
		}
	})
}

// writeSyncRunRecord writes a single-line JSONL run record for `sync` into
// output/runs/<today>.jsonl, with the given extra fields merged in (mirrors
// the shape writeRunRecord produces). Used to test the adoption-metric
// read-back path (loadLatestAdoptionStats / newestSyncRunLine) without
// running a real sync.
func writeSyncRunRecord(t *testing.T, root string, extra map[string]any) {
	t.Helper()
	runsDir := filepath.Join(root, "output", "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	record := map[string]any{
		"command":   "sync",
		"status":    "ok",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}
	for k, v := range extra {
		record[k] = v
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	dayFile := filepath.Join(runsDir, time.Now().UTC().Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(dayFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, string(data)); err != nil {
		t.Fatal(err)
	}
}

// TestLoadLatestAdoptionStats covers the issue #23 read-back path: status
// and digest must read the adoption ratio cached by the last `sync` run
// record, never recompute it, and must render nothing (ok=false) when the
// data isn't there — a missing runs dir, or an old record from before this
// feature shipped — rather than claiming a misleading zero.
func TestLoadLatestAdoptionStats(t *testing.T) {
	t.Run("no output/runs directory at all", func(t *testing.T) {
		root := t.TempDir()
		snaps, ok := loadLatestAdoptionStats(root)
		if ok {
			t.Errorf("ok = true with no runs dir, want false; snaps=%v", snaps)
		}
	})

	t.Run("sync record predates this feature (no adoption keys)", func(t *testing.T) {
		root := t.TempDir()
		writeSyncRunRecord(t, root, map[string]any{"extracted": 3, "absorbed": 1})
		snaps, ok := loadLatestAdoptionStats(root)
		if ok {
			t.Errorf("ok = true for a pre-feature record, want false; snaps=%v", snaps)
		}
	})

	t.Run("both windows present, parsed correctly", func(t *testing.T) {
		root := t.TempDir()
		writeSyncRunRecord(t, root, map[string]any{
			"adoption_kb_first_7d_ratio":  0.625,
			"adoption_kb_first_7d_num":    5,
			"adoption_kb_first_7d_den":    8,
			"adoption_kb_first_30d_ratio": 0.4,
			"adoption_kb_first_30d_num":   4,
			"adoption_kb_first_30d_den":   10,
		})
		snaps, ok := loadLatestAdoptionStats(root)
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if len(snaps) != 2 {
			t.Fatalf("snaps = %v, want 2 entries", snaps)
		}
		byDays := map[int]adoptionSnapshot{}
		for _, s := range snaps {
			byDays[s.Days] = s
		}
		if s := byDays[7]; s.Ratio != 0.625 || s.Numerator != 5 || s.Denominator != 8 {
			t.Errorf("7d snapshot = %+v, want ratio=0.625 num=5 den=8", s)
		}
		if s := byDays[30]; s.Ratio != 0.4 || s.Numerator != 4 || s.Denominator != 10 {
			t.Errorf("30d snapshot = %+v, want ratio=0.4 num=4 den=10", s)
		}
	})
}

// TestRenderStatusAdoptionBlock asserts the "KB-first adoption" block
// appears in `scribe status` output exactly when loadLatestAdoptionStats
// has data, formatted as a percentage (verifies the %5.0f%% * 100 math).
func TestRenderStatusAdoptionBlock(t *testing.T) {
	t.Run("no sync record — block absent", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("domains: [acme]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := renderStatus(&buf, root); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(buf.String(), "KB-first adoption") {
			t.Errorf("adoption block present with no sync record:\n%s", buf.String())
		}
	})

	t.Run("sync record present — block renders as a percentage", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("domains: [acme]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeSyncRunRecord(t, root, map[string]any{
			"adoption_kb_first_7d_ratio": 0.625,
			"adoption_kb_first_7d_num":   5,
			"adoption_kb_first_7d_den":   8,
		})
		var buf bytes.Buffer
		if err := renderStatus(&buf, root); err != nil {
			t.Fatal(err)
		}
		lines := normalizeLines(buf.String())
		if !containsLine(lines, "KB-first adoption (queried KB before first edit):") {
			t.Errorf("missing adoption header:\n%s", buf.String())
		}
		want := "7d: 62% (5/8 decision sessions)"
		if !containsLine(lines, want) {
			t.Errorf("missing line %q\ngot:\n%s", want, buf.String())
		}
	})
}

// normalizeLines collapses each non-empty output line's whitespace to
// single spaces so assertions don't encode the %-22s column padding.
func normalizeLines(out string) []string {
	var lines []string
	for line := range strings.SplitSeq(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			lines = append(lines, strings.Join(fields, " "))
		}
	}
	return lines
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}
