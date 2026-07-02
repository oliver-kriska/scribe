package main

import (
	"testing"
)

// TestFetchSessionTranscript_ToolRows is a smoke test closing a
// pre-existing gap: session_transcript.go's fetchSessionTranscript reads
// the ccrider `content` and `sequence` columns (session_transcript.go:58-59)
// but had no test at all, and the fixture schema didn't even carry those
// columns until the issue #23 adoption-metric plan's Step 1 added them.
//
// Covers two shapes:
//   - the real-world one (adoption.go's file comment): an assistant-type
//     row whose extracted text_content is empty but whose content column
//     carries the raw tool payload — fetchSessionTranscript must fall back
//     to content for ToolText.
//   - the defensive one roleFromCcriderType still special-cases ("tool")
//     for forward compatibility with a hypothetical future ccrider split,
//     even though no current ccrider version emits it.
func TestFetchSessionTranscript_ToolRows(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	sid := insertFixtureSession(t, db, "sess-1", "/p/alpha", 3, "", "", "mixed transcript")

	insertFixtureToolMessage(t, db, sid, "user", "", 1)
	// user row needs text_content, not content, to be readable as dialog —
	// use raw SQL since insertFixtureToolMessage only sets content.
	//nolint:noctx // test fixture
	if _, err := db.Exec(`UPDATE messages SET text_content = ? WHERE session_id = ? AND sequence = 1`,
		"what should I do first?", sid); err != nil {
		t.Fatal(err)
	}

	// Real-world shape: assistant turn that's purely a tool call — empty
	// text_content, content carries the raw payload.
	insertFixtureToolMessage(t, db, sid, "assistant", editContent, 2)

	// Defensive shape: a literal "tool" type row, per roleFromCcriderType's
	// forward-compat case (never emitted by current ccrider, per
	// adoption.go's derivation, but the mapping function still handles it).
	insertFixtureToolMessage(t, db, sid, "tool", qmdMCPContent, 3)

	turns, err := fetchSessionTranscript(dbPath, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 {
		t.Fatalf("got %d turns, want 3: %+v", len(turns), turns)
	}

	// Sequence order preserved.
	if turns[0].Sequence != 1 || turns[1].Sequence != 2 || turns[2].Sequence != 3 {
		t.Errorf("turns out of sequence order: %+v", turns)
	}

	if turns[0].Role != "user" || turns[0].Text != "what should I do first?" {
		t.Errorf("turn 0 = %+v, want user dialog", turns[0])
	}

	if turns[1].Role != "assistant" || turns[1].ToolText != editContent {
		t.Errorf("turn 1 = %+v, want assistant role with ToolText=editContent (content fallback)", turns[1])
	}

	if turns[2].Role != "tool" || turns[2].ToolText != qmdMCPContent {
		t.Errorf("turn 2 = %+v, want tool role with ToolText=qmdMCPContent", turns[2])
	}
}
