package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// autoFixArticle applies deterministic, non-destructive frontmatter
// repairs to a single wiki article at `rel` (KB-relative path) under
// `root`. Returns (changes_applied, new_content, err). new_content is ""
// when no changes are needed, so the caller can skip the write. The
// fixes only touch fields where the correct answer is obvious:
//
//   - add missing tags/related/sources as empty lists
//   - add missing confidence: medium
//   - add missing domain: general
//   - add missing updated: <today>
//   - add missing created: <today> (only when file mtime unavailable)
//   - reformat 2026/04/20 or 2026.04.20 → 2026-04-20 in created/updated
//   - strip trailing whitespace on every line
//   - clamp an invalid/missing `type` to the path's canonical type
//     (decisions/→decision, …; wiki/ & sessions/ → research). The
//     directory IS the taxonomy (validate.go: validTypes "mirrors the
//     wikiDirs taxonomy"), so this is a lossless correction, not the
//     "mis-categorizing" the old policy feared — same inference the
//     envelope seam (clampEnvelopeFrontmatter) makes for new writes.
//     Only fires when the current type is absent or not in validTypes;
//     a valid-but-dir-mismatched type is not a lint error and is left.
//   - clamp a present-but-invalid `domain` to `general` (the universal
//     catch-all), matching the seam. Missing domain is still handled by
//     the add-default below.
//
// Skipped (require human / LLM decision):
//
//   - missing title (author must pick)
//   - invalid confidence (silent "fix" would misrepresent)
//   - frontmatter that does not parse even after the above (no closing
//     ---, unescaped `:` in a value, etc.) — returned as an error so
//     the caller SKIPs and reports it instead of writing a file lint
//     still rejects
//   - body-level issues (size, orphan, etc)
//
// Returns a slice of human-readable change descriptions so the CLI can
// log what actually happened.
func autoFixArticle(root, rel string, content []byte) ([]string, []byte, error) {
	s := string(content)
	joinedFence := false
	openingFenceFixed := false
	switch {
	case strings.HasPrefix(s, "---\n"), strings.HasPrefix(s, "---\r\n"):
		// clean opening fence — proceed
	case strings.HasPrefix(s, "--- "), strings.HasPrefix(s, "---\t"):
		// The opening `---` has trailing junk on its line. Two distinct shapes,
		// disambiguated by what remains after the dashes on that first line:
		//   (a) trailing-whitespace-only fence ("--- \n"): the body is intact,
		//       only the fence line carries stray spaces/tabs — drop them.
		//   (b) joined fence ("--- title: ..."): the `---` ran into the first
		//       key — split to "---\n<firstkey>".
		// Both require a closing fence to be treated as frontmatter at all, so
		// body prose that merely starts with "--- " stays a silent no-op.
		// Residual corruption (e.g. space-indented keys) then surfaces later as
		// a SKIP via the honesty guard, never a silent success.
		firstLine, _, _ := strings.Cut(s[3:], "\n")
		rest := strings.TrimLeft(firstLine, " \t")
		_, _, restIsKey := splitFrontmatterLine(rest)
		hasClose := strings.Contains(s, "\n---")
		switch {
		case rest == "" && hasClose:
			s = "---" + s[3+len(firstLine):]
			openingFenceFixed = true
		case restIsKey && hasClose:
			s = "---\n" + strings.TrimLeft(s[3:], " \t")
			joinedFence = true
		default:
			return nil, nil, nil
		}
	default:
		return nil, nil, nil // no frontmatter — skip (body-only stubs)
	}

	// Locate closing frontmatter delimiter. Tolerate trailing whitespace
	// on the fence line (and CRLF) — the validator's parseFrontmatter
	// prefix-matches "\n---", so a "--- " fence is valid per `scribe
	// lint` yet this used to bail with "no closing ---". The validator
	// and fixer must agree on fence syntax; otherwise --fix SKIPs files
	// lint reports as clean. The matched fence is normalized to a bare
	// "---" below, so a recognized-but-noncanonical fence becomes a real
	// repair instead of a silent pass-through.
	closeFenceRE := regexp.MustCompile(`\n---[ \t]*(?:\r?\n|$)`)
	loc := closeFenceRE.FindStringIndex(s[4:])
	if loc == nil {
		return nil, nil, errors.New("malformed frontmatter (no closing ---)")
	}
	fmStart := 4
	fmEnd := 4 + loc[0]      // start of the "\n" preceding the fence
	afterFence := 4 + loc[1] // first byte after the fence line
	fmBlock := s[fmStart:fmEnd]
	bodyAfter := s[afterFence:]
	fenceWasNoncanonical := s[fmEnd:afterFence] != "\n---\n"

	present := presentKeys(fmBlock)

	var changes []string
	lines := strings.Split(fmBlock, "\n")

	if joinedFence {
		changes = append(changes, "split joined opening fence to ---")
	}
	if openingFenceFixed {
		changes = append(changes, "normalized opening frontmatter fence to bare ---")
	}
	if fenceWasNoncanonical {
		changes = append(changes, "normalized closing frontmatter fence to bare ---")
	}

	// Remove a nested `frontmatter:` map (the ingestion artifact where a
	// source file's own frontmatter was wrapped instead of merged), promoting
	// a more specific nested domain first so the strip keeps the better
	// signal. Runs before the domain clamp so a promoted domain is validated.
	if newLines, promoted, stripped := stripNestedFrontmatterBlock(lines, validDomainsForRoot(root)); stripped {
		lines = newLines
		if promoted != "" {
			changes = append(changes, fmt.Sprintf("removed nested `frontmatter:` block (promoted domain: %s)", promoted))
		} else {
			changes = append(changes, "removed nested `frontmatter:` block (ingestion artifact)")
		}
	}

	// Collapse duplicate top-level keys (keep the LAST, matching
	// parseFrontmatter's deduplicateYAMLKeys). The validator reads a
	// duplicated field last-wins; the domain/type clamps below read it
	// first-wins via frontmatterValue/replaceFMLine. That split is why a
	// file carrying both `domain: general` and `domain: <invalid>` was
	// reported as an error by lint yet skipped by --fix forever — the
	// clamp saw the valid first copy and did nothing. Collapsing here
	// realigns the fixer's view with the validator's so the residual
	// (invalid) value actually reaches the clamp.
	if deduped, removed := collapseDuplicateFrontmatterKeys(lines); removed > 0 {
		lines = deduped
		changes = append(changes, fmt.Sprintf("collapsed %d duplicate frontmatter key(s)", removed))
	}

	// Strip trailing whitespace on each frontmatter line.
	trailingStripped := 0
	for i, line := range lines {
		cleaned := strings.TrimRight(line, " \t")
		if cleaned != line {
			lines[i] = cleaned
			trailingStripped++
		}
	}
	if trailingStripped > 0 {
		changes = append(changes, fmt.Sprintf("stripped trailing whitespace (%d line(s))", trailingStripped))
	}

	// Normalize slash/dot dates on created/updated.
	for i, line := range lines {
		key, rest, ok := splitFrontmatterLine(line)
		if !ok {
			continue
		}
		if key != "created" && key != "updated" {
			continue
		}
		normalized, didFix := normalizeDateValue(rest)
		if didFix {
			lines[i] = key + ": " + normalized
			changes = append(changes, fmt.Sprintf("normalized %s date format", key))
		}
	}

	// Coerce a scalar tags/related/sources value into an inline list. The
	// LLM (and hand edits) sometimes emit `tags: a, b, c` — a comma string
	// where the schema wants a sequence — which validate rejects as
	// "should be a list, got: string". Only a present, non-empty, non-flow
	// scalar is rewritten; a block list (`tags:` with `- item` children) or
	// an existing `[...]` is left untouched, so this stays idempotent.
	for _, field := range []string{"tags", "related", "sources"} {
		if newLines, didFix := coerceScalarListField(lines, field); didFix {
			lines = newLines
			changes = append(changes, fmt.Sprintf("coerced scalar %s → list", field))
		}
	}

	// Normalize a block-form `aliases:` list: re-indent stray items to two
	// spaces, single-quote entries that need it (@handles, etc.), and drop
	// duplicates. This repairs the invalid-YAML frontmatter the un-quoted
	// identity-apply writer used to emit (`  - @omarsar0`) so the file lint
	// rejected becomes a real FIX instead of a perpetual SKIP. No-op on a
	// well-formed list, so it's idempotent.
	if newLines, didFix := normalizeAliasesBlock(lines); didFix {
		lines = newLines
		changes = append(changes, "normalized aliases block (quoting/indent/dedup)")
	}

	// Append missing keys with safe defaults.
	today := time.Now().Format("2006-01-02")
	missingDefaults := []struct {
		key string
		val string
	}{
		{"tags", "[]"},
		{"related", "[]"},
		{"sources", "[]"},
		{"confidence", "medium"},
		{"domain", "general"},
		{"updated", today},
		{"created", today},
	}
	for _, d := range missingDefaults {
		if !present[d.key] {
			lines = append(lines, d.key+": "+d.val)
			changes = append(changes, fmt.Sprintf("added missing %s: %s", d.key, d.val))
		}
	}

	// Type clamp from the path. The directory is the taxonomy, so an
	// invalid/missing type in a typed dir has one correct answer. Only
	// act when the current type is absent or not in validTypes — a
	// valid-but-dir-mismatched type (e.g. a decision-typed note filed in
	// wiki/) is not a lint error and stays untouched.
	if canonical := canonicalTypeForRel(rel); canonical != "" {
		cur := typeValue(lines)
		if cur == "" || !validTypes[cur] {
			if replaceFMLine(lines, "type", canonical) {
				changes = append(changes, fmt.Sprintf("clamped invalid type %q → %q (from %s/)", cur, canonical, topDir(rel)))
			} else {
				lines = append([]string{"type: " + canonical}, lines...)
				changes = append(changes, fmt.Sprintf("set missing type: %s (from %s/)", canonical, topDir(rel)))
			}
		}
	}

	// Domain clamp. missingDefaults above already adds `domain: general`
	// when absent; this handles the present-but-invalid case (a domain
	// not in scribe.yaml + universals — the `research`/`{{DOMAIN}}`/
	// `<frontmatter domain or 'general'>` lint class). general is the
	// universal catch-all, always valid; same floor the seam uses.
	if domv := frontmatterValue(lines, "domain"); domv != "" {
		if domains := validDomainsForRoot(root); !domains[domv] {
			if replaceFMLine(lines, "domain", "general") {
				changes = append(changes, fmt.Sprintf("clamped invalid domain %q → general", domv))
			}
		}
	}

	// Authority defaults from type. This only fills unset entries so we
	// don't override deliberate overrides the author made. Mapping comes
	// from the schema principle: decisions are load-bearing, curated wiki
	// types are contextual, raw-ish surfaces are opinion-level. Runs after
	// the type clamp so a clamped type gets the right authority.
	if !present["authority"] {
		if auth := defaultAuthorityForType(typeValue(lines)); auth != "" {
			lines = append(lines, "authority: "+auth)
			changes = append(changes, fmt.Sprintf("added missing authority: %s (from type)", auth))
		}
	}

	newFM := strings.Join(lines, "\n")
	result := []byte(s[:fmStart] + newFM + "\n---\n" + bodyAfter)

	// Honesty guard: never claim a fix on frontmatter lint still
	// rejects. The "no closing ---" subclass already errored above; this
	// catches the has-delimiter-but-invalid-YAML subclass (unescaped `:`
	// in a value, a bad map key like the literal {{DOMAIN}}). These need
	// human/LLM repair — surface them as a SKIP, regardless of whether
	// cosmetic changes (trailing ws, defaults) would otherwise apply, so
	// the operator sees the true residual instead of a misleading FIX.
	if _, perr := parseFrontmatter(result); perr != nil {
		return nil, nil, fmt.Errorf("still invalid YAML after deterministic fixes (manual repair needed): %w", perr)
	}

	if len(changes) == 0 {
		return nil, nil, nil
	}
	return changes, result, nil
}

// aliasItemRE matches a YAML block-sequence item at any indentation, so a
// stray over-indented entry (the corruption shape) is still collected.
var aliasItemRE = regexp.MustCompile(`^\s*-\s+(.*)$`)

// canonicalAliasItem decides the value text to place after "  - " for one
// raw alias entry, and the case-insensitive key used to dedup it. It is
// deliberately conservative: an entry that is already validly quoted (single
// or double) is kept verbatim, so well-formed files don't churn; only a bare
// value that YAML would misparse (e.g. an @handle) gets quoted. Returns an
// empty key for a blank entry so the caller skips it.
func canonicalAliasItem(raw string) (lineVal, dedupKey string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	// Already quoted and balanced → trust as valid, keep the original text.
	// Dedup on the unquoted inner value.
	if len(raw) >= 2 {
		q := raw[0]
		if (q == '"' || q == '\'') && raw[len(raw)-1] == q {
			inner := raw[1 : len(raw)-1]
			if q == '\'' {
				inner = strings.ReplaceAll(inner, "''", "'")
			}
			return raw, strings.ToLower(inner)
		}
	}
	// Unquoted: quote only if YAML needs it; otherwise keep verbatim.
	return yamlQuoteScalar(raw), strings.ToLower(raw)
}

// normalizeAliasesBlock rewrites a block-form `aliases:` list into a clean,
// valid form: every entry re-emitted at two-space indent, quoted when YAML
// needs it, with case-insensitive duplicates removed (first spelling wins).
// Returns (lines, false) when there is no block-form aliases list or it is
// already clean — so the fixer stays idempotent. Inline form
// (`aliases: [a, b]`) is left untouched.
func normalizeAliasesBlock(lines []string) ([]string, bool) {
	idx := -1
	for i, line := range lines {
		if k, rest, ok := splitFrontmatterLine(line); ok && k == "aliases" {
			if strings.TrimSpace(rest) != "" {
				return lines, false // inline or scalar form — leave alone
			}
			idx = i
			break
		}
	}
	if idx == -1 {
		return lines, false
	}

	// Collect contiguous list items (any indent) following the key. The
	// first non-item, non-blank line ends the block (typically the next
	// column-0 key).
	end := idx + 1
	var items []string
	seen := make(map[string]bool)
	for j := idx + 1; j < len(lines); j++ {
		m := aliasItemRE.FindStringSubmatch(lines[j])
		if m == nil {
			if strings.TrimSpace(lines[j]) == "" {
				end = j + 1
				continue
			}
			break
		}
		end = j + 1
		lineVal, dedupKey := canonicalAliasItem(m[1])
		if dedupKey == "" || seen[dedupKey] {
			continue
		}
		seen[dedupKey] = true
		items = append(items, lineVal)
	}

	rebuilt := make([]string, 0, len(items)+1)
	rebuilt = append(rebuilt, "aliases:")
	for _, it := range items {
		rebuilt = append(rebuilt, "  - "+it)
	}

	old := lines[idx:end]
	if slices.Equal(old, rebuilt) {
		return lines, false
	}
	out := append([]string{}, lines[:idx]...)
	out = append(out, rebuilt...)
	out = append(out, lines[end:]...)
	return out, true
}

// topDir returns the first path segment of a KB-relative path
// ("decisions/x.md" → "decisions"), or "" for an empty path.
func topDir(rel string) string {
	rel = strings.TrimPrefix(rel, "./")
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return rel
}

// canonicalTypeForRel returns the single valid `type` for the directory
// `rel` lives in. Typed dirs map 1:1 via dirCanonicalType; wiki/ and
// sessions/ are the general-knowledge dirs with no canonical type, so
// they fall back to "research" (the loosest-schema valid type) — the
// same rule clampEnvelopeFrontmatter applies to new envelope writes, so
// on-disk repair and the live seam stay consistent. "" means "unknown
// dir — don't infer" (defensive; walkArticles only walks wikiDirs).
func canonicalTypeForRel(rel string) string {
	top := topDir(rel)
	if t, ok := dirCanonicalType[top]; ok {
		return t
	}
	if top == "wiki" || top == "sessions" {
		return "research"
	}
	return ""
}

// fmUnit is one top-level `key:` line plus the contiguous indented child
// lines it owns, as a [start, end) range into a split frontmatter block.
// A blank or non-key line that isn't an indented child is its own
// passthrough unit (key ""), so comments and blanks survive untouched.
type fmUnit struct {
	key        string
	start, end int
}

// frontmatterUnits groups split frontmatter lines into fmUnits. Shared by
// the duplicate-key collapse and the nested-`frontmatter:` strip so both
// treat block-valued keys (a key plus its indented children) as one unit.
func frontmatterUnits(lines []string) []fmUnit {
	var units []fmUnit
	for i := 0; i < len(lines); {
		key, _, ok := splitFrontmatterLine(lines[i])
		if !ok {
			units = append(units, fmUnit{key: "", start: i, end: i + 1})
			i++
			continue
		}
		j := i + 1
		for j < len(lines) && (strings.HasPrefix(lines[j], " ") || strings.HasPrefix(lines[j], "\t")) {
			j++
		}
		units = append(units, fmUnit{key: key, start: i, end: j})
		i = j
	}
	return units
}

// collapseDuplicateFrontmatterKeys removes earlier occurrences of any
// top-level key that appears more than once in a split frontmatter block,
// keeping the LAST occurrence — the same last-wins rule parseFrontmatter's
// deduplicateYAMLKeys applies, so the fixer's field values match the
// validator's. When a removed occurrence owns a block value (indented
// child lines), those children are removed with it. Returns the rewritten
// lines and the number of duplicate key blocks dropped (0 = no change).
func collapseDuplicateFrontmatterKeys(lines []string) ([]string, int) {
	units := frontmatterUnits(lines)

	count := make(map[string]int)
	lastIdx := make(map[string]int)
	for idx, u := range units {
		if u.key == "" {
			continue
		}
		count[u.key]++
		lastIdx[u.key] = idx
	}

	removed := 0
	out := make([]string, 0, len(lines))
	for idx, u := range units {
		if u.key != "" && count[u.key] > 1 && idx != lastIdx[u.key] {
			removed++
			continue // drop this earlier duplicate (and its child lines)
		}
		out = append(out, lines[u.start:u.end]...)
	}
	return out, removed
}

// stripNestedFrontmatterBlock removes a top-level `frontmatter:` key whose
// value is a nested map — the ingestion artifact where an already-
// frontmattered source file had its own frontmatter wrapped as a nested
// block instead of merged into the top level. No schema field is named
// `frontmatter`, so a top-level one is always the artifact. Before removing,
// a more specific `domain:` from the nested block is promoted to the top
// level when the top level is empty or the generic "general" and the nested
// value is a valid configured domain — so the strip preserves the better
// signal instead of discarding it. Returns (lines, promotedDomain, changed).
func stripNestedFrontmatterBlock(lines []string, validDomains map[string]bool) ([]string, string, bool) {
	units := frontmatterUnits(lines)
	idx := -1
	for i, u := range units {
		if u.key == "frontmatter" {
			idx = i
			break
		}
	}
	if idx == -1 {
		return lines, "", false
	}

	// Pull a candidate domain out of the nested block before dropping it.
	nestedDomain := ""
	for _, l := range lines[units[idx].start:units[idx].end] {
		if k, rest, ok := splitFrontmatterLine(strings.TrimLeft(l, " \t")); ok && k == "domain" {
			nestedDomain = strings.Trim(strings.TrimSpace(rest), `"'`)
			break
		}
	}

	out := make([]string, 0, len(lines))
	for i, u := range units {
		if i == idx {
			continue // drop the `frontmatter:` unit and its children
		}
		out = append(out, lines[u.start:u.end]...)
	}

	promoted := ""
	if nestedDomain != "" && nestedDomain != "general" && validDomains[nestedDomain] {
		if top := frontmatterValue(out, "domain"); top == "" || top == "general" {
			if replaceFMLine(out, "domain", nestedDomain) {
				promoted = nestedDomain
			}
		}
	}
	return out, promoted, true
}

// dedupeFrontmatterKeys applies collapseDuplicateFrontmatterKeys to the
// leading `---` frontmatter block of a full document, returning the
// rewritten content and the number of duplicate keys dropped. Content
// without a well-formed frontmatter block is returned unchanged. Used by
// the extraction seam (clampEnvelopeFrontmatter) so a model that emits a
// duplicate key never persists one to disk — the on-disk repair in
// autoFixArticle is the safety net, this is the prevention.
func dedupeFrontmatterKeys(content string) (string, int) {
	lines := strings.Split(content, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "---" {
		return content, 0
	}
	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			closeIdx = i
			break
		}
	}
	if closeIdx < 1 {
		return content, 0
	}
	deduped, removed := collapseDuplicateFrontmatterKeys(lines[1:closeIdx])
	if removed == 0 {
		return content, 0
	}
	out := make([]string, 0, len(lines))
	out = append(out, lines[0])
	out = append(out, deduped...)
	out = append(out, lines[closeIdx:]...)
	return strings.Join(out, "\n"), removed
}

// stripNestedFrontmatterDoc applies stripNestedFrontmatterBlock to the
// leading frontmatter block of a full document, returning the rewritten
// content, any promoted domain, and whether a change was made. Used by the
// extraction seam (clampEnvelopeFrontmatter) so a model that wraps source
// frontmatter as a nested `frontmatter:` map never persists one to disk;
// autoFixArticle is the on-disk safety net for files already written.
func stripNestedFrontmatterDoc(content string, validDomains map[string]bool) (string, string, bool) {
	lines := strings.Split(content, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "---" {
		return content, "", false
	}
	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			closeIdx = i
			break
		}
	}
	if closeIdx < 1 {
		return content, "", false
	}
	stripped, promoted, changed := stripNestedFrontmatterBlock(lines[1:closeIdx], validDomains)
	if !changed {
		return content, "", false
	}
	out := make([]string, 0, len(lines))
	out = append(out, lines[0])
	out = append(out, stripped...)
	out = append(out, lines[closeIdx:]...)
	return strings.Join(out, "\n"), promoted, true
}

// coerceScalarListField rewrites a top-level `key: a, b, c` scalar into an
// inline YAML list `key: [a, b, c]`, quoting items that YAML would
// otherwise misparse. It is a no-op — returning (lines, false) — when the
// key is absent, empty (block form or genuinely blank), or already a flow
// list (`[...]`), keeping the fixer idempotent. Used for tags/related/
// sources, whose schema requires a sequence.
func coerceScalarListField(lines []string, key string) ([]string, bool) {
	for i, line := range lines {
		k, rest, ok := splitFrontmatterLine(line)
		if !ok || k != key {
			continue
		}
		rest = strings.TrimSpace(rest)
		if rest == "" || strings.HasPrefix(rest, "[") {
			return lines, false // block form, empty, or already a flow list
		}
		var items []string
		for piece := range strings.SplitSeq(rest, ",") {
			piece = strings.TrimSpace(piece)
			piece = strings.Trim(piece, `"'`)
			if piece != "" {
				items = append(items, yamlQuoteScalar(piece))
			}
		}
		if len(items) == 0 {
			return lines, false
		}
		out := append([]string{}, lines...)
		out[i] = key + ": [" + strings.Join(items, ", ") + "]"
		return out, true
	}
	return lines, false
}

// frontmatterValue returns the value of a top-level scalar `key:` line
// in a split frontmatter block, quotes trimmed, or "" if absent.
func frontmatterValue(lines []string, key string) string {
	for _, line := range lines {
		k, rest, ok := splitFrontmatterLine(line)
		if ok && k == key {
			return strings.Trim(strings.TrimSpace(rest), `"'`)
		}
	}
	return ""
}

// replaceFMLine rewrites the first top-level `key:` line's whole value
// to `key: val` (unquoted scalar — type/domain are bare identifiers).
// Returns false if the key is absent so the caller can decide whether
// to insert. Indented/continuation lines never match (splitFrontmatterLine
// rejects them), so a nested child is never clobbered.
func replaceFMLine(lines []string, key, val string) bool {
	for i, line := range lines {
		if k, _, ok := splitFrontmatterLine(line); ok && k == key {
			lines[i] = key + ": " + val
			return true
		}
	}
	return false
}

// typeValue returns the value of a top-level `type:` line in the
// frontmatter, or "" if absent. Handles simple scalar form only.
func typeValue(lines []string) string {
	for _, line := range lines {
		key, rest, ok := splitFrontmatterLine(line)
		if ok && key == "type" {
			return strings.Trim(strings.TrimSpace(rest), `"'`)
		}
	}
	return ""
}

// defaultAuthorityForType returns the authority we want to assign when
// the field is missing. Canonical for decisions (load-bearing policy),
// contextual for the curated wiki types, opinion for raw. Empty string
// means "don't backfill" — used for unknown types so we don't invent.
func defaultAuthorityForType(t string) string {
	switch t {
	case "decision":
		return "canonical"
	case "solution", "pattern", "tool", "research", "project":
		return "contextual"
	case "person", "article":
		return "opinion"
	default:
		return ""
	}
}

// presentKeys returns a set of top-level YAML keys in a frontmatter block.
// Handles nested children by requiring the key to be at column 0.
func presentKeys(block string) map[string]bool {
	out := make(map[string]bool)
	for line := range strings.SplitSeq(block, "\n") {
		key, _, ok := splitFrontmatterLine(line)
		if ok {
			out[key] = true
		}
	}
	return out
}

// splitFrontmatterLine pulls the key off a "key: value" line. Indented
// children (list items, nested maps) return ok=false so they don't
// falsely mark keys as present.
var fmKeyRE = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_-]*)\s*:(.*)$`)

func splitFrontmatterLine(line string) (key, rest string, ok bool) {
	if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	m := fmKeyRE.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	return m[1], strings.TrimSpace(m[2]), true
}

// normalizeDateValue takes the right-hand side of a date line and
// returns it as YYYY-MM-DD if it matches an unambiguous alternate
// format. Returns (normalized, true) when a fix was applied.
var (
	slashDateRE = regexp.MustCompile(`^(\d{4})[/.](\d{1,2})[/.](\d{1,2})$`)
)

func normalizeDateValue(val string) (string, bool) {
	val = strings.TrimSpace(val)
	val = strings.Trim(val, `"'`)
	if dateRE.MatchString(val) {
		return val, false
	}
	if m := slashDateRE.FindStringSubmatch(val); m != nil {
		y, mo, d := m[1], padTwo(m[2]), padTwo(m[3])
		return fmt.Sprintf("%s-%s-%s", y, mo, d), true
	}
	return val, false
}

func padTwo(s string) string {
	if len(s) == 1 {
		return "0" + s
	}
	return s
}

// runLintFix applies autoFixArticle across the set of files and reports
// totals. Files are rewritten in place unless dryRun is true.
func runLintFix(root string, files []string, dryRun bool) (fixed, skipped int, err error) {
	for _, path := range files {
		data, rErr := os.ReadFile(path)
		if rErr != nil {
			skipped++
			continue
		}
		changes, newContent, fErr := autoFixArticle(root, relPath(root, path), data)
		if fErr != nil {
			fmt.Printf("  SKIP %s: %v\n", relPath(root, path), fErr)
			skipped++
			continue
		}
		if len(changes) == 0 {
			continue
		}
		prefix := "FIX"
		if dryRun {
			prefix = "WOULD FIX"
		}
		for _, c := range changes {
			fmt.Printf("  %s %s: %s\n", prefix, relPath(root, path), c)
		}
		if !dryRun {
			if wErr := os.WriteFile(path, newContent, 0o644); wErr != nil {
				fmt.Printf("  SKIP %s: write failed: %v\n", relPath(root, path), wErr)
				skipped++
				continue
			}
		}
		fixed++
	}
	return fixed, skipped, nil
}
