package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type OrphansCmd struct {
	JSON        bool `help:"Output JSON."`
	OrphansOnly bool `help:"Show only orphan articles (hide missing pages). Mirrors zk's --orphan." name:"orphans"`
	MissingOnly bool `help:"Show only missing pages (hide orphans). Mirrors zk's --missing-backlink." name:"missing"`
}

type orphanReport struct {
	Orphans []string `json:"orphans"`
	Missing []string `json:"missing"`
}

func (o *OrphansCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}

	// Load or compute backlinks
	blPath := filepath.Join(root, "wiki", "_backlinks.json")
	backlinks := make(map[string][]string)
	if data, err := os.ReadFile(blPath); err == nil {
		_ = json.Unmarshal(data, &backlinks)
	}

	// Collect titles (and aliases) from articles so that both resolve as
	// valid link targets. Aliases come from the optional `aliases:` YAML
	// field and live in the same namespace as canonical titles for the
	// purposes of orphan / missing-page detection.
	allTitles := make(map[string]bool)
	allTargets := make(map[string]bool)

	err = walkArticles(root, func(_ string, content []byte) error {
		fm, ferr := parseFrontmatter(content)
		if ferr == nil && fm.Title != "" {
			allTitles[fm.Title] = true
			for _, alias := range toStringSlice(fm.Aliases) {
				if alias != "" {
					allTitles[alias] = true
				}
			}
			return nil
		}
		// Fall back to the fast title scan if frontmatter didn't parse.
		if title := extractTitleFast(content); title != "" {
			allTitles[title] = true
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Scan ALL markdown for wikilinks (including _-prefixed)
	err = walkAllMarkdown(root, func(_ string, content []byte) error {
		for _, link := range extractWikilinks(content) {
			allTargets[link] = true
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Orphans: titles with no inbound links
	var orphans []string
	for title := range allTitles {
		if _, hasBacklinks := backlinks[title]; !hasBacklinks {
			// Also check if it's a target of any link
			if !allTargets[title] {
				orphans = append(orphans, title)
			}
		}
	}
	sort.Strings(orphans)

	// Missing: link targets that don't match any title
	var missing []string
	for target := range allTargets {
		if !allTitles[target] {
			missing = append(missing, target)
		}
	}
	sort.Strings(missing)

	// If neither filter flag is set, show both (classic behavior). Otherwise
	// only the requested sections are printed. Setting both flags is
	// equivalent to setting neither — defensive, not an error.
	showOrphans := !o.MissingOnly || o.OrphansOnly
	showMissing := !o.OrphansOnly || o.MissingOnly

	if o.JSON {
		report := orphanReport{}
		if showOrphans {
			report.Orphans = orphans
		}
		if showMissing {
			report.Missing = missing
		}
		data, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(data))
		return nil
	}

	if showOrphans {
		if len(orphans) > 0 {
			fmt.Printf("Orphan articles (%d) — no inbound wikilinks:\n", len(orphans))
			for _, t := range orphans {
				fmt.Printf("  - %s\n", t)
			}
		} else {
			fmt.Println("No orphan articles found.")
		}
	}

	if showMissing {
		if showOrphans {
			fmt.Println()
		}
		if len(missing) > 0 {
			fmt.Printf("Missing pages (%d) — linked but don't exist:\n", len(missing))
			for _, t := range missing {
				fmt.Printf("  - [[%s]]\n", t)
			}
		} else {
			fmt.Println("No missing page links found.")
		}
	}

	return nil
}
