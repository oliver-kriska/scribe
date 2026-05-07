package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeArticleForView creates a minimal article. Caller passes raw
// frontmatter lines so tests can drive any field shape (including
// list-typed values like tags).
func writeArticleForView(t *testing.T, dir, sub, slug string, fmLines []string) string {
	t.Helper()
	full := filepath.Join(dir, sub, slug+".md")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\n" + strings.Join(fmLines, "\n") + "\n---\n\nbody\n"
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return full
}

func TestMatchFilter_LeafOpsCoverClosedSet(t *testing.T) {
	row := articleRow{
		rel: "decisions/foo.md",
		fm: map[string]any{
			"title":   "Foo",
			"type":    "decision",
			"status":  "active",
			"weight":  10,
			"tags":    []any{"go", "kb"},
			"updated": "2026-04-01",
		},
	}

	cases := []struct {
		name string
		node FilterNode
		want bool
	}{
		{"eq match", FilterNode{Field: "type", Op: OpEq, Value: "decision"}, true},
		{"eq miss", FilterNode{Field: "type", Op: OpEq, Value: "research"}, false},
		{"ne", FilterNode{Field: "type", Op: OpNe, Value: "research"}, true},
		{"gt int", FilterNode{Field: "weight", Op: OpGt, Value: 5}, true},
		{"le int", FilterNode{Field: "weight", Op: OpLe, Value: 10}, true},
		{"in list", FilterNode{Field: "type", Op: OpIn, Value: []any{"decision", "pattern"}}, true},
		{"in miss", FilterNode{Field: "type", Op: OpIn, Value: []any{"research"}}, false},
		{"has list", FilterNode{Field: "tags", Op: OpHas, Value: "kb"}, true},
		{"has miss", FilterNode{Field: "tags", Op: OpHas, Value: "rust"}, false},
		{"contains", FilterNode{Field: "title", Op: OpContain, Value: "fo"}, true},
		{"contains case", FilterNode{Field: "title", Op: OpContain, Value: "FO"}, true},
		{"exists", FilterNode{Field: "status", Op: OpExists}, true},
		{"exists empty fails", FilterNode{Field: "missing_field", Op: OpExists}, false},
		{"missing absent", FilterNode{Field: "missing_field", Op: OpMissing}, true},
		{"date lt", FilterNode{Field: "updated", Op: OpLt, Value: "2026-05-01"}, true},
	}
	for _, c := range cases {
		got := matchFilter(c.node, row)
		if got != c.want {
			t.Errorf("%s: got=%v want=%v", c.name, got, c.want)
		}
	}
}

func TestMatchFilter_Containers(t *testing.T) {
	row := articleRow{
		fm: map[string]any{"type": "decision", "status": "active", "domain": "enaia"},
	}

	and := FilterNode{And: []FilterNode{
		{Field: "type", Op: OpEq, Value: "decision"},
		{Field: "status", Op: OpEq, Value: "active"},
	}}
	if !matchFilter(and, row) {
		t.Errorf("AND of two truthy clauses must match")
	}

	andMixed := FilterNode{And: []FilterNode{
		{Field: "type", Op: OpEq, Value: "decision"},
		{Field: "domain", Op: OpEq, Value: "other"},
	}}
	if matchFilter(andMixed, row) {
		t.Errorf("AND short-circuits on first false")
	}

	or := FilterNode{Or: []FilterNode{
		{Field: "type", Op: OpEq, Value: "research"},
		{Field: "type", Op: OpEq, Value: "decision"},
	}}
	if !matchFilter(or, row) {
		t.Errorf("OR must match when any clause is true")
	}

	not := FilterNode{Not: &FilterNode{Field: "type", Op: OpEq, Value: "research"}}
	if !matchFilter(not, row) {
		t.Errorf("NOT must invert a false leaf")
	}
}

func TestValidateFilterNode_RejectsMixedAndUnknownOps(t *testing.T) {
	mixed := FilterNode{
		And:   []FilterNode{{Field: "type", Op: OpEq, Value: "decision"}},
		Field: "status",
		Op:    OpEq,
		Value: "active",
	}
	if err := validateFilterNode(mixed); err == nil {
		t.Errorf("mixing container + leaf shapes must error")
	}

	unknown := FilterNode{Field: "type", Op: "matches", Value: ".*"}
	if err := validateFilterNode(unknown); err == nil {
		t.Errorf("unknown op must error")
	}

	empty := FilterNode{}
	if err := validateFilterNode(empty); err != nil {
		t.Errorf("empty node should be valid (matches everything); got %v", err)
	}
}

func TestRunView_FilterSortLimit(t *testing.T) {
	dir := t.TempDir()
	writeArticleForView(t, dir, "decisions", "a", []string{
		`title: "A"`, `type: decision`, `status: active`, `updated: "2026-04-01"`,
	})
	writeArticleForView(t, dir, "decisions", "b", []string{
		`title: "B"`, `type: decision`, `status: active`, `updated: "2026-05-01"`,
	})
	writeArticleForView(t, dir, "decisions", "c", []string{
		`title: "C"`, `type: decision`, `status: superseded`, `updated: "2026-04-15"`,
	})
	writeArticleForView(t, dir, "research", "d", []string{
		`title: "D"`, `type: research`, `status: active`, `updated: "2026-04-20"`,
	})

	rows, err := loadArticleRows(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(rows))
	}

	vf := &ViewFile{
		Filters: FilterNode{And: []FilterNode{
			{Field: "type", Op: OpEq, Value: "decision"},
			{Field: "status", Op: OpEq, Value: "active"},
		}},
		Sort: []SortKey{{Field: "updated", Direction: "desc"}},
		View: ViewSpec{Columns: []string{"title", "updated"}, Limit: 5},
	}

	out := runView(vf, rows)
	if len(out) != 2 {
		t.Fatalf("expected 2 active decisions, got %d", len(out))
	}
	if out[0]["title"] != "B" {
		t.Errorf("desc-sort by updated should put B first, got %q", out[0]["title"])
	}
}

func TestRunView_LimitTrimsAfterSort(t *testing.T) {
	dir := t.TempDir()
	for i, slug := range []string{"a", "b", "c"} {
		writeArticleForView(t, dir, "decisions", slug, []string{
			`title: "` + strings.ToUpper(slug) + `"`,
			`type: decision`,
			`status: active`,
			`weight: ` + []string{"1", "3", "2"}[i],
		})
	}

	rows, err := loadArticleRows(dir)
	if err != nil {
		t.Fatal(err)
	}
	vf := &ViewFile{
		Filters: FilterNode{Field: "type", Op: OpEq, Value: "decision"},
		Sort:    []SortKey{{Field: "weight", Direction: "desc"}},
		View:    ViewSpec{Columns: []string{"title", "weight"}, Limit: 2},
	}
	out := runView(vf, rows)
	if len(out) != 2 || out[0]["title"] != "B" || out[1]["title"] != "C" {
		t.Errorf("expected top-2 by weight desc to be [B, C]; got %+v", out)
	}
}

func TestRenderMarkdownTable_EscapesPipes(t *testing.T) {
	rows := []map[string]string{
		{"title": "with | pipe", "n": "1"},
	}
	out := renderMarkdownTable([]string{"title", "n"}, rows)
	if !strings.Contains(out, `with \| pipe`) {
		t.Errorf("pipe must be escaped in cell; got %q", out)
	}
	if !strings.Contains(out, "| --- |") {
		t.Errorf("missing separator row; got %q", out)
	}
}

func TestLoadViewFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	viewPath := filepath.Join(dir, "active-decisions.scribe-view.yaml")
	if err := os.WriteFile(viewPath, []byte(`name: "Active decisions"
filters:
  and:
    - { field: type, op: eq, value: decision }
    - { field: status, op: eq, value: active }
sort:
  - { field: updated, direction: desc }
view:
  columns: [title, updated, domain]
  limit: 20
`), 0o644); err != nil {
		t.Fatal(err)
	}
	vf, err := loadViewFile(viewPath)
	if err != nil {
		t.Fatal(err)
	}
	if vf.Name != "Active decisions" {
		t.Errorf("name not parsed: %q", vf.Name)
	}
	if len(vf.Filters.And) != 2 {
		t.Errorf("expected 2 AND clauses, got %d", len(vf.Filters.And))
	}
	if len(vf.View.Columns) != 3 || vf.View.Limit != 20 {
		t.Errorf("view shape wrong: %+v", vf.View)
	}
}
