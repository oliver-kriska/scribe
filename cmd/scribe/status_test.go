package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
