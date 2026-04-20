package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LinkCmd finds orphan articles and proposes contextual hosts for them.
//
// An orphan is an article with zero inbound wikilinks. The command scores
// candidate hosts by tag overlap, shared domain, title term hits, and a
// bonus for project overviews. Above a threshold, a `## See Also` entry is
// proposed. Below it, the orphan either falls through to a shared-neighbor
// second pass (if --related is set) or the hub index file.
type LinkCmd struct {
	Apply    bool   `help:"Write proposed See Also links to host articles."`
	Orphan   string `help:"Link a single orphan by wikilink title (skips discovery)."`
	Hub      bool   `help:"Also create/update hub _index.md files for remaining orphans."`
	Related  bool   `help:"Run a second scoring pass based on shared-neighbor wikilinks before hub fallback. Borrowed from zk's --related."`
	MinScore int    `help:"Minimum candidate score to propose as host." default:"8"`
	Top      int    `help:"Show top N candidates per orphan (dry-run only)." default:"3"`
}

// article is a minimal in-memory representation used by the linker.
type article struct {
	Path     string
	Title    string
	Domain   string
	Tags     []string
	Body     string   // first ~2000 chars, lowercased
	Outbound []string // outbound wikilink targets (from full body + related frontmatter)
	Rolling  bool
	IsOrphan bool
}

type linkCandidate struct {
	Host    *article
	Score   float64
	Reasons []string
}

func (c *LinkCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}

	articles, err := loadArticles(root)
	if err != nil {
		return fmt.Errorf("load articles: %w", err)
	}

	titleIndex := make(map[string]*article, len(articles))
	for i := range articles {
		titleIndex[articles[i].Title] = articles[i]
	}

	// Compute orphans from backlinks file (same source as `scribe orphans`).
	orphanTitles, err := computeOrphans(root, titleIndex)
	if err != nil {
		return fmt.Errorf("compute orphans: %w", err)
	}

	// Filter orphans if --orphan flag is set.
	if c.Orphan != "" {
		if _, ok := titleIndex[c.Orphan]; !ok {
			return fmt.Errorf("orphan %q not found by title", c.Orphan)
		}
		orphanTitles = []string{c.Orphan}
	}

	if len(orphanTitles) == 0 {
		fmt.Println("No orphans found. Nothing to do.")
		return nil
	}

	fmt.Printf("Scoring %d orphan(s) against %d candidate hosts...\n\n", len(orphanTitles), len(articles))

	applied := 0
	unlinked := make([]string, 0)

	for _, title := range orphanTitles {
		orphan := titleIndex[title]
		if orphan == nil {
			continue
		}
		candidates := scoreHosts(orphan, articles, c.MinScore)

		// Fallback: if the primary scorer found nothing and --related is
		// set, look for hosts that share outbound wikilinks with the
		// orphan. This catches topically-adjacent articles the tag/domain
		// heuristic misses (zk calls this "related notes").
		if len(candidates) == 0 && c.Related {
			candidates = scoreHostsByRelated(orphan, articles, 2)
		}

		if len(candidates) == 0 {
			fmt.Printf("  [no host]  %s\n", title)
			unlinked = append(unlinked, title)
			continue
		}

		best := candidates[0]
		fmt.Printf("  %-8s  %s\n", fmt.Sprintf("%.1f", best.Score), title)
		fmt.Printf("      -> %s  (%s)\n", relPath(root, best.Host.Path), strings.Join(best.Reasons, ", "))

		// Show runners-up in dry-run mode for visibility.
		if !c.Apply && c.Top > 1 {
			for i := 1; i < len(candidates) && i < c.Top; i++ {
				cand := candidates[i]
				fmt.Printf("         %.1f  %s  (%s)\n", cand.Score, relPath(root, cand.Host.Path), strings.Join(cand.Reasons, ", "))
			}
		}

		if c.Apply {
			if err := appendSeeAlso(best.Host.Path, title); err != nil {
				fmt.Printf("      !! failed: %v\n", err)
				continue
			}
			applied++
		}
	}

	fmt.Println()
	if c.Apply {
		fmt.Printf("Applied %d See Also links. %d orphans unlinked.\n", applied, len(unlinked))
	} else {
		fmt.Printf("Dry run: would link %d orphans via See Also. %d unlinked.\n",
			len(orphanTitles)-len(unlinked), len(unlinked))
		fmt.Println("Re-run with --apply to write changes.")
	}

	if c.Hub && len(unlinked) > 0 {
		if err := addToHubs(root, unlinked, titleIndex, c.Apply); err != nil {
			return fmt.Errorf("hub fallback: %w", err)
		}
	}

	return nil
}

// loadArticles walks the KB and returns a minimal article record for each file.
func loadArticles(root string) ([]*article, error) {
	var articles []*article
	err := walkArticles(root, func(path string, content []byte) error {
		fm, err := parseFrontmatter(content)
		if err != nil {
			return nil //nolint:nilerr // skip unparseable frontmatter, continue walk
		}
		if fm.Title == "" {
			return nil
		}

		body := string(content)
		if len(body) > 2500 {
			body = body[:2500]
		}

		// Outbound links come from the full article body plus the
		// `related:` frontmatter field. Frontmatter links are deliberate
		// author-curated neighbors and should count even when the field
		// format (YAML quoted strings) trips up the body-level regex.
		outbound := extractWikilinks(content)
		seen := make(map[string]bool, len(outbound))
		for _, t := range outbound {
			seen[t] = true
		}
		for _, r := range toStringSlice(fm.Related) {
			// related entries look like "[[Title]]" — strip the brackets.
			r = strings.TrimSpace(r)
			r = strings.TrimPrefix(r, "[[")
			r = strings.TrimSuffix(r, "]]")
			if idx := strings.Index(r, "|"); idx > 0 {
				r = r[:idx]
			}
			r = strings.TrimSpace(r)
			if r != "" && !seen[r] {
				seen[r] = true
				outbound = append(outbound, r)
			}
		}

		art := &article{
			Path:     path,
			Title:    fm.Title,
			Domain:   fm.Domain,
			Tags:     toStringSlice(fm.Tags),
			Body:     strings.ToLower(body),
			Outbound: outbound,
			Rolling:  fm.Rolling,
		}
		articles = append(articles, art)
		return nil
	})
	return articles, err
}

// computeOrphans reads backlinks and returns titles with zero inbound links.
// Matches the logic in `scribe orphans` but returns only the titles.
func computeOrphans(root string, titleIndex map[string]*article) ([]string, error) {
	blPath := filepath.Join(root, "wiki", "_backlinks.json")
	backlinks := make(map[string][]string)
	if data, err := os.ReadFile(blPath); err == nil {
		_ = json.Unmarshal(data, &backlinks)
	}

	allTargets := make(map[string]bool)
	err := walkAllMarkdown(root, func(_ string, content []byte) error {
		for _, link := range extractWikilinks(content) {
			allTargets[link] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	var orphans []string
	for title, art := range titleIndex {
		if art.Rolling {
			continue // rolling memory files are hosted via overview links, not orphans to relink
		}
		if _, hasBacklinks := backlinks[title]; hasBacklinks {
			continue
		}
		if allTargets[title] {
			continue
		}
		orphans = append(orphans, title)
	}
	sort.Strings(orphans)
	return orphans, nil
}

// scoreHosts returns candidate host articles above the min score, sorted by score.
func scoreHosts(orphan *article, articles []*article, minScore int) []linkCandidate {
	var candidates []linkCandidate
	orphanTerms := tokenize(orphan.Title)

	for _, host := range articles {
		if host.Path == orphan.Path {
			continue
		}
		if host.Rolling {
			continue // don't route orphans into rolling memory files
		}
		// Skip _index.md and friends (already excluded by walkArticles).

		score := 0.0
		reasons := []string{}

		// Tag overlap: +3 per shared tag.
		shared := intersectCount(orphan.Tags, host.Tags)
		if shared > 0 {
			score += float64(shared) * 3
			reasons = append(reasons, fmt.Sprintf("%d shared tag(s)", shared))
		}

		// Same domain (not "general"): +5.
		if orphan.Domain != "" && orphan.Domain == host.Domain && orphan.Domain != "general" {
			score += 5
			reasons = append(reasons, "same domain: "+orphan.Domain)
		}

		// Title terms found in host body: +2 per hit (first 2K chars).
		hits := 0
		for _, t := range orphanTerms {
			if len(t) < 4 {
				continue
			}
			if strings.Contains(host.Body, t) {
				hits++
			}
		}
		if hits > 0 {
			score += float64(hits) * 2
			reasons = append(reasons, fmt.Sprintf("title terms matched %dx", hits))
		}

		// Project overview bonus: +4.
		if strings.HasSuffix(host.Path, "/overview.md") {
			score += 4
			reasons = append(reasons, "project overview")
		}

		if score >= float64(minScore) {
			candidates = append(candidates, linkCandidate{
				Host:    host,
				Score:   score,
				Reasons: reasons,
			})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})
	return candidates
}

// scoreHostsByRelated finds candidate hosts that share at least minShared
// outbound wikilinks with the orphan. Used as a fallback when the primary
// tag/domain/body scorer finds no host — topically-adjacent articles often
// share common neighbors even when their tags don't overlap. Borrowed from
// zk's "related notes" feature.
func scoreHostsByRelated(orphan *article, articles []*article, minShared int) []linkCandidate {
	orphanLinks := make(map[string]bool, len(orphan.Outbound))
	for _, t := range orphan.Outbound {
		orphanLinks[t] = true
	}
	if len(orphanLinks) == 0 {
		return nil
	}
	var candidates []linkCandidate
	for _, host := range articles {
		if host.Path == orphan.Path || host.Rolling {
			continue
		}
		shared := 0
		for _, t := range host.Outbound {
			if orphanLinks[t] {
				shared++
			}
		}
		if shared >= minShared {
			candidates = append(candidates, linkCandidate{
				Host:    host,
				Score:   float64(shared) * 3,
				Reasons: []string{fmt.Sprintf("%d shared neighbor(s)", shared)},
			})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})
	return candidates
}

// appendSeeAlso adds "- [[Title]]" under a "## See Also" section in the host file.
// Creates the section if it doesn't exist. Idempotent: skips if the link already exists.
func appendSeeAlso(hostPath, orphanTitle string) error {
	data, err := os.ReadFile(hostPath)
	if err != nil {
		return err
	}
	content := string(data)
	linkLine := fmt.Sprintf("- [[%s]]", orphanTitle)

	// Idempotency: if the article already links to this orphan, skip.
	if strings.Contains(content, "[["+orphanTitle+"]]") {
		return nil
	}

	if idx := strings.Index(content, "\n## See Also\n"); idx >= 0 {
		// Section exists — find end of section and append.
		sectionStart := idx + len("\n## See Also\n")
		// Find next "## " heading or EOF.
		tail := content[sectionStart:]
		endOfSection := strings.Index(tail, "\n## ")
		if endOfSection < 0 {
			endOfSection = len(tail)
		}
		insertAt := sectionStart + len(strings.TrimRight(tail[:endOfSection], "\n"))
		newContent := content[:insertAt] + "\n" + linkLine + content[insertAt:]
		return os.WriteFile(hostPath, []byte(newContent), 0o644)
	}

	// No section — append at end of file.
	trimmed := strings.TrimRight(content, "\n")
	newContent := trimmed + "\n\n## See Also\n\n" + linkLine + "\n"
	return os.WriteFile(hostPath, []byte(newContent), 0o644)
}

// addToHubs groups unlinked orphans by directory and appends them to hub _index.md files.
func addToHubs(root string, orphans []string, titleIndex map[string]*article, apply bool) error {
	// Group orphans by their top-level directory.
	hubGroups := make(map[string][]string)
	for _, title := range orphans {
		art := titleIndex[title]
		if art == nil {
			continue
		}
		rel, err := filepath.Rel(root, art.Path)
		if err != nil {
			continue
		}
		parts := strings.SplitN(rel, string(filepath.Separator), 2)
		if len(parts) < 2 {
			continue
		}
		topDir := parts[0]
		hubGroups[topDir] = append(hubGroups[topDir], title)
	}

	if len(hubGroups) == 0 {
		return nil
	}

	fmt.Println()
	fmt.Println("Hub fallback:")
	for dir, titles := range hubGroups {
		hubPath := filepath.Join(root, dir, "_index.md")
		fmt.Printf("  %s  -> %d orphan(s)\n", relPath(root, hubPath), len(titles))
		for _, t := range titles {
			fmt.Printf("      - %s\n", t)
		}
		if apply {
			if err := writeHubIndex(hubPath, dir, titles); err != nil {
				fmt.Printf("      !! failed: %v\n", err)
			}
		}
	}
	return nil
}

// writeHubIndex creates or updates a hub _index.md file with orphan wikilinks.
func writeHubIndex(hubPath, dir string, titles []string) error {
	existing, err := os.ReadFile(hubPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	if len(existing) == 0 {
		// Create a fresh hub index.
		name := titleCase(dir)
		var sb strings.Builder
		fmt.Fprintf(&sb, `---
title: "%s Index"
type: research
created: 2026-04-10
updated: 2026-04-10
domain: general
confidence: high
tags: [index, %s]
related: []
sources: []
---

# %s Index

A flat listing of articles in this directory.

## Articles

`, name, dir, name)
		for _, t := range titles {
			fmt.Fprintf(&sb, "- [[%s]]\n", t)
		}
		return os.WriteFile(hubPath, []byte(sb.String()), 0o644)
	}

	// Append missing titles to the existing file.
	current := string(existing)
	var toAdd []string
	for _, t := range titles {
		if !strings.Contains(current, "[["+t+"]]") {
			toAdd = append(toAdd, t)
		}
	}
	if len(toAdd) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString(strings.TrimRight(current, "\n"))
	for _, t := range toAdd {
		fmt.Fprintf(&sb, "\n- [[%s]]", t)
	}
	sb.WriteString("\n")
	return os.WriteFile(hubPath, []byte(sb.String()), 0o644)
}

// titleCase capitalises the first rune of s. Replacement for strings.Title.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// toStringSlice coerces a YAML any value into a []string.
func toStringSlice(v any) []string {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{t}
	}
	return nil
}

// intersectCount returns the number of elements present in both slices (case-insensitive).
func intersectCount(a, b []string) int {
	set := make(map[string]bool, len(a))
	for _, x := range a {
		set[strings.ToLower(strings.TrimSpace(x))] = true
	}
	n := 0
	for _, y := range b {
		if set[strings.ToLower(strings.TrimSpace(y))] {
			n++
		}
	}
	return n
}

// tokenize splits a string into lowercased, non-stopword tokens >= 4 chars.
func tokenize(s string) []string {
	s = strings.ToLower(s)
	var tokens []string
	scanner := bufio.NewScanner(strings.NewReader(s))
	scanner.Split(bufio.ScanWords)
	for scanner.Scan() {
		w := strings.Trim(scanner.Text(), ".,;:!?()[]{}\"'`")
		if len(w) < 4 {
			continue
		}
		if linkStopwords[w] {
			continue
		}
		tokens = append(tokens, w)
	}
	return tokens
}

var linkStopwords = map[string]bool{
	"with": true, "from": true, "that": true, "this": true, "into": true,
	"when": true, "what": true, "where": true, "which": true, "their": true,
	"them": true, "they": true, "your": true, "about": true, "over": true,
	"under": true, "more": true, "most": true, "some": true, "than": true,
	"then": true, "have": true, "been": true, "were": true, "will": true,
	"would": true, "should": true, "could": true,
}
