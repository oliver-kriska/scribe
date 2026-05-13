package main

import (
	"regexp"
	"strings"
)

// Strips fabricated [cNN-fM] brackets from envelope content before the
// actions hit disk. A "real" ID is one that appears in the merged
// facts file for this raw article (output/facts/<slug>.json) — i.e.
// produced by the actual facts pass. Anything else is the model
// completing the citation pattern from training data.
//
// Strategy is strip-not-fail because the bracket is purely a
// downstream-audit footer. The blockquote + "— Source: <file>" is
// still a valid citation without it. Losing the whole wiki article
// over a stray bracket is worse than dropping the bracket.
//
// The regex requires \d+-f\d+ so it can't catch general markdown like
// `[abc]` or `[note 1]`. The leading optional whitespace is consumed
// so removing a bracket doesn't leave a dangling space before
// punctuation.

var factIDBracketRE = regexp.MustCompile(`[ \t]*\[c\d+-f\d+\]`)

// stripUnknownFactIDs removes any [cNN-fM] bracket from content whose
// ID isn't present in the valid set. Returns the cleaned content and
// the list of stripped IDs (deduped, in first-seen order) so the
// caller can log a single summary line.
func stripUnknownFactIDs(content string, valid map[string]bool) (cleaned string, strippedIDs []string) {
	if content == "" {
		return content, nil
	}
	seen := map[string]bool{}
	out := factIDBracketRE.ReplaceAllStringFunc(content, func(m string) string {
		id := strings.Trim(m, " \t[]")
		if valid[id] {
			return m
		}
		if !seen[id] {
			seen[id] = true
			strippedIDs = append(strippedIDs, id)
		}
		return ""
	})
	return out, strippedIDs
}
