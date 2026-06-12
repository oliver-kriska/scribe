package main

// Native Go fuzz targets for the wiki-action envelope: the JSON parse
// layer, the path validator, the pre-apply content sanitizer, and a
// dry-run pass through the full apply pipeline. Envelopes are emitted
// by local 4–7B models, so malformed JSON, hallucinated paths, and
// corrupt frontmatter are the expected input — the validator and
// sanitizer ARE the security boundary.
//
// Disk writes are never fuzzed: the apply target runs DryRun-only
// against a t.TempDir KB and asserts the KB is byte-identical after
// every iteration. No network, no user paths.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var fuzzEnvelopeSeeds = []string{
	// Canonical pass-2 envelope (wiki_actions_test.go).
	`{"entity":"Foo","actions":[{"op":"create","path":"wiki/foo.md","content":"---\ntitle: Foo\ntype: research\ndomain: general\n---\n\nbody [[Bar]]\n"}]}`,
	// V2 with meta side-channel.
	`{"version":2,"entity":"Sess","actions":[],"meta":[{"op":"sessions_log_append","session_id":"abc-123"},{"op":"log_append","line":"mined session abc-123"},{"op":"rolling_memory_append","domain":"general","target":"learnings","content":"a paragraph"}]}`,
	// Every wiki op, plus failure shapes.
	`{"actions":[{"op":"append","path":"wiki/existing.md","content":"\nmore\n"},{"op":"replace_section","path":"wiki/existing.md","heading":"Notes","content":"new body"},{"op":"update_frontmatter","path":"wiki/existing.md","frontmatter":{"updated":"2026-06-12","sources":["x"],"title":"clobber"}}]}`,
	// Decay marker appends (stale target, fresh target, missing target).
	`{"actions":[{"op":"append","path":"tools/stale.md","content":"\n<!-- decay-candidate 2026-06-12 -->\n"},{"op":"append","path":"wiki/fresh.md","content":"\n<!-- decay-candidate 2026-06-12 -->\n"},{"op":"append","path":"wiki/nope.md","content":"\n<!-- decay-candidate 2026-06-12 -->\n"}]}`,
	// Path attacks: traversal, absolute, underscore artifact, doubled
	// extension, hallucinated top dir (remap candidate), empty.
	`{"actions":[{"op":"create","path":"../../etc/cron.d/evil","content":"x"},{"op":"create","path":"/tmp/abs.md","content":"x"},{"op":"append","path":"wiki/_absorb_log.json","content":"{"},{"op":"create","path":"wiki/readme.md.md","content":"x"},{"op":"create","path":"middleware/foo.md","content":"x"},{"op":"create","path":"","content":"x"}]}`,
	// Unknown ops, missing fields, unknown meta op.
	`{"actions":[{"op":"delete","path":"wiki/foo.md"},{"op":"replace_section","path":"wiki/existing.md","content":"no heading"}],"meta":[{"op":"format_disk"}]}`,
	// Corrupt content the sanitizer must repair: fabricated fact IDs,
	// broken related:, unparseable frontmatter, template leak.
	`{"actions":[{"op":"create","path":"wiki/corrupt.md","content":"---\ntitle: C\nrelated: [][AuthoredUp][LangChain]\n---\n\nclaim [c12-f3] cited\n"},{"op":"create","path":"wiki/leak.md","content":"---\ntitle: {{TITLE}}\ndomain: {{DOMAIN}}\n---\nbody\n"},{"op":"create","path":"wiki/unclosed.md","content":"---\ntitle: never closed\n"}]}`,
	// Version oddities.
	`{"version":99,"actions":[{"op":"create","path":"wiki/v99.md","content":"x"}]}`,
	`{"version":1,"actions":[{"op":"create","path":"wiki/v1.md","content":"x"}],"meta":[{"op":"log_append","line":"v1 with meta"}]}`,
	// Shape garbage.
	`{"actions":[]}`, `{"actions":null}`, `{}`, `[]`, `null`, `42`, `"actions"`,
	`{"actions":[{"path":"wiki/foo.md"}]}`, `{"actions":[{"op":"create"}]}`,
	`{"actions":[{"op":"create","path":"wiki/x.md","frontmatter":{"a":{"b":[1,2,{"c":null}]}}}]}`,
	"not json at all", "",
}

// FuzzParseEnvelope checks the envelope parse layer:
//
//  1. parseEnvelope and parseEnvelopeV2 never panic on any input.
//  2. parseEnvelope success implies ≥1 action and op+path on every one.
//  3. parseEnvelopeV2 success implies op+path on every action (empty
//     action lists are legal in V2 — meta-only envelopes).
//
// Run longer:
//
//	go test ./cmd/scribe -tags sqlite_fts5 -run '^$' -fuzz '^FuzzParseEnvelope$' -fuzztime 5m
func FuzzParseEnvelope(f *testing.F) {
	for _, s := range fuzzEnvelopeSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, jsonText string) {
		env, err := parseEnvelope(jsonText)
		if err == nil {
			if len(env.Actions) == 0 {
				t.Fatalf("parseEnvelope accepted an envelope with no actions: %q", jsonText)
			}
			for i, a := range env.Actions {
				if a.Op == "" || a.Path == "" {
					t.Fatalf("parseEnvelope accepted action[%d] with missing op/path: %q", i, jsonText)
				}
			}
		}

		env2, err2 := parseEnvelopeV2(jsonText, "fuzz")
		if err2 == nil {
			for i, a := range env2.Actions {
				if a.Op == "" || a.Path == "" {
					t.Fatalf("parseEnvelopeV2 accepted action[%d] with missing op/path: %q", i, jsonText)
				}
			}
		}
	})
}

// FuzzValidateActionPath checks the KB-root sandbox:
//
//  1. Never panics.
//  2. An accepted path always resolves INSIDE the root, lands under a
//     known wiki dir, never targets an underscore-prefixed artifact,
//     and never carries a doubled .md extension.
//
// Run longer:
//
//	go test ./cmd/scribe -tags sqlite_fts5 -run '^$' -fuzz '^FuzzValidateActionPath$' -fuzztime 5m
func FuzzValidateActionPath(f *testing.F) {
	seeds := []string{
		"wiki/foo.md", "projects/x/overview.md", "research/deep/dive.md",
		"../../etc/passwd", "/etc/passwd", "wiki/../wiki/ok.md",
		"wiki/../../escape.md", "wiki/_index.md", "wiki/sub/_sections.json",
		"wiki/readme.md.md", "wiki/README.MD.md", "middleware/foo.md",
		"", ".", "..", "wiki", "wiki/", "wiki//double//slash.md",
		"wiki/\x00null.md", "wiki/ünïcödé.md", "sessions/2026/log.md",
		"wiki/./.././projects/sneak.md", "wiki\\windows\\style.md",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	allowedTop := make(map[string]bool, len(wikiDirs))
	for _, d := range wikiDirs {
		allowedTop[d] = true
	}
	const root = "/kb-fuzz-root"
	f.Fuzz(func(t *testing.T, rel string) {
		abs, err := validateActionPath(root, rel)
		if err != nil {
			return // rejection is always a safe outcome
		}
		r, rerr := filepath.Rel(root, abs)
		if rerr != nil || filepath.IsAbs(r) || r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) {
			t.Fatalf("accepted path escapes KB root: rel=%q abs=%q", rel, abs)
		}
		top := strings.SplitN(r, string(os.PathSeparator), 2)[0]
		if !allowedTop[top] {
			t.Fatalf("accepted path outside wiki dirs: rel=%q abs=%q top=%q", rel, abs, top)
		}
		if strings.HasPrefix(filepath.Base(abs), "_") {
			t.Fatalf("accepted underscore-prefixed artifact: rel=%q abs=%q", rel, abs)
		}
		if strings.HasSuffix(strings.ToLower(filepath.Base(abs)), ".md.md") {
			t.Fatalf("accepted doubled .md extension: rel=%q abs=%q", rel, abs)
		}
	})
}

// FuzzNormalizeRelatedFrontmatter checks the related: repair seam:
//
//  1. Never panics.
//  2. Content without a leading/closing --- block, or without a
//     related: key, is returned byte-for-byte unchanged (the function's
//     documented conservative contract).
//  3. When it does rewrite, only the related: value region changes —
//     prefix and body are preserved — and the emitted line is itself
//     valid YAML (the function's whole purpose is "valid quoted line is
//     strictly safer than corruption", so emitting invalid YAML is a
//     contract violation).
//
// Run longer:
//
//	go test ./cmd/scribe -tags sqlite_fts5 -run '^$' -fuzz '^FuzzNormalizeRelatedFrontmatter$' -fuzztime 5m
func FuzzNormalizeRelatedFrontmatter(f *testing.F) {
	seeds := []string{
		"---\ntitle: X\nrelated: [\"[[A]]\", \"[[B]]\"]\n---\nbody\n",
		"---\nrelated: [][AuthoredUp][LangChain]\n---\n",
		"---\nrelated: [A, B]\ntags: [x]\n---\n",
		"---\nrelated: [\"\\[X\\]\"]\n---\n",
		"---\nrelated:\n  - \"[[A]]\"\n  - \"[[B]]\"\n---\n",
		"---\nrelated: []\n---\n",
		"---\nrelated:\n---\n",
		"---\ntitle: no related here\n---\nbody mentions related: [[A]]\n",
		"no frontmatter related: [[A]]\n",
		"---\nrelated: [[A]], [[B]]\n",
		"---\nrelated: [[A|Display]]\n---\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, content string) {
		out := normalizeRelatedFrontmatter(content)

		lines := strings.Split(content, "\n")
		if len(lines) < 2 || strings.TrimSpace(lines[0]) != "---" {
			if out != content {
				t.Fatalf("changed content without a frontmatter block:\nin:  %q\nout: %q", content, out)
			}
			return
		}
		closeIdx := -1
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				closeIdx = i
				break
			}
		}
		if closeIdx < 0 {
			if out != content {
				t.Fatalf("changed content without a closing delimiter:\nin:  %q\nout: %q", content, out)
			}
			return
		}
		relIdx := -1
		for i := 1; i < closeIdx; i++ {
			if strings.HasPrefix(lines[i], "related:") {
				relIdx = i
				break
			}
		}
		if relIdx < 0 {
			if out != content {
				t.Fatalf("changed content without a related: key:\nin:  %q\nout: %q", content, out)
			}
			return
		}
		end := closeIdx
		for i := relIdx + 1; i < closeIdx; i++ {
			if relatedKeyBoundRE.MatchString(lines[i]) {
				end = i
				break
			}
		}

		outLines := strings.Split(out, "\n")
		if want := relIdx + 1 + (len(lines) - end); len(outLines) != want {
			t.Fatalf("unexpected output shape: got %d lines, want %d\nin:  %q\nout: %q", len(outLines), want, content, out)
		}
		for i := 0; i < relIdx; i++ {
			if outLines[i] != lines[i] {
				t.Fatalf("prefix line %d mutated: %q -> %q", i, lines[i], outLines[i])
			}
		}
		for i := end; i < len(lines); i++ {
			if outLines[relIdx+1+(i-end)] != lines[i] {
				t.Fatalf("suffix line %d mutated: %q -> %q", i, lines[i], outLines[relIdx+1+(i-end)])
			}
		}

		var parsed map[string]any
		if err := yaml.Unmarshal([]byte(outLines[relIdx]), &parsed); err != nil {
			t.Fatalf("normalized related line is not valid YAML: %v\nline: %q\nin:   %q", err, outLines[relIdx], content)
		}
	})
}

// snapshotKB returns path→content for every file under root.
func snapshotKB(tb testing.TB, root string) map[string]string {
	tb.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		snap[path] = string(data)
		return nil
	})
	if err != nil {
		tb.Fatalf("snapshot KB: %v", err)
	}
	return snap
}

// FuzzApplyWikiActionsDryRun pushes parsed envelopes through the full
// apply pipeline — sanitizer, frontmatter clamp, path validation, op
// dispatch, meta dispatch — in DryRun mode against a real KB layout:
//
//  1. Never panics, whatever the envelope shape.
//  2. applyWikiActions never returns a top-level error for a non-empty
//     root (per-action failures land in result.Errors).
//  3. DryRun NEVER mutates the KB: every file is byte-identical after
//     every iteration (reads — decay guards, stat probes — are fine).
//
// Run longer:
//
//	go test ./cmd/scribe -tags sqlite_fts5 -run '^$' -fuzz '^FuzzApplyWikiActionsDryRun$' -fuzztime 5m
func FuzzApplyWikiActionsDryRun(f *testing.F) {
	root := f.TempDir()
	mustWrite := func(rel, content string) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			f.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			f.Fatal(err)
		}
	}
	mustWrite("wiki/existing.md", "---\ntitle: Existing\ntype: research\ncreated: 2026-01-01\nupdated: 2026-01-01\ndomain: general\nconfidence: medium\ntags: [seed]\nrelated: []\nsources: []\n---\n\n# Existing\n\n## Notes\n\nseed body\n\n## See Also\n\n- [[Other]]\n")
	mustWrite("tools/stale.md", "---\ntitle: Stale Tool\ntype: tool\ncreated: 2020-01-01\nupdated: 2020-01-01\ndomain: general\nconfidence: low\ntags: []\nrelated: []\nsources: []\n---\n\nold\n")
	mustWrite("wiki/fresh.md", "---\ntitle: Fresh\ntype: research\ncreated: 2099-01-01\nupdated: 2099-01-01\ndomain: general\nconfidence: medium\ntags: []\nrelated: []\nsources: []\n---\n\nnew\n")
	before := snapshotKB(f, root)

	for _, s := range fuzzEnvelopeSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, jsonText string) {
		env, err := parseEnvelopeV2(jsonText, "fuzz")
		if err != nil {
			return
		}
		opts := entityWriterApplyOptions()
		opts.DryRun = true
		if _, aerr := applyWikiActions(root, env, opts); aerr != nil {
			t.Fatalf("applyWikiActions returned top-level error on non-empty root: %v\nenvelope: %q", aerr, jsonText)
		}
		after := snapshotKB(t, root)
		if len(after) != len(before) {
			t.Fatalf("dry-run changed KB file count: before=%d after=%d\nenvelope: %q", len(before), len(after), jsonText)
		}
		for path, want := range before {
			if got, ok := after[path]; !ok || got != want {
				t.Fatalf("dry-run mutated %s\nenvelope: %q", path, jsonText)
			}
		}
	})
}
