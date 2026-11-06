package main

import (
	"fmt"
	"strings"
)

// maxRelatedJoinParts bounds how many adjacent related: entries one repair
// may rejoin: a title with up to three commas.
const maxRelatedJoinParts = 4

var openingBracket = map[rune]rune{')': '(', ']': '[', '}': '{'}

// bracketsBalanced reports whether every (, [ and { in s is closed by the
// matching bracket, in order. A link target with an unbalanced bracket is
// a split artifact: "Default Target Stack (Verified Current Versions" and
// "2026-10-09)" are the two halves of one title cut at its comma.
func bracketsBalanced(s string) bool {
	var stack []rune
	for _, r := range s {
		switch r {
		case '(', '[', '{':
			stack = append(stack, r)
		case ')', ']', '}':
			if len(stack) == 0 || stack[len(stack)-1] != openingBracket[r] {
				return false
			}
			stack = stack[:len(stack)-1]
		}
	}
	return len(stack) == 0
}

// relatedEntryInner returns the link text of a related: entry: "[[X]]"
// yields "X", wrapped=true; a bare "X" yields itself.
func relatedEntryInner(entry string) (inner string, wrapped bool) {
	trimmed := strings.TrimSpace(entry)
	if strings.HasPrefix(trimmed, "[[") && strings.HasSuffix(trimmed, "]]") && len(trimmed) >= 4 {
		return strings.TrimSpace(trimmed[2 : len(trimmed)-2]), true
	}
	return trimmed, false
}

// repairSplitRelated undoes the comma split of a related: list. Entries are
// the list's strings, "[[X]]" or bare. Adjacent entries are rejoined with
// ", " when either
//
//   - known is non-nil, the joined text is a title or alias known() accepts,
//     and no piece is one on its own; or
//   - the first and last pieces both have unbalanced brackets and the
//     joined text is balanced.
//
// At most maxRelatedJoinParts entries join, nearest end first. An entry
// that still has unbalanced brackets afterwards is dropped. Entries the
// repair does not touch are returned verbatim. notes describes each change;
// it is empty when nothing changed.
func repairSplitRelated(entries []string, known func(string) bool) (out, notes []string) {
	inners := make([]string, len(entries))
	for i, entry := range entries {
		inners[i], _ = relatedEntryInner(entry)
	}
	out = make([]string, 0, len(entries))
	for i := 0; i < len(entries); {
		if end := relatedJoinEnd(inners, i, known); end > i {
			joined := strings.Join(inners[i:end+1], ", ")
			if _, wrapped := relatedEntryInner(entries[i]); wrapped {
				joined = "[[" + joined + "]]"
			}
			notes = append(notes, fmt.Sprintf("rejoined split related entries %s → %s", strings.Join(entries[i:end+1], " + "), joined))
			out = append(out, joined)
			i = end + 1
			continue
		}
		if !bracketsBalanced(inners[i]) {
			notes = append(notes, "dropped related entry with unbalanced brackets "+entries[i])
			i++
			continue
		}
		out = append(out, entries[i])
		i++
	}
	return out, notes
}

// relatedJoinEnd returns the index of the last entry to rejoin with
// inners[start], or start when no join applies.
func relatedJoinEnd(inners []string, start int, known func(string) bool) int {
	firstUnbalanced := !bracketsBalanced(inners[start])
	for end := start + 1; end < len(inners) && end < start+maxRelatedJoinParts; end++ {
		joined := strings.Join(inners[start:end+1], ", ")
		if known != nil && known(joined) && !anyKnown(inners[start:end+1], known) {
			return end
		}
		if firstUnbalanced && !bracketsBalanced(inners[end]) && bracketsBalanced(joined) {
			return end
		}
	}
	return start
}

func anyKnown(names []string, known func(string) bool) bool {
	for _, name := range names {
		if known(name) {
			return true
		}
	}
	return false
}

// relatedStrings returns a decoded related: value as strings, or ok=false
// when it is not a list of strings.
func relatedStrings(v any) ([]string, bool) {
	switch list := v.(type) {
	case []string:
		return list, true
	case []any:
		entries := make([]string, 0, len(list))
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			entries = append(entries, s)
		}
		return entries, true
	default:
		return nil, false
	}
}

// repairRelatedValue applies repairSplitRelated to a decoded related: value.
// A value that is not a list of strings is returned unchanged.
func repairRelatedValue(v any, known func(string) bool) (any, []string) {
	entries, ok := relatedStrings(v)
	if !ok {
		return v, nil
	}
	repaired, notes := repairSplitRelated(entries, known)
	if len(notes) == 0 {
		return v, nil
	}
	out := make([]any, len(repaired))
	for i, entry := range repaired {
		out[i] = entry
	}
	return out, notes
}
