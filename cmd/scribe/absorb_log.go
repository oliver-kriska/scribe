package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// absorb_log.go implements Phase 3C — content-aware absorb idempotency.
// The on-disk artifact wiki/_absorb_log.json grew up as a flat
// {filename: ISO-timestamp} map. That gives us "skip if seen by name"
// semantics, but it's blind in two ways:
//
//  1. A raw article that gets re-fetched (same name, different bytes)
//     would be skipped even though the new content deserves a fresh
//     absorb pass.
//  2. Two raw articles with different names but identical bodies
//     (common when the user re-imports the same paper from two
//     sources) would both absorb, producing duplicate entity proposals
//     and wasted tokens.
//
// Phase 3C upgrades the schema to {filename: {sha, at}} and teaches
// the loader to accept both formats so existing logs upgrade in
// place. The sha lets the absorb pipeline detect content drift
// (re-absorb) and cross-name duplication (skip + log).

// AbsorbLogEntry is one record in wiki/_absorb_log.json.
//
// SHA is the hex sha256 of the raw article bytes at absorb time.
// Empty for legacy entries written before Phase 3C; in that case the
// file is treated as "absorbed at unknown content" and a re-run will
// recompute SHA, write it back, and skip if nothing else changed.
type AbsorbLogEntry struct {
	SHA string `json:"sha,omitempty"`
	At  string `json:"at"`
}

// AbsorbLog is the typed view of wiki/_absorb_log.json.
type AbsorbLog map[string]AbsorbLogEntry

// UnmarshalJSON tolerates two shapes for backwards compat:
//
//	v1 (legacy): {"foo.md": "2025-01-01T..."}
//	v2:          {"foo.md": {"sha":"...","at":"..."}}
//
// Each value is parsed as object first, then string, then ignored.
// "Ignored" here means a stray malformed entry doesn't block loading
// the rest — better to fail-open and re-absorb a single quirky entry
// than refuse to load the whole log.
func (al *AbsorbLog) UnmarshalJSON(data []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := AbsorbLog{}
	for name, value := range raw {
		var entry AbsorbLogEntry
		if err := json.Unmarshal(value, &entry); err == nil && (entry.At != "" || entry.SHA != "") {
			out[name] = entry
			continue
		}
		var legacy string
		if err := json.Unmarshal(value, &legacy); err == nil {
			out[name] = AbsorbLogEntry{At: legacy}
			continue
		}
	}
	*al = out
	return nil
}

// loadAbsorbLog reads wiki/_absorb_log.json. Missing file returns an
// empty log (not an error) — first-run idempotency means absent ==
// "nothing absorbed yet". Read errors fall through to nil + error so
// the caller can decide whether to abort.
func loadAbsorbLog(path string) (AbsorbLog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return AbsorbLog{}, nil
		}
		return nil, fmt.Errorf("read absorb log: %w", err)
	}
	var log AbsorbLog
	if err := json.Unmarshal(data, &log); err != nil {
		return nil, fmt.Errorf("parse absorb log: %w", err)
	}
	if log == nil {
		log = AbsorbLog{}
	}
	return log, nil
}

// saveAbsorbLog writes the log atomically (tmp + rename) to avoid
// truncating a valid file when the encode fails partway through.
// Keys are sorted so diffs in the file (committed to git in the KB)
// stay readable.
func saveAbsorbLog(path string, log AbsorbLog) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir absorb log dir: %w", err)
	}
	keys := make([]string, 0, len(log))
	for k := range log {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]AbsorbLogEntry, len(log))
	for _, k := range keys {
		ordered[k] = log[k]
	}
	data, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal absorb log: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write absorb log tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename absorb log: %w", err)
	}
	return nil
}

// absorbDecision is the verdict produced by checkAbsorbDecision: do
// we absorb this raw article, skip it as already-done, or skip it
// because the same content already absorbed under a different name?
type absorbDecision int

const (
	absorbDecisionRun absorbDecision = iota
	absorbDecisionSkipSameContent
	absorbDecisionSkipDupContent
	absorbDecisionRunRefresh
)

// checkAbsorbDecision routes one raw article against the log. The
// shaErr return is non-nil only when sha could not be computed — in
// that case the caller should fall back to filename-only behavior
// (absorb if absent, skip if present) so a transient I/O error
// doesn't leave an article unprocessed forever.
func checkAbsorbDecision(log AbsorbLog, name, sha string) absorbDecision {
	if entry, ok := log[name]; ok {
		// Legacy entry (no sha) preserves the original Phase 1
		// "skip if seen by name" behavior; sha-equal is the same
		// verdict for v2 entries; sha-mismatch means the file was
		// re-fetched and we want to re-absorb.
		if entry.SHA == "" || entry.SHA == sha {
			return absorbDecisionSkipSameContent
		}
		return absorbDecisionRunRefresh
	}
	// Different name, but same sha already absorbed elsewhere?
	if sha != "" {
		for other, entry := range log {
			if other == name {
				continue
			}
			if entry.SHA != "" && entry.SHA == sha {
				return absorbDecisionSkipDupContent
			}
		}
	}
	return absorbDecisionRun
}

// findDupName returns the existing log entry name whose sha matches
// the given sha, if any. Used by callers that need to log "skipped X
// (dup of Y)" diagnostics after checkAbsorbDecision returns
// absorbDecisionSkipDupContent.
func findDupName(log AbsorbLog, exclude, sha string) string {
	if sha == "" {
		return ""
	}
	for name, entry := range log {
		if name == exclude {
			continue
		}
		if entry.SHA == sha {
			return name
		}
	}
	return ""
}
