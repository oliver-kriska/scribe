package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// appleNanos converts a wall-clock time into the chat.db date column unit
// (nanoseconds since 2001-01-01 UTC).
func appleNanos(t time.Time) int64 {
	return (t.UTC().Unix() - appleEpochOffset) * 1_000_000_000
}

func TestMessageURLs(t *testing.T) {
	t.Run("text column wins", func(t *testing.T) {
		m := chatMessage{
			Text:           "check https://example.com/a and https://example.com/b",
			AttributedBody: []byte("https://from-blob.example/ignored"),
		}
		urls := messageURLs(m)
		if len(urls) != 2 || urls[0] != "https://example.com/a" || urls[1] != "https://example.com/b" {
			t.Errorf("urls = %v", urls)
		}
	})

	t.Run("attributedBody fallback with WHttpURL sentinel", func(t *testing.T) {
		blob := append([]byte{0x01, 0x02}, []byte("https://example.com/pathWHttpURL\x01garbage")...)
		m := chatMessage{Text: "no links here", AttributedBody: blob}
		urls := messageURLs(m)
		if len(urls) != 1 || urls[0] != "https://example.com/path" {
			t.Errorf("urls = %v, want clean URL truncated at sentinel", urls)
		}
	})

	t.Run("no urls anywhere", func(t *testing.T) {
		if urls := messageURLs(chatMessage{Text: "plain note"}); urls != nil {
			t.Errorf("urls = %v, want nil", urls)
		}
	})
}

func TestSortCapturedMessages(t *testing.T) {
	ts := appleNanos(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	messages := []chatMessage{
		{Text: "https://example.com/article.", Date: ts},           // trailing punctuation trimmed
		{Text: "https://example.com/article", Date: ts},            // dup of the above after trim
		{Text: "https://maps.google.com/place/x", Date: ts},        // skip domain
		{Text: "https://already.example/seen", Date: ts},           // pre-captured
		{Text: "Permission request https://x.example/y", Date: ts}, // control message
		{Text: "an idea worth keeping around", Date: ts},           // note
		{Text: "Liked an idea worth keeping around", Date: ts},     // reaction noise
		{Text: "short", Date: ts},                                  // <=10 chars
	}
	captured := map[string]bool{"https://already.example/seen": true}

	urls, notes := sortCapturedMessages(messages, captured, []string{"maps.google.com"})

	if len(urls) != 1 {
		t.Fatalf("urls = %+v, want 1", urls)
	}
	u := urls[0]
	if u.URL != "https://example.com/article" {
		t.Errorf("URL = %q, want punctuation trimmed", u.URL)
	}
	if u.Date != "2026-06-01" {
		t.Errorf("Date = %q", u.Date)
	}
	if !captured["https://example.com/article"] {
		t.Error("accepted URL not marked captured")
	}

	if len(notes) != 1 || notes[0].Text != "an idea worth keeping around" {
		t.Errorf("notes = %+v", notes)
	}
}

func TestWriteURLCaptures(t *testing.T) {
	rawDir := t.TempDir()
	item := capturedURL{
		URL:        "https://example.com/some/article",
		Date:       "2026-06-01",
		SourceText: "https://example.com/some/article",
	}

	t.Run("writes a stub article", func(t *testing.T) {
		c := &CaptureCmd{}
		saved := c.writeURLCaptures(rawDir, []capturedURL{item})
		if len(saved) != 1 {
			t.Fatalf("saved = %v", saved)
		}
		wantName := "2026-06-01-imessage-example-com-some-article.md"
		if saved[0] != wantName {
			t.Errorf("filename = %q, want %q", saved[0], wantName)
		}
		data, err := os.ReadFile(filepath.Join(rawDir, wantName))
		if err != nil {
			t.Fatal(err)
		}
		content := string(data)
		for _, want := range []string{
			`source_url: "https://example.com/some/article"`,
			"fetched_via: stub",
			"captured: 2026-06-01",
			"tags: [imessage]",
			"Captured from iMessage self-chat on 2026-06-01.",
		} {
			if !strings.Contains(content, want) {
				t.Errorf("missing %q in:\n%s", want, content)
			}
		}
	})

	t.Run("existing file is not overwritten", func(t *testing.T) {
		c := &CaptureCmd{}
		saved := c.writeURLCaptures(rawDir, []capturedURL{item})
		if len(saved) != 0 {
			t.Errorf("second pass saved %v, want none", saved)
		}
	})

	t.Run("dry run writes nothing", func(t *testing.T) {
		dryDir := t.TempDir()
		c := &CaptureCmd{DryRun: true}
		var saved []string
		out := captureLintStdout(t, func() {
			saved = c.writeURLCaptures(dryDir, []capturedURL{item})
		})
		if len(saved) != 0 {
			t.Errorf("dry run saved %v", saved)
		}
		entries, _ := os.ReadDir(dryDir)
		if len(entries) != 0 {
			t.Errorf("dry run created files: %v", entries)
		}
		if !strings.Contains(out, "WOULD CAPTURE") {
			t.Errorf("dry run output:\n%s", out)
		}
	})
}

func TestWriteNoteCaptures(t *testing.T) {
	rawDir := t.TempDir()
	long := strings.Repeat("idea ", 30) // >80 chars, title must truncate
	notes := []capturedNote{
		{Text: long, Date: "2026-06-02"},
	}

	c := &CaptureCmd{}
	saved := c.writeNoteCaptures(rawDir, notes)
	if len(saved) != 1 {
		t.Fatalf("saved = %v", saved)
	}
	if !strings.HasPrefix(saved[0], "2026-06-02-imessage-note-") {
		t.Errorf("filename = %q", saved[0])
	}
	data, err := os.ReadFile(filepath.Join(rawDir, saved[0]))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "fetched_via: imessage-note") {
		t.Errorf("missing via marker:\n%s", content)
	}
	if !strings.Contains(content, "tags: [imessage, note]") {
		t.Errorf("missing tags:\n%s", content)
	}
	// Title is clipped to 80 chars; body keeps the full text.
	titleLine := ""
	for line := range strings.SplitSeq(content, "\n") {
		if strings.HasPrefix(line, "title:") {
			titleLine = line
			break
		}
	}
	if len(titleLine) > len(`title: ""`)+80 {
		t.Errorf("title not truncated: %q", titleLine)
	}
	if !strings.Contains(content, strings.TrimSpace(long)) {
		t.Errorf("full note text missing from body:\n%s", content)
	}

	// Existing note is skipped on a second run.
	if again := c.writeNoteCaptures(rawDir, notes); len(again) != 0 {
		t.Errorf("second pass saved %v", again)
	}
}

func TestWriteCaptureFile_CreatesParents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep", "nested", "file.md")
	if err := writeCaptureFile(path, "content\n"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "content\n" {
		t.Errorf("read back: %q, %v", data, err)
	}
}

func TestCaptureStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scripts", "imessage-state.json")

	t.Run("missing file yields zero state", func(t *testing.T) {
		st, err := loadCaptureState(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.LastCapture != nil || st.CapturedCount != 0 || len(st.CapturedURLs) != 0 {
			t.Errorf("zero state = %+v", st)
		}
	})

	t.Run("save creates parents and round-trips", func(t *testing.T) {
		last := "2026-06-01"
		st := &captureState{
			LastCapture:   &last,
			CapturedURLs:  []string{"https://example.com/a"},
			CapturedCount: 7,
		}
		if err := saveCaptureState(path, st); err != nil {
			t.Fatal(err)
		}
		if fileExists(path + ".tmp") {
			t.Error("tmp file left behind")
		}
		got, err := loadCaptureState(path)
		if err != nil {
			t.Fatal(err)
		}
		if got.LastCapture == nil || *got.LastCapture != "2026-06-01" {
			t.Errorf("LastCapture = %v", got.LastCapture)
		}
		if got.CapturedCount != 7 || len(got.CapturedURLs) != 1 {
			t.Errorf("round-trip lost data: %+v", got)
		}
	})

	t.Run("malformed JSON is an error not a reset", func(t *testing.T) {
		bad := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(bad, []byte("{nope"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadCaptureState(bad); err == nil {
			t.Error("want parse error — silently resetting state would re-capture everything")
		}
	})
}
