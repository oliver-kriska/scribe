package main

import (
	"bytes"
	"slices"
)

// byteRange is a half-open [Start, End) byte range of a document.
type byteRange struct{ Start, End int }

// codeRanges returns the byte ranges of content that wikilink scanning must
// ignore: fenced code blocks, then double-backtick spans, then
// single-backtick spans, each searched only outside the ranges found
// before it. Double spans go first because “ `foo` “ nests a
// single-backtick pair inside a double-backtick span.
//
// A code span that sits wholly inside the text of a [[wikilink]] is not
// returned: the backticks are part of the linked title, as in
// [[The `foo` flag]]. A span that contains a link (`[[x]]`) or straddles one
// is still code.
func codeRanges(content []byte) []byteRange {
	fences := matchRanges(codeFenceRE.FindAllIndex(content, -1), 0)
	var spans []byteRange
	for _, gap := range gapsWithin(0, len(content), fences) {
		segment := content[gap.Start:gap.End]
		doubles := matchRanges(codeSpanDoubleRE.FindAllIndex(segment, -1), gap.Start)
		spans = append(spans, doubles...)
		for _, inner := range gapsWithin(gap.Start, gap.End, doubles) {
			sub := content[inner.Start:inner.End]
			spans = append(spans, matchRanges(codeSpanRE.FindAllIndex(sub, -1), inner.Start)...)
		}
	}
	links := wikilinkTextRanges(content, fences)
	out := slices.Clone(fences)
	for _, span := range spans {
		if !containedInAny(span, links) {
			out = append(out, span)
		}
	}
	slices.SortFunc(out, func(a, b byteRange) int { return a.Start - b.Start })
	return out
}

func matchRanges(idx [][]int, offset int) []byteRange {
	out := make([]byteRange, 0, len(idx))
	for _, m := range idx {
		out = append(out, byteRange{Start: m[0] + offset, End: m[1] + offset})
	}
	return out
}

// gapsWithin returns the parts of [start, end) that no range covers.
// ranges must be sorted and lie within [start, end).
func gapsWithin(start, end int, ranges []byteRange) []byteRange {
	var gaps []byteRange
	pos := start
	for _, r := range ranges {
		if r.Start > pos {
			gaps = append(gaps, byteRange{Start: pos, End: r.Start})
		}
		pos = max(pos, r.End)
	}
	if pos < end {
		gaps = append(gaps, byteRange{Start: pos, End: end})
	}
	return gaps
}

// wikilinkTextRanges returns the inner-text range of every wikilink outside
// a fenced block.
func wikilinkTextRanges(content []byte, fences []byteRange) []byteRange {
	var out []byteRange
	for _, m := range wikilinkRE.FindAllSubmatchIndex(content, -1) {
		whole := byteRange{Start: m[0], End: m[1]}
		if overlapsAny(whole, fences) {
			continue
		}
		out = append(out, byteRange{Start: m[2], End: m[3]})
	}
	return out
}

func containedInAny(r byteRange, ranges []byteRange) bool {
	return slices.ContainsFunc(ranges, func(o byteRange) bool { return r.Start >= o.Start && r.End <= o.End })
}

func overlapsAny(r byteRange, ranges []byteRange) bool {
	return slices.ContainsFunc(ranges, func(o byteRange) bool { return r.Start < o.End && o.Start < r.End })
}

// stripCode returns content with every codeRanges range removed.
func stripCode(content []byte) []byte {
	ranges := codeRanges(content)
	if len(ranges) == 0 {
		return content
	}
	var buf bytes.Buffer
	buf.Grow(len(content))
	pos := 0
	for _, r := range ranges {
		if r.Start > pos {
			buf.Write(content[pos:r.Start])
		}
		pos = max(pos, r.End)
	}
	buf.Write(content[min(pos, len(content)):])
	return buf.Bytes()
}
