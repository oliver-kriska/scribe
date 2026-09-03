package main

import "unicode/utf8"

// truncateBytes cuts s to at most maxBytes bytes without splitting a
// multi-byte rune. A plain s[:n] on UTF-8 text lands inside a rune
// whenever the boundary falls mid-character and leaves a broken
// sequence that YAML/JSON encoders reject or replace with U+FFFD.
// (truncateUTF8 in hot.go is the rune-count variant with an ellipsis,
// for display; this one is the byte budget for prompt packets.)
func truncateBytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// tailBytes keeps the last maxBytes bytes of s, starting on a rune
// boundary. Used where the newest text matters (log excerpts).
func tailBytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}
