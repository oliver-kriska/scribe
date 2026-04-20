package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type IndexCmd struct {
	DryRun bool `help:"Show diff without writing." short:"n"`
}

type articleEntry struct {
	title   string
	summary string // "type, domain"
	dir     string
}

func (i *IndexCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}

	// Collect articles grouped by directory
	groups := make(map[string][]articleEntry)

	err = walkArticles(root, func(path string, content []byte) error {
		fm, err := parseFrontmatter(content)
		if err != nil {
			return nil //nolint:nilerr // skip unparseable frontmatter, continue walk
		}
		rel := relPath(root, path)
		dir := filepath.Dir(rel)

		// Extract first non-frontmatter paragraph as summary hint
		summary := fmt.Sprintf("%s, %s", fm.Type, fm.Domain)

		// Build one-line description from first content line after frontmatter
		body := extractBody(content)
		if desc := firstSentence(body); desc != "" {
			// Truncate to reasonable length without slicing through a wikilink.
			if len(desc) > 80 {
				desc = truncateOutsideWikilink(desc, 80)
			}
			summary = desc + " (" + summary + ")"
		} else {
			summary = "(" + summary + ")"
		}

		groups[dir] = append(groups[dir], articleEntry{
			title:   fm.Title,
			summary: summary,
			dir:     dir,
		})
		return nil
	})
	if err != nil {
		return err
	}

	// Sort groups by directory name, entries by title
	var dirs []string
	for d := range groups {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	totalArticles := 0
	var sb strings.Builder
	fmt.Fprintf(&sb, `---
title: KB Index
last_updated: %s
article_count: %%d
---

# Index

`, time.Now().Format("2006-01-02"))

	for _, dir := range dirs {
		entries := groups[dir]
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].title < entries[j].title
		})

		fmt.Fprintf(&sb, "## %s/\n\n", dir)
		for _, e := range entries {
			fmt.Fprintf(&sb, "- [[%s]] -- %s\n", e.title, e.summary)
			totalArticles++
		}
		sb.WriteString("\n")
	}

	// Replace placeholder with actual count
	result := strings.Replace(sb.String(), "article_count: %d", fmt.Sprintf("article_count: %d", totalArticles), 1)

	outPath := filepath.Join(root, "wiki", "_index.md")

	if i.DryRun {
		existing, err := os.ReadFile(outPath)
		if err != nil {
			fmt.Printf("would create _index.md with %d articles\n", totalArticles)
		} else if string(existing) != result {
			fmt.Printf("_index.md would change (%d articles)\n", totalArticles)
		} else {
			fmt.Println("_index.md is up to date")
		}
		return nil
	}

	// Atomic write
	tmp := outPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(result), 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}

	fmt.Printf("wrote %s (%d articles)\n", relPath(root, outPath), totalArticles)
	return nil
}

// truncateOutsideWikilink trims s to roughly limit characters but backs up to
// avoid leaving a half-open `[[...` wikilink inside the result. Without this
// guard, a summary like `... the [[Claude Elixir Phoenix Plugin]] sparked ...`
// cut at character 80 might land mid-link, producing `... the [[Claude Elixir
// Phoenix...` — an unclosed wikilink that the extractor then tries to match
// across lines against the next entry's `]]`.
func truncateOutsideWikilink(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := max(limit-3, 0) // room for the trailing ellipsis
	head := s[:cut]
	// If the tail opens a `[[` that never closes inside the cut, back up to
	// just before that opener so the truncation stays outside the link.
	if idx := strings.LastIndex(head, "[["); idx >= 0 {
		if !strings.Contains(head[idx:], "]]") {
			head = strings.TrimRight(head[:idx], " ")
		}
	}
	return head + "..."
}

// extractBody returns content after the closing --- frontmatter delimiter.
func extractBody(content []byte) string {
	s := string(content)
	if !strings.HasPrefix(s, "---") {
		return s
	}
	end := strings.Index(s[3:], "\n---")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(s[end+7:]) // skip past "\n---\n"
}

// firstSentence returns the first meaningful sentence from body text.
func firstSentence(body string) string {
	if body == "" {
		return ""
	}
	// Skip headings
	lines := strings.SplitSeq(body, "\n")
	for line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "---") {
			continue
		}
		// Found first content line — take first sentence
		if idx := strings.Index(line, ". "); idx > 0 && idx < 120 {
			return line[:idx+1]
		}
		if len(line) < 120 {
			return line
		}
		return truncateOutsideWikilink(line, 120)
	}
	return ""
}
