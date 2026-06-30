package main

import "testing"

// qmd collection list output captured from a real install (issue #27 item 5).
const qmdCollectionListFixture = `Collections (2):

scriptorium (qmd://scriptorium/)
  Pattern:  **/*.md
  Files:    9478
  Updated:  1d ago

enaia (qmd://enaia/) [excluded]
  Pattern:  **/*.md
  Files:    1243
  Updated:  2d ago
`

func TestQmdCollectionFilesUpdated(t *testing.T) {
	files, updated := qmdCollectionFilesUpdated(qmdCollectionListFixture, "scriptorium")
	if files != "9478" || updated != "1d ago" {
		t.Errorf("scriptorium: files=%q updated=%q, want 9478 / 1d ago", files, updated)
	}

	// The second block must not bleed into the first's numbers.
	files, updated = qmdCollectionFilesUpdated(qmdCollectionListFixture, "enaia")
	if files != "1243" || updated != "2d ago" {
		t.Errorf("enaia: files=%q updated=%q, want 1243 / 2d ago", files, updated)
	}

	// A collection that doesn't exist → empty, no panic.
	if files, _ := qmdCollectionFilesUpdated(qmdCollectionListFixture, "nope"); files != "" {
		t.Errorf("missing collection: files=%q, want empty", files)
	}
}

func TestQmdField(t *testing.T) {
	show := "Collection: scriptorium\n  Path:     /Users/x/Projects/scriptorium\n  Pattern:  **/*.md\n"
	if got := qmdField(show, "Path:"); got != "/Users/x/Projects/scriptorium" {
		t.Errorf("Path = %q", got)
	}
	if got := qmdField(show, "Missing:"); got != "" {
		t.Errorf("absent field = %q, want empty", got)
	}
}
