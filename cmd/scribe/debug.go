package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// DebugCmd groups low-level diagnostic helpers for investigating
// KB bugs without reaching for Python one-liners.
type DebugCmd struct {
	Wikilinks DebugWikilinksCmd `cmd:"" help:"Extract and print wikilinks from a single file (catches code-fence parity bugs)."`
	Backlinks DebugBacklinksCmd `cmd:"" help:"Show all articles that link to a given title."`
}

// --- debug wikilinks ---

type DebugWikilinksCmd struct {
	File string `arg:"" help:"Path to a markdown file."`
	Raw  bool   `help:"Also print raw wikilink matches (before dedup)."`
	JSON bool   `help:"Output JSON."`
}

func (c *DebugWikilinksCmd) Run() error {
	content, err := os.ReadFile(c.File)
	if err != nil {
		return fmt.Errorf("read %s: %w", c.File, err)
	}

	// Clean content the same way extractWikilinks does, so we can show
	// what the real extractor sees (not just the naive regex matches).
	cleaned := stripCode(content)

	// All raw matches (post-cleanup).
	rawMatches := wikilinkRE.FindAllSubmatch(cleaned, -1)
	rawTargets := make([]string, 0, len(rawMatches))
	for _, m := range rawMatches {
		rawTargets = append(rawTargets, string(m[1]))
	}

	// Canonical dedupped list (same logic as extractWikilinks).
	links := extractWikilinks(content)

	// Also compute what the naive scan (no code stripping) would pull,
	// so we can show when stripping saves us from false positives.
	naiveMatches := wikilinkRE.FindAllSubmatch(content, -1)
	naiveTargets := make([]string, 0, len(naiveMatches))
	for _, m := range naiveMatches {
		naiveTargets = append(naiveTargets, string(m[1]))
	}

	// Diff: which raw matches the code-stripping removed (likely false positives).
	naiveSet := make(map[string]int)
	for _, t := range naiveTargets {
		naiveSet[t]++
	}
	cleanedSet := make(map[string]int)
	for _, t := range rawTargets {
		cleanedSet[t]++
	}
	var suppressed []string
	for t, n := range naiveSet {
		if n > cleanedSet[t] {
			suppressed = append(suppressed, t)
		}
	}
	sort.Strings(suppressed)

	if c.JSON {
		out := map[string]any{
			"file":            c.File,
			"links":           links,
			"raw_matches":     rawTargets,
			"naive_matches":   naiveTargets,
			"code_suppressed": suppressed,
			"link_count":      len(links),
			"raw_count":       len(rawTargets),
			"naive_count":     len(naiveTargets),
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	fmt.Printf("File: %s\n", c.File)
	fmt.Printf("  bytes: %d\n", len(content))
	fmt.Printf("  naive matches (no code strip): %d\n", len(naiveTargets))
	fmt.Printf("  post-strip matches           : %d\n", len(rawTargets))
	fmt.Printf("  deduped wikilinks            : %d\n", len(links))

	if len(suppressed) > 0 {
		fmt.Println("\nSuppressed by code-fence/inline-code stripping (would-be false positives):")
		for _, t := range suppressed {
			fmt.Printf("  [[%s]]\n", t)
		}
	}

	if c.Raw && len(rawTargets) > 0 {
		fmt.Println("\nRaw post-strip matches (with duplicates):")
		for _, t := range rawTargets {
			fmt.Printf("  [[%s]]\n", t)
		}
	}

	if len(links) > 0 {
		fmt.Println("\nExtracted wikilinks:")
		for _, t := range links {
			fmt.Printf("  [[%s]]\n", t)
		}
	}
	return nil
}

// --- debug backlinks ---

type DebugBacklinksCmd struct {
	Title string `arg:"" help:"Article title to look up."`
	Fresh bool   `help:"Recompute from disk instead of reading _backlinks.json."`
	JSON  bool   `help:"Output JSON."`
}

func (c *DebugBacklinksCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}

	// _backlinks.json is keyed by canonical title, so an alias or a case
	// variant has to be resolved the way a [[wikilink]] would be.
	title, err := resolveBacklinksKey(root, c.Title)
	if err != nil {
		return err
	}

	var sources []string

	if c.Fresh {
		sources, err = computeBacklinksFor(root, title)
		if err != nil {
			return err
		}
	} else {
		path := filepath.Join(root, "wiki", "_backlinks.json")
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read backlinks: %w", err)
		}
		var all map[string][]string
		if err := json.Unmarshal(data, &all); err != nil {
			return fmt.Errorf("parse backlinks: %w", err)
		}
		sources = all[title]
	}

	if c.JSON {
		out := map[string]any{
			"title":   title,
			"count":   len(sources),
			"sources": sources,
			"fresh":   c.Fresh,
		}
		if title != c.Title {
			out["resolved_from"] = c.Title
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	if title != c.Title {
		fmt.Printf("Backlinks for: %s (resolved from %s)\n", title, c.Title)
	} else {
		fmt.Printf("Backlinks for: %s\n", title)
	}
	if c.Fresh {
		fmt.Println("  (computed fresh from disk)")
	}
	fmt.Printf("  count: %d\n", len(sources))
	if len(sources) == 0 {
		fmt.Println("\n  ⚠ orphan — nothing links here")
		return nil
	}
	fmt.Println()
	for _, s := range sources {
		fmt.Printf("  ← %s\n", s)
	}
	return nil
}

// resolveBacklinksKey maps a title, alias or case variant to the canonical
// title _backlinks.json uses as its key. An ambiguous name stays as given;
// a missing one gets the normalized form a dangling link is keyed by.
func resolveBacklinksKey(root, name string) (string, error) {
	scan, err := scanArticles(root)
	if err != nil {
		return "", err
	}
	canonical, _ := newLinkResolver(scan.entries).resolve(name)
	return canonical, nil
}

// computeBacklinksFor walks the KB and returns sources linking to title.
// It reads the same backlinksMap as `scribe backlinks`, so the two cannot
// disagree about what counts as a link.
func computeBacklinksFor(root, title string) ([]string, error) {
	graph, err := buildLinkGraph(root)
	if err != nil {
		return nil, err
	}
	return backlinksMap(graph)[title], nil
}
