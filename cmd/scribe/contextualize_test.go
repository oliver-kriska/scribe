package main

import (
	"strings"
	"testing"
)

func TestInsertRetrievalContext(t *testing.T) {
	paragraph := "This is the context."

	t.Run("inserts after frontmatter", func(t *testing.T) {
		in := "---\ntitle: foo\n---\nbody line 1\nbody line 2\n"
		got, err := insertRetrievalContext(in, paragraph)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, retrievalContextMarker) {
			t.Error("marker missing")
		}
		if !strings.Contains(got, "body line 1") {
			t.Error("body lost")
		}
		// Marker must come after the closing --- and before body.
		mkIdx := strings.Index(got, retrievalContextMarker)
		closingIdx := strings.Index(got, "\n---\n")
		bodyIdx := strings.Index(got, "body line 1")
		if closingIdx >= mkIdx || mkIdx >= bodyIdx {
			t.Errorf("ordering wrong: closing=%d marker=%d body=%d", closingIdx, mkIdx, bodyIdx)
		}
	})

	t.Run("no-op when marker already present", func(t *testing.T) {
		in := "---\ntitle: foo\n---\n" + retrievalContextMarker + "\nbody\n"
		got, err := insertRetrievalContext(in, paragraph)
		if err != nil {
			t.Fatal(err)
		}
		if got != in {
			t.Errorf("content should be unchanged when marker present")
		}
	})

	t.Run("prepends when no frontmatter", func(t *testing.T) {
		in := "body without frontmatter\n"
		got, err := insertRetrievalContext(in, paragraph)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(strings.TrimLeft(got, "\n"), retrievalContextMarker) {
			t.Error("marker should be near the top when no frontmatter")
		}
		if !strings.Contains(got, "body without frontmatter") {
			t.Error("body lost")
		}
	})

	t.Run("errors on malformed frontmatter", func(t *testing.T) {
		in := "---\nno closing delimiter here\n"
		_, err := insertRetrievalContext(in, paragraph)
		if err == nil {
			t.Error("expected error for malformed frontmatter")
		}
	})
}
