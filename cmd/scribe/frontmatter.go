// frontmatter.go — article-level primitives shared by every command:
// frontmatter parsing, wikilink/title extraction, and the markdown
// walkers over the KB content dirs.
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Frontmatter represents YAML frontmatter from a wiki article.
// Note: Created/Updated use `any` because Go's YAML parser auto-converts
// YYYY-MM-DD to time.Time. We handle both string and time.Time in code.
type Frontmatter struct {
	Title      string `yaml:"title"`
	Type       string `yaml:"type"`
	Created    any    `yaml:"created"`
	Updated    any    `yaml:"updated"`
	Domain     string `yaml:"domain"`
	Confidence string `yaml:"confidence"`
	Tags       any    `yaml:"tags"`
	Related    any    `yaml:"related"`
	Sources    any    `yaml:"sources"`
	// Aliases lists alternate titles that wikilinks may use to reference
	// this article. Borrowed from zk's `aliases:` convention: lets absorbed
	// content refer to an article by a variant spelling (e.g. "Hlidac shopu"
	// vs "Hlidac Shopu") without generating a missing-link warning. Orphan
	// and lint passes treat aliases as valid existing titles.
	Aliases any    `yaml:"aliases,omitempty"`
	Status  string `yaml:"status,omitempty"`
	Rolling bool   `yaml:"rolling,omitempty"`
	// Contributor records who first created the article — stamped
	// automatically at commit time (stampContributor) from the user
	// config or git identity. In shared team KBs this is the provenance
	// signal beyond git blame; dream's contradiction resolution may
	// consult it when weighing competing claims.
	Contributor string `yaml:"contributor,omitempty"`
	// Stack is intentionally `any`: scaffolding (project overview frontmatter,
	// LLM-written drops) sometimes ships it as a YAML list (`stack: [Go, ...]`)
	// and sometimes as a plain string ("Go + SQLite + CGO"). Both shapes are
	// valid in the corpus today; the lint pass walks frontmatter as raw maps,
	// so the typed struct only needs to *accept* the value, not normalize it.
	// Fields like Tags/Related/Sources use the same approach.
	Stack   any    `yaml:"stack,omitempty"`
	Verdict string `yaml:"verdict,omitempty"`
	Problem string `yaml:"problem,omitempty"`
	Depth   string `yaml:"depth,omitempty"`
	// Authority marks how load-bearing an article's claims are when two
	// sources contradict. Used by `scribe lint --resolve` to pick a winner.
	//   canonical — intentional decisions/policies; wins over everything
	//   contextual — curated solutions/patterns; wins over opinion
	//   opinion — raw captures, tweets, excerpts; loses by default
	// Absent = "contextual" for wiki pages, "opinion" for raw articles.
	Authority string `yaml:"authority,omitempty"`
	// IndexTier (Phase 5B) controls qmd ranking weight. Computed by
	// scribe from body length, heading count, and article type unless
	// the human pinned a specific value via IndexTierOverride.
	//   stub      — ≤80 words OR fetched_via=stub; excluded from search
	//   brief     — 81–199 words OR fxtwitter capture
	//   standard  — 200–1999 words; ordinary article
	//   deep      — 2000+ words OR ≥5 sections in sidecar
	//   reference — explicit human marker for canonical artifacts
	IndexTier         string `yaml:"index_tier,omitempty"`
	IndexTierOverride string `yaml:"index_tier_override,omitempty"`

	// Phase 6A typed relations. Each field replaces a slice of the
	// generic `related:` list with a typed edge whose semantics
	// `scribe lint --resolve` and Phase 6B contradiction detection
	// can reason about. Type-specific allowed sets:
	//   decision  — supersedes, superseded_by, contradicts
	//   solution  — applies_to (pattern), derived_from (research)
	//   pattern   — instance_of, specializes
	//   research  — extends, cited_by, informs
	// Untyped `related:` stays as the easy-out for genuinely loose
	// connections. Each typed field carries [[Wikilinks]] just like
	// related:; the typing is purely about what the edge *means*.
	Supersedes   any `yaml:"supersedes,omitempty"`
	SupersededBy any `yaml:"superseded_by,omitempty"`
	Contradicts  any `yaml:"contradicts,omitempty"`
	AppliesTo    any `yaml:"applies_to,omitempty"`
	DerivedFrom  any `yaml:"derived_from,omitempty"`
	InstanceOf   any `yaml:"instance_of,omitempty"`
	Specializes  any `yaml:"specializes,omitempty"`
	Extends      any `yaml:"extends,omitempty"`
	CitedBy      any `yaml:"cited_by,omitempty"`
	Informs      any `yaml:"informs,omitempty"`
	// RelationsLocked tells the LLM relation migrator (Phase 6A v2)
	// to leave this article alone. Useful for hand-curated cases.
	RelationsLocked bool `yaml:"relations_locked,omitempty"`
}

// parseFrontmatter extracts YAML frontmatter from markdown content.
func parseFrontmatter(content []byte) (*Frontmatter, error) {
	s := string(content)
	if !strings.HasPrefix(s, "---") {
		return nil, fmt.Errorf("no frontmatter delimiter")
	}
	end := strings.Index(s[3:], "\n---")
	if end < 0 {
		return nil, fmt.Errorf("no closing frontmatter delimiter")
	}
	yamlBytes := []byte(s[3 : end+3])
	var fm Frontmatter
	if err := yaml.Unmarshal(yamlBytes, &fm); err != nil {
		// Handle duplicate keys
		deduped := deduplicateYAMLKeys(string(yamlBytes))
		if err2 := yaml.Unmarshal([]byte(deduped), &fm); err2 != nil {
			return nil, fmt.Errorf("invalid YAML: %w", err)
		}
	}
	return &fm, nil
}

// parseFrontmatterRaw extracts raw YAML map for field presence checking.
// Handles duplicate keys (common in LLM-generated frontmatter) by deduplicating
// before parsing — last value wins, matching Python's yaml.safe_load behavior.
func parseFrontmatterRaw(content []byte) (map[string]any, error) {
	s := string(content)
	if !strings.HasPrefix(s, "---") {
		return nil, fmt.Errorf("no frontmatter delimiter")
	}
	end := strings.Index(s[3:], "\n---")
	if end < 0 {
		return nil, fmt.Errorf("no closing frontmatter delimiter")
	}
	yamlBytes := []byte(s[3 : end+3])
	var raw map[string]any
	if err := yaml.Unmarshal(yamlBytes, &raw); err != nil {
		// Go's yaml.v3 rejects duplicate keys. Try deduplicating.
		deduped := deduplicateYAMLKeys(string(yamlBytes))
		if err2 := yaml.Unmarshal([]byte(deduped), &raw); err2 != nil {
			return nil, fmt.Errorf("invalid YAML: %w", err)
		}
	}
	return raw, nil
}

// deduplicateYAMLKeys removes duplicate top-level keys, keeping the last occurrence.
func deduplicateYAMLKeys(yamlStr string) string {
	// YAML that already parses needs no repair — and the line surgery
	// below can only damage it. A flow collection or quoted scalar that
	// spans lines puts value text at column 0, which the line scanner
	// misreads as a top-level key: on "a: {x: 1,\ny: 2}\ny: 9" it would
	// blank the continuation line as a "duplicate" of the real y: key
	// and corrupt the flow map (found by FuzzDeduplicateYAMLKeys). Both
	// callers only invoke dedup after a parse failure, so this guard is
	// normally a no-op; it exists to keep the function's contract honest
	// standalone — output is parseable whenever the input was.
	var probe map[string]any
	if yaml.Unmarshal([]byte(yamlStr), &probe) == nil {
		return yamlStr
	}
	lines := strings.Split(yamlStr, "\n")
	seen := make(map[string]int) // key -> last line index
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Only top-level keys (no leading whitespace, has colon)
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			if colonIdx := strings.Index(trimmed, ":"); colonIdx > 0 {
				key := trimmed[:colonIdx]
				if prev, exists := seen[key]; exists {
					lines[prev] = "" // blank out earlier occurrence
				}
				seen[key] = i
			}
		}
	}
	var result []string
	for _, line := range lines {
		if line != "" || len(result) == 0 {
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}

// extractWikilinks returns all [[Target]] links from markdown content.
// Strips fenced code blocks AND inline code spans before scanning to avoid
// false positives from code. Order matters: fenced blocks → double-backtick
// spans → single-backtick spans. Double first because “ `foo` “ nests a
// single-backtick pair inside a double-backtick span, and stripping single
// first leaves a stray “ “ “ that throws off multiline parity.
// Handles piped links [[Target|Display]] by extracting just the target.
func extractWikilinks(content []byte) []string {
	cleaned := codeFenceRE.ReplaceAll(content, nil)
	cleaned = codeSpanDoubleRE.ReplaceAll(cleaned, nil)
	cleaned = codeSpanRE.ReplaceAll(cleaned, nil)
	matches := wikilinkRE.FindAllSubmatch(cleaned, -1)
	seen := make(map[string]bool)
	var links []string
	for _, m := range matches {
		target := string(m[1])
		// Handle piped links: [[Target|Display Text]]
		if idx := strings.Index(target, "|"); idx > 0 {
			target = target[:idx]
		}
		target = strings.TrimSpace(target)
		if target != "" && !seen[target] {
			seen[target] = true
			links = append(links, target)
		}
	}
	return links
}

// extractTitleFast extracts the title from frontmatter using regex.
// Faster than full YAML parsing when only the title is needed.
func extractTitleFast(content []byte) string {
	m := titleLineRE.FindSubmatch(content)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// walkArticles walks all .md files in wiki dirs, skipping meta files and _-prefixed files.
// Use this for title collection and article enumeration.
func walkArticles(root string, fn func(path string, content []byte) error) error {
	return walkMarkdown(root, true, fn)
}

// walkAllMarkdown walks all .md files in wiki dirs, including _-prefixed files.
// Use this for wikilink scanning (links in _index.md should still count).
func walkAllMarkdown(root string, fn func(path string, content []byte) error) error {
	return walkMarkdown(root, false, fn)
}

func walkMarkdown(root string, skipUnderscored bool, fn func(path string, content []byte) error) error {
	for _, dir := range wikiDirs {
		dirPath := filepath.Join(root, dir)
		if _, err := os.Stat(dirPath); os.IsNotExist(err) {
			continue
		}
		err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil //nolint:nilerr // skip unreadable, continue walk
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".md") {
				return nil
			}
			if skipFiles[info.Name()] {
				return nil
			}
			if strings.HasPrefix(info.Name(), ".") {
				return nil
			}
			if skipUnderscored && strings.HasPrefix(info.Name(), "_") {
				return nil
			}
			content, err := os.ReadFile(path) //nolint:gosec // user-supplied KB root, deliberate walk
			if err != nil {
				return nil //nolint:nilerr // skip unreadable, continue walk
			}
			return fn(path, content)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// countLines counts lines in a byte slice.
func countLines(content []byte) int {
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	n := 0
	for scanner.Scan() {
		n++
	}
	return n
}

// relPath returns path relative to root, or the original path on error.
func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}
