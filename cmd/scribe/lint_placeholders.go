package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// lint_placeholders.go cleans the on-disk fingerprint of a prompt-template
// variable that leaked unsubstituted into a KB path. When an extraction prompt
// referenced a {{VAR}} the caller never supplied, the model echoed it verbatim
// into the article's title→path (and sometimes frontmatter), producing real
// directories like `projects/{{DOMAIN}}/` (observed in scriptorium, 2026-05).
//
// The cause is fixed at the seam: loadPrompt (claude.go) strips any residual
// {{VAR}} before the prompt ships, and autoFixArticle clamps a present-but-
// invalid `domain: {{DOMAIN}}` frontmatter value to `general`. This module is
// the remediation arm for paths already committed: `scribe doctor` surfaces
// them and `scribe lint --fix` removes them. A {{VAR}} path component is never
// legitimate — no KB names a folder with double braces — so removal is
// unambiguous, and the KB's auto-commit makes it git-recoverable.

// placeholderPathRE matches an unsubstituted prompt placeholder ({{NAME}},
// uppercase/digits/underscore — the prompt-var naming convention) anywhere in
// a path component. Same shape as claude.go's promptPlaceholderRE; kept
// separate so the lint arm doesn't reach into the prompt-loader internals.
var placeholderPathRE = regexp.MustCompile(`\{\{[A-Z0-9_]+\}\}`)

// findPlaceholderArtifacts returns the KB-relative path of every top-most file
// or directory under the wiki dirs whose name carries an unsubstituted {{VAR}}
// placeholder. A matching directory is reported once (the walk does not
// descend), not once per child. Generated/hidden trees (_sections, _-prefixed,
// dotfiles) are skipped — they regenerate clean from the canonical articles
// once the source artifact is gone and `scribe index` reruns. Results are
// sorted for stable output.
func findPlaceholderArtifacts(root string) []string {
	var out []string
	for _, dir := range wikiDirs {
		dirPath := filepath.Join(root, dir)
		if _, err := os.Stat(dirPath); os.IsNotExist(err) {
			continue
		}
		_ = filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil {
				return nil //nolint:nilerr // skip unreadable, continue walk
			}
			base := info.Name()
			if info.IsDir() {
				if path == dirPath {
					return nil
				}
				if strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".") {
					return filepath.SkipDir
				}
				if placeholderPathRE.MatchString(base) {
					out = append(out, relPath(root, path))
					return filepath.SkipDir // report the dir once; don't descend
				}
				return nil
			}
			if placeholderPathRE.MatchString(base) {
				out = append(out, relPath(root, path))
			}
			return nil
		})
	}
	sort.Strings(out)
	return out
}

// fixPlaceholderArtifacts removes every placeholder-named artifact (the whole
// tree for a directory). Returns the count of top-level artifacts removed. The
// content survives in git history; the path itself is corrupt and cannot be
// repaired in place — there is no correct {{VAR}} substitution after the fact.
func fixPlaceholderArtifacts(root string, dryRun bool) (removed int) {
	verb := "FIX"
	if dryRun {
		verb = "WOULD FIX"
	}
	for _, rel := range findPlaceholderArtifacts(root) {
		fmt.Printf("  %s remove placeholder-path artifact %s (unsubstituted template var leaked into path; git-recoverable)\n", verb, rel)
		if !dryRun {
			if err := os.RemoveAll(filepath.Join(root, rel)); err != nil {
				fmt.Printf("  SKIP %s: remove failed: %v\n", rel, err)
				continue
			}
		}
		removed++
	}
	return removed
}
