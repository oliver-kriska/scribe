package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// JSONL ledgers (staleness, contradictions) share one reader and one
// writer so they cannot drift apart in durability.
//
// The reader is line-based on purpose. The previous staleness reader
// used json.Decoder in a loop and `continue`d on a decode error — but a
// Decoder latches its first error, so one truncated line made the loop
// spin forever inside `scribe doctor`, `stale` and every team sync.
// Splitting on newlines and skipping the lines that do not parse turns
// that into "one lost row".
//
// The writer is tmp + rename so a crash or full disk can never leave the
// half-written file that produced that truncated line in the first place.

// readJSONLines decodes every parseable line of path into T. A missing
// file is (nil, nil). Lines that are blank or do not decode are skipped;
// the count of skipped lines is returned so callers can log it.
func readJSONLines[T any](path string) (rows []T, skipped int, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row T
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			skipped++
			continue
		}
		rows = append(rows, row)
	}
	return rows, skipped, nil
}

// writeJSONLines encodes rows one per line and replaces path atomically.
// An empty rows slice removes the file (a ledger with nothing in it is
// represented by absence, which is what every reader already expects).
func writeJSONLines[T any](path string, rows []T) error {
	if len(rows) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := range rows {
		if err := enc.Encode(rows[i]); err != nil {
			return fmt.Errorf("encode row %d: %w", i, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(path, buf.Bytes(), 0o644)
}
