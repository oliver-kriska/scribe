package main

import (
	"strconv"
	"strings"
)

// yaml_scalar.go centralizes one rule scribe kept getting wrong: emitting a
// string into YAML frontmatter without checking whether it can survive as a
// plain (unquoted) scalar. The identity-apply path wrote `  - @omarsar0` for
// an @handle alias, which YAML rejects ("@" is a reserved indicator that
// cannot start a token), silently corrupting people/*.md frontmatter. Any
// code that writes a user-derived value into frontmatter should route it
// through yamlQuoteScalar.

// yamlQuoteScalar returns s ready to drop into a YAML block ("key: <scalar>"
// or "  - <scalar>"). Values that would be misparsed as something other than
// a string — indicator-led (@, -, :, etc.), reserved words, numbers, or
// containing structural sequences — are single-quoted with internal "'"
// doubled. Plain strings pass through unquoted to keep diffs (and re-runs)
// stable.
func yamlQuoteScalar(s string) string {
	if !yamlNeedsQuote(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// yamlNeedsQuote reports whether s must be quoted to round-trip as a YAML
// string scalar. Deliberately conservative: a false positive only adds
// harmless quotes, while a false negative corrupts the document.
func yamlNeedsQuote(s string) bool {
	if s == "" {
		return true
	}
	// Leading/trailing whitespace doesn't survive a plain scalar.
	if s != strings.TrimSpace(s) {
		return true
	}
	// First-rune indicators that can't begin a plain scalar.
	switch s[0] {
	case '!', '&', '*', '-', '?', ':', ',', '[', ']', '{', '}',
		'#', '|', '>', '@', '`', '"', '\'', '%', ' ', '\t':
		return true
	}
	// Structural sequences anywhere in the value.
	if strings.Contains(s, ": ") || strings.HasSuffix(s, ":") ||
		strings.Contains(s, " #") || strings.ContainsAny(s, "\t\n") {
		return true
	}
	// Reserved words YAML would decode to bool/null.
	switch strings.ToLower(s) {
	case "true", "false", "null", "yes", "no", "on", "off", "~":
		return true
	}
	// A bare number would decode to int/float, not a string.
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	return false
}
