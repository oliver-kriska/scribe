package main

import (
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
