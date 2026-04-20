package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type BacklinksCmd struct {
	DryRun bool `help:"Show what would change without writing." short:"n"`
	JSON   bool `help:"Output JSON to stdout instead of writing file."`
}

func (b *BacklinksCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}

	// Map: title -> path (for resolving link targets)
	titleToPath := make(map[string]string)
	// Map: path -> title
	pathToTitle := make(map[string]string)
	// Map: path -> []wikilink targets
	pathToLinks := make(map[string][]string)

	// First pass: collect titles of real articles (underscored files have no title).
	err = walkArticles(root, func(path string, content []byte) error {
		fm, err := parseFrontmatter(content)
		if err != nil {
			return nil //nolint:nilerr // skip unparseable file, continue walk
		}
		rel := relPath(root, path)
		titleToPath[fm.Title] = rel
		pathToTitle[rel] = fm.Title
		links := extractWikilinks(content)
		if len(links) > 0 {
			pathToLinks[rel] = links
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Second pass: include wikilinks from hub _index.md files.
	// They lack frontmatter titles, so attribute them to their relative path.
	err = walkAllMarkdown(root, func(path string, content []byte) error {
		rel := relPath(root, path)
		if _, already := pathToLinks[rel]; already {
			return nil
		}
		links := extractWikilinks(content)
		if len(links) > 0 {
			pathToLinks[rel] = links
			if _, ok := pathToTitle[rel]; !ok {
				pathToTitle[rel] = rel // use path as pseudo-title for hub files
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Build backlinks: target title -> [source titles]
	backlinks := make(map[string][]string)
	for sourcePath, links := range pathToLinks {
		sourceTitle := pathToTitle[sourcePath]
		for _, target := range links {
			// Only add if source != target
			if target != sourceTitle {
				backlinks[target] = append(backlinks[target], sourceTitle)
			}
		}
	}

	// Sort source lists for deterministic output
	for target := range backlinks {
		sort.Strings(backlinks[target])
	}

	data, err := json.MarshalIndent(backlinks, "", "  ")
	if err != nil {
		return fmt.Errorf("json marshal: %w", err)
	}
	data = append(data, '\n')

	if b.JSON {
		_, err = os.Stdout.Write(data)
		return err
	}

	outPath := filepath.Join(root, "wiki", "_backlinks.json")

	if b.DryRun {
		existing, err := os.ReadFile(outPath)
		if err != nil {
			fmt.Println("would create _backlinks.json")
		} else if string(existing) != string(data) {
			fmt.Println("_backlinks.json would change")
		} else {
			fmt.Println("_backlinks.json is up to date")
		}
		return nil
	}

	// Atomic write
	tmp := outPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}

	fmt.Printf("wrote %s (%d targets)\n", relPath(root, outPath), len(backlinks))
	return nil
}
