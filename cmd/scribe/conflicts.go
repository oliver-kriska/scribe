package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// conflictHit is one KB markdown file containing unresolved git
// merge-conflict markers.
type conflictHit struct {
	Rel  string // path relative to KB root
	Line int    // 1-based line of the first marker
}

// findConflictMarkers scans the wiki content dirs plus raw/articles for
// unresolved git merge-conflict markers. Team KBs auto-pull before sync,
// so a botched manual merge can land "<<<<<<< HEAD" blocks in articles
// where they poison search results and LLM context until someone
// notices. Only the "<<<<<<< " and ">>>>>>> " line prefixes count — a
// bare "=======" line is a legal setext-heading underline in markdown
// and can't be used as a signal on its own. One hit per file (first
// marker), sorted by path.
func findConflictMarkers(root string) []conflictHit {
	var hits []conflictHit
	seen := map[string]bool{}
	record := func(path string, content []byte) error {
		rel := relPath(root, path)
		if seen[rel] {
			return nil
		}
		if line := firstConflictMarkerLine(content); line > 0 {
			seen[rel] = true
			hits = append(hits, conflictHit{Rel: rel, Line: line})
		}
		return nil
	}

	_ = walkAllMarkdown(root, record)

	// raw/articles isn't part of wikiDirs but pulled merges land there
	// too (captured URLs, collected research).
	rawDir := filepath.Join(root, "raw", "articles")
	_ = filepath.Walk(rawDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // skip unreadable, continue walk
		}
		if !strings.HasSuffix(path, ".md") || strings.HasPrefix(info.Name(), ".") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil //nolint:nilerr // skip unreadable, continue walk
		}
		return record(path, content)
	})

	sort.Slice(hits, func(i, j int) bool { return hits[i].Rel < hits[j].Rel })
	return hits
}

// firstConflictMarkerLine returns the 1-based line number of the first
// git conflict marker in content, or 0 when there is none. Manual line
// splitting instead of bufio.Scanner — articles can exceed Scanner's
// 64K default line limit.
func firstConflictMarkerLine(content []byte) int {
	openMarker := []byte("<<<<<<< ")
	closeMarker := []byte(">>>>>>> ")
	lineNo := 1
	for start := 0; start < len(content); lineNo++ {
		end := bytes.IndexByte(content[start:], '\n')
		var line []byte
		if end < 0 {
			line = content[start:]
			start = len(content)
		} else {
			line = content[start : start+end]
			start += end + 1
		}
		if bytes.HasPrefix(line, openMarker) || bytes.HasPrefix(line, closeMarker) {
			return lineNo
		}
	}
	return 0
}
