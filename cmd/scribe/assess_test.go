package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHumanFileSize covers the three size buckets and the missing-file
// case. It's used only for log output, so a regression wouldn't corrupt
// data — it just makes logs misleading.
func TestHumanFileSize(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name    string
		bytes   int
		wantSub string
	}{
		{"tiny", 42, "42b"},
		{"kilobyte", 2048, "2.0k"},
		{"megabyte", 2 * 1024 * 1024, "2.0M"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, make([]byte, tc.bytes), 0o644); err != nil {
				t.Fatal(err)
			}
			got := humanFileSize(path)
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("got %q, want to contain %q", got, tc.wantSub)
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		if got := humanFileSize(filepath.Join(dir, "nope")); got != "missing" {
			t.Errorf("got %q, want \"missing\"", got)
		}
	})
}

// TestAssessTracksCovered is a static check: the assessTracks slice and
// the assess-consolidate prompt template's placeholders must stay in
// sync. If someone adds a track to the Go slice but forgets the prompt
// template variable, the consolidation call receives unsubstituted
// "{{NEW_OUT}}" text and writes nonsense.
func TestAssessTracksCovered(t *testing.T) {
	data, err := promptFS.ReadFile("prompts/assess-consolidate.md")
	if err != nil {
		t.Fatal(err)
	}
	tmpl := string(data)
	for _, track := range assessTracks {
		// Prompt uses uppercase track name + "_OUT" placeholder.
		placeholder := "{{" + strings.ToUpper(track.name) + "_OUT}}"
		if !strings.Contains(tmpl, placeholder) {
			t.Errorf("consolidate prompt missing placeholder %s for track %q",
				placeholder, track.name)
		}
		// The track's own prompt must exist in the embedded FS.
		if _, err := promptFS.ReadFile("prompts/" + track.prompt); err != nil {
			t.Errorf("track %q references missing prompt %s: %v",
				track.name, track.prompt, err)
		}
	}
}
