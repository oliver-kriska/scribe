package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// autoFixArticle applies deterministic, non-destructive frontmatter
// repairs to a single wiki article. Returns (changes_applied, new_content,
// err). new_content is "" when no changes are needed, so the caller can
// skip the write. The fixes only touch fields where the correct answer
// is obvious:
//
//   - add missing tags/related/sources as empty lists
//   - add missing confidence: medium
//   - add missing domain: general
//   - add missing updated: <today>
//   - add missing created: <today> (only when file mtime unavailable)
//   - reformat 2026/04/20 or 2026.04.20 → 2026-04-20 in created/updated
//   - strip trailing whitespace on every line
//
// Skipped (require human decision):
//
//   - missing title (author must pick)
//   - missing type (mis-categorizing would be worse than missing)
//   - invalid type or confidence (silent "fix" would misrepresent)
//   - body-level issues (size, orphan, etc)
//
// Returns a slice of human-readable change descriptions so the CLI can
// log what actually happened.
func autoFixArticle(content []byte) ([]string, []byte, error) {
	s := string(content)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return nil, nil, nil // no frontmatter — skip
	}

	// Locate closing frontmatter delimiter.
	end := strings.Index(s[4:], "\n---\n")
	if end < 0 {
		// Allow CRLF end marker too.
		end = strings.Index(s[4:], "\n---\r\n")
		if end < 0 {
			return nil, nil, fmt.Errorf("malformed frontmatter (no closing ---)")
		}
	}
	fmStart := 4
	fmEnd := 4 + end // start of the "\n---" marker
	fmBlock := s[fmStart:fmEnd]
	body := s[fmEnd:]

	present := presentKeys(fmBlock)

	var changes []string
	lines := strings.Split(fmBlock, "\n")

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

	// Authority defaults from type. This only fills unset entries so we
	// don't override deliberate overrides the author made. Mapping comes
	// from the schema principle: decisions are load-bearing, curated wiki
	// types are contextual, raw-ish surfaces are opinion-level.
	if !present["authority"] {
		if auth := defaultAuthorityForType(typeValue(lines)); auth != "" {
			lines = append(lines, "authority: "+auth)
			changes = append(changes, fmt.Sprintf("added missing authority: %s (from type)", auth))
		}
	}

	if len(changes) == 0 {
		return nil, nil, nil
	}

	newFM := strings.Join(lines, "\n")
	return changes, []byte(s[:fmStart] + newFM + body), nil
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
		changes, newContent, fErr := autoFixArticle(data)
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
