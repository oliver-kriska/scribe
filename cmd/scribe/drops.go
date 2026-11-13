package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Drop-file staging. collectDropFiles copies each project's unprocessed
// `.claude/<kb>/*.md` handoffs into output/drops-<project>/; extraction
// feeds them to the model; finishDropStaging removes what the model saw.
//
// Two rules keep a drop from being lost:
//   - only drops the model actually received are removed from staging —
//     the envelope path caps its files block, and anything past the cap
//     stays staged for the next extraction (which projectsNeedingExtraction
//     schedules as long as the staging dir is non-empty);
//   - staged names are collision-safe, because a worktree and the main
//     checkout can each carry a same-named drop file.

// dropStagingDir is where a project's collected drops wait for extraction.
func dropStagingDir(root, pname string) string {
	return filepath.Join(root, "output", "drops-"+pname)
}

// stagedDrops lists the staged drop files in stable (name) order.
func stagedDrops(staging string) []string {
	drops, _ := filepath.Glob(filepath.Join(staging, "*.md"))
	sort.Strings(drops)
	return drops
}

// stagedDropDigests hashes every staged drop once, so collection can
// skip a drop whose bytes are already staged without re-reading the
// whole staging dir per drop (the scan was quadratic: 85 drops against
// 65k leftover copies made one sync spend two minutes collecting).
func stagedDropDigests(staging string) map[[sha256.Size]byte]struct{} {
	seen := map[[sha256.Size]byte]struct{}{}
	for _, p := range stagedDrops(staging) {
		if data, err := os.ReadFile(p); err == nil {
			seen[sha256.Sum256(data)] = struct{}{}
		}
	}
	return seen
}

// stagedDropDest picks a destination name for src inside staging that
// does not clobber a file already there (a same-named drop from another
// worktree, or a leftover deferred from the previous run).
func stagedDropDest(staging, src string) string {
	base := filepath.Base(src)
	dest := filepath.Join(staging, base)
	if !fileExists(dest) {
		return dest
	}
	stem := strings.TrimSuffix(base, ".md")
	tag := shortHash(filepath.Dir(src))[:8]
	dest = filepath.Join(staging, fmt.Sprintf("%s-%s.md", stem, tag))
	for i := 2; fileExists(dest); i++ {
		dest = filepath.Join(staging, fmt.Sprintf("%s-%s-%d.md", stem, tag, i))
	}
	return dest
}

// finishDropStaging removes the drops the model consumed and stamps the
// manifest. Drops still staged afterwards are logged and left for the
// next extraction. The stamp advances whenever anything was consumed:
// it gates *collection* (sources newer than it are copied again), and
// every staged file was already collected.
func finishDropStaging(pname, staging string, consumed []string, entry *ProjectEntry, timestamp string) {
	if !dirExists(staging) {
		return
	}
	for _, p := range consumed {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			logMsg("sync", " [%s] remove consumed drop %s: %v", pname, p, err)
		}
	}
	if len(consumed) > 0 {
		entry.LastDropProcessed = timestamp
	}
	left := stagedDrops(staging)
	if len(left) == 0 {
		os.RemoveAll(staging)
		return
	}
	logMsg("sync", " [%s] %d drop file(s) deferred to the next extraction (files budget) — still staged in %s", pname, len(left), staging)
}

// dropMergeTarget returns the KB page (root-relative) a staged drop asks
// to merge into, or "" when it asks for a new page or names no page that
// exists. A drop with `action: append|update` names its page by `target`
// or by `title`. The extract prompt allows `append` only on a path the
// model was shown, and extraction never shows existing pages, so the
// model answered such drops with `create`, which the executor refuses
// on an existing file: the update was lost (3 of 19 drops on 2026-11-13).
// titles is called only when a lookup by title is needed.
func dropMergeTarget(root string, data []byte, titles func() map[string]string) string {
	raw, err := parseFrontmatterRaw(data)
	if err != nil {
		return ""
	}
	action, _ := raw["action"].(string)
	if action != "append" && action != "update" {
		return ""
	}
	if t, _ := raw["target"].(string); t != "" {
		if abs, err := validateActionPath(root, t); err == nil && fileExists(abs) {
			return filepath.ToSlash(t)
		}
	}
	title, _ := raw["title"].(string)
	if title = strings.ToLower(strings.TrimSpace(title)); title == "" {
		return ""
	}
	return titles()[title]
}

// kbTitlePaths maps each lowercased article title to its root-relative
// path. A title shared by two pages maps to "" (ambiguous): merging a
// drop into the wrong one is worse than letting the model create.
func kbTitlePaths(root string) map[string]string {
	out := map[string]string{}
	_ = walkArticles(root, func(path string, content []byte) error {
		fm, err := parseFrontmatter(content)
		if err != nil || fm == nil || fm.Title == "" {
			return nil //nolint:nilerr // unparseable article: skip it, keep walking
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil //nolint:nilerr // outside root: skip
		}
		key := strings.ToLower(strings.TrimSpace(fm.Title))
		if _, dup := out[key]; dup {
			out[key] = ""
			return nil
		}
		out[key] = filepath.ToSlash(rel)
		return nil
	})
	return out
}
