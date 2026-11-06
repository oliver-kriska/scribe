package main

import (
	"slices"
	"strings"
	"sync"
	"unicode"

	"gopkg.in/yaml.v3"
)

// kbTitleLookup returns the title and alias set of every article under
// root. The walk runs once, on first use.
func kbTitleLookup(root string) *knownTitles {
	load := sync.OnceValue(func() titleSet {
		names := map[string]bool{}
		_ = walkArticles(root, func(_ string, content []byte) error {
			if title, ok := plainFrontmatterTitle(content); ok {
				names[title] = true
				return nil
			}
			fm, err := parseFrontmatter(content)
			if err != nil {
				if title := extractTitleFast(content); title != "" {
					names[title] = true
				}
				return nil
			}
			if fm.Title != "" {
				names[fm.Title] = true
			}
			for _, alias := range toStringSlice(fm.Aliases) {
				if alias != "" {
					names[alias] = true
				}
			}
			return nil
		})
		return newTitleSet(names)
	})
	return &knownTitles{
		has:      func(name string) bool { return load().names[name] },
		complete: func(fragment string) (string, bool) { return load().complete(fragment) },
	}
}

// titleSet is a name set plus its sorted keys for prefix lookups.
type titleSet struct {
	names  map[string]bool
	sorted []string
}

func newTitleSet(names map[string]bool) titleSet {
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	slices.Sort(sorted)
	return titleSet{names: names, sorted: sorted}
}

// complete returns the single balanced name that starts with fragment + ", ".
func (s titleSet) complete(fragment string) (string, bool) {
	prefix := fragment + ", "
	match := ""
	for i, _ := slices.BinarySearch(s.sorted, prefix); i < len(s.sorted) && strings.HasPrefix(s.sorted[i], prefix); i++ {
		if !bracketsBalanced(s.sorted[i]) {
			continue
		}
		if match != "" {
			return "", false
		}
		match = s.sorted[i]
	}
	return match, match != ""
}

// plainFrontmatterTitle reads the title without a YAML parse when that is
// exact: a frontmatter block with no aliases: key and a title: line whose
// value is plain or double-quoted with no escape, quote, comment marker or
// flow syntax inside. ok=false sends the caller to parseFrontmatter. On a
// ~12k-article KB this cut the lookup's CPU time about 4x (0.46s → 0.11s);
// a one-file `lint --changed` in sync pays the whole walk.
func plainFrontmatterTitle(content []byte) (string, bool) {
	span, err := frontmatterSpan(string(content))
	if err != nil {
		return "", false
	}
	title, found := "", false
	for line := range strings.SplitSeq(string(content[span.YAMLStart:span.YAMLEnd]), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "aliases:") {
			return "", false
		}
		rest, ok := strings.CutPrefix(line, "title:")
		if !ok {
			continue
		}
		if found {
			return "", false // duplicate key: let the parser decide
		}
		v := strings.TrimSpace(rest)
		if inner, ok := strings.CutPrefix(v, `"`); ok {
			if v, ok = strings.CutSuffix(inner, `"`); !ok {
				return "", false
			}
		}
		if v == "" || v == "~" || strings.EqualFold(v, "null") || strings.ContainsAny(v, `"'\\#`) || strings.ContainsAny(v[:1], "[{&*!|>%@`-?:") || strings.Contains(v, ": ") {
			return "", false
		}
		title, found = v, true
	}
	return title, found
}

// relatedRegion returns the [start, end) line range of the top-level
// related: key and its continuation lines (indented, "- " items, or a
// flow list's closing "]"), or start = -1 when the key is absent.
func relatedRegion(lines []string) (start, end int) {
	start = -1
	for i, line := range lines {
		if key, _, ok := splitFrontmatterLine(line); ok && key == "related" {
			start = i
			break
		}
	}
	if start < 0 {
		return -1, -1
	}
	end = start + 1
	for end < len(lines) {
		line := lines[end]
		if line == "" || !strings.ContainsAny(line[:1], " \t-]") {
			break
		}
		end++
	}
	return start, end
}

// repairRelatedLines runs repairSplitRelated over the related: list in a
// split frontmatter block. Only the related: lines change, and only when a
// repair applies: they are re-rendered in the same style (flow or block),
// each entry double-quoted. Any other value shape, or a region that does
// not parse, is left alone.
func repairRelatedLines(lines []string, known *knownTitles) ([]string, []string) {
	start, end := relatedRegion(lines)
	if start < 0 {
		return lines, nil
	}
	crlf := strings.HasSuffix(lines[start], "\r")
	region := make([]string, 0, end-start)
	for _, line := range lines[start:end] {
		region = append(region, strings.TrimSuffix(line, "\r"))
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(strings.Join(region, "\n")), &parsed); err != nil {
		return lines, nil
	}
	entries, ok := relatedStrings(parsed["related"])
	if !ok {
		return lines, nil
	}
	repaired, notes := repairSplitRelated(entries, known)
	if len(notes) == 0 {
		return lines, nil
	}
	rendered := renderRelatedLines(region, repaired)
	if crlf {
		for i := range rendered {
			rendered[i] += "\r"
		}
	}
	out := make([]string, 0, len(lines)-(end-start)+len(rendered))
	out = append(out, lines[:start]...)
	out = append(out, rendered...)
	out = append(out, lines[end:]...)
	return out, notes
}

// renderRelatedLines renders entries in the style of the original region:
// a block list keeps its item indent, anything else becomes one flow line.
func renderRelatedLines(region []string, entries []string) []string {
	_, rest, _ := splitFrontmatterLine(region[0])
	item := ""
	if len(region) > 1 {
		item = strings.TrimLeft(region[1], " \t")
	}
	if rest == "" && strings.HasPrefix(item, "-") && len(entries) > 0 {
		indent := region[1][:len(region[1])-len(item)]
		out := make([]string, 0, 1+len(entries))
		out = append(out, "related:")
		for _, entry := range entries {
			out = append(out, indent+"- "+relatedYAMLQuote(entry))
		}
		return out
	}
	quoted := make([]string, 0, len(entries))
	for _, entry := range entries {
		quoted = append(quoted, relatedYAMLQuote(entry))
	}
	return []string{"related: [" + strings.Join(quoted, ", ") + "]"}
}

// relatedYAMLQuote double-quotes s, the form related: entries are written
// in, unless s holds a character that would need escaping there; then it
// single-quotes it.
func relatedYAMLQuote(s string) string {
	if strings.ContainsAny(s, `"\`) || strings.ContainsFunc(s, unicode.IsControl) {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return `"` + s + `"`
}

// splitRelatedFindings returns what repairRelatedLines would change in
// content's frontmatter, without changing it.
func splitRelatedFindings(content []byte, known *knownTitles) []string {
	span, err := frontmatterSpan(string(content))
	if err != nil {
		return nil
	}
	block := strings.TrimSuffix(string(content[span.YAMLStart:span.YAMLEnd]), "\n")
	_, notes := repairRelatedLines(strings.Split(block, "\n"), known)
	return notes
}
