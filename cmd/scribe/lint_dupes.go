package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// lint_dupes.go detects and cleans the on-disk fingerprint of the
// KB-self-ingestion bug: a KB that extracted itself fed its own filenames
// back in as article titles, producing pages like `readme.md.md` and
// `readme_md.md` alongside the real `readme.md` (reported: forest, 2026-05).
//
// The cause is fixed elsewhere (discovery/extraction/session-mining now skip
// any scribe KB, and validateActionPath refuses a doubled `.md.md` write), so
// no new artifacts can appear. This module is the remediation arm: `scribe
// lint` flags the artifacts already committed, and `scribe lint --fix` removes
// or renames them. The KB is a git repo with auto-commit, so a removal is
// always recoverable.

// dupArtifact is a wiki file whose name carries the filename-as-title
// fingerprint. Two shapes are recognized:
//
//   - "doubled-ext": basename ends in ".md.md" (e.g. readme.md.md). Always
//     malformed — no page legitimately carries a doubled markdown extension.
//   - "slug-dot": basename is "<X>_md.md" (e.g. readme_md.md), the slugified
//     form of the filename "<X>.md" (the slugifier turns "." into "_"). Only
//     treated as a duplicate when the canonical "<X>.md" sibling actually
//     exists, since "<X>_md" can otherwise be a legitimate article slug.
type dupArtifact struct {
	Path         string // absolute path to the malformed file
	Rel          string // KB-relative path (for display)
	CanonicalRel string // KB-relative canonical sibling
	canonicalAbs string
	canonicalOK  bool   // canonical sibling exists on disk
	Shape        string // "doubled-ext" | "slug-dot"
}

// action is what `lint --fix` does with the artifact: "remove" when a
// canonical sibling already holds the content (this file is a redundant
// duplicate), or "rename" when there is none (recover the content under the
// valid single-.md name). slug-dot artifacts are only ever classified when a
// canonical exists, so they always "remove".
func (d dupArtifact) action() string {
	if d.canonicalOK {
		return "remove"
	}
	return "rename"
}

// findSelfIngestionDuplicates scans the wiki dirs for dupArtifacts, skipping
// underscore-prefixed generated files and the curated skip set. Results are
// sorted by KB-relative path for stable output.
func findSelfIngestionDuplicates(root string) []dupArtifact {
	var out []dupArtifact
	for _, dir := range wikiDirs {
		dirPath := filepath.Join(root, dir)
		if _, err := os.Stat(dirPath); os.IsNotExist(err) {
			continue
		}
		_ = filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil //nolint:nilerr // skip unreadable, continue walk
			}
			name := info.Name()
			if !strings.HasSuffix(strings.ToLower(name), ".md") {
				return nil
			}
			if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") || skipFiles[name] {
				return nil
			}
			if d, ok := classifyDupArtifact(root, path, name); ok {
				out = append(out, d)
			}
			return nil
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out
}

// classifyDupArtifact decides whether path (basename name) is a filename-as-
// title duplicate and resolves its canonical sibling. Returns ok=false for
// ordinary articles.
func classifyDupArtifact(root, path, name string) (dupArtifact, bool) {
	dir := filepath.Dir(path)

	// Shape 1: doubled markdown extension — collapse repeats to a single .md.
	if strings.HasSuffix(strings.ToLower(name), ".md.md") {
		canonical := name
		for strings.HasSuffix(strings.ToLower(canonical), ".md.md") {
			canonical = canonical[:len(canonical)-len(".md")]
		}
		canAbs := filepath.Join(dir, canonical)
		return dupArtifact{
			Path:         path,
			Rel:          relPath(root, path),
			CanonicalRel: relPath(root, canAbs),
			canonicalAbs: canAbs,
			canonicalOK:  fileExists(canAbs),
			Shape:        "doubled-ext",
		}, true
	}

	// Shape 2: "<X>_md.md" — the slugified form of the filename "<X>.md".
	// Only a duplicate when the canonical "<X>.md" exists; "<X>_md" on its
	// own may be a legitimate slug, so leave it untouched.
	if stem, ok := strings.CutSuffix(name, ".md"); ok {
		if base, ok := strings.CutSuffix(stem, "_md"); ok && base != "" {
			canAbs := filepath.Join(dir, base+".md")
			if fileExists(canAbs) {
				return dupArtifact{
					Path:         path,
					Rel:          relPath(root, path),
					CanonicalRel: relPath(root, canAbs),
					canonicalAbs: canAbs,
					canonicalOK:  true,
					Shape:        "slug-dot",
				}, true
			}
		}
	}
	return dupArtifact{}, false
}

// findSelfNamedDirs returns wiki-dir paths of directories named after the KB
// itself (matching kb_name or the KB-root basename, case-insensitive) — the
// directory-level fingerprint of session self-ingestion: mining the sessions
// where the KB is curated makes the extractor file pages under a project
// folder named for the KB (e.g. wiki/scriptorium/, projects/scriptorium/).
// Generated trees (_sections, _-prefixed) are skipped — they regenerate.
// Returns KB-relative dir paths, sorted. Detection only; the contents may
// hold unique fragments that need a human merge, so this is never auto-fixed.
func findSelfNamedDirs(root string) []string {
	names := map[string]bool{
		strings.ToLower(kbName(root)):        true,
		strings.ToLower(filepath.Base(root)): true,
	}
	var out []string
	for _, dir := range wikiDirs {
		dirPath := filepath.Join(root, dir)
		if _, err := os.Stat(dirPath); os.IsNotExist(err) {
			continue
		}
		_ = filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || !info.IsDir() {
				return nil //nolint:nilerr // skip unreadable, continue walk
			}
			base := info.Name()
			// Skip generated/hidden trees and their descendants.
			if strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			if path != dirPath && names[strings.ToLower(base)] {
				out = append(out, relPath(root, path))
			}
			return nil
		})
	}
	sort.Strings(out)
	return out
}

// fixSelfIngestionDuplicates removes redundant duplicates (canonical sibling
// present) and renames orphan doubled-extension files to the canonical
// single-.md name. Prints one line per action. Returns (removed, renamed).
func fixSelfIngestionDuplicates(root string, dryRun bool) (removed, renamed int) {
	verb := "FIX"
	if dryRun {
		verb = "WOULD FIX"
	}
	for _, d := range findSelfIngestionDuplicates(root) {
		switch d.action() {
		case "remove":
			fmt.Printf("  %s remove duplicate %s (canonical %s holds the content)\n", verb, d.Rel, d.CanonicalRel)
			if !dryRun {
				if err := os.Remove(d.Path); err != nil {
					fmt.Printf("  SKIP %s: remove failed: %v\n", d.Rel, err)
					continue
				}
			}
			removed++
		case "rename":
			// Guard against clobbering a canonical that appeared mid-pass.
			if fileExists(d.canonicalAbs) {
				fmt.Printf("  SKIP %s: canonical %s now exists; not overwriting\n", d.Rel, d.CanonicalRel)
				continue
			}
			fmt.Printf("  %s rename %s → %s (doubled .md extension)\n", verb, d.Rel, d.CanonicalRel)
			if !dryRun {
				if err := os.Rename(d.Path, d.canonicalAbs); err != nil {
					fmt.Printf("  SKIP %s: rename failed: %v\n", d.Rel, err)
					continue
				}
			}
			renamed++
		}
	}
	return removed, renamed
}

// --- byte-identical duplicates (auto-removed by --fix) ---
//
// The `--duplicates` exact tier hashes the NORMALIZED body and is report-only:
// two pages with the same body but different frontmatter (title, sources) need
// a human to pick the canonical one. Byte-for-byte identical pages are the
// stronger case — keeping either loses nothing — so `lint --fix` collapses
// them automatically. The KB auto-commits, so a wrongly-removed page is one
// `git checkout` away (the user accepted this tradeoff).

// keepPreferred reports whether KB-relative path a is the better page to KEEP
// over b when the two are byte-identical: prefer the shallower path (a copy
// nested under a self-named or stray dir loses to the canonical flat page),
// then the lexicographically smaller. Fully deterministic so a repeated cron
// --fix never flip-flops which copy survives.
func keepPreferred(a, b string) bool {
	if da, db := strings.Count(a, "/"), strings.Count(b, "/"); da != db {
		return da < db
	}
	return a < b
}

// findByteIdenticalDuplicates groups wiki articles whose FULL file bytes
// (frontmatter included) hash identically, returning each group of 2+ with the
// keep-page first. Aggregation/rolling files are not excluded here: two
// byte-identical copies of one are still redundant.
func findByteIdenticalDuplicates(root string) [][]string {
	byHash := map[string][]string{}
	_ = walkArticles(root, func(path string, content []byte) error {
		sum := sha256.Sum256(content)
		h := hex.EncodeToString(sum[:])
		byHash[h] = append(byHash[h], relPath(root, path))
		return nil
	})
	var groups [][]string
	for _, rels := range byHash {
		if len(rels) < 2 {
			continue
		}
		sort.Slice(rels, func(i, j int) bool { return keepPreferred(rels[i], rels[j]) })
		groups = append(groups, rels)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i][0] < groups[j][0] })
	return groups
}

// fixByteIdenticalDuplicates keeps the preferred page in each byte-identical
// group and removes the rest. Returns the count removed.
func fixByteIdenticalDuplicates(root string, dryRun bool) (removed int) {
	verb := "FIX"
	if dryRun {
		verb = "WOULD FIX"
	}
	for _, g := range findByteIdenticalDuplicates(root) {
		keep := g[0]
		for _, rel := range g[1:] {
			fmt.Printf("  %s remove byte-identical duplicate %s (identical to %s; git-recoverable)\n", verb, rel, keep)
			if !dryRun {
				if err := os.Remove(filepath.Join(root, rel)); err != nil {
					fmt.Printf("  SKIP %s: remove failed: %v\n", rel, err)
					continue
				}
			}
			removed++
		}
	}
	return removed
}
