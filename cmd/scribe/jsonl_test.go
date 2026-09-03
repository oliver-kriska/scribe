package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type jsonlRow struct {
	ID string `json:"id"`
	N  int    `json:"n"`
}

func TestJSONLinesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "ledger.jsonl")
	in := []jsonlRow{{"a", 1}, {"b", 2}}
	if err := writeJSONLines(path, in); err != nil {
		t.Fatal(err)
	}
	out, skipped, err := readJSONLines[jsonlRow](path)
	if err != nil || skipped != 0 || len(out) != 2 || out[1].ID != "b" {
		t.Fatalf("round trip: rows=%v skipped=%d err=%v", out, skipped, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}
	// Empty rows remove the file; a missing file reads as empty.
	if err := writeJSONLines(path, []jsonlRow(nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("empty write must remove the ledger")
	}
	if out, _, err := readJSONLines[jsonlRow](path); err != nil || out != nil {
		t.Errorf("missing file: rows=%v err=%v", out, err)
	}
}

// TestJSONLinesTruncatedLineTerminates is the regression for the staleness
// ledger hang: a half-written trailing line must cost one row, not an
// infinite loop.
func TestJSONLinesTruncatedLineTerminates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	body := "{\"id\":\"a\",\"n\":1}\n{\"id\":\"b\",\"n\":2}\n{\"id\":\"c\",\"n\":"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var out []jsonlRow
	var skipped int
	go func() {
		out, skipped, _ = readJSONLines[jsonlRow](path)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not terminate on a truncated line")
	}
	if len(out) != 2 || skipped != 1 {
		t.Errorf("want 2 rows + 1 skipped, got %v skipped=%d", out, skipped)
	}
}

// TestStalenessLedgerTruncatedLineTerminates pins the real reader, not
// just the helper.
func TestStalenessLedgerTruncatedLineTerminates(t *testing.T) {
	root := t.TempDir()
	path := stalenessLedgerPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	good := StalenessEntry{Version: stalenessLedgerVersion, ID: "s-1", Path: "wiki/a.md"}
	if err := writeJSONLines(path, []StalenessEntry{good}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"version\":1,\"id\":\"s-2\",\"path\":\"wi"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	done := make(chan struct{})
	var entries []StalenessEntry
	go func() {
		entries, _ = readStalenessLedger(root)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("readStalenessLedger hung on a truncated line")
	}
	if len(entries) != 1 || entries[0].ID != "s-1" {
		t.Errorf("want the intact row only, got %+v", entries)
	}
}
