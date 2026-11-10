package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

// Pass-2 re-absorb regenerates a page from its source and overwrites it
// whole. Before these guards it kept only `created`. A 2026-11-10 history
// scan of the reference KB found what that cost: ~3.5k related links to
// live articles, retitles that broke every inbound [[link]], and 178
// rewrites that shrank a page by >20% and dropped whole sections (157
// pages, a rolling learnings file among them). carryOnOverwrite runs the
// guards in order on the page about to be written.

const (
	// carrySectionMaxBytes caps how much old body text one overwrite may
	// re-add, so a page that keeps being regenerated can't grow unbounded.
	carrySectionMaxBytes = 8000
	// carrySectionMinBytes skips trivial sections (an empty heading, a
	// one-line stub) that aren't worth resurrecting.
	carrySectionMinBytes = 80
	// carrySectionCovered is the share of a dropped section's word
	// trigrams that must appear in the new body for it to count as merged
	// under another heading rather than lost.
	carrySectionCovered = 0.5
	carriedMarker       = "<!-- scribe:carried "
)

// carriedScalarKeys are frontmatter keys the regenerated page usually
// omits but that other writers own: status (a dream supersede must
// survive re-absorb), contributor (provenance), index_tier and authority
// (tier write / lint). Each is restored only when the new page lacks it.
var carriedScalarKeys = []string{"status", "contributor", "index_tier", "authority"}

// carryOnOverwrite applies every overwrite guard. abs is the page being
// replaced; content is the model's new page. A fresh create (no file at
// abs) passes through untouched.
func carryOnOverwrite(i int, abs, content string) string {
	existing, err := os.ReadFile(abs)
	if err != nil {
		return content
	}
	old, err := parseFrontmatter(existing)
	if err != nil || old == nil {
		return content
	}
	name := filepath.Base(abs)
	content = carryRelatedOnOverwrite(i, abs, content)
	content = carryTitleOnOverwrite(i, name, old, content)
	content = carryScalarsOnOverwrite(i, name, existing, content)
	// Rolling pages (learnings, decisions-log) shed old entries on purpose:
	// rolling compaction moves them to an archive file. Re-adding them would
	// undo the compaction and duplicate the archive.
	if !old.Rolling {
		content = carrySectionsOnOverwrite(i, name, string(existing), content)
	}
	return content
}

// carryTitleOnOverwrite keeps the existing title, because [[links]]
// resolve by title and a retitle silently breaks every inbound link
// (link --fix-broken then strips them from the linking pages). The new
// title is kept as an alias, so links written against it resolve too, and
// a body H1 that repeated the new title is renamed with it.
func carryTitleOnOverwrite(i int, name string, old *Frontmatter, content string) string {
	oldTitle := strings.TrimSpace(old.Title)
	fm, err := parseFrontmatter([]byte(content))
	if oldTitle == "" || err != nil || fm == nil {
		return content
	}
	newTitle := strings.TrimSpace(fm.Title)
	// Exact compare: even a separator-only retitle ("Handoff with Ack" →
	// "Handoff-with-Ack") broke inbound links in the reference KB.
	if newTitle == "" || newTitle == oldTitle {
		return content
	}
	content = setFrontmatterScalar(content, "title", yamlScalar(oldTitle))
	aliases := mergeStrings(stringList(old.Aliases), stringList(fm.Aliases), []string{newTitle})
	content = setFrontmatterList(content, "aliases", aliases)
	if span, err := frontmatterSpan(content); err == nil {
		body := content[span.BodyStart:]
		if h1 := "# " + newTitle; strings.HasPrefix(body, h1+"\n") || strings.Contains(body, "\n"+h1+"\n") {
			content = content[:span.BodyStart] + strings.Replace(body, h1+"\n", "# "+oldTitle+"\n", 1)
		}
	}
	logMsg("envelope", "action[%d] create %s: kept title %q (new title %q kept as an alias)", i, name, oldTitle, newTitle)
	return content
}

// carryScalarsOnOverwrite restores carriedScalarKeys the new page omits.
func carryScalarsOnOverwrite(i int, name string, existing []byte, content string) string {
	var oldMap, newMap map[string]any
	if !decodeFrontmatterMap(string(existing), &oldMap) || !decodeFrontmatterMap(content, &newMap) {
		return content
	}
	var kept []string
	for _, key := range carriedScalarKeys {
		v, ok := oldMap[key].(string)
		if _, has := newMap[key]; has || !ok || strings.TrimSpace(v) == "" {
			continue
		}
		content = setFrontmatterScalar(content, key, yamlScalar(v))
		kept = append(kept, key)
	}
	if len(kept) > 0 {
		logMsg("envelope", "action[%d] create %s: kept frontmatter %v the overwrite omitted", i, name, kept)
	}
	return content
}

// carrySectionsOnOverwrite re-adds `## ` sections of the old body that the
// new body drops and does not cover. A section counts as covered, and is
// not re-added, when its heading survives or at least
// carrySectionCovered of its word trigrams appear in the new body (the
// model merged it under another heading). Re-added sections go at the
// end, each marked with a dated scribe:carried comment, up to
// carrySectionMaxBytes per overwrite. Every carry and every skip-for-cap
// is logged.
func carrySectionsOnOverwrite(i int, name, existing, content string) string {
	oldSpan, err := frontmatterSpan(existing)
	if err != nil {
		return content
	}
	newSpan, err := frontmatterSpan(content)
	if err != nil {
		return content
	}
	newBody := content[newSpan.BodyStart:]
	newHeadings := map[string]bool{}
	for _, s := range splitH2Sections(newBody) {
		newHeadings[linkFoldKey(s.heading)] = true
	}
	newGrams := wordTrigrams(newBody)
	today := time.Now().UTC().Format(time.DateOnly)
	var add strings.Builder
	var carried, capped []string
	for _, s := range splitH2Sections(existing[oldSpan.BodyStart:]) {
		text := strings.TrimSpace(s.body)
		if newHeadings[linkFoldKey(s.heading)] || len(text) < carrySectionMinBytes {
			continue
		}
		grams := wordTrigrams(text)
		if len(grams) == 0 || trigramCoverage(grams, newGrams) >= carrySectionCovered {
			continue
		}
		if add.Len()+len(s.raw) > carrySectionMaxBytes {
			capped = append(capped, s.heading)
			continue
		}
		block := s.raw
		if !strings.Contains(block, carriedMarker) {
			block = "## " + s.heading + "\n\n" + carriedMarker + today + " -->\n" + strings.TrimLeft(s.body, "\n")
		}
		add.WriteString("\n" + strings.TrimRight(block, "\n") + "\n")
		carried = append(carried, s.heading)
	}
	if len(capped) > 0 {
		logMsg("envelope", "action[%d] create %s: %d dropped section(s) over the %d-byte carry cap were not kept: %v", i, name, len(capped), carrySectionMaxBytes, capped)
	}
	if len(carried) == 0 {
		return content
	}
	logMsg("envelope", "action[%d] create %s: kept %d section(s) the overwrite dropped: %v", i, name, len(carried), carried)
	return strings.TrimRight(content, "\n") + "\n" + add.String()
}

// h2Section is one `## ` section: heading text, body (without the heading
// line) and raw text (heading line included).
type h2Section struct {
	heading, body, raw string
}

// splitH2Sections splits a markdown body at `## ` headings outside fenced
// code. Text before the first heading is not a section.
func splitH2Sections(body string) []h2Section {
	var out []h2Section
	var cur *h2Section
	var raw, text strings.Builder
	flush := func() {
		if cur != nil {
			cur.body, cur.raw = text.String(), raw.String()
			out = append(out, *cur)
		}
	}
	inFence := false
	for _, line := range strings.SplitAfter(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(line, "## ") {
			flush()
			cur = &h2Section{heading: strings.TrimSpace(strings.TrimPrefix(line, "## "))}
			raw.Reset()
			text.Reset()
			raw.WriteString(line)
			continue
		}
		if cur != nil {
			raw.WriteString(line)
			text.WriteString(line)
		}
	}
	flush()
	return out
}

// wordTrigrams returns the set of lowercase word trigrams in s.
func wordTrigrams(s string) map[string]bool {
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := map[string]bool{}
	for j := 0; j+2 < len(words); j++ {
		out[words[j]+" "+words[j+1]+" "+words[j+2]] = true
	}
	return out
}

func trigramCoverage(grams, in map[string]bool) float64 {
	hit := 0
	for g := range grams {
		if in[g] {
			hit++
		}
	}
	return float64(hit) / float64(len(grams))
}

// decodeFrontmatterMap decodes content's frontmatter into a generic map.
func decodeFrontmatterMap(content string, out *map[string]any) bool {
	span, err := frontmatterSpan(content)
	if err != nil {
		return false
	}
	return yaml.Unmarshal([]byte(content[span.YAMLStart:span.YAMLEnd]), out) == nil && *out != nil
}

// yamlScalar renders s as a YAML scalar, quoting only when YAML needs it.
func yamlScalar(s string) string {
	b, err := yaml.Marshal(s)
	if err != nil {
		return relatedYAMLQuote(s)
	}
	return strings.TrimSpace(string(b))
}

// stringList reads a frontmatter list-or-scalar value as strings.
func stringList(v any) []string {
	if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
		return []string{s}
	}
	list, _ := relatedStrings(v)
	return list
}

// mergeStrings concatenates lists, dropping blanks and fold-key duplicates.
func mergeStrings(lists ...[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, list := range lists {
		for _, s := range list {
			k := linkFoldKey(s)
			if strings.TrimSpace(s) == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, s)
		}
	}
	return out
}

// setFrontmatterList replaces (or adds) a top-level list key as one flow
// line, dropping any block-list continuation lines of the old value.
func setFrontmatterList(content, key string, values []string) string {
	span, err := frontmatterSpan(content)
	if err != nil {
		return content
	}
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, relatedYAMLQuote(v))
	}
	line := key + ": [" + strings.Join(quoted, ", ") + "]"
	lines := strings.Split(content[span.YAMLStart:span.YAMLEnd], "\n")
	start, end := frontmatterKeyRegion(lines, key)
	if start < 0 {
		start, end = len(lines), len(lines)
		if start > 0 && lines[start-1] == "" {
			start, end = start-1, start-1
		}
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:start]...)
	out = append(out, line)
	out = append(out, lines[end:]...)
	return content[:span.YAMLStart] + strings.Join(out, "\n") + content[span.YAMLEnd:]
}
