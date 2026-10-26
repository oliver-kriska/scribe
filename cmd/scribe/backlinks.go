package main

import (
	"bytes"
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

	g, err := collectArticleLinks(root)
	if err != nil {
		return err
	}

	// Build backlinks: target title -> [source titles]
	backlinks := make(map[string][]string)
	for sourcePath, links := range g.pathToLinks {
		sourceTitle := g.pathToTitle[sourcePath]
		for _, target := range links {
			// Skip self-links and titles held only by superseded articles.
			if target != sourceTitle && !g.supersededTitles[target] {
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
		switch {
		case err != nil:
			fmt.Println("would create _backlinks.json")
		case !bytes.Equal(existing, data):
			fmt.Println("_backlinks.json would change")
		default:
			fmt.Println("_backlinks.json is up to date")
		}
		return nil
	}

	// Atomic write
	if err := writeFileAtomic(outPath, data, 0o644); err != nil {
		return err
	}

	fmt.Printf("wrote %s (%d targets)\n", relPath(root, outPath), len(backlinks))
	return nil
}
