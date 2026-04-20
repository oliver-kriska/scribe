package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyDensity(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantDense string
		minWords  int
	}{
		{
			name:      "brief tweet",
			body:      "Quick thought about async extraction.",
			wantDense: "brief",
		},
		{
			name:      "standard essay",
			body:      strings.Repeat("word ", 1000) + "\n\n## One heading\n\nmore prose.",
			wantDense: "standard",
			minWords:  1000,
		},
		{
			name:      "dense by word count",
			body:      strings.Repeat("word ", 2500),
			wantDense: "dense",
			minWords:  2500,
		},
		{
			name:      "dense by heading count",
			body:      strings.Repeat("word ", 800) + "\n\n## A\n## B\n## C\n## D\n",
			wantDense: "dense",
		},
		{
			name:      "code fences don't count as headings",
			body:      "hi\n\n```\n## not a heading\n## nor this\n## nor this\n## nor this\n```\n\n",
			wantDense: "brief",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			words, density := classifyDensity(tc.body)
			if density != tc.wantDense {
				t.Errorf("density = %q, want %q (words=%d)", density, tc.wantDense, words)
			}
			if tc.minWords > 0 && words < tc.minWords {
				t.Errorf("word count = %d, want >= %d", words, tc.minWords)
			}
		})
	}
}

func TestReadRawDensity(t *testing.T) {
	tmpDir := t.TempDir()
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "frontmatter wins over heuristic",
			body: "---\ndensity: brief\n---\n\n" + strings.Repeat("word ", 3000),
			want: "brief",
		},
		{
			name: "heuristic fallback when frontmatter missing field",
			body: "---\ntitle: foo\n---\n\n" + strings.Repeat("word ", 3000),
			want: "dense",
		},
		{
			name: "heuristic fallback for no frontmatter at all",
			body: "short note",
			want: "brief",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(tmpDir, tc.name+".md")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := readRawDensity(path); got != tc.want {
				t.Errorf("readRawDensity = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStripFrontmatter(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"---\nfoo: bar\n---\n\nbody\n", "body\n"},
		{"no frontmatter here", "no frontmatter here"},
		{"---\nmalformed", "---\nmalformed"},
	}
	for _, tc := range cases {
		got := stripFrontmatter(tc.in)
		if got != tc.want {
			t.Errorf("stripFrontmatter(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRawArticleOptsIntoAbsorb(t *testing.T) {
	tmpDir := t.TempDir()
	cases := []struct {
		name string
		fm   string
		want bool
	}{
		{"absorb-true", "---\nabsorb: true\n---\nbody\n", true},
		{"named-domain", "---\ndomain: acme\n---\nbody\n", true},
		{"general-domain", "---\ndomain: general\n---\nbody\n", false},
		{"no-opt-in", "---\ntitle: foo\n---\nbody\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(tmpDir, tc.name+".md")
			if err := os.WriteFile(path, []byte(tc.fm), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := rawArticleOptsIntoAbsorb(path); got != tc.want {
				t.Errorf("rawArticleOptsIntoAbsorb = %v, want %v", got, tc.want)
			}
		})
	}
}
