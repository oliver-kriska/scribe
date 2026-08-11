package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// --- D1: queue-entry injection via remote-controlled fields ---

// TestQueueEntryTitleCannotInjectURL is the D1 regression. A pull adapter
// feeds REMOTE text into the line-based queue format — Pinboard's bookmarklet
// fills a bookmark's title from the page's own <title>. Before the fix,
// renderQueueEntry emitted the title raw and parseQueueEntry took the LAST
// occurrence of a key, so a newline in the title let the page forge its own
// `url:` line and redirect what drainOne fetched and absorbed.
func TestQueueEntryTitleCannotInjectURL(t *testing.T) {
	const realURL = "https://real.example/article"
	const payload = "https://attacker.example/payload"

	inbox := t.TempDir()
	path, err := writeQueueEntry(inbox, sourceItemToQueue("pinboard", SourceItem{
		URL:   realURL,
		Title: "Benign Title\nurl: " + payload,
		ID:    "h1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "\nurl: "+payload) {
		t.Errorf("injected line survived into the queue file:\n%s", data)
	}
	entry := parseQueueEntry(string(data))
	if entry["url"] != realURL {
		t.Errorf("drainOne would fetch %q, want %q\nfile:\n%s", entry["url"], realURL, data)
	}
	// The title text itself is preserved, just flattened onto one line.
	if !strings.Contains(entry["title"], "Benign Title") {
		t.Errorf("title lost its content: %q", entry["title"])
	}
}

// TestQueueEntryFieldsRejectLineBreaks covers every single-line field, not
// just the title, so a future adapter mapping remote data onto tags/domain/
// source can't reopen the hole.
func TestQueueEntryFieldsRejectLineBreaks(t *testing.T) {
	const realURL = "https://real.example/x"
	inbox := t.TempDir()
	// The URL itself is the one field scribe intends to fetch, so it is not
	// the attack surface — everything else here is remote-controlled text a
	// pull adapter maps straight off a bookmark.
	path, err := writeQueueEntry(inbox, queueFields{
		URL:    realURL,
		Title:  "T\r\nurl: https://evil.example/2",
		Tags:   []string{"ok", "bad\nurl: https://evil.example/3"},
		Domain: "general\nurl: https://evil.example/4",
		Source: "pinboard\nurl: https://evil.example/5",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entry := parseQueueEntry(string(data))
	if entry["url"] != realURL {
		t.Errorf("url = %q, want %q\nfile:\n%s", entry["url"], realURL, data)
	}
	// Exactly one LINE may begin with `url:`. The injected text survives as
	// inline characters inside a flattened value, which is harmless — what
	// matters is that it can never start a line of its own.
	urlLines := 0
	for _, ln := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "url:") {
			urlLines++
		}
	}
	if urlLines != 1 {
		t.Errorf("want exactly one url: line, found %d:\n%s", urlLines, data)
	}
}

// TestParseQueueEntryFirstKeyWins pins the belt-and-braces half: even for a
// file this process did not render (hand-edited, or a future producer that
// forgets to sanitize), an appended duplicate key cannot override the real
// value.
func TestParseQueueEntryFirstKeyWins(t *testing.T) {
	raw := "url: https://real.example/a\ntitle: T\nurl: https://attacker.example/b\n"
	if got := parseQueueEntry(raw)["url"]; got != "https://real.example/a" {
		t.Errorf("url = %q, want the first occurrence to win", got)
	}
}

// TestQueueLineValueKeepsOrdinaryText guards against over-correcting: normal
// titles must pass through untouched.
func TestQueueLineValueKeepsOrdinaryText(t *testing.T) {
	for _, s := range []string{
		"A Perfectly Normal Title",
		"Title with: a colon",
		"Ünïcödé — em dash, quotes “x”",
	} {
		if got := queueLineValue(s); got != s {
			t.Errorf("queueLineValue(%q) = %q, want unchanged", s, got)
		}
	}
}

// --- C1: mid-run checkpointing ---

// TestPullSourceCheckpointsMidRun is the C1 regression. CLAUDE.md requires
// checkpointed writes; before the fix pullSource persisted state exactly once,
// after the loop, so a killed --all-history backfill that had already written
// hundreds of inbox entries recorded no seen-set and re-queued every one of
// them on the next run.
func TestPullSourceCheckpointsMidRun(t *testing.T) {
	root := testKB(t, "")

	// Observe every state write through the seam (mirrors eachRunner's shape).
	type snapshot struct {
		seen   int
		cursor string
	}
	var writes []snapshot
	orig := saveSourceStateFn
	saveSourceStateFn = func(path string, st *sourceState) error {
		writes = append(writes, snapshot{seen: len(st.Seen), cursor: string(st.Cursor)})
		return orig(path, st)
	}
	t.Cleanup(func() { saveSourceStateFn = orig })

	const total = 60
	items := make([]SourceItem, 0, total)
	for i := range total {
		items = append(items, item(fmt.Sprintf("https://x%d.example", i), fmt.Sprintf("id-%d", i)))
	}
	src := &fakeSource{name: "fake", cursor: "v1", items: items}

	n, err := pullSource(root, src, FetchOpts{}, 0, false)
	if err != nil {
		t.Fatalf("pullSource: %v", err)
	}
	if n != total {
		t.Fatalf("queued %d, want %d", n, total)
	}

	// 60 items at one checkpoint per 25, plus the final write = 3.
	wantCheckpoints := total / sourceCheckpointEvery
	if len(writes) != wantCheckpoints+1 {
		t.Errorf("state written %d times, want %d checkpoints + 1 final", len(writes), wantCheckpoints)
	}
	if writes[0].seen != sourceCheckpointEvery {
		t.Errorf("first checkpoint recorded %d seen ids, want %d", writes[0].seen, sourceCheckpointEvery)
	}

	// The cursor must NOT advance on a mid-run checkpoint: a crash with an
	// advanced cursor would let the next run's unchanged-probe short-circuit
	// the un-queued remainder away for good.
	for i, w := range writes[:len(writes)-1] {
		if w.cursor != "" {
			t.Errorf("checkpoint %d advanced the cursor to %q; only a complete pass may", i, w.cursor)
		}
	}
	if last := writes[len(writes)-1]; last.cursor == "" {
		t.Error("final write did not advance the cursor after a complete pass")
	}
}

// TestCheckpointedStateResumesWithoutRequeueing proves the property the
// checkpoint exists for: after an interrupted run, the items already written
// are not queued a second time.
func TestCheckpointedStateResumesWithoutRequeueing(t *testing.T) {
	root := testKB(t, "")
	statePath := sourceStatePath(root, "fake")

	// Simulate a run killed after 30 of 60 items: the checkpoint at 25 is on
	// disk, the final write never happened.
	seen := map[string]bool{}
	for i := range 25 {
		seen[fmt.Sprintf("id-%d", i)] = true
	}
	if err := checkpointSourceState(statePath, &sourceState{}, seen); err != nil {
		t.Fatal(err)
	}

	st, err := loadSourceState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Seen) != 25 {
		t.Fatalf("checkpoint persisted %d ids, want 25", len(st.Seen))
	}
	if len(st.Cursor) != 0 {
		t.Errorf("checkpoint persisted a cursor (%s); it must stay unset until a complete pass", st.Cursor)
	}

	const total = 60
	items := make([]SourceItem, 0, total)
	for i := range total {
		items = append(items, item(fmt.Sprintf("https://x%d.example", i), fmt.Sprintf("id-%d", i)))
	}
	n, err := pullSource(root, &fakeSource{name: "fake", cursor: "v1", items: items}, FetchOpts{}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != total-25 {
		t.Errorf("resumed run queued %d, want %d (the 25 checkpointed ids must not re-queue)", n, total-25)
	}
	if got := inboxFiles(t, root); len(got) != total-25 {
		t.Errorf("inbox has %d entries, want %d — checkpointed items were re-queued", len(got), total-25)
	}
}

// TestCheckpointFailureDoesNotAbortRun — a checkpoint is an optimization, not
// a correctness requirement, so a failing one must be logged and stepped over
// rather than losing the whole pull.
func TestCheckpointFailureDoesNotAbortRun(t *testing.T) {
	root := testKB(t, "")
	orig := saveSourceStateFn
	calls := 0
	saveSourceStateFn = func(path string, st *sourceState) error {
		calls++
		if calls == 1 { // the first mid-run checkpoint
			return errors.New("simulated disk failure")
		}
		return orig(path, st)
	}
	t.Cleanup(func() { saveSourceStateFn = orig })

	const total = 30
	items := make([]SourceItem, 0, total)
	for i := range total {
		items = append(items, item(fmt.Sprintf("https://y%d.example", i), fmt.Sprintf("id-%d", i)))
	}
	n, err := pullSource(root, &fakeSource{name: "fake", cursor: "v1", items: items}, FetchOpts{}, 0, false)
	if err != nil {
		t.Fatalf("a failed checkpoint must not fail the run: %v", err)
	}
	if n != total {
		t.Errorf("queued %d, want %d", n, total)
	}
	st, err := loadSourceState(sourceStatePath(root, "fake"))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Seen) != total {
		t.Errorf("final state has %d seen ids, want %d", len(st.Seen), total)
	}
	var cur map[string]string
	if err := json.Unmarshal(st.Cursor, &cur); err != nil || cur["c"] != "v1" {
		t.Errorf("cursor = %s, want it advanced after the complete pass", st.Cursor)
	}
}
