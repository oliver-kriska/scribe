package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

	graph, err := buildLinkGraph(root)
	if err != nil {
		return err
	}
	backlinks := backlinksMap(graph)

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
