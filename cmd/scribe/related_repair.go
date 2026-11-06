package main

import (
	"fmt"
	"strings"
)

// maxRelatedJoinParts bounds how many adjacent related: entries a
// title-lookup repair may rejoin: a title with up to three commas.
const maxRelatedJoinParts = 4

// maxBracketJoinParts bounds a bracket-balance repair, which needs no
// lookup: both ends must be unbalanced and only balance together, a much
// stronger signal, and real titles list a dozen short ids inside one
// parenthesis ("Campfire Review Threads (9bc7, 620a, …, 8e0f)").
const maxBracketJoinParts = 16

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
// At most maxRelatedJoinParts entries join by title, maxBracketJoinParts
// by brackets, nearest end first. An entry
// that still has unbalanced brackets afterwards is dropped. Entries the
// repair does not touch are returned verbatim. notes describes each change;
// it is empty when nothing changed.
func repairSplitRelated(entries []string, known *knownTitles) (out, notes []string) {
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
			// The other half may be gone (a dedupe kept one copy of a shared
			// "2023)"), but the KB can still name the whole title.
			if full, ok := known.Complete(inners[i]); ok {
				restored := full
				if _, wrapped := relatedEntryInner(entries[i]); wrapped {
					restored = "[[" + full + "]]"
				}
				notes = append(notes, fmt.Sprintf("completed split related entry %s → %s", entries[i], restored))
				out = append(out, restored)
			} else {
				notes = append(notes, "dropped related entry with unbalanced brackets "+entries[i])
			}
			i++
			continue
		}
		out = append(out, entries[i])
		i++
	}
	if len(notes) > 0 {
		out, notes = dedupeRelatedEntries(out, notes)
	}
	return out, notes
}

// dedupeRelatedEntries drops a later entry that names the same link as an
// earlier one: a rejoined or completed title can repeat one already listed.
func dedupeRelatedEntries(entries, notes []string) (out, outNotes []string) {
	seen := make(map[string]bool, len(entries))
	out = entries[:0:0]
	for _, entry := range entries {
		inner, _ := relatedEntryInner(entry)
		if seen[inner] {
			notes = append(notes, "dropped duplicate related entry "+entry)
			continue
		}
		seen[inner] = true
		out = append(out, entry)
	}
	return out, notes
}

// relatedJoinEnd returns the index of the last entry to rejoin with
// inners[start], or start when no join applies.
func relatedJoinEnd(inners []string, start int, known *knownTitles) int {
	firstUnbalanced := !bracketsBalanced(inners[start])
	for end := start + 1; end < len(inners) && end < start+maxBracketJoinParts; end++ {
		joined := strings.Join(inners[start:end+1], ", ")
		if end < start+maxRelatedJoinParts && known.Has(joined) && !anyKnown(inners[start:end+1], known) {
			return end
		}
		if firstUnbalanced && !bracketsBalanced(inners[end]) && bracketsBalanced(joined) {
			return end
		}
	}
	return start
}

func anyKnown(names []string, known *knownTitles) bool {
	for _, name := range names {
		if known.Has(name) {
			return true
		}
	}
	return false
}

// knownTitles is the KB's title and alias set as the related: repair uses
// it. A nil *knownTitles knows nothing, which limits the repair to
// bracket balance.
type knownTitles struct {
	has      func(string) bool
	complete func(fragment string) (string, bool)
}

// Has reports whether name is a title or alias.
func (k *knownTitles) Has(name string) bool {
	return k != nil && k.has != nil && k.has(name)
}

// Complete returns the one title or alias that starts with fragment + ", "
// and has balanced brackets, the whole of a title whose other pieces were
// lost. ok=false when none or several match.
func (k *knownTitles) Complete(fragment string) (string, bool) {
	if k == nil || k.complete == nil {
		return "", false
	}
	return k.complete(fragment)
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
func repairRelatedValue(v any, known *knownTitles) (any, []string) {
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
