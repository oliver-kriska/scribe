package main

import "testing"

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
