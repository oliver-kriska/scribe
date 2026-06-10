package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// resolveContributor returns the identity to stamp into the
// `contributor:` frontmatter of newly created articles. Resolution
// order:
//
//  1. `contributor:` in the user-level config (~/.config/scribe/config.yaml).
//     This is per-person, NOT per-KB — in a shared team KB the repo's
//     scribe.yaml is common to everyone, so an override there would
//     misattribute every member's extractions to one name.
//  2. `git config user.name` resolved in the KB checkout (picks up
//     per-repo identity overrides).
//  3. `git config user.email` as a last resort.
//
// Empty result means "no identity available" — callers skip stamping
// rather than writing an empty field.
func resolveContributor(root string) string {
	if uc := loadUserConfig(); uc.Contributor != "" {
		return uc.Contributor
	}
	if name := runCmd(root, "git", "config", "user.name"); name != "" {
		return name
	}
	return runCmd(root, "git", "config", "user.email")
}

// yamlSingleQuote wraps s in YAML single quotes, doubling embedded
// quotes per the YAML spec. Names are arbitrary user strings ("Tim
// O'Brien", "Team: Platform") so the value can't be written as a bare
// scalar the way type/domain identifiers are.
func yamlSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// stampContributor injects `contributor: '<name>'` into the frontmatter
// of every NEW article about to be committed. It runs at the gitAddWiki
// staging funnel, which every commit path shares (project extract,
// session mining, absorb, dream, deep) — so it covers both envelope-mode
// writes and legacy tool-mode writes where the model creates files
// directly, without threading a template var through every prompt.
//
// Only newly created files are stamped: `contributor:` records who first
// produced the article; later edits are visible in git history. Files
// that already carry the key (e.g. a hand-written article or a drop file
// that set its own attribution) are left alone.
func stampContributor(root string) {
	name := resolveContributor(root)
	if name == "" {
		return
	}
	stamped := 0
	for _, rel := range newWikiMarkdownFiles(root) {
		abs := filepath.Join(root, rel)
		raw, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		content := string(raw)
		if !strings.HasPrefix(content, "---") {
			continue // no frontmatter — not an article
		}
		fm, err := parseFrontmatterRaw(raw)
		if err != nil {
			continue // unparseable — lint's problem, not ours
		}
		if _, has := fm["contributor"]; has {
			continue
		}
		updated := setFrontmatterScalar(content, "contributor", yamlSingleQuote(name))
		if updated == content {
			continue
		}
		if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil {
			continue
		}
		stamped++
	}
	if stamped > 0 {
		logMsg("git", "stamped contributor %q on %d new article(s)", name, stamped)
	}
}

// newWikiMarkdownFiles returns KB-relative paths of .md files under the
// wiki content dirs that are new to git — untracked or index-added —
// per `git status --porcelain`. Underscore-prefixed basenames
// (_index.md, _unfetched-links.md, …) are scribe-generated artifacts,
// not articles, and are excluded.
func newWikiMarkdownFiles(root string) []string {
	args := []string{"status", "--porcelain", "--untracked-files=all", "--"}
	args = append(args, wikiDirs...)
	out := runCmd(root, "git", args...)
	if out == "" {
		return nil
	}
	var files []string
	for line := range strings.SplitSeq(out, "\n") {
		if len(line) < 4 {
			continue
		}
		status := line[:2]
		// New files only: untracked (??) or added to the index (A in
		// either column, covering ordinary adds and add-after-merge).
		if status != "??" && status[0] != 'A' && status[1] != 'A' {
			continue
		}
		path := strings.TrimSpace(line[3:])
		// Renames/copies report "old -> new"; the new path is the file
		// on disk.
		if idx := strings.Index(path, " -> "); idx >= 0 {
			path = path[idx+4:]
		}
		// Paths with special characters come back C-style quoted.
		if strings.HasPrefix(path, `"`) {
			if unquoted, err := strconv.Unquote(path); err == nil {
				path = unquoted
			}
		}
		if !strings.HasSuffix(path, ".md") {
			continue
		}
		if strings.HasPrefix(filepath.Base(path), "_") {
			continue
		}
		files = append(files, path)
	}
	return files
}
