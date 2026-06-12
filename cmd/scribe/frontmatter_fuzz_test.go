package main

// Native Go fuzz targets for the frontmatter parsing surface. Every
// article scribe touches is LLM-written, so malformed/hostile input is
// the NORMAL case for these parsers — a panic here is a production
// crash on the next cron run.
//
// All targets run as plain unit tests (seeds only) under `make test`.
// No network, no user paths — pure functions over in-memory bytes.

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// fuzzFrontmatterSeeds are representative frontmatter shapes pulled
// from the existing test suite and the corruption catalog in
// wiki_actions.go (LLM-emitted YAML: duplicate keys, flow lists,
// template leaks, CRLF, unicode).
var fuzzFrontmatterSeeds = []string{
	// Canonical article frontmatter (templates/kb-CLAUDE.md shape).
	"---\ntitle: \"Article Title\"\ntype: solution\ncreated: 2026-06-01\nupdated: 2026-06-02\ndomain: general\nconfidence: medium\ntags: [tag1, tag2]\nrelated: [\"[[Other Article]]\"]\nsources: []\n---\n\n# Body\n\ntext\n",
	// Duplicate keys — the case deduplicateYAMLKeys exists for.
	"---\ntitle: First\ntitle: Second\ntype: tool\n---\nbody\n",
	// No closing delimiter.
	"---\ntitle: Unclosed\n",
	// No frontmatter at all.
	"# Just a heading\n\nbody\n",
	// Empty frontmatter.
	"---\n---\n",
	// Block scalar with blank lines and a list.
	"---\ndescription: |\n  line one\n\n  line two\ntags:\n  - a\n  - b\n---\n",
	// YAML anchors/aliases and merge keys.
	"---\nbase: &b {x: 1}\nderived:\n  <<: *b\n  y: 2\n---\n",
	// Dates (auto-converted to time.Time by yaml.v3), floats, bools.
	"---\ncreated: 2026-01-02\nupdated: 2026-01-02T15:04:05Z\nweight: 0.5\nrolling: true\n---\n",
	// Unsubstituted template variable leak ({{VAR}} as a map key).
	"---\ntitle: {{TITLE}}\ndomain: {{DOMAIN}}\n---\n",
	// CRLF line endings.
	"---\r\ntitle: CRLF\r\ntype: tool\r\n---\r\nbody\r\n",
	// Unicode + tabs.
	"---\ntitle: \"riešenie — naïve café\"\ntype:\tsolution\n---\n",
	// Flow map spanning lines (continuation at column 0).
	"---\nmeta: {x: 1,\ny: 2}\n---\n",
	// Deeply nested-ish flow value and stray brackets.
	"---\nrelated: [][AuthoredUp][LangChain]\n---\n",
	// Comment lines and blank lines between keys.
	"---\n# generated\ntitle: X\n\n# trailing\ntype: pattern\n---\n",
	// Scalar (non-map) frontmatter body.
	"---\njust a string\n---\n",
	// Numeric and null-ish keys.
	"---\n1: one\nnull: nothing\n~: tilde\n---\n",
}

// FuzzParseFrontmatter checks the three frontmatter invariants:
//
//  1. parseFrontmatter / parseFrontmatterRaw never panic on any input.
//  2. When parseFrontmatterRaw succeeds, the parse→serialize→parse
//     round-trip (the exact write path updateFrontmatter uses:
//     "---\n" + marshalFrontmatter(raw) + "---\n") must stay parseable
//     and reach a fixed point after one normalization cycle.
//  3. The typed parseFrontmatter never returns (nil, nil).
//
// Run longer:
//
//	go test ./cmd/scribe -tags sqlite_fts5 -run '^$' -fuzz '^FuzzParseFrontmatter$' -fuzztime 5m
func FuzzParseFrontmatter(f *testing.F) {
	for _, s := range fuzzFrontmatterSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, content []byte) {
		fm, err := parseFrontmatter(content)
		if err == nil && fm == nil {
			t.Fatalf("parseFrontmatter returned (nil, nil) for %q", content)
		}

		raw, rawErr := parseFrontmatterRaw(content)
		if rawErr != nil {
			return // unparseable input is fine; not panicking is the invariant
		}

		// Round-trip cycle 1: serialize the raw map the way
		// updateFrontmatter writes it back to disk, then re-parse.
		enc1, err := marshalFrontmatter(raw)
		if err != nil {
			t.Fatalf("marshalFrontmatter failed after successful parse: %v\ninput: %q", err, content)
		}
		doc1 := []byte("---\n" + enc1 + "---\n")
		raw2, err := parseFrontmatterRaw(doc1)
		if err != nil {
			t.Fatalf("re-parse of serialized frontmatter failed: %v\ninput: %q\nserialized: %q", err, content, doc1)
		}

		// Round-trip cycle 2: after one normalization cycle the encoding
		// must be a fixed point (yaml.v3 marshals maps with sorted keys,
		// so encoding is deterministic).
		enc2, err := marshalFrontmatter(raw2)
		if err != nil {
			t.Fatalf("marshalFrontmatter failed on round-tripped map: %v\ninput: %q", err, content)
		}
		doc2 := []byte("---\n" + enc2 + "---\n")
		raw3, err := parseFrontmatterRaw(doc2)
		if err != nil {
			t.Fatalf("second re-parse failed: %v\nserialized: %q", err, doc2)
		}
		enc3, err := marshalFrontmatter(raw3)
		if err != nil {
			t.Fatalf("third marshal failed: %v", err)
		}
		if enc2 != enc3 {
			t.Fatalf("round-trip not stable after one cycle:\ncycle2: %q\ncycle3: %q\ninput: %q", enc2, enc3, content)
		}
	})
}

// FuzzDeduplicateYAMLKeys checks the dedup repair invariants:
//
//  1. Never panics.
//  2. Output is parseable whenever the input was — the repair pass must
//     never make valid YAML invalid (line surgery on flow collections
//     or quoted scalars spanning lines is the historical hazard).
//
// Run longer:
//
//	go test ./cmd/scribe -tags sqlite_fts5 -run '^$' -fuzz '^FuzzDeduplicateYAMLKeys$' -fuzztime 5m
func FuzzDeduplicateYAMLKeys(f *testing.F) {
	seeds := []string{
		"title: First\ntitle: Second\ntype: tool",
		"a: 1\nb: 2\na: 3",
		"# comment\na: 1\n\na: 2",
		"tags:\n  - a\n  - b\ntags: [c]",
		"a: {x: 1,\ny: 2}",
		"a: [1,\nb: 2]",
		"a: \"multi\nline\"",
		"description: |\n  block\n\n  scalar\ndescription: short",
		"key with spaces: 1\nkey with spaces: 2",
		"a:\n", "", ":\n:", "\t a: 1",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, yamlStr string) {
		deduped := deduplicateYAMLKeys(yamlStr)

		var direct map[string]any
		if yaml.Unmarshal([]byte(yamlStr), &direct) != nil {
			return // input didn't parse; dedup is best-effort repair there
		}
		var after map[string]any
		if err := yaml.Unmarshal([]byte(deduped), &after); err != nil {
			t.Fatalf("deduplicateYAMLKeys broke parseable YAML: %v\ninput: %q\noutput: %q", err, yamlStr, deduped)
		}
	})
}

// FuzzExtractWikilinks checks the wikilink extractor invariants:
//
//  1. Never panics (regex pipeline over arbitrary bytes).
//  2. Every returned target is non-empty, trimmed, unique, and contains
//     no ']' or newline (the regex and pipe-cut guarantee this).
//
// extractTitleFast rides along: never panics, never returns a newline.
//
// Run longer:
//
//	go test ./cmd/scribe -tags sqlite_fts5 -run '^$' -fuzz '^FuzzExtractWikilinks$' -fuzztime 5m
func FuzzExtractWikilinks(f *testing.F) {
	seeds := []string{
		"See [[Other Article]] and [[Piped|Display Text]].\n",
		"```go\n[[not a link]]\n```\nreal [[Link]]\n",
		"inline `[[code link]]` and ``double `tick` span [[hidden]]`` then [[Visible]]\n",
		"[[]] [[ ]] [[|pipe-first]] [[a|b|c]]\n",
		"unclosed [[link\nnext [[Real Link]] line\n",
		"[[A]][[A]][[B]]\n",
		"related: [\"[[Quoted Link]]\"]\n",
		"nested [[a[b]] weird\n",
		"--- \ntitle: x\n---\n[[Body Link]]\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, content []byte) {
		links := extractWikilinks(content)
		seen := make(map[string]bool, len(links))
		for _, l := range links {
			if l == "" {
				t.Fatalf("empty wikilink target from %q", content)
			}
			if strings.TrimSpace(l) != l {
				t.Fatalf("untrimmed wikilink target %q from %q", l, content)
			}
			if strings.ContainsAny(l, "]\n") {
				t.Fatalf("wikilink target %q contains ']' or newline (input %q)", l, content)
			}
			if seen[l] {
				t.Fatalf("duplicate wikilink target %q from %q", l, content)
			}
			seen[l] = true
		}

		if title := extractTitleFast(content); strings.Contains(title, "\n") {
			t.Fatalf("extractTitleFast returned multi-line title %q", title)
		}
	})
}
