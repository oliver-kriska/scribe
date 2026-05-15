package main

import (
	"path/filepath"
	"time"
)

// codex_log.go is the C3 idempotency ledger. Codex rollouts are not in
// ccrider's DB, so a mining pass that re-scans ~/.codex/ each run
// needs a durable processed-set to avoid re-mining the same session.
// The on-disk shape intentionally mirrors wiki/_sessions_log.json
// ({"processed":{<id>:{...}}}) so the existing generic helpers
// (loadProcessedSessionIDs, updateJSONFile) are reused unchanged — one
// ledger format, two surfaces.
//
// The dedup key is the rollout's session_meta `id` (a stable UUID per
// Codex session), not the file path: a session can be resumed into a
// new rollout file, and we don't want to re-mine it on resume.

func codexSessionsLogPath(root string) string {
	return filepath.Join(root, "wiki", "_codex_sessions_log.json")
}

// loadProcessedCodexIDs returns the set of Codex session IDs already
// mined (or deliberately skipped), so the driver can filter them out
// before scoring.
func loadProcessedCodexIDs(root string) map[string]bool {
	ids := loadProcessedSessionIDs(codexSessionsLogPath(root))
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// markCodexProcessed records a Codex session as handled. Idempotent —
// re-marking an existing id just refreshes its record (matches the
// ccrider session-log semantics). `reason` is empty for a normal mine
// and set for a skip (below MinScore, empty transcript, etc.) so a
// later `scribe doctor`/sessions inspection can explain the decision.
func markCodexProcessed(root, id, cwd, reason string) error {
	if id == "" {
		return nil
	}
	return updateJSONFile(codexSessionsLogPath(root), func(data map[string]any) {
		processed, _ := data["processed"].(map[string]any)
		if processed == nil {
			processed = make(map[string]any)
			data["processed"] = processed
		}
		rec := map[string]any{
			"extracted": time.Now().UTC().Format(time.RFC3339),
			"cwd":       cwd,
		}
		if reason != "" {
			rec["skipped"] = true
			rec["reason"] = reason
		}
		processed[id] = rec
	})
}
