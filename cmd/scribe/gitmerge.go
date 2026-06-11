package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// gitmerge.go — semantic conflict resolution for the committed team
// coordination files.
//
// derivedRegenerable files (index, backlinks, digest) can take either
// side of a conflict because the content regenerates after the pull.
// The files here CANNOT: they accumulate state from every machine, so
// picking a side throws away a teammate's writes — and on a team KB
// where every member's cron fires at the same wall-clock slots,
// concurrent commits to these files are the normal case, not the edge.
// Without a semantic merge the first concurrent push leaves every
// other clone permanently failing its pulls (rebase aborts on the
// same conflict forever while local commits pile up).

// semanticMergers maps repo-relative paths to functions that produce
// merged content from a conflict's two sides. `ours` is the upstream
// (remote) side during a rebase, `theirs` the local commit being
// replayed; either may be nil on a delete/modify conflict.
var semanticMergers = map[string]func(ours, theirs []byte) []byte{
	"scripts/extraction-ledger.json": mergeLedgerContent,
	"scripts/dream-lease.json":       mergeLeaseContent,
	"log.md":                         mergeUnionLines,
}

// semanticResolve merges one conflicted path during a rebase and stages
// the result. Returns false when the blobs can't be read or written —
// the caller aborts the rebase.
func semanticResolve(repoPath, rel string) bool {
	merge := semanticMergers[rel]
	if merge == nil {
		return false
	}
	ours, oursErr := gitShowBytes(repoPath, ":2:"+rel)
	theirs, theirsErr := gitShowBytes(repoPath, ":3:"+rel)
	if oursErr != nil && theirsErr != nil {
		return false
	}
	if oursErr != nil {
		ours = nil
	}
	if theirsErr != nil {
		theirs = nil
	}
	merged := merge(ours, theirs)
	if err := os.WriteFile(filepath.Join(repoPath, rel), merged, 0o644); err != nil {
		return false
	}
	_, err := runCmdErr(repoPath, "git", "add", "--", rel)
	return err == nil
}

// mergeLedgerContent unions the extraction-ledger maps, keeping the
// newest entry per repo key. Safe by the ledger's own contract: it is
// an optimization (skip duplicate extraction), never a source of
// truth, so the worst possible merge outcome is one redundant
// extraction.
func mergeLedgerContent(ours, theirs []byte) []byte {
	parse := func(b []byte) map[string]ledgerEntry {
		var l extractionLedger
		if len(b) == 0 || json.Unmarshal(b, &l) != nil || l.Repos == nil {
			return map[string]ledgerEntry{}
		}
		return l.Repos
	}
	merged := parse(ours)
	for key, theirEntry := range parse(theirs) {
		ourEntry, ok := merged[key]
		if !ok || ledgerEntryNewer(theirEntry, ourEntry) {
			merged[key] = theirEntry
		}
	}
	data, err := json.MarshalIndent(&extractionLedger{Repos: merged}, "", "  ")
	if err != nil {
		return ours
	}
	return append(data, '\n')
}

// ledgerEntryNewer reports whether a was extracted after b.
func ledgerEntryNewer(a, b ledgerEntry) bool {
	ta, errA := time.Parse(time.RFC3339, a.ExtractedAt)
	tb, errB := time.Parse(time.RFC3339, b.ExtractedAt)
	if errA != nil || errB != nil {
		return a.ExtractedAt > b.ExtractedAt
	}
	return ta.After(tb)
}

// mergeLeaseContent resolves a dream-lease conflict in the REMOTE's
// favor: the first claim to reach origin wins the race, and the loser's
// acquireDreamLease re-check then sees the winner's lease and backs
// off. (Latest-expiry semantics would let the racing loser keep its own
// claim and dream anyway.)
func mergeLeaseContent(ours, theirs []byte) []byte {
	if len(ours) > 0 {
		return ours
	}
	return theirs
}

// mergeUnionLines merges an append-only text file: the remote side's
// content followed by every local line not already present. For two
// machines that appended different tails to a common base this yields
// base + remote tail + local tail — nothing lost, order stable.
func mergeUnionLines(ours, theirs []byte) []byte {
	if len(ours) == 0 {
		return theirs
	}
	if len(theirs) == 0 {
		return ours
	}
	seen := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimRight(string(ours), "\n"), "\n") {
		seen[line] = true
	}
	var extra []string
	for line := range strings.SplitSeq(strings.TrimRight(string(theirs), "\n"), "\n") {
		if !seen[line] {
			extra = append(extra, line)
		}
	}
	out := strings.TrimRight(string(ours), "\n") + "\n"
	if len(extra) > 0 {
		out += strings.Join(extra, "\n") + "\n"
	}
	return []byte(out)
}
