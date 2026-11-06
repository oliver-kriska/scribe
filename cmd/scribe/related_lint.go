package main

import (
	"strings"
	"sync"
	"unicode"

	"gopkg.in/yaml.v3"
)

// kbTitleLookup returns a function reporting whether name is a title or
// alias of any article under root. The walk runs once, on first use.
func kbTitleLookup(root string) func(string) bool {
	load := sync.OnceValue(func() map[string]bool {
		names := map[string]bool{}
		_ = walkArticles(root, func(_ string, content []byte) error {
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
		return names
	})
	return func(name string) bool { return load()[name] }
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
func repairRelatedLines(lines []string, known func(string) bool) ([]string, []string) {
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
func splitRelatedFindings(content []byte, known func(string) bool) []string {
	span, err := frontmatterSpan(string(content))
	if err != nil {
		return nil
	}
	block := strings.TrimSuffix(string(content[span.YAMLStart:span.YAMLEnd]), "\n")
	_, notes := repairRelatedLines(strings.Split(block, "\n"), known)
	return notes
}
