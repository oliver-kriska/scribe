package main

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestYamlQuoteScalar(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		// The bug that started this: @handles must be quoted.
		{"@omarsar0", "'@omarsar0'"},
		// Other indicator-led values.
		{"- dash", "'- dash'"},
		{":colon", "':colon'"},
		{"*anchor", "'*anchor'"},
		{"#hash", "'#hash'"},
		// Structural sequences mid-value.
		{"key: value", "'key: value'"},
		{"trailing colon:", "'trailing colon:'"},
		// Reserved words and numbers would decode to non-strings.
		{"true", "'true'"},
		{"NULL", "'NULL'"},
		{"42", "'42'"},
		{"3.14", "'3.14'"},
		// A mid-scalar single quote is valid plain YAML — left unquoted.
		{"O'Brien", "O'Brien"},
		// When another rule forces quoting, the embedded quote is doubled.
		{"@O'Brien", "'@O''Brien'"},
		// Plain strings pass through untouched (clean diffs, idempotent).
		{"Omar Sanseviero", "Omar Sanseviero"},
		{"plain-slug", "plain-slug"},
		{"CamelCase", "CamelCase"},
	}
	for _, tc := range cases {
		if got := yamlQuoteScalar(tc.in); got != tc.want {
			t.Errorf("yamlQuoteScalar(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestYAMLDoubleQuoteRoundTrips: every writer that emits `title: "…"`
// used to escape only the quote character, so a title ending in a
// backslash (or carrying a newline) broke the article's frontmatter.
func TestYAMLDoubleQuoteRoundTrips(t *testing.T) {
	cases := []string{
		"plain",
		`with "quotes"`,
		`ends with backslash \`,
		`back\slash "and" quote`,
		"line one\nline two",
		"tab\tsep",
		"unicode — dash ✓",
		"",
	}
	for _, in := range cases {
		doc := "title: " + yamlDoubleQuote(in) + "\n"
		var out struct {
			Title string `yaml:"title"`
		}
		if err := yaml.Unmarshal([]byte(doc), &out); err != nil {
			t.Errorf("%q: emitted YAML does not parse: %v (%s)", in, err, doc)
			continue
		}
		if out.Title != in {
			t.Errorf("%q: round trip gave %q", in, out.Title)
		}
	}
}

func TestUnquoteYAMLScalar(t *testing.T) {
	cases := map[string]string{
		`"[[Old]]"`:      "[[Old]]",
		`'[[Old]]'`:      "[[Old]]",
		`[[Old]]`:        "[[Old]]",
		`"say \"hi\""`:   `say "hi"`,
		`'it''s'`:        "it's",
		`  "padded"  `:   "padded",
		`"ends with \\"`: `ends with \`,
		`"unterminated`:  `"unterminated`,
	}
	for in, want := range cases {
		if got := unquoteYAMLScalar(in); got != want {
			t.Errorf("unquoteYAMLScalar(%q) = %q, want %q", in, got, want)
		}
	}
}
