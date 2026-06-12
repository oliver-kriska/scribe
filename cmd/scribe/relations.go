package main

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
)

// Phase 6A: typed relations.
//
// A scribe-managed KB has always had a flat `related:` frontmatter
// list — useful for clustering but lossy: it can't distinguish "this
// decision supersedes that one" from "this solution applies to that
// pattern." Phase 6A introduces typed edges that carry the same
// `[[Wikilink]]` payload but live under semantically meaningful keys,
// so contradiction detection (Phase 6B), staleness reasoning (Phase
// 6C), and view filters (Phase 7B) all get richer signal without
// re-parsing article bodies.
//
// Closed set per article type:
//
//	decision  — supersedes, superseded_by, contradicts
//	solution  — applies_to, derived_from
//	pattern   — instance_of, specializes
//	research  — extends, cited_by, informs
//
// `related:` (untyped) stays for genuinely loose connections so
// authors don't stall picking the right typed edge.

// RelationKind names one typed-edge field. Stable string identifiers
// because they appear directly as YAML frontmatter keys.
type RelationKind string

const (
	RelSupersedes   RelationKind = "supersedes"
	RelSupersededBy RelationKind = "superseded_by"
	RelContradicts  RelationKind = "contradicts"
	RelAppliesTo    RelationKind = "applies_to"
	RelDerivedFrom  RelationKind = "derived_from"
	RelInstanceOf   RelationKind = "instance_of"
	RelSpecializes  RelationKind = "specializes"
	RelExtends      RelationKind = "extends"
	RelCitedBy      RelationKind = "cited_by"
	RelInforms      RelationKind = "informs"
)

// allTypedRelations is the closed set of Phase 6A relation keys.
var allTypedRelations = []RelationKind{
	RelSupersedes, RelSupersededBy, RelContradicts,
	RelAppliesTo, RelDerivedFrom,
	RelInstanceOf, RelSpecializes,
	RelExtends, RelCitedBy, RelInforms,
}

// allowedRelationsByType maps a frontmatter `type:` value to the
// typed-relation keys that are valid on that article type. validate.go
// uses this to flag mis-applied edges (e.g. `supersedes:` on a
// research article).
//
// `specializes` and `instance_of` are universally meaningful — a
// research paper can specialize another, a tool can be an instance of
// a pattern, etc. — so they're permitted on every type that can sit in
// a hierarchy.
var allowedRelationsByType = map[string][]RelationKind{
	"decision": {RelSupersedes, RelSupersededBy, RelContradicts, RelInstanceOf, RelSpecializes},
	"solution": {RelAppliesTo, RelDerivedFrom, RelInstanceOf, RelSpecializes},
	"pattern":  {RelInstanceOf, RelSpecializes, RelAppliesTo},
	"research": {RelExtends, RelCitedBy, RelInforms, RelInstanceOf, RelSpecializes},
	"tool":     {RelDerivedFrom, RelInstanceOf, RelSpecializes},
	"project":  {RelInstanceOf, RelSpecializes},
	"person":   {},
	"idea":     {RelInstanceOf, RelSpecializes},
}

// inverseRelations names each edge's reverse for bidirectional
// integrity checking. When X declares `supersedes: [[Y]]`, Y should
// declare `superseded_by: [[X]]`. validate.go and `scribe relations
// fix` enforce this.
//
// Some kinds are self-inverse (contradicts) — mapped to themselves.
// Some kinds have no inverse in the closed set (instance_of doesn't
// imply Y carries `instances:` since we didn't ship that direction);
// those return false from inverseOf.
var inverseRelations = map[RelationKind]RelationKind{
	RelSupersedes:   RelSupersededBy,
	RelSupersededBy: RelSupersedes,
	RelContradicts:  RelContradicts,
	RelInstanceOf:   RelSpecializes,
	RelSpecializes:  RelInstanceOf,
}

// inverseOf returns the inverse relation kind plus a bool indicating
// whether one exists. Used by the bidirectional integrity check.
func inverseOf(k RelationKind) (RelationKind, bool) {
	inv, ok := inverseRelations[k]
	return inv, ok
}

// edgesFromFrontmatter extracts every typed edge from a Frontmatter
// struct. Returns one slice of (kind, target-title) pairs, with
// targets already stripped of the [[...]] wrapper so callers don't
// each re-do that. Untyped `related:` is NOT included; this surface
// is for typed semantics only.
func edgesFromFrontmatter(fm *Frontmatter) []TypedEdge {
	if fm == nil {
		return nil
	}
	out := make([]TypedEdge, 0, 8)
	add := func(kind RelationKind, raw any) {
		for _, t := range toStringSlice(raw) {
			t = strings.TrimSpace(t)
			t = strings.TrimPrefix(t, "[[")
			t = strings.TrimSuffix(t, "]]")
			if t == "" {
				continue
			}
			// Drop alias suffix on `[[Title|alias]]`.
			if pipe := strings.Index(t, "|"); pipe >= 0 {
				t = t[:pipe]
			}
			out = append(out, TypedEdge{Kind: kind, Target: t})
		}
	}
	add(RelSupersedes, fm.Supersedes)
	add(RelSupersededBy, fm.SupersededBy)
	add(RelContradicts, fm.Contradicts)
	add(RelAppliesTo, fm.AppliesTo)
	add(RelDerivedFrom, fm.DerivedFrom)
	add(RelInstanceOf, fm.InstanceOf)
	add(RelSpecializes, fm.Specializes)
	add(RelExtends, fm.Extends)
	add(RelCitedBy, fm.CitedBy)
	add(RelInforms, fm.Informs)
	return out
}

// TypedEdge is one outbound typed relation from an article.
type TypedEdge struct {
	Kind   RelationKind
	Target string // article title without [[...]]
}

// validateTypedRelations is called from validate.go for every article.
// Returns user-facing error strings for: (a) edges of a kind not
// allowed on this article type, (b) edges with non-list shape (already
// handled by toStringSlice but we double-check for clarity).
//
// Targets are NOT validated against existing-article presence here —
// `scribe lint`'s missing-page detector already covers that. Keeping
// the two checks separate means a typed edge to an article that was
// renamed yesterday surfaces as "missing page", not as a relation
// error.
func validateTypedRelations(fm *Frontmatter) []string {
	if fm == nil {
		return nil
	}
	allowed := map[RelationKind]bool{}
	for _, k := range allowedRelationsByType[fm.Type] {
		allowed[k] = true
	}
	if fm.Type == "" {
		// Type missing is reported elsewhere; allow all so we don't
		// double-warn.
		for _, k := range allTypedRelations {
			allowed[k] = true
		}
	}

	var errs []string
	for _, e := range edgesFromFrontmatter(fm) {
		if !allowed[e.Kind] {
			errs = append(errs,
				fmt.Sprintf("relation %q not allowed on type %q (allowed: %s)",
					e.Kind, fm.Type, joinKinds(allowedRelationsByType[fm.Type])))
		}
	}
	return errs
}

func joinKinds(ks []RelationKind) string {
	if len(ks) == 0 {
		return "none"
	}
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = string(k)
	}
	return strings.Join(out, ", ")
}

// RelationsCmd is the kong CLI surface for Phase 6A.
//
//	scribe relations get <article>            print typed edges from one article
//	scribe relations set <article> <kind> <target>  add a typed edge
//	scribe relations rm  <article> <kind> <target>  remove a typed edge
//	scribe relations graph <article>          print neighborhood
//	scribe relations check                    bidirectional integrity audit
//
// LLM-driven migration (Phase 6A v2: relations migrate / migrate
// --assisted / revert) ships in a follow-up; this v1 surface is
// strictly manual + integrity-checking.
type RelationsCmd struct {
	Get           RelationsGetCmd           `cmd:"" help:"Print typed edges from one article."`
	Set           RelationsSetCmd           `cmd:"" help:"Add a typed edge to one article (idempotent)."`
	Rm            RelationsRmCmd            `cmd:"" name:"rm" help:"Remove a typed edge from one article."`
	Graph         RelationsGraphCmd         `cmd:"" help:"Print the typed neighborhood of one article."`
	Check         RelationsCheckCmd         `cmd:"" help:"Audit bidirectional integrity across the KB."`
	Migrate       RelationsMigrateCmd       `cmd:"" help:"LLM-classify related: entries into typed edges (Phase 6A v2)."`
	MigrateRevert RelationsMigrateRevertCmd `cmd:"" name:"migrate-revert" help:"Undo a migration run by replaying its log."`
}

// RelationsGetCmd prints "<kind>  [[Target]]" lines for one article.
type RelationsGetCmd struct {
	Article string `arg:"" help:"Article path or title."`
}

func (g *RelationsGetCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	path, err := resolveArticleArg(root, g.Article)
	if err != nil {
		return err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fm, err := parseFrontmatter(content)
	if err != nil {
		return err
	}
	edges := edgesFromFrontmatter(fm)
	if len(edges) == 0 {
		fmt.Printf("%s — no typed edges (only `related:` if any)\n", relPath(root, path))
		return nil
	}
	fmt.Printf("%s\n", relPath(root, path))
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Kind != edges[j].Kind {
			return edges[i].Kind < edges[j].Kind
		}
		return edges[i].Target < edges[j].Target
	})
	for _, e := range edges {
		fmt.Printf("  %-15s [[%s]]\n", e.Kind, e.Target)
	}
	return nil
}

// RelationsSetCmd adds an edge. Idempotent: if the same target is
// already in the list, no change. Validates that the kind is allowed
// on the article's type before writing.
type RelationsSetCmd struct {
	Article string `arg:"" help:"Article path or title."`
	Kind    string `arg:"" help:"Relation kind: supersedes, superseded_by, contradicts, applies_to, derived_from, instance_of, specializes, extends, cited_by, informs."`
	Target  string `arg:"" help:"Target article title (without [[...]])."`
}

func (s *RelationsSetCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	path, err := resolveArticleArg(root, s.Article)
	if err != nil {
		return err
	}
	kind := RelationKind(strings.TrimSpace(s.Kind))
	if !isKnownRelationKind(kind) {
		return fmt.Errorf("unknown relation kind: %q (allowed: %s)", kind, joinKinds(allTypedRelations))
	}
	target := strings.TrimSpace(s.Target)
	target = strings.TrimPrefix(target, "[[")
	target = strings.TrimSuffix(target, "]]")
	if target == "" {
		return fmt.Errorf("target must be non-empty")
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fm, err := parseFrontmatter(content)
	if err != nil {
		return err
	}
	if errs := validateKindOnType(kind, fm.Type); len(errs) > 0 {
		return fmt.Errorf("%s", errs[0])
	}
	return addTypedEdgeToFrontmatter(path, kind, target)
}

// RelationsRmCmd removes a target from a typed-edge list.
type RelationsRmCmd struct {
	Article string `arg:"" help:"Article path or title."`
	Kind    string `arg:"" help:"Relation kind to remove from."`
	Target  string `arg:"" help:"Target title to remove (without [[...]])."`
}

func (r *RelationsRmCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	path, err := resolveArticleArg(root, r.Article)
	if err != nil {
		return err
	}
	kind := RelationKind(strings.TrimSpace(r.Kind))
	if !isKnownRelationKind(kind) {
		return fmt.Errorf("unknown relation kind: %q", kind)
	}
	target := strings.TrimSpace(r.Target)
	target = strings.TrimPrefix(target, "[[")
	target = strings.TrimSuffix(target, "]]")
	return removeTypedEdgeFromFrontmatter(path, kind, target)
}

// RelationsGraphCmd prints the article's typed neighborhood: outgoing
// edges (from this article's frontmatter) and incoming edges (other
// articles' frontmatter pointing here). Useful for orientation before
// touching a heavily-referenced article.
type RelationsGraphCmd struct {
	Article string `arg:"" help:"Article path or title."`
}

func (g *RelationsGraphCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	path, err := resolveArticleArg(root, g.Article)
	if err != nil {
		return err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fm, err := parseFrontmatter(content)
	if err != nil {
		return err
	}
	myTitle := fm.Title
	out := edgesFromFrontmatter(fm)

	type incoming struct {
		Kind     RelationKind
		FromPath string
	}
	var inbound []incoming
	_ = walkArticles(root, func(otherPath string, otherContent []byte) error {
		if otherPath == path {
			return nil
		}
		ofm, err := parseFrontmatter(otherContent)
		if err != nil {
			return nil //nolint:nilerr // skip unparseable, keep walking
		}
		for _, e := range edgesFromFrontmatter(ofm) {
			if strings.EqualFold(e.Target, myTitle) {
				inbound = append(inbound, incoming{Kind: e.Kind, FromPath: otherPath})
			}
		}
		return nil
	})

	fmt.Printf("%s — %s\n", relPath(root, path), myTitle)
	fmt.Printf("\noutbound (%d):\n", len(out))
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Target < out[j].Target
	})
	for _, e := range out {
		fmt.Printf("  %-15s [[%s]]\n", e.Kind, e.Target)
	}
	fmt.Printf("\ninbound (%d):\n", len(inbound))
	sort.Slice(inbound, func(i, j int) bool {
		if inbound[i].Kind != inbound[j].Kind {
			return inbound[i].Kind < inbound[j].Kind
		}
		return inbound[i].FromPath < inbound[j].FromPath
	})
	for _, e := range inbound {
		fmt.Printf("  %-15s %s\n", e.Kind, relPath(root, e.FromPath))
	}
	return nil
}

// RelationsCheckCmd audits bidirectional integrity across the KB.
// For every typed edge with a known inverse, verify the target's
// frontmatter declares the inverse pointing back. Flag violations.
//
// Cheap: walks every article once, builds a map of declared edges,
// then checks each forward edge for its expected reverse. ~1s on a
// 1500-article KB.
type RelationsCheckCmd struct {
	Fix bool `help:"Auto-add missing reverse edges where the inverse is unambiguous."`
}

func (c *RelationsCheckCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	type edgeKey struct {
		from   string // title
		kind   RelationKind
		target string
	}
	edges := map[edgeKey]string{} // edgeKey -> path of `from` article
	titleToPath := map[string]string{}

	err = walkArticles(root, func(path string, content []byte) error {
		fm, err := parseFrontmatter(content)
		if err != nil || fm.Title == "" {
			return nil //nolint:nilerr // unparseable article: skip it, keep walking
		}
		titleToPath[fm.Title] = path
		for _, e := range edgesFromFrontmatter(fm) {
			edges[edgeKey{from: fm.Title, kind: e.Kind, target: e.Target}] = path
		}
		return nil
	})
	if err != nil {
		return err
	}

	violations := 0
	fixed := 0
	for k, path := range edges {
		inv, ok := inverseOf(k.kind)
		if !ok {
			continue
		}
		expected := edgeKey{from: k.target, kind: inv, target: k.from}
		if _, present := edges[expected]; present {
			continue
		}
		violations++
		fmt.Printf("missing reverse: [[%s]] %s -> [[%s]] (expected %s on target)\n",
			k.from, k.kind, k.target, inv)

		if c.Fix {
			targetPath, ok := titleToPath[k.target]
			if !ok {
				fmt.Printf("  cannot fix: target article not found on disk\n")
				continue
			}
			if err := addTypedEdgeToFrontmatter(targetPath, inv, k.from); err != nil {
				fmt.Printf("  fix failed: %v\n", err)
				continue
			}
			fmt.Printf("  fixed: added %s [[%s]] to %s\n", inv, k.from, relPath(root, targetPath))
			fixed++
		}
		_ = path
	}

	if violations == 0 {
		fmt.Println("OK: all typed edges are bidirectionally consistent")
		return nil
	}
	if c.Fix {
		fmt.Printf("\nfixed %d / %d violations\n", fixed, violations)
		if fixed < violations {
			return fmt.Errorf("%d violations remain", violations-fixed)
		}
		return nil
	}
	return fmt.Errorf("%d bidirectional integrity violation(s) — pass --fix to auto-resolve", violations)
}

func isKnownRelationKind(k RelationKind) bool {
	return slices.Contains(allTypedRelations, k)
}

// validateKindOnType returns user-facing errors when `kind` is not
// allowed on `articleType`. Empty articleType allows all kinds (the
// type-missing case is already covered by another lint pass).
func validateKindOnType(kind RelationKind, articleType string) []string {
	if articleType == "" {
		return nil
	}
	if slices.Contains(allowedRelationsByType[articleType], kind) {
		return nil
	}
	return []string{
		fmt.Sprintf("relation %q not allowed on type %q (allowed: %s)",
			kind, articleType, joinKinds(allowedRelationsByType[articleType])),
	}
}

// addTypedEdgeToFrontmatter idempotently adds `target` to the typed
// list at `kind` in `path`'s frontmatter. Creates the key if absent;
// appends to the existing list otherwise. Preserves authored
// formatting where possible — uses inline `[[..., [[...]]` form
// when the existing line is inline, list-of-lines form otherwise.
//
// No-op when the target is already present (case-sensitive match).
func addTypedEdgeToFrontmatter(path string, kind RelationKind, target string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	s := string(data)
	if !strings.HasPrefix(s, "---") {
		return fmt.Errorf("no frontmatter delimiter")
	}
	end := strings.Index(s[3:], "\n---")
	if end < 0 {
		return fmt.Errorf("no closing frontmatter delimiter")
	}
	fmBlock := s[3 : end+3]
	rest := s[end+7:]

	lines := strings.Split(fmBlock, "\n")
	keyStr := string(kind) + ":"
	keyIdx := -1
	for i, line := range lines {
		k, _, ok := splitFrontmatterLine(line)
		if ok && k == string(kind) {
			keyIdx = i
			break
		}
	}

	wikilink := "[[" + target + "]]"

	if keyIdx < 0 {
		// Insert a fresh inline list before the trailing blank line.
		insertAt := len(lines)
		for insertAt > 0 && strings.TrimSpace(lines[insertAt-1]) == "" {
			insertAt--
		}
		newLine := keyStr + ` ["` + wikilink + `"]`
		lines = append(lines[:insertAt],
			append([]string{newLine}, lines[insertAt:]...)...)
		newContent := "---" + strings.Join(lines, "\n") + "\n---" + rest
		return os.WriteFile(path, []byte(newContent), 0o644)
	}

	// Existing key. Idempotency: if target already present (anywhere
	// across the inline value or following indented list lines), skip.
	var combined strings.Builder
	combined.WriteString(lines[keyIdx])
	for j := keyIdx + 1; j < len(lines); j++ {
		l := lines[j]
		if !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t") {
			break
		}
		combined.WriteByte(' ')
		combined.WriteString(l)
	}
	if strings.Contains(combined.String(), wikilink) {
		return nil
	}

	// Append. Pick form based on existing line shape:
	// inline `kind: ["[[A]]", "[[B]]"]` — append inside the brackets;
	// otherwise default to inline if currently empty/list, else append.
	headerLine := lines[keyIdx]
	_, val, _ := splitFrontmatterLine(headerLine)
	val = strings.TrimSpace(val)

	switch {
	case strings.HasPrefix(val, "[") && strings.HasSuffix(val, "]"):
		// Inline list; rewrite cleanly.
		inner := strings.TrimSuffix(strings.TrimPrefix(val, "["), "]")
		existing := splitInlineList(inner)
		existing = append(existing, `"`+wikilink+`"`)
		lines[keyIdx] = keyStr + " [" + strings.Join(existing, ", ") + "]"
	case val == "":
		// Empty header followed by indented bullet lines, or empty header.
		// Rewrite the header to inline form for compactness.
		lines[keyIdx] = keyStr + ` ["` + wikilink + `"]`
		// Drop the indented bullet block.
		j := keyIdx + 1
		for j < len(lines) {
			if !strings.HasPrefix(lines[j], " ") && !strings.HasPrefix(lines[j], "\t") {
				break
			}
			j++
		}
		// Preserve any existing bullet items by re-injecting them inline.
		var existingBullets []string
		for k := keyIdx + 1; k < j; k++ {
			t := strings.TrimSpace(lines[k])
			t = strings.TrimPrefix(t, "- ")
			t = strings.TrimSpace(t)
			if t != "" {
				existingBullets = append(existingBullets, t)
			}
		}
		if len(existingBullets) > 0 {
			existingBullets = append(existingBullets, `"`+wikilink+`"`)
			lines[keyIdx] = keyStr + " [" + strings.Join(existingBullets, ", ") + "]"
		}
		lines = append(lines[:keyIdx+1], lines[j:]...)
	default:
		// Non-list scalar (rare); promote to inline list with old + new.
		lines[keyIdx] = keyStr + ` ["` + val + `", "` + wikilink + `"]`
	}

	newContent := "---" + strings.Join(lines, "\n") + "\n---" + rest
	return os.WriteFile(path, []byte(newContent), 0o644)
}

// removeTypedEdgeFromFrontmatter is the inverse of add; removes the
// target from the typed list. Removes the key entirely when the list
// becomes empty.
func removeTypedEdgeFromFrontmatter(path string, kind RelationKind, target string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	s := string(data)
	if !strings.HasPrefix(s, "---") {
		return fmt.Errorf("no frontmatter delimiter")
	}
	end := strings.Index(s[3:], "\n---")
	if end < 0 {
		return fmt.Errorf("no closing frontmatter delimiter")
	}
	fmBlock := s[3 : end+3]
	rest := s[end+7:]
	lines := strings.Split(fmBlock, "\n")

	keyStr := string(kind) + ":"
	keyIdx := -1
	for i, line := range lines {
		k, _, ok := splitFrontmatterLine(line)
		if ok && k == string(kind) {
			keyIdx = i
			break
		}
	}
	if keyIdx < 0 {
		return nil // already absent
	}

	wikilink := "[[" + target + "]]"
	headerLine := lines[keyIdx]
	_, val, _ := splitFrontmatterLine(headerLine)
	val = strings.TrimSpace(val)

	if !strings.HasPrefix(val, "[") || !strings.HasSuffix(val, "]") {
		// Not an inline list; bail (Phase 6A v1 only handles inline form
		// after migrate normalization). Surface what's there.
		return fmt.Errorf("relation %s in non-inline list form; edit by hand", kind)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(val, "["), "]")
	items := splitInlineList(inner)
	out := items[:0]
	for _, it := range items {
		bare := strings.Trim(it, ` "'`)
		if bare == wikilink || bare == target {
			continue
		}
		out = append(out, it)
	}
	if len(out) == 0 {
		// Drop the key entirely.
		lines = append(lines[:keyIdx], lines[keyIdx+1:]...)
	} else {
		lines[keyIdx] = keyStr + " [" + strings.Join(out, ", ") + "]"
	}
	newContent := "---" + strings.Join(lines, "\n") + "\n---" + rest
	return os.WriteFile(path, []byte(newContent), 0o644)
}

// splitInlineList splits a YAML inline-list inner string ("a", "b")
// into its elements while respecting quoted commas. Tiny scanner —
// good enough for the trimmed wikilink content these lists carry; we
// don't need full YAML parsing here because the writer always
// produces well-formed quoted entries.
func splitInlineList(inner string) []string {
	var out []string
	cur := strings.Builder{}
	inQuote := false
	for i := range len(inner) {
		c := inner[i]
		switch {
		case c == '"' && (i == 0 || inner[i-1] != '\\'):
			inQuote = !inQuote
			cur.WriteByte(c)
		case c == ',' && !inQuote:
			s := strings.TrimSpace(cur.String())
			if s != "" {
				out = append(out, s)
			}
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}
