package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSource is a deterministic Source for driver tests. It records how many
// times Fetch ran so short-circuit/dedup behavior is observable.
type fakeSource struct {
	name       string
	items      []SourceItem
	cursor     string
	fetchCount int
}

func (f *fakeSource) Name() string                            { return f.name }
func (f *fakeSource) Configured(*ScribeConfig) (bool, string) { return true, "" }
func (f *fakeSource) Fetch(_ context.Context, _ *ScribeConfig, _ json.RawMessage, _ FetchOpts) ([]SourceItem, json.RawMessage, error) {
	f.fetchCount++
	cur, err := json.Marshal(map[string]string{"c": f.cursor})
	if err != nil {
		return nil, nil, err
	}
	return f.items, cur, nil
}

// testKB writes a minimal scribe.yaml pointing lock_dir at an isolated temp
// dir, so pullSource never touches the machine-wide /tmp lock the real cron
// uses. Extra yaml lines let callers add an integrations block.
func testKB(t *testing.T, extraYAML string) string {
	t.Helper()
	root := t.TempDir()
	lockDir := t.TempDir()
	yaml := "lock_dir: " + lockDir + "\n" + extraYAML
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func inboxFiles(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "output", "inbox"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".url") {
			out = append(out, e.Name())
		}
	}
	return out
}

func item(url, id string) SourceItem { return SourceItem{URL: url, ID: id, Title: id} }

func TestPullSourceQueuesAndDedups(t *testing.T) {
	root := testKB(t, "")
	src := &fakeSource{name: "fake", cursor: "v1", items: []SourceItem{
		item("https://a.com", "a"),
		item("https://b.com", "b"),
		item("https://c.com", "c"),
	}}

	n, err := pullSource(root, src, FetchOpts{}, 0, false)
	if err != nil {
		t.Fatalf("pullSource: %v", err)
	}
	if n != 3 {
		t.Fatalf("queued %d, want 3", n)
	}
	if got := inboxFiles(t, root); len(got) != 3 {
		t.Fatalf("inbox has %d .url files, want 3: %v", len(got), got)
	}

	// State persisted: seen has all three, cursor advanced.
	st, err := loadSourceState(sourceStatePath(root, "fake"))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Seen) != 3 {
		t.Errorf("seen = %v, want 3 ids", st.Seen)
	}
	if len(st.Cursor) == 0 {
		t.Error("cursor not persisted after a complete pass")
	}
	if st.LastPull == "" {
		t.Error("last_pull not stamped")
	}

	// Second run with the same items → nothing new queued (dedup via seen).
	n2, err := pullSource(root, src, FetchOpts{}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Errorf("second run queued %d, want 0 (all seen)", n2)
	}
	if got := inboxFiles(t, root); len(got) != 3 {
		t.Errorf("inbox grew to %d, want still 3", len(got))
	}
}

func TestPullSourceMaxCapDoesNotAdvanceCursor(t *testing.T) {
	root := testKB(t, "")
	src := &fakeSource{name: "fake", cursor: "v1", items: []SourceItem{
		item("https://1", "1"), item("https://2", "2"), item("https://3", "3"),
		item("https://4", "4"), item("https://5", "5"),
	}}

	// Capped run: only 2 of 5 queued, cursor must NOT advance so a later
	// backfill run resumes instead of being short-circuited away.
	n, err := pullSource(root, src, FetchOpts{}, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("queued %d, want 2 (capped)", n)
	}
	st, _ := loadSourceState(sourceStatePath(root, "fake"))
	if len(st.Cursor) != 0 {
		t.Errorf("cursor advanced on a capped run: %s", st.Cursor)
	}
	if len(st.Seen) != 2 {
		t.Errorf("seen = %v, want the 2 queued ids", st.Seen)
	}

	// Uncapped follow-up queues the remaining 3 and advances the cursor.
	n2, err := pullSource(root, src, FetchOpts{}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 3 {
		t.Fatalf("follow-up queued %d, want 3", n2)
	}
	st2, _ := loadSourceState(sourceStatePath(root, "fake"))
	if len(st2.Cursor) == 0 {
		t.Error("cursor not advanced after a complete pass")
	}
	if len(st2.Seen) != 5 {
		t.Errorf("seen = %v, want all 5", st2.Seen)
	}
}

func TestPullSourceSkipDomains(t *testing.T) {
	root := testKB(t, "integrations:\n  fake:\n    skip_domains: [\"skip.com\"]\n")
	src := &fakeSource{name: "fake", items: []SourceItem{
		item("https://keep.com/x", "k"),
		item("https://skip.com/y", "s"),
	}}
	n, err := pullSource(root, src, FetchOpts{}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("queued %d, want 1 (skip.com filtered)", n)
	}
	files := inboxFiles(t, root)
	if len(files) != 1 || !strings.Contains(files[0], "keep-com") {
		t.Errorf("inbox = %v, want only the keep.com entry", files)
	}
	// Skipped item is NOT marked seen (a later config change may allow it).
	st, _ := loadSourceState(sourceStatePath(root, "fake"))
	for _, id := range st.Seen {
		if id == "s" {
			t.Error("skipped item marked seen; should stay eligible")
		}
	}
}

func taggedItem(url, id string, tags ...string) SourceItem {
	return SourceItem{URL: url, ID: id, Title: id, Tags: tags}
}

func TestPullSourceTagFilterFromConfig(t *testing.T) {
	// OR filter: keep bookmarks carrying at least one configured tag, case-
	// insensitively; drop the rest; untagged config ingests all.
	root := testKB(t, "integrations:\n  fake:\n    tags: [\"kb\", \"read\"]\n")
	src := &fakeSource{name: "fake", items: []SourceItem{
		taggedItem("https://a.com", "a", "go", "KB"),  // has kb (case-insensitive) → keep
		taggedItem("https://b.com", "b", "rust"),      // no match → drop
		taggedItem("https://c.com", "c", "read", "x"), // has read → keep
		taggedItem("https://d.com", "d"),              // no tags → drop
	}}
	n, err := pullSource(root, src, FetchOpts{}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("queued %d, want 2 (a and c)", n)
	}
	st, _ := loadSourceState(sourceStatePath(root, "fake"))
	// Filtered-out items are NOT marked seen (widening the filter re-includes).
	for _, id := range st.Seen {
		if id == "b" || id == "d" {
			t.Errorf("filtered item %q marked seen; should stay eligible", id)
		}
	}
}

func TestPullSourceTagFilterOverride(t *testing.T) {
	// A per-run --tag override (FetchOpts.Tags) wins over the configured tags.
	root := testKB(t, "integrations:\n  fake:\n    tags: [\"kb\"]\n")
	src := &fakeSource{name: "fake", items: []SourceItem{
		taggedItem("https://a.com", "a", "kb"),
		taggedItem("https://b.com", "b", "elixir"),
	}}
	n, err := pullSource(root, src, FetchOpts{Tags: []string{"elixir"}}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("queued %d, want 1 (override selects elixir, not kb)", n)
	}
}

func TestPullSourceNoTagFilterKeepsAll(t *testing.T) {
	root := testKB(t, "")
	src := &fakeSource{name: "fake", items: []SourceItem{
		taggedItem("https://a.com", "a", "go"),
		taggedItem("https://b.com", "b"),
	}}
	n, err := pullSource(root, src, FetchOpts{}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("queued %d, want 2 (no filter = all)", n)
	}
}

func TestPullSourceDryRunWritesNothing(t *testing.T) {
	root := testKB(t, "")
	src := &fakeSource{name: "fake", items: []SourceItem{item("https://a.com", "a")}}
	n, err := pullSource(root, src, FetchOpts{}, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("dry-run reported %d would-queue, want 1", n)
	}
	if got := inboxFiles(t, root); len(got) != 0 {
		t.Errorf("dry-run wrote %d files, want 0", len(got))
	}
	if _, err := os.Stat(sourceStatePath(root, "fake")); !os.IsNotExist(err) {
		t.Error("dry-run wrote a state file, want none")
	}
}

func TestSourceItemToQueue(t *testing.T) {
	it := SourceItem{
		URL:    "https://x.com",
		Title:  "X",
		Tags:   []string{"go"},
		Note:   "annotation",
		Unread: true,
	}
	f := sourceItemToQueue("pinboard", it)
	if f.Source != "pinboard" {
		t.Errorf("Source = %q", f.Source)
	}
	if f.Note != "annotation" {
		t.Errorf("Note = %q", f.Note)
	}
	if !containsFold(f.Tags, "to-read") {
		t.Errorf("Tags = %v, want to-read added for unread", f.Tags)
	}
	if f.Domain != "general" {
		t.Errorf("Domain = %q, want general", f.Domain)
	}
}

func TestQueueNoteRoundTrip(t *testing.T) {
	cases := []string{
		"single line",
		"line one\nline two",
		"has a \\ backslash",
		"windows\r\nnewline",
		"",
	}
	for _, in := range cases {
		got := decodeQueueNote(encodeQueueNote(in))
		want := strings.ReplaceAll(in, "\r\n", "\n")
		if got != want {
			t.Errorf("roundtrip(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(encodeQueueNote(in), "\n") {
			t.Errorf("encoded note still contains a raw newline: %q", encodeQueueNote(in))
		}
	}
}

func TestQueueNoteBlockquote(t *testing.T) {
	got := queueNoteBlockquote("first\nsecond")
	if got != "> first\n> second" {
		t.Errorf("blockquote = %q", got)
	}
}

func TestWriteQueueEntryRoundTripsNoteAndSource(t *testing.T) {
	inbox := t.TempDir()
	path, err := writeQueueEntry(inbox, queueFields{
		URL:    "https://x.com",
		Title:  "X",
		Tags:   []string{"go", "pinboard"},
		Note:   "line one\nline two",
		Source: "pinboard",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entry := parseQueueEntry(string(data))
	if entry["url"] != "https://x.com" || entry["source"] != "pinboard" {
		t.Errorf("parsed entry missing fields: %v", entry)
	}
	if decodeQueueNote(entry["note"]) != "line one\nline two" {
		t.Errorf("note did not round-trip: %q", entry["note"])
	}
}

func TestWriteQueueEntryUniquePaths(t *testing.T) {
	inbox := t.TempDir()
	// Same URL twice → the second must not overwrite the first.
	p1, err := writeQueueEntry(inbox, queueFields{URL: "https://x.com"})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := writeQueueEntry(inbox, queueFields{URL: "https://x.com"})
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 {
		t.Errorf("both entries wrote to %s; expected disambiguated paths", p1)
	}
}
