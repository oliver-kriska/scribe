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

// semanticMergers (special_files.go) maps repo-relative paths to
// functions that produce merged content from a conflict's two sides.
// `ours` is the upstream (remote) side during a rebase, `theirs` the
// local commit being replayed; either may be nil on a delete/modify
// conflict.

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

// mergeSessionsLogContent merges a session-mining log
// ({"last_scan":…, "processed":{<id>:…}}) from both sides of a conflict.
// Serves wiki/_sessions_log.json and wiki/_codex_sessions_log.json, which
// share one on-disk shape by design (codex_log.go).
//
// Every machine mines its OWN sessions, so the two sides almost always
// carry disjoint `processed` keys and a union is exactly right. The line
// that actually collides is `last_scan`: updateScanTimestamp rewrites it
// on every `sync --sessions` run whether or not anything was mined, and
// cron fires that job at the same wall-clock slots on every machine.
// Without this merge the rebase aborts, and per the note at the top of
// this file every later pull then fails on the same conflict.
//
// Entries are kept as json.RawMessage rather than a typed struct: values
// are heterogeneously shaped (an early format stored a bare timestamp
// string, the current one an object) and unknown top-level keys must
// survive a round-trip.
func mergeSessionsLogContent(ours, theirs []byte) []byte {
	parse := func(b []byte) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		if len(b) == 0 || json.Unmarshal(b, &m) != nil || m == nil {
			return map[string]json.RawMessage{}
		}
		return m
	}
	processed := func(doc map[string]json.RawMessage) map[string]json.RawMessage {
		var out map[string]json.RawMessage
		if raw, ok := doc["processed"]; ok {
			if json.Unmarshal(raw, &out) != nil || out == nil {
				out = map[string]json.RawMessage{}
			}
		} else {
			out = map[string]json.RawMessage{}
		}
		return out
	}

	ourDoc, theirDoc := parse(ours), parse(theirs)
	merged := processed(ourDoc)
	for id, theirEntry := range processed(theirDoc) {
		ourEntry, ok := merged[id]
		if !ok || sessionEntryNewer(theirEntry, ourEntry) {
			merged[id] = theirEntry
		}
	}

	// Union top-level keys so a field added by a newer scribe on one
	// machine is not dropped by an older one on the other.
	out := map[string]json.RawMessage{}
	for k, v := range theirDoc {
		out[k] = v
	}
	for k, v := range ourDoc {
		out[k] = v
	}
	out["last_scan"] = newerTimestampField(ourDoc["last_scan"], theirDoc["last_scan"])
	if len(out["last_scan"]) == 0 {
		delete(out, "last_scan")
	}

	procData, err := json.Marshal(merged)
	if err != nil {
		return ours
	}
	out["processed"] = procData

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return ours
	}
	return append(data, '\n')
}

// sessionEntryNewer reports whether processed entry a is newer than b.
// Handles both shapes: a bare RFC3339 string, or an object carrying an
// "extracted" timestamp. Unparseable entries never win, so `ours` stands.
func sessionEntryNewer(a, b json.RawMessage) bool {
	ta, okA := sessionEntryTime(a)
	tb, okB := sessionEntryTime(b)
	if !okA {
		return false
	}
	if !okB {
		return true
	}
	return ta.After(tb)
}

func sessionEntryTime(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 {
		return time.Time{}, false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		t, err := time.Parse(time.RFC3339, s)
		return t, err == nil
	}
	var obj struct {
		Extracted string `json:"extracted"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Extracted != "" {
		t, err := time.Parse(time.RFC3339, obj.Extracted)
		return t, err == nil
	}
	return time.Time{}, false
}

// newerTimestampField picks the later of two RFC3339 JSON strings,
// falling back to whichever side is present.
func newerTimestampField(ours, theirs json.RawMessage) json.RawMessage {
	to, okO := sessionEntryTime(ours)
	tt, okT := sessionEntryTime(theirs)
	switch {
	case okO && okT:
		if tt.After(to) {
			return theirs
		}
		return ours
	case okT:
		return theirs
	case okO:
		return ours
	case len(ours) > 0:
		return ours
	default:
		return theirs
	}
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
