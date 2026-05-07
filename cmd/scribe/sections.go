package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Phase 5A: section sidecar.
//
// For every wiki article with H1/H2/H3 headings, scribe persists a
// section index next to the article (parallel tree at
// wiki/_sections/<dir>/<slug>.json). Each entry pins one section's
// title, slugified anchor ID, line range, byte range, and a token
// estimate. The sidecar gives qmd / future tooling a way to retrieve
// a section without re-parsing the markdown on every query.
//
// Layout choice: parallel tree, mirroring the existing _atomic_facts/
// convention. Wiki dirs stay clean for human navigation; agents and
// tooling read the parallel tree. Decision recorded in
// docs/structured-memory-plan.md (Phase 5A).
//
// Anchor IDs follow Obsidian / Logseq block-anchor syntax so wikilinks
// like `[[Article#^methods]]` work in both vault tools without a
// scribe-specific dialect.

const sectionsSidecarVersion = 1

// SectionsSidecar is the schema persisted to wiki/_sections/<...>.json.
type SectionsSidecar struct {
	Version     int       `json:"version"`
	Article     string    `json:"article"`      // path relative to KB root
	ExtractedAt string    `json:"extracted_at"` // RFC3339 UTC
	Extractor   string    `json:"extractor"`    // "scribe-sections@<version>"
	Sections    []Section `json:"sections"`
}

// Section captures one heading's span inside an article. Line numbers
// are 1-based and inclusive on both ends. Byte range is [start, end)
// over the article body (NOT including frontmatter — the byte range
// is relative to the post-frontmatter body slice, so consumers can
// substring straight from a body that's already had frontmatter
// stripped).
type Section struct {
	ID    string `json:"id"`    // slugified, Obsidian/Logseq ^anchor compatible
	Title string `json:"title"` // heading text, no leading #
	Level int    `json:"level"` // 1, 2, or 3
	Lines [2]int `json:"lines"` // [start, end] 1-based inclusive
	Bytes [2]int `json:"bytes"` // [start, end) over article body
	// Tokens is a rough estimate at 4 bytes/token (matches chunker.go's
	// approximation). Useful for budget-based retrieval and never used
	// for billing.
	Tokens int `json:"tokens"`
}

// sectionsHeadingRE matches H1/H2/H3 markdown headings. Constrained to
// 1–3 hashes because deeper levels (h4+) rarely carry retrieval value
// and would explode sidecar size on KBs that nest aggressively.
var sectionsHeadingRE = regexp.MustCompile(`(?m)^(#{1,3}) +(.+?)\s*$`)

// extractSections parses an article body and returns one Section per
// heading. Body is the article *after* frontmatter has been stripped;
// pass the same slice you'd hand to a chunker.
//
// Algorithm: find every heading offset, span each section from its
// heading to the next heading (or end of body), compute line numbers
// from byte offsets. ID collisions get -2/-3/... suffixes so wikilink
// targets stay unique inside one article.
func extractSections(body []byte) []Section {
	matches := sectionsHeadingRE.FindAllSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return nil
	}

	out := make([]Section, 0, len(matches))
	idCounts := make(map[string]int)
	for i, m := range matches {
		startByte := m[0]
		endByte := len(body)
		if i+1 < len(matches) {
			endByte = matches[i+1][0]
		}
		level := m[3] - m[2]
		title := strings.TrimSpace(string(body[m[4]:m[5]]))

		id := sectionAnchorSlug(title)
		idCounts[id]++
		if n := idCounts[id]; n > 1 {
			id = fmt.Sprintf("%s-%d", id, n)
		}

		out = append(out, Section{
			ID:     id,
			Title:  title,
			Level:  level,
			Lines:  [2]int{byteOffsetToLine(body, startByte), byteOffsetToLine(body, endByte-1)},
			Bytes:  [2]int{startByte, endByte},
			Tokens: (endByte - startByte) / 4,
		})
	}
	return out
}

// sectionAnchorSlug derives a stable URL-safe ID from a heading title.
// Lowercase, alphanumerics + hyphens only. Matches the GitHub /
// Obsidian default heading-anchor scheme so existing wikilinks like
// `[[Article#methods-and-results]]` resolve through this sidecar
// without per-tool dialect.
var sectionSlugStripRE = regexp.MustCompile(`[^a-z0-9\s-]+`)

func sectionAnchorSlug(title string) string {
	s := strings.ToLower(strings.TrimSpace(title))
	s = sectionSlugStripRE.ReplaceAllString(s, "")
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "section"
	}
	return s
}

// byteOffsetToLine converts a byte offset into a 1-based line number
// by counting newlines in the body up to that offset. Cheap; the
// largest article in scriptorium is ~6 KB and we call this twice per
// heading, so the linear scan stays well below 1 ms even on the
// fattest KBs.
func byteOffsetToLine(body []byte, offset int) int {
	if offset > len(body) {
		offset = len(body)
	}
	if offset < 0 {
		offset = 0
	}
	line := 1
	for i := 0; i < offset; i++ {
		if body[i] == '\n' {
			line++
		}
	}
	return line
}

// sectionsSidecarPath returns the on-disk location of an article's
// sidecar. Articles live under root/<dir>/<slug>.md; sidecars live
// under root/wiki/_sections/<dir>/<slug>.json.
//
// Subtle: if the article is itself under root/wiki/, we strip the
// leading "wiki/" before recombining so the parallel tree is rooted
// at root/wiki/_sections/ regardless of whether the article lives in
// root/wiki/research/ or root/research/. Both layouts exist in the
// wild because wikiDirs lists "wiki" and the topic dirs side by side.
func sectionsSidecarPath(root, articlePath string) string {
	rel, err := filepath.Rel(root, articlePath)
	if err != nil {
		rel = articlePath
	}
	rel = strings.TrimPrefix(rel, "wiki"+string(filepath.Separator))
	rel = strings.TrimSuffix(rel, ".md") + ".json"
	return filepath.Join(root, "wiki", "_sections", rel)
}

// writeSectionsSidecar persists the sidecar JSON. Empty section lists
// (article had no H1-H3 headings) get no file written — a stale
// sidecar from a previous pass is removed instead, so the on-disk
// state is always consistent with the article.
func writeSectionsSidecar(root, articlePath string, body []byte) error {
	sections := extractSections(body)
	out := sectionsSidecarPath(root, articlePath)

	if len(sections) == 0 {
		// No headings now; clean up any prior sidecar.
		if err := os.Remove(out); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale sidecar: %w", err)
		}
		return nil
	}

	rel, _ := filepath.Rel(root, articlePath)
	sidecar := SectionsSidecar{
		Version:     sectionsSidecarVersion,
		Article:     filepath.ToSlash(rel),
		ExtractedAt: time.Now().UTC().Format(time.RFC3339),
		Extractor:   "scribe-sections@" + version,
		Sections:    sections,
	}

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("mkdir sidecar parent: %w", err)
	}
	data, err := json.MarshalIndent(sidecar, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sidecar: %w", err)
	}
	if err := os.WriteFile(out, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write sidecar: %w", err)
	}
	return nil
}

// readSectionsSidecar loads a previously-written sidecar. Returns
// (nil, nil) when the article has no sidecar — caller should treat
// that as "article has no headings or wasn't covered by the last
// build pass" and fall back to live parsing if needed.
func readSectionsSidecar(root, articlePath string) (*SectionsSidecar, error) {
	path := sectionsSidecarPath(root, articlePath)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read sidecar: %w", err)
	}
	var s SectionsSidecar
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse sidecar: %w", err)
	}
	if s.Version != sectionsSidecarVersion {
		// Future-proofing: an old schema becomes a soft cache miss,
		// not a hard error. The next build pass overwrites it.
		return nil, nil
	}
	return &s, nil
}

// articleBody returns the body of an article (everything after the
// closing `---` frontmatter delimiter), or the whole content if
// there's no frontmatter. Thin []byte wrapper around the existing
// stripFrontmatter so byte/line offsets in the sidecar are
// body-relative.
func articleBody(content []byte) []byte {
	return []byte(stripFrontmatter(string(content)))
}

// SectionsCmd is the kong CLI surface for Phase 5A. Subcommands:
//
//	scribe sections build [--all]   rebuild sidecars for every wiki article
//	scribe sections list <article>  print sections in an article
//	scribe sections get <article> <id>  print a single section's body
type SectionsCmd struct {
	Build SectionsBuildCmd `cmd:"" help:"Recompute section sidecars for every wiki article."`
	List  SectionsListCmd  `cmd:"" help:"List sections in one article."`
	Get   SectionsGetCmd   `cmd:"" help:"Print one section's body by anchor ID."`
}

// SectionsBuildCmd recomputes every sidecar from scratch. Cheap: pure
// regex over already-loaded markdown, no LLM, no network. Suitable
// to run from `scribe sync` on every cycle once Phase 5A is wired in.
type SectionsBuildCmd struct {
	Verbose bool `help:"Log every article touched." short:"v"`
}

func (b *SectionsBuildCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	written, removed, scanned := 0, 0, 0
	err = walkArticles(root, func(path string, content []byte) error {
		scanned++
		body := articleBody(content)
		sections := extractSections(body)
		if len(sections) == 0 {
			out := sectionsSidecarPath(root, path)
			if _, statErr := os.Stat(out); statErr == nil {
				if rmErr := os.Remove(out); rmErr == nil {
					removed++
					if b.Verbose {
						logMsg("sections", "removed (no headings): %s", relPath(root, path))
					}
				}
			}
			return nil
		}
		if err := writeSectionsSidecar(root, path, body); err != nil {
			logMsg("sections", "write failed for %s: %v", relPath(root, path), err)
			return nil
		}
		written++
		if b.Verbose {
			logMsg("sections", "wrote %d sections: %s", len(sections), relPath(root, path))
		}
		return nil
	})
	if err != nil {
		return err
	}
	logMsg("sections", "build done: scanned=%d wrote=%d removed=%d", scanned, written, removed)
	runStats = map[string]any{
		"sections_scanned": scanned,
		"sections_wrote":   written,
		"sections_removed": removed,
	}
	return nil
}

// SectionsListCmd prints the section index for one article. Resolves
// `article` either as an absolute path, a path relative to KB root,
// or the article title (looked up by walking until the title matches).
type SectionsListCmd struct {
	Article string `arg:"" help:"Article path or title."`
	JSON    bool   `help:"Emit raw sidecar JSON instead of human-readable list."`
}

func (l *SectionsListCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	path, err := resolveArticleArg(root, l.Article)
	if err != nil {
		return err
	}
	sidecar, err := readSectionsSidecar(root, path)
	if err != nil {
		return err
	}
	if sidecar == nil {
		// Try a live extract — useful if the build pass hasn't run yet.
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("article unreadable: %w", readErr)
		}
		sections := extractSections(articleBody(content))
		if len(sections) == 0 {
			return fmt.Errorf("no sections (article has no H1-H3 headings, no sidecar)")
		}
		rel, _ := filepath.Rel(root, path)
		sidecar = &SectionsSidecar{
			Version:  sectionsSidecarVersion,
			Article:  filepath.ToSlash(rel),
			Sections: sections,
		}
	}
	if l.JSON {
		out, _ := json.MarshalIndent(sidecar, "", "  ")
		fmt.Println(string(out))
		return nil
	}
	fmt.Printf("%s\n", sidecar.Article)
	for _, s := range sidecar.Sections {
		indent := strings.Repeat("  ", s.Level-1)
		fmt.Printf("  %s^%-30s  L%d-L%d  ~%d tok  %s%s\n",
			indent, s.ID, s.Lines[0], s.Lines[1], s.Tokens, indent, s.Title)
	}
	return nil
}

// SectionsGetCmd prints one section's body to stdout. The section is
// identified by anchor ID (the same `^foo` used in wikilinks). Output
// is the raw markdown of the section, suitable for piping into other
// tools or feeding into agent prompts.
type SectionsGetCmd struct {
	Article string `arg:"" help:"Article path or title."`
	ID      string `arg:"" help:"Section anchor ID (e.g. methods, why-it-works). Leading ^ tolerated."`
}

func (g *SectionsGetCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	path, err := resolveArticleArg(root, g.Article)
	if err != nil {
		return err
	}
	want := strings.TrimPrefix(strings.TrimSpace(g.ID), "^")
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("article unreadable: %w", err)
	}
	body := articleBody(content)
	sections := extractSections(body)
	for _, s := range sections {
		if s.ID == want {
			start, end := s.Bytes[0], s.Bytes[1]
			if end > len(body) {
				end = len(body)
			}
			fmt.Print(string(body[start:end]))
			return nil
		}
	}
	avail := make([]string, 0, len(sections))
	for _, s := range sections {
		avail = append(avail, "^"+s.ID)
	}
	sort.Strings(avail)
	return fmt.Errorf("section ^%s not found in %s. available: %s",
		want, relPath(root, path), strings.Join(avail, ", "))
}

// resolveArticleArg accepts either a path (absolute, relative to cwd,
// or relative to KB root) or an article title and returns the
// absolute file path. Title resolution walks all wiki articles and
// matches case-insensitively against the `title:` frontmatter field.
//
// Path resolution wins when the arg points at an existing .md file.
// Title resolution is the fallback so users don't have to memorize
// wiki layout for one-off lookups.
func resolveArticleArg(root, arg string) (string, error) {
	if strings.HasSuffix(arg, ".md") {
		if _, err := os.Stat(arg); err == nil {
			abs, _ := filepath.Abs(arg)
			return abs, nil
		}
		candidate := filepath.Join(root, arg)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		// scribe KBs sometimes carry parallel topic dirs at root AND
		// under wiki/ (e.g. decisions/ next to wiki/decisions/). When
		// the bare path doesn't resolve, try the wiki/ prefix before
		// falling through to title lookup.
		if !strings.HasPrefix(arg, "wiki/") {
			candidate = filepath.Join(root, "wiki", arg)
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
	}

	var matched string
	wantLower := strings.ToLower(strings.TrimSpace(arg))
	err := walkArticles(root, func(path string, content []byte) error {
		if matched != "" {
			return nil
		}
		fm, err := parseFrontmatter(content)
		if err != nil {
			return nil //nolint:nilerr // skip unparseable, keep searching
		}
		if strings.EqualFold(fm.Title, arg) || strings.EqualFold(fm.Title, wantLower) {
			matched = path
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if matched == "" {
		return "", fmt.Errorf("article not found: %s (tried as path and as title)", arg)
	}
	return matched, nil
}
