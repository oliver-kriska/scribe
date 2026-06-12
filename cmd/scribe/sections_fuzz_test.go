package main

// Native Go fuzz target for the section index extractor (Phase 5A).
// Section sidecars are built from arbitrary article bodies on every
// sync cycle; the byte/line ranges they persist are consumed later by
// `sections get` and tooling that slices the body with them — so
// out-of-bounds offsets are latent panics in every consumer.
//
// Runs as a plain unit test (seeds only) under `make test`. Pure
// function, no disk, no network.

import (
	"strings"
	"testing"
)

// FuzzExtractSections checks the section extractor invariants:
//
//  1. Never panics.
//  2. Levels are 1–3 (the regex's #{1,3} contract).
//  3. Byte ranges are in-bounds, non-empty, strictly ordered, and
//     contiguous: each section ends where the next begins, the last
//     ends at len(body) — so body[s.Bytes[0]:s.Bytes[1]] is always a
//     safe slice for every consumer.
//  4. Line ranges are 1-based, ordered, and within the body's line
//     count.
//  5. Anchor IDs are non-empty and unique within one article.
//
// Run longer:
//
//	go test ./cmd/scribe -tags sqlite_fts5 -run '^$' -fuzz '^FuzzExtractSections$' -fuzztime 5m
func FuzzExtractSections(f *testing.F) {
	seeds := []string{
		// sections_test.go shapes.
		"intro paragraph\n\n# Title\n\nfirst section body\n\n## Subhead\n\nsecond section body\n\n## Another\n\nthird section body\n",
		"just a paragraph\n\nanother paragraph\n",
		"# Setup\n\nA\n\n# Setup\n\nB\n\n# Setup\n\nC\n",
		"# A\nbody A\n# B\nbody B\n",
		// Heading edge cases: h4 (ignored), trailing whitespace, CRLF,
		// unicode titles, all-symbol titles (slug falls back), heading
		// at EOF with no trailing newline, empty body.
		"#### too deep\n## kept\n",
		"##   spaced out   \ncontent\n",
		"# CRLF heading\r\nbody\r\n## another\r\n",
		"# Setup & Configuration!\n# @@@\n#  \t\n",
		"text\n### last heading no newline",
		"", "#", "# ", "#x not a heading\n# real\n",
		"# šíření — ünïcödé ヘッダ\nbody\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		sections := extractSections(body)
		if len(sections) == 0 {
			return
		}
		lineCount := strings.Count(string(body), "\n") + 1
		seenIDs := make(map[string]bool, len(sections))
		for i, s := range sections {
			if s.Level < 1 || s.Level > 3 {
				t.Fatalf("section %d level %d out of range [1,3]", i, s.Level)
			}
			if s.ID == "" {
				t.Fatalf("section %d has empty anchor ID (title %q)", i, s.Title)
			}
			if seenIDs[s.ID] {
				t.Fatalf("duplicate anchor ID %q in one article", s.ID)
			}
			seenIDs[s.ID] = true

			start, end := s.Bytes[0], s.Bytes[1]
			if start < 0 || end > len(body) || start >= end {
				t.Fatalf("section %d byte range [%d,%d) out of bounds for body of %d bytes", i, start, end, len(body))
			}
			if i+1 < len(sections) && end != sections[i+1].Bytes[0] {
				t.Fatalf("section %d ends at %d but section %d starts at %d (not contiguous)", i, end, i+1, sections[i+1].Bytes[0])
			}
			if i == len(sections)-1 && end != len(body) {
				t.Fatalf("last section ends at %d, want len(body)=%d", end, len(body))
			}

			if s.Lines[0] < 1 || s.Lines[1] < s.Lines[0] || s.Lines[1] > lineCount {
				t.Fatalf("section %d line range %v out of bounds [1,%d]", i, s.Lines, lineCount)
			}
		}
	})
}
