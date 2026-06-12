package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Phase 7B — declarative views.
//
// A view file is a `.scribe-view.yaml` in `wiki/_views/` that declares
// a filter expression, a sort spec, and a column list over the
// frontmatter of every wiki article. `scribe view <name>` evaluates
// the file and prints a Markdown table, JSON, or CSV.
//
// Scope discipline (per plan §7B): frontmatter-only filters, no joins,
// no body scans, no aggregates beyond count. The evaluator is a small
// closed-set DSL — adding a comparison op is one switch arm, but new
// shapes (joins, computed columns) belong in v2 if they belong at all.

// ViewFile is the on-disk schema of a `.scribe-view.yaml`. Top-level
// `name`/`description` are optional human-facing labels; everything
// else is consumed by the runner.
type ViewFile struct {
	Name        string     `yaml:"name,omitempty"`
	Description string     `yaml:"description,omitempty"`
	Filters     FilterNode `yaml:"filters"`
	Sort        []SortKey  `yaml:"sort,omitempty"`
	View        ViewSpec   `yaml:"view"`
}

// FilterNode is a recursive tree. Container nodes carry `and`, `or`,
// or `not`; leaf nodes carry `field` + `op` + `value`. Exactly one
// shape applies per node — the parser tolerates either, but having
// both on the same node is a configuration error.
type FilterNode struct {
	And   []FilterNode `yaml:"and,omitempty"`
	Or    []FilterNode `yaml:"or,omitempty"`
	Not   *FilterNode  `yaml:"not,omitempty"`
	Field string       `yaml:"field,omitempty"`
	Op    string       `yaml:"op,omitempty"`
	Value any          `yaml:"value,omitempty"`
}

type SortKey struct {
	Field     string `yaml:"field"`
	Direction string `yaml:"direction,omitempty"` // asc | desc; default asc
}

type ViewSpec struct {
	Columns []string `yaml:"columns"`
	Limit   int      `yaml:"limit,omitempty"`
}

// Closed set of leaf operators. Anything outside this set is a
// configuration error — fail fast at parse time rather than at every
// per-row evaluation.
const (
	OpEq      = "eq"
	OpNe      = "ne"
	OpLt      = "lt"
	OpLe      = "le"
	OpGt      = "gt"
	OpGe      = "ge"
	OpIn      = "in"       // row.field equals any element of value (slice)
	OpHas     = "has"      // row.field is a slice and contains value
	OpExists  = "exists"   // row.field is set (non-nil, non-empty string)
	OpMissing = "missing"  // row.field is not set
	OpContain = "contains" // string field contains substring; case-insensitive
)

// viewsDir is the canonical home for view files. Always under wiki/
// so the version-controlled views travel with the KB.
func viewsDir(root string) string {
	return filepath.Join(root, "wiki", "_views")
}

// loadViewFile reads and validates one view file. Validation runs at
// load time so syntactic errors surface to `scribe view --check`
// without per-row noise during evaluation.
func loadViewFile(path string) (*ViewFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var vf ViewFile
	if err := yaml.Unmarshal(data, &vf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := validateFilterNode(vf.Filters); err != nil {
		return nil, fmt.Errorf("filters in %s: %w", path, err)
	}
	if len(vf.View.Columns) == 0 {
		return nil, fmt.Errorf("view.columns must list at least one column in %s", path)
	}
	for _, sk := range vf.Sort {
		if sk.Field == "" {
			return nil, fmt.Errorf("sort entry missing 'field' in %s", path)
		}
		if sk.Direction != "" && sk.Direction != "asc" && sk.Direction != "desc" {
			return nil, fmt.Errorf("sort.direction must be asc or desc in %s", path)
		}
	}
	return &vf, nil
}

// validateFilterNode walks the tree and ensures every node is either
// purely a container or purely a leaf, with a recognized operator on
// leaves. Empty top-level filters are valid (matches every article).
func validateFilterNode(n FilterNode) error {
	hasContainer := len(n.And) > 0 || len(n.Or) > 0 || n.Not != nil
	hasLeaf := n.Field != "" || n.Op != "" || n.Value != nil
	if hasContainer && hasLeaf {
		return errors.New("node mixes container (and/or/not) with leaf (field/op/value)")
	}
	if hasLeaf {
		if n.Field == "" || n.Op == "" {
			return errors.New("leaf node missing field or op")
		}
		switch n.Op {
		case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe, OpIn, OpHas, OpContain:
			// ok — value required
			if n.Value == nil {
				return fmt.Errorf("op %q requires a value", n.Op)
			}
		case OpExists, OpMissing:
			// no value
		default:
			return fmt.Errorf("unknown op %q", n.Op)
		}
	}
	for _, c := range n.And {
		if err := validateFilterNode(c); err != nil {
			return err
		}
	}
	for _, c := range n.Or {
		if err := validateFilterNode(c); err != nil {
			return err
		}
	}
	if n.Not != nil {
		if err := validateFilterNode(*n.Not); err != nil {
			return err
		}
	}
	return nil
}

// articleRow holds one article's frontmatter as a generic map plus
// the on-disk path. We carry the raw map (not the typed Frontmatter
// struct) so view filters and column projections can reference any
// frontmatter key without scribe needing a typed field for it.
type articleRow struct {
	path string
	rel  string
	fm   map[string]any
}

// loadArticleRows walks every wiki article and returns the union for
// the view runner. Skips articles whose frontmatter doesn't parse;
// silent because that's a lint concern, not a view concern.
func loadArticleRows(root string) ([]articleRow, error) {
	var rows []articleRow
	err := walkArticles(root, func(path string, content []byte) error {
		fm, err := parseFrontmatterRaw(content)
		if err != nil || fm == nil {
			return nil //nolint:nilerr // skip unparseable frontmatter
		}
		rel, _ := filepath.Rel(root, path)
		rows = append(rows, articleRow{
			path: path,
			rel:  filepath.ToSlash(rel),
			fm:   fm,
		})
		return nil
	})
	return rows, err
}

// matchFilter evaluates the filter tree against one article. Returns
// false on container nodes with mixed shapes; the load-time validator
// catches that case earlier — this guard is defense-in-depth.
func matchFilter(n FilterNode, row articleRow) bool {
	// Empty node matches everything (top-level missing filters).
	if len(n.And) == 0 && len(n.Or) == 0 && n.Not == nil &&
		n.Field == "" && n.Op == "" {
		return true
	}
	if len(n.And) > 0 {
		for _, c := range n.And {
			if !matchFilter(c, row) {
				return false
			}
		}
		return true
	}
	if len(n.Or) > 0 {
		for _, c := range n.Or {
			if matchFilter(c, row) {
				return true
			}
		}
		return false
	}
	if n.Not != nil {
		return !matchFilter(*n.Not, row)
	}
	return matchLeaf(n.Field, n.Op, n.Value, row)
}

func matchLeaf(field, op string, want any, row articleRow) bool {
	got, present := row.fm[field]

	switch op {
	case OpExists:
		return present && !isZeroish(got)
	case OpMissing:
		return !present || isZeroish(got)
	}

	if !present {
		// Field absent — everything except `missing` (handled above)
		// is a non-match. Don't try to coerce nil.
		return false
	}

	switch op {
	case OpEq:
		return scalarsEqual(got, want)
	case OpNe:
		return !scalarsEqual(got, want)
	case OpLt, OpLe, OpGt, OpGe:
		c, ok := compareScalars(got, want)
		if !ok {
			return false
		}
		switch op {
		case OpLt:
			return c < 0
		case OpLe:
			return c <= 0
		case OpGt:
			return c > 0
		case OpGe:
			return c >= 0
		}
	case OpIn:
		// `want` is a list; got matches any element.
		ws, ok := want.([]any)
		if !ok {
			return false
		}
		for _, w := range ws {
			if scalarsEqual(got, w) {
				return true
			}
		}
		return false
	case OpHas:
		// `got` is a list; checks for `want` membership.
		gs, ok := got.([]any)
		if !ok {
			return false
		}
		for _, g := range gs {
			if scalarsEqual(g, want) {
				return true
			}
		}
		return false
	case OpContain:
		// Substring match, both sides coerced to string.
		return strings.Contains(
			strings.ToLower(toString(got)),
			strings.ToLower(toString(want)),
		)
	}
	return false
}

// scalarsEqual coerces both sides to a comparable shape. Strings and
// numbers compare directly; time.Time → date string for cross-type
// comparison ("updated == 2026-05-07").
func scalarsEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == b
	}
	if at, ok := a.(time.Time); ok {
		a = at.Format("2006-01-02")
	}
	if bt, ok := b.(time.Time); ok {
		b = bt.Format("2006-01-02")
	}
	if af, aok := toFloat(a); aok {
		if bf, bok := toFloat(b); bok {
			return af == bf
		}
	}
	return toString(a) == toString(b)
}

// compareScalars returns -1/0/1 like strings.Compare. Returns ok=false
// if the values can't be ordered (e.g. comparing a list to a number).
func compareScalars(a, b any) (int, bool) {
	if at, ok := a.(time.Time); ok {
		a = at.Format("2006-01-02")
	}
	if bt, ok := b.(time.Time); ok {
		b = bt.Format("2006-01-02")
	}
	if af, aok := toFloat(a); aok {
		if bf, bok := toFloat(b); bok {
			switch {
			case af < bf:
				return -1, true
			case af > bf:
				return 1, true
			default:
				return 0, true
			}
		}
	}
	as, bs := toString(a), toString(b)
	return strings.Compare(as, bs), true
}

func isZeroish(v any) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) == ""
	}
	if l, ok := v.([]any); ok {
		return len(l) == 0
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err == nil {
			return f, true
		}
	}
	return 0, false
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case time.Time:
		return x.Format("2006-01-02")
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = toString(e)
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprintf("%v", x)
	}
}

// runView returns the projected rows for a parsed view file.
func runView(vf *ViewFile, rows []articleRow) []map[string]string {
	var matched []articleRow
	for _, r := range rows {
		if matchFilter(vf.Filters, r) {
			matched = append(matched, r)
		}
	}

	if len(vf.Sort) > 0 {
		sort.SliceStable(matched, func(i, j int) bool {
			for _, sk := range vf.Sort {
				ai := matched[i].fm[sk.Field]
				aj := matched[j].fm[sk.Field]
				c, ok := compareScalars(ai, aj)
				if !ok {
					continue
				}
				if c == 0 {
					continue
				}
				if sk.Direction == "desc" {
					return c > 0
				}
				return c < 0
			}
			return matched[i].rel < matched[j].rel
		})
	}

	if vf.View.Limit > 0 && len(matched) > vf.View.Limit {
		matched = matched[:vf.View.Limit]
	}

	out := make([]map[string]string, 0, len(matched))
	for _, r := range matched {
		row := make(map[string]string, len(vf.View.Columns)+1)
		row["_path"] = r.rel
		for _, col := range vf.View.Columns {
			if col == "path" {
				row[col] = r.rel
				continue
			}
			row[col] = toString(r.fm[col])
		}
		out = append(out, row)
	}
	return out
}

// renderMarkdownTable writes a GitHub-flavored Markdown table.
func renderMarkdownTable(columns []string, rows []map[string]string) string {
	var sb strings.Builder
	sb.WriteString("| ")
	sb.WriteString(strings.Join(columns, " | "))
	sb.WriteString(" |\n|")
	for range columns {
		sb.WriteString(" --- |")
	}
	sb.WriteString("\n")
	for _, r := range rows {
		sb.WriteString("| ")
		cells := make([]string, len(columns))
		for i, col := range columns {
			cells[i] = mdEscape(r[col])
		}
		sb.WriteString(strings.Join(cells, " | "))
		sb.WriteString(" |\n")
	}
	return sb.String()
}

// mdEscape replaces pipe characters that would break Markdown table
// cells. Newlines collapse to spaces — preserves layout, accepts the
// loss of multi-line cells.
func mdEscape(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// ---- CLI ----

// ViewCmd is the flat `scribe view` surface: pass a view name as the
// positional argument to evaluate it; pass --list to enumerate the
// available view files; pass --show to dump the parsed schema. Output
// format defaults to Markdown; --json and --csv switch it.
type ViewCmd struct {
	Name string `arg:"" optional:"" help:"View name (without .scribe-view.yaml suffix)."`
	List bool   `help:"List view files in wiki/_views/ instead of running one."`
	Show bool   `help:"Print the parsed view schema instead of running it."`
	JSON bool   `help:"Output JSON array."`
	CSV  bool   `help:"Output CSV."`
}

func (c *ViewCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	if c.List {
		return listViews(root)
	}
	if c.Name == "" {
		return errors.New("usage: scribe view <name> | scribe view --list")
	}
	path := resolveViewPath(root, c.Name)
	vf, err := loadViewFile(path)
	if err != nil {
		return err
	}
	if c.Show {
		data, _ := yaml.Marshal(vf)
		fmt.Println(string(data))
		return nil
	}
	rows, err := loadArticleRows(root)
	if err != nil {
		return err
	}
	out := runView(vf, rows)
	switch {
	case c.JSON:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	case c.CSV:
		w := csv.NewWriter(os.Stdout)
		_ = w.Write(vf.View.Columns)
		for _, row := range out {
			cells := make([]string, len(vf.View.Columns))
			for i, col := range vf.View.Columns {
				cells[i] = row[col]
			}
			if err := w.Write(cells); err != nil {
				return err
			}
		}
		w.Flush()
		return w.Error()
	default:
		fmt.Print(renderMarkdownTable(vf.View.Columns, out))
		fmt.Printf("\n_(%d rows)_\n", len(out))
	}
	return nil
}

func listViews(root string) error {
	dir := viewsDir(root)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("(no views — create wiki/_views/<name>.scribe-view.yaml)")
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".scribe-view.yaml") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".scribe-view.yaml")
		vf, err := loadViewFile(filepath.Join(dir, e.Name()))
		if err != nil {
			fmt.Printf("%-40s  [INVALID] %v\n", name, err)
			continue
		}
		desc := vf.Description
		if desc == "" {
			desc = vf.Name
		}
		fmt.Printf("%-40s  %s\n", name, desc)
	}
	return nil
}

// resolveViewPath accepts either a bare name (`active-decisions`) or a
// full path. Adds the `.scribe-view.yaml` suffix when the bare name
// arrives without it.
func resolveViewPath(root, name string) string {
	if strings.HasSuffix(name, ".scribe-view.yaml") || strings.Contains(name, "/") {
		if filepath.IsAbs(name) {
			return name
		}
		return filepath.Join(root, name)
	}
	return filepath.Join(viewsDir(root), name+".scribe-view.yaml")
}
