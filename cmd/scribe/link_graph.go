package main

import (
	"path/filepath"
	"sort"
	"strings"
)

// linkEdge is one source file linking to one target.
type linkEdge struct {
	SourcePath  string // KB-relative
	SourceTitle string // article title; for a file without a title, its SourcePath
	Generated   bool   // source file name starts with "_"
}

type graphArticle struct {
	Path     string
	Title    string
	Rolling  bool
	Updated  string
	Outbound int // distinct non-self, non-empty normalized link targets, any status
}

// brokenLink is a link that only resolved by case, slug or path.
type brokenLink struct{ Raw, Canonical, SourcePath string }

// linkGraph is the one wikilink graph that `scribe backlinks`,
// `debug --fresh`, lint and orphans all read, so they cannot disagree.
type linkGraph struct {
	Articles         []graphArticle        // live articles with frontmatter, walk order
	Inbound          map[string][]linkEdge // canonical title -> edges; self-links excluded
	Missing          map[string][]linkEdge // normalized raw target -> edges
	Broken           []brokenLink          // non-generated sources only
	Ambiguous        int                   // distinct ambiguous raw targets
	SupersededTitles map[string]bool       // titles + aliases held only by superseded articles
	Unparsed         int                   // articles whose frontmatter did not parse
	Unreadable       int                   // dirs/files the walk could not read
}

// articleScan is the result of pass 1.
type articleScan struct {
	entries    []resolverEntry
	articles   []graphArticle
	titleByRel map[string]string
	superseded map[string]bool // KB-relative paths
	unparsed   int
}

func scanArticles(root string) (articleScan, error) {
	scan := articleScan{titleByRel: map[string]string{}, superseded: map[string]bool{}}
	_, err := walkMarkdownCounting(root, true, func(path string, content []byte) error {
		rel := filepath.ToSlash(relPath(root, path))
		fm, err := parseFrontmatter(content)
		if err != nil {
			scan.unparsed++
			if title := extractTitleFast(content); title != "" {
				scan.entries = append(scan.entries, resolverEntry{Title: title, Path: rel})
				scan.titleByRel[rel] = title
			}
			return nil
		}
		if fm.Title == "" {
			return nil
		}
		superseded := isSuperseded(fm)
		scan.entries = append(scan.entries, resolverEntry{
			Title: fm.Title, Aliases: toStringSlice(fm.Aliases), Path: rel, Superseded: superseded,
		})
		if superseded {
			scan.superseded[rel] = true
			return nil
		}
		scan.titleByRel[rel] = fm.Title
		scan.articles = append(scan.articles, graphArticle{
			Path: rel, Title: fm.Title, Rolling: fm.Rolling, Updated: stringFromAny(fm.Updated),
		})
		return nil
	})
	return scan, err
}

// supersededOnlyTitles returns the titles and aliases that no live entry holds.
func supersededOnlyTitles(entries []resolverEntry) map[string]bool {
	liveKeys := map[string]bool{}
	for _, entry := range entries {
		if !entry.Superseded {
			liveKeys[entry.Title] = true
			for _, alias := range entry.Aliases {
				liveKeys[alias] = true
			}
		}
	}
	only := map[string]bool{}
	for _, entry := range entries {
		if !entry.Superseded {
			continue
		}
		for _, key := range append([]string{entry.Title}, entry.Aliases...) {
			if key != "" && !liveKeys[key] {
				only[key] = true
			}
		}
	}
	return only
}

// buildLinkGraph walks articles (pass 1) to build the resolver, then every
// markdown file including _-prefixed hubs (pass 2) to resolve each link.
// Superseded files are never link sources.
func buildLinkGraph(root string) (linkGraph, error) {
	scan, err := scanArticles(root)
	if err != nil {
		return linkGraph{}, err
	}
	resolver := newLinkResolver(scan.entries)
	graph := linkGraph{
		Articles:         scan.articles,
		Inbound:          map[string][]linkEdge{},
		Missing:          map[string][]linkEdge{},
		SupersededTitles: supersededOnlyTitles(scan.entries),
		Unparsed:         scan.unparsed,
	}
	outbound := map[string]int{}
	ambiguous := map[string]bool{}
	skipped, err := walkMarkdownCounting(root, false, func(path string, content []byte) error {
		rel := filepath.ToSlash(relPath(root, path))
		if scan.superseded[rel] {
			return nil
		}
		sourceTitle, ok := scan.titleByRel[rel]
		if !ok {
			sourceTitle = rel
		}
		edge := linkEdge{SourcePath: rel, SourceTitle: sourceTitle, Generated: strings.HasPrefix(filepath.Base(rel), "_")}
		seenTargets := map[string]bool{}
		seenCanonical := map[string]bool{}
		for _, raw := range extractWikilinks(content) {
			target := normalizeLinkTarget(raw)
			if target == "" || seenTargets[target] {
				continue
			}
			canonical, status := resolver.resolve(target)
			if status != linkMissing && canonical == sourceTitle {
				continue
			}
			seenTargets[target] = true
			outbound[rel]++
			// extractWikilinks strips code spans, so a link to a title with
			// code arrives without it; the file text is already right.
			if status == linkFuzzy && !edge.Generated && collapseSpace(stripCodeSpans(canonical)) != collapseSpace(target) {
				graph.Broken = append(graph.Broken, brokenLink{Raw: raw, Canonical: canonical, SourcePath: rel})
			}
			if seenCanonical[canonical] {
				continue
			}
			seenCanonical[canonical] = true
			switch status {
			case linkAmbiguous:
				ambiguous[target] = true
			case linkMissing:
				graph.Missing[canonical] = append(graph.Missing[canonical], edge)
			default:
				graph.Inbound[canonical] = append(graph.Inbound[canonical], edge)
			}
		}
		return nil
	})
	if err != nil {
		return linkGraph{}, err
	}
	for i := range graph.Articles {
		graph.Articles[i].Outbound = outbound[graph.Articles[i].Path]
	}
	graph.Ambiguous = len(ambiguous)
	// Pass 2 walks a superset of pass 1's files, so its skips are the total.
	graph.Unreadable = skipped
	return graph, nil
}

// backlinksMap is the _backlinks.json content: target -> sorted, deduped
// source titles. A title held only by superseded articles is not a key; a
// missing link keeps its normalized raw text as the key.
func backlinksMap(graph linkGraph) map[string][]string {
	titlesByTarget := map[string]map[string]bool{}
	add := func(target string, edges []linkEdge) {
		if titlesByTarget[target] == nil {
			titlesByTarget[target] = map[string]bool{}
		}
		for _, edge := range edges {
			titlesByTarget[target][edge.SourceTitle] = true
		}
	}
	for target, edges := range graph.Inbound {
		if !graph.SupersededTitles[target] {
			add(target, edges)
		}
	}
	for target, edges := range graph.Missing {
		add(target, edges)
	}
	backlinks := make(map[string][]string, len(titlesByTarget))
	for target, titles := range titlesByTarget {
		sources := make([]string, 0, len(titles))
		for title := range titles {
			sources = append(sources, title)
		}
		sort.Strings(sources)
		backlinks[target] = sources
	}
	return backlinks
}

// graphOrphans returns the sorted, deduped titles of live, non-rolling
// articles that no other file links to.
func graphOrphans(g linkGraph) []string {
	seen := map[string]bool{}
	orphans := []string{}
	for _, article := range g.Articles {
		if article.Rolling || seen[article.Title] || len(g.Inbound[article.Title]) > 0 {
			continue
		}
		seen[article.Title] = true
		orphans = append(orphans, article.Title)
	}
	sort.Strings(orphans)
	return orphans
}

// graphMissing returns the sorted raw targets that match no article.
func graphMissing(g linkGraph) []string {
	missing := make([]string, 0, len(g.Missing))
	for target := range g.Missing {
		missing = append(missing, target)
	}
	sort.Strings(missing)
	return missing
}

func collapseSpace(text string) string { return strings.Join(strings.Fields(text), " ") }
