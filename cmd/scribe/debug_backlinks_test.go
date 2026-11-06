package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// _backlinks.json is keyed by canonical title, so `debug backlinks` must
// resolve an alias or case variant before the lookup, with and without
// --fresh.
func TestDebugBacklinksResolvesAliasAndCase(t *testing.T) {
	resolverTestKB(t)
	runBacklinks(t)
	want := []string{"B", "C", "wiki/_index.md"}

	for _, fresh := range []bool{false, true} {
		for _, name := range []string{"Alpha", "alpha", "a"} {
			var err error
			out := captureLintStdout(t, func() {
				err = (&DebugBacklinksCmd{Title: name, Fresh: fresh, JSON: true}).Run()
			})
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Title        string   `json:"title"`
				Sources      []string `json:"sources"`
				ResolvedFrom string   `json:"resolved_from"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("bad JSON: %v\n%s", err, out)
			}
			if got.Title != "A" || got.ResolvedFrom != name || !slices.Equal(got.Sources, want) {
				t.Errorf("fresh=%v %q: got %+v, want title A resolved from %q with %v", fresh, name, got, name, want)
			}
		}
	}

	out := captureLintStdout(t, func() {
		if err := (&DebugBacklinksCmd{Title: "alpha"}).Run(); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(out, "Backlinks for: A (resolved from alpha)") || !strings.Contains(out, "count: 3") {
		t.Errorf("text output should name the canonical title and the alias:\n%s", out)
	}

	out = captureLintStdout(t, func() {
		if err := (&DebugBacklinksCmd{Title: "A"}).Run(); err != nil {
			t.Error(err)
		}
	})
	if strings.Contains(out, "resolved from") {
		t.Errorf("an exact title should not report a resolution:\n%s", out)
	}
}
