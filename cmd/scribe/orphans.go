package main

import (
	"encoding/json"
	"fmt"
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

	graph, err := buildLinkGraph(root)
	if err != nil {
		return err
	}
	orphans := graphOrphans(graph)
	missing := graphMissing(graph)

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
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
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
