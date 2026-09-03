package main

import (
	"strconv"
	"strings"
	"unicode"
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

// yamlDoubleQuote returns s as a YAML double-quoted scalar. Writers that
// emit `title: "…"` used to escape only the quote character, so a title
// ending in a backslash (`…\`) turned the closing quote into `\"` and the
// article stopped parsing; embedded newlines and control characters broke
// it the same way. Every byte that cannot sit inside a double-quoted YAML
// scalar is escaped here.
func yamlDoubleQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if unicode.IsControl(r) || r == utf8RuneError {
				continue // no representation worth keeping
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

const utf8RuneError = '\uFFFD'

// unquoteYAMLScalar strips one layer of YAML quoting from a raw frontmatter
// value so it can be re-emitted through yamlDoubleQuote instead of being
// wrapped in a second pair of quotes ("\"[[Old]]\"" is not valid YAML).
// Plain values pass through unchanged.
func unquoteYAMLScalar(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		inner := v[1 : len(v)-1]
		if u, err := strconv.Unquote(v); err == nil {
			return u
		}
		return strings.ReplaceAll(inner, `\"`, `"`)
	}
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	}
	return v
}
