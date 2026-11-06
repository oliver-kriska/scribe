package main

// articleLinks is the wikilink graph `scribe backlinks` writes and
// `scribe debug --fresh` recomputes: which file links where, keyed by
// relative path, with each file's title. Both build from this one walk so
// they cannot disagree.
type articleLinks struct {
	pathToTitle map[string]string
	pathToLinks map[string][]string
	// supersededTitles holds titles that belong only to superseded
	// articles; those are neither link sources nor link targets. A title
	// also used by a live article is not in this set.
	supersededTitles map[string]bool
}

// collectArticleLinks walks articles (which have titles), then hub
// _index.md files (which don't, so their path stands in as the title).
// Superseded articles are skipped in both passes.
func collectArticleLinks(root string) (articleLinks, error) {
	g := articleLinks{
		pathToTitle:      make(map[string]string),
		pathToLinks:      make(map[string][]string),
		supersededTitles: make(map[string]bool),
	}
	supersededPaths := make(map[string]bool)
	liveTitles := make(map[string]bool)

	err := walkArticles(root, func(path string, content []byte) error {
		fm, err := parseFrontmatter(content)
		if err != nil {
			return nil //nolint:nilerr // skip unparseable file, continue walk
		}
		rel := relPath(root, path)
		if isSuperseded(fm) {
			g.supersededTitles[fm.Title] = true
			supersededPaths[rel] = true
			return nil
		}
		liveTitles[fm.Title] = true
		g.pathToTitle[rel] = fm.Title
		if links := extractWikilinks(content); len(links) > 0 {
			g.pathToLinks[rel] = links
		}
		return nil
	})
	if err != nil {
		return articleLinks{}, err
	}
	for title := range liveTitles {
		delete(g.supersededTitles, title)
	}

	err = walkAllMarkdown(root, func(path string, content []byte) error {
		rel := relPath(root, path)
		if supersededPaths[rel] {
			return nil
		}
		if _, already := g.pathToLinks[rel]; already {
			return nil
		}
		if links := extractWikilinks(content); len(links) > 0 {
			g.pathToLinks[rel] = links
			if _, ok := g.pathToTitle[rel]; !ok {
				g.pathToTitle[rel] = rel // path as pseudo-title for hub files
			}
		}
		return nil
	})
	if err != nil {
		return articleLinks{}, err
	}
	return g, nil
}
