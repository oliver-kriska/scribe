package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEdgesFromFrontmatter_AllKinds(t *testing.T) {
	fm := &Frontmatter{
		Title:        "Demo",
		Type:         "decision",
		Supersedes:   []any{"[[Old Decision]]"},
		SupersededBy: []any{"[[Newer Decision]]"},
		Contradicts:  []any{"[[Conflicting Decision]]"},
	}
	got := edgesFromFrontmatter(fm)
	if len(got) != 3 {
		t.Fatalf("want 3 edges, got %d: %+v", len(got), got)
	}
	wantKinds := map[RelationKind]string{
		RelSupersedes:   "Old Decision",
		RelSupersededBy: "Newer Decision",
		RelContradicts:  "Conflicting Decision",
	}
	for _, e := range got {
		if want, ok := wantKinds[e.Kind]; !ok || want != e.Target {
			t.Errorf("unexpected edge: %+v", e)
		}
	}
}

func TestEdgesFromFrontmatter_StripsWikilinkBrackets(t *testing.T) {
	fm := &Frontmatter{Type: "solution", AppliesTo: []any{"[[Pattern X]]", "Pattern Y"}}
	edges := edgesFromFrontmatter(fm)
	if len(edges) != 2 {
		t.Fatalf("want 2 edges, got %d", len(edges))
	}
	if edges[0].Target != "Pattern X" || edges[1].Target != "Pattern Y" {
		t.Errorf("targets stripped wrong: %+v", edges)
	}
}

func TestEdgesFromFrontmatter_DropsAliasSuffix(t *testing.T) {
	fm := &Frontmatter{Type: "decision", Supersedes: []any{"[[Old Title|Display]]"}}
	edges := edgesFromFrontmatter(fm)
	if len(edges) != 1 || edges[0].Target != "Old Title" {
		t.Errorf("alias suffix should be dropped: %+v", edges)
	}
}

func TestValidateTypedRelations_AllowedOnType(t *testing.T) {
	// supersedes is allowed on decision, not on research.
	ok := &Frontmatter{Type: "decision", Supersedes: []any{"[[X]]"}}
	if errs := validateTypedRelations(ok); len(errs) > 0 {
		t.Errorf("supersedes on decision should be allowed: %v", errs)
	}
	bad := &Frontmatter{Type: "research", Supersedes: []any{"[[X]]"}}
	errs := validateTypedRelations(bad)
	if len(errs) == 0 {
		t.Errorf("supersedes on research should be rejected")
	}
}

func TestInverseOf(t *testing.T) {
	cases := []struct {
		in   RelationKind
		want RelationKind
		ok   bool
	}{
		{RelSupersedes, RelSupersededBy, true},
		{RelSupersededBy, RelSupersedes, true},
		{RelContradicts, RelContradicts, true}, // self-inverse
		{RelInstanceOf, RelSpecializes, true},
		{RelSpecializes, RelInstanceOf, true},
		{RelExtends, "", false}, // no closed-set inverse defined
		{RelDerivedFrom, "", false},
	}
	for _, c := range cases {
		got, ok := inverseOf(c.in)
		if ok != c.ok {
			t.Errorf("%s: ok=%v want %v", c.in, ok, c.ok)
		}
		if c.ok && got != c.want {
			t.Errorf("%s: got %s, want %s", c.in, got, c.want)
		}
	}
}

func TestAddTypedEdge_NewKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	original := "---\ntitle: \"A\"\ntype: decision\n---\n\nbody\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := addTypedEdgeToFrontmatter(path, RelSupersedes, "B"); err != nil {
		t.Fatalf("add: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), `supersedes: ["[[B]]"]`) {
		t.Errorf("expected new key with inline list; got:\n%s", got)
	}
}

func TestAddTypedEdge_AppendsToExistingInline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	original := "---\ntitle: \"A\"\ntype: decision\nsupersedes: [\"[[B]]\"]\n---\n\nbody\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := addTypedEdgeToFrontmatter(path, RelSupersedes, "C"); err != nil {
		t.Fatalf("add: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), `"[[B]]"`) || !strings.Contains(string(got), `"[[C]]"`) {
		t.Errorf("expected both edges present; got:\n%s", got)
	}
}

func TestAddTypedEdge_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	original := "---\ntitle: \"A\"\ntype: decision\nsupersedes: [\"[[B]]\"]\n---\n\nbody\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := addTypedEdgeToFrontmatter(path, RelSupersedes, "B"); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(path)
	count := strings.Count(string(got), `"[[B]]"`)
	if count != 1 {
		t.Errorf("idempotent add should yield 1 entry, got %d:\n%s", count, got)
	}
}

func TestRemoveTypedEdge_RemovesAndDropsEmptyKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	original := "---\ntitle: \"A\"\ntype: decision\nsupersedes: [\"[[B]]\"]\n---\n\nbody\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeTypedEdgeFromFrontmatter(path, RelSupersedes, "B"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if strings.Contains(string(got), "supersedes:") {
		t.Errorf("empty supersedes key should be dropped; got:\n%s", got)
	}
}

func TestRemoveTypedEdge_KeepsOtherTargets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	original := "---\ntitle: \"A\"\ntype: decision\nsupersedes: [\"[[B]]\", \"[[C]]\"]\n---\n\nbody\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeTypedEdgeFromFrontmatter(path, RelSupersedes, "B"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), `"[[C]]"`) {
		t.Errorf("expected C preserved; got:\n%s", got)
	}
	if strings.Contains(string(got), `"[[B]]"`) {
		t.Errorf("expected B removed; got:\n%s", got)
	}
}

func TestSplitInlineList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`"a", "b", "c"`, []string{`"a"`, `"b"`, `"c"`}},
		{`"a, b", "c"`, []string{`"a, b"`, `"c"`}}, // comma inside quotes preserved
		{`"a"`, []string{`"a"`}},
		{``, nil},
	}
	for _, c := range cases {
		got := splitInlineList(c.in)
		if len(got) != len(c.want) {
			t.Errorf("split(%q): got %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("split(%q)[%d]: got %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestValidateKindOnType(t *testing.T) {
	if errs := validateKindOnType(RelSupersedes, "decision"); len(errs) > 0 {
		t.Errorf("supersedes on decision should be allowed: %v", errs)
	}
	errs := validateKindOnType(RelSupersedes, "research")
	if len(errs) == 0 {
		t.Errorf("supersedes on research should be rejected")
	}
	if errs := validateKindOnType(RelSupersedes, ""); len(errs) > 0 {
		t.Errorf("empty type should be a pass-through: %v", errs)
	}
}
