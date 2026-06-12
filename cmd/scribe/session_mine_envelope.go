package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// session_mine_envelope.go is the Phase 4C orchestrator path. The
// previous session-mine path delegated everything to `claude -p` with
// ccrider MCP tools; the orchestrator pulls transcripts in Go,
// inlines them into a tightened prompt, and applies the EnvelopeV2
// the model returns. No MCP, no filesystem tools — runs cleanly
// against Ollama.
//
// The legacy path is still wired in mineSessionBatches; the
// dispatcher picks one based on cfg.SessionMine.Mode. Keeping both
// paths live lets users compare per-session output before they flip
// the cron over.

// envelopeCorrectiveSuffix is the corrective prompt appended on retry
// when the first envelope call fails to parse. Mirrors the pass-2
// retry at sync.go (Phase 4B). Local models occasionally wrap the
// envelope in prose or code fences on the first attempt; the
// imperative re-instruction recovers most of those without burning
// much extra wallclock.
const envelopeCorrectiveSuffix = "\n\n## CORRECTION\n\nYour previous response could not be parsed as a JSON envelope. Output ONLY one JSON object matching the WikiActionEnvelope shape described above. No prose. No markdown fences. No explanation. The object is the entire response.\n"

// runSessionEnvelopeOnce is one call of the envelope-mode mine.
// Returns the parsed envelope or an error. The caller decides
// whether to retry, fall back, or accept the partial result.
//
// promptBase is the loadPrompt key ("session-mine" or
// "session-extract"); promptForProvider picks the per-provider
// variant. Caller-supplied vars (SESSION_ID, RELATED_SESSIONS, …)
// are merged with the orchestrator-supplied TRANSCRIPT + TODAY.
//
// opLabel feeds the cost ledger so batch ("session-mine") vs
// large-serial ("session-extract-large") rows stay distinct in
// `scribe cost` output — the legacy tools path already drew this
// distinction and the envelope path needs to preserve it.
//
// corrective=true appends envelopeCorrectiveSuffix to the prompt so
// retries get a sharper "OUTPUT ONLY JSON" instruction rather than
// the identical input the first attempt failed on.
func runSessionEnvelopeOnce(parent context.Context, provider llmProviderGenerator, opLabel, promptBase, projectPath, sessionID, relatedSessions, transcript string, extraVars map[string]string, timeout time.Duration, corrective bool) (WikiActionEnvelope, error) {
	vars := map[string]string{
		"KB_DIR":           mustKBRoot(),
		"SESSION_ID":       sessionID,
		"SESSION_ID_LIST":  sessionID,
		"PROJECT_PATH":     projectPath,
		"RELATED_SESSIONS": relatedSessions,
		"TRANSCRIPT":       transcript,
		"TODAY":            time.Now().UTC().Format("2006-01-02"),
	}
	for k, v := range extraVars {
		vars[k] = v
	}
	promptName := promptForProvider(promptBase, providerNameFor(provider))
	prompt, err := loadPrompt(promptName, vars)
	if err != nil {
		return WikiActionEnvelope{}, fmt.Errorf("load %s prompt: %w", promptBase, err)
	}
	if corrective {
		prompt += envelopeCorrectiveSuffix
	}
	callCtx, cancel := context.WithTimeout(withOpLabel(parent, opLabel), timeout)
	defer cancel()
	out, err := generateMaybeJSON(callCtx, provider, prompt)
	if err != nil {
		return WikiActionEnvelope{}, err
	}
	jsonText, ok := extractJSON(out)
	if !ok {
		return WikiActionEnvelope{}, fmt.Errorf("no JSON envelope in provider output (%d bytes)", len(out))
	}
	// session-mine envelopes can legally have an empty actions list
	// (no extractable knowledge), so parseEnvelopeV2 (which routes
	// through parseEnvelopeAllowEmpty) is the right parser. V1
	// envelopes still load; the version check only logs.
	env, err := parseEnvelopeV2(jsonText, opLabel)
	if err != nil {
		return WikiActionEnvelope{}, err
	}
	return env, nil
}

// providerNameFor returns "anthropic" or "ollama" so prompt selection
// can branch even though llmProviderGenerator doesn't expose the
// provider kind directly. Uses a type assertion against the concrete
// provider type rather than string-parsing Name() — a future tweak to
// the Name() format won't silently break prompt selection.
func providerNameFor(p llmProviderGenerator) string {
	if p == nil {
		return "anthropic"
	}
	if _, ok := p.(*ollamaProvider); ok {
		return "ollama"
	}
	return "anthropic"
}

// fetchAndRenderTranscript pulls a transcript via the ccrider DB and
// renders it into a flat string the prompt can inline. Returns "" on
// any error (the caller should treat empty as "skip this session").
func fetchAndRenderTranscript(dbPath, sessionID string, maxChars int) string {
	turns, err := fetchSessionTranscript(dbPath, sessionID)
	if err != nil {
		return ""
	}
	if len(turns) == 0 {
		return ""
	}
	return renderTranscriptForPrompt(turns, maxChars)
}

// mineSessionEnvelope runs one session through the envelope path:
// fetch transcript → render → call LLM → apply envelope. Returns
// (rateLimited, err). nil err and rateLimited=false means success
// (including the legal "no actions" outcome — sessions_log_append
// from the meta block still records the session as processed).
//
// opLabel feeds the cost ledger so the caller's batch label
// ("session-mine" vs "session-extract-large") shows up in
// `scribe cost` rollups — without this, every envelope-mode mining
// row collapses into a single "session-mine" bucket and the parallel
// vs serial split is lost. promptBase picks the prompt family
// (typically the same as opLabel's base).
//
// The Ollama num_ctx for this call is tagged onto the context here
// (cfg.NumCtx, default 16384). Bigger transcripts need a bigger
// num_ctx; without it Ollama silently truncates the tail.
func mineSessionEnvelope(ctx context.Context, root, dbPath, sessionID string, provider llmProviderGenerator, cfg SessionMineConfig, opLabel, promptBase string) (bool, error) {
	ctx = withOllamaNumCtx(ctx, cfg.NumCtx)
	related := queryRelatedSessions(dbPath, sessionID, 7, 10)
	transcript := fetchAndRenderTranscript(dbPath, sessionID, cfg.TranscriptMaxChars)
	if transcript == "" {
		// Empty / unreadable transcript: record the session as
		// processed via a direct meta apply so the next run doesn't
		// re-queue it. Skip the LLM entirely.
		_ = applyMetaAction(root, MetaAction{Op: "sessions_log_append", SessionID: sessionID}, ApplyOptions{})
		return false, nil
	}
	projectPath := lookupSessionProjectPath(dbPath, sessionID)
	timeout := time.Duration(cfg.TimeoutMin) * time.Minute
	env, err := runSessionEnvelopeOnce(ctx, provider, opLabel, promptBase, projectPath, sessionID, formatRelatedSessions(related), transcript, nil, timeout, false)
	if err != nil {
		if errors.Is(err, ErrRateLimit) || errors.Is(err, ErrDailyBudgetExhausted) {
			return true, err
		}
		// One-shot corrective retry on parse failure (same shape as
		// pass-2 envelope at sync.go:1851). Anthropic rarely needs it;
		// local models occasionally wrap the envelope in prose or
		// markdown fences. The corrective suffix sharpens the
		// "OUTPUT ONLY JSON" instruction so the retry isn't an
		// identical-prompt do-over.
		logMsg("sync", "%s envelope %s: first attempt failed (%v) — retrying with corrective prompt", opLabel, sessionID, err)
		env, err = runSessionEnvelopeOnce(ctx, provider, opLabel, promptBase, projectPath, sessionID, formatRelatedSessions(related), transcript, nil, timeout, true)
		if err != nil {
			return false, err
		}
	}
	// Session-mine writes entities distilled from a transcript — a different
	// source than any existing curated doc, so it must not overwrite one. A
	// session referencing the ahrefs studies overwrote that 14-study hub with
	// a session-grounded stub on 2026-06-03; entityWriterApplyOptions keeps
	// create create-if-absent and protects provenance frontmatter.
	res, applyErr := applyWikiActions(root, env, entityWriterApplyOptions())
	if applyErr != nil {
		return false, applyErr
	}
	if len(res.Errors) > 0 {
		logMsg("sync", "%s envelope %s: %d applied, %d errors: %v", opLabel, sessionID, len(res.Applied), len(res.Errors), res.Errors)
	} else {
		logMsg("sync", "%s envelope %s: applied %d action(s)", opLabel, sessionID, len(res.Applied))
	}
	// Belt-and-suspenders: even if the model forgot the
	// sessions_log_append meta op, we record the session as processed
	// so the next run doesn't re-queue it. applyMetaAction is
	// idempotent — re-recording an existing entry just updates the
	// timestamp.
	if !envelopeIncludesSessionLog(env, sessionID) {
		_ = applyMetaAction(root, MetaAction{Op: "sessions_log_append", SessionID: sessionID}, ApplyOptions{})
	}
	return false, nil
}

// promptBaseForSessionLabel maps a runtime op label like
// "session-extract-large" or "session-mine" back to the prompt-base
// stem used by promptForProvider. Keeps prompt selection independent
// of the per-batch label (which carries size/parallelism semantics
// the prompt doesn't care about).
func promptBaseForSessionLabel(label string) string {
	switch {
	case strings.HasPrefix(label, "session-extract"):
		return "session-extract"
	case strings.HasPrefix(label, "session-mine"):
		return "session-mine"
	default:
		return "session-mine"
	}
}

// envelopeIncludesSessionLog reports whether the envelope contains a
// sessions_log_append meta op for the target session id. Used by the
// belt-and-suspenders guard above.
func envelopeIncludesSessionLog(env WikiActionEnvelope, sessionID string) bool {
	for _, m := range env.Meta {
		if m.Op == "sessions_log_append" && (m.SessionID == sessionID || m.SessionID == "") {
			return true
		}
	}
	return false
}

// lookupSessionProjectPath returns the project_path column for a
// session, or "" on error. Used to populate the prompt's
// {{PROJECT_PATH}} variable so the model has the same orientation
// info the legacy MCP path got from get_session_messages.
func lookupSessionProjectPath(dbPath, sessionID string) string {
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		return ""
	}
	defer db.Close()
	var projectPath string
	//nolint:noctx // CLI top-level
	_ = db.QueryRow("SELECT COALESCE(project_path, '') FROM sessions WHERE session_id = ?", sessionID).Scan(&projectPath)
	return projectPath
}

// mineSessionBatchesEnvelope is the Phase 4C top-level driver. It
// fans out across sessionIDs with bounded parallelism, fetching each
// transcript in Go and calling the LLM with the V2 envelope prompt.
// Returns (totalMined, rateLimited) — same shape the legacy
// mineSessionBatches returns, so the dispatcher swap is transparent
// to mineSessions.
//
// Concurrency: parallel applies the same way as the tools path, but
// without claude-p's process overhead each session is mostly LLM
// wallclock. Ollama serializes through the GPU anyway, so parallel=1
// is a reasonable default for local providers; the user controls it
// via sync.parallel_extractions.
func (s *SyncCmd) mineSessionBatchesEnvelope(root string, sessionIDs []string, parallel int, label string, cfg *ScribeConfig) (int, bool) {
	if len(sessionIDs) == 0 {
		return 0, false
	}
	if parallel < 1 {
		parallel = 1
	}
	provider := newLLMProvider(cfg.SessionMine.Provider, cfg.SessionMine.Model, cfg.SessionMine.OllamaURL, root)
	dbPath := cfg.CcriderDB
	checkpointInterval := cfg.Sync.CheckpointInterval
	if checkpointInterval <= 0 {
		checkpointInterval = 5
	}

	logMsg("sync", "%s envelope: %d sessions, parallel=%d (provider=%s)", label, len(sessionIDs), parallel, provider.Name())

	type job struct {
		sid   string
		index int
	}
	type res struct {
		sid         string
		ok          bool
		rateLimited bool
		err         error
	}

	in := make(chan job)
	out := make(chan res, len(sessionIDs))

	ctx := context.Background()

	promptBase := promptBaseForSessionLabel(label)
	for range parallel {
		go func() {
			for j := range in {
				rateLimited, err := mineSessionEnvelope(ctx, root, dbPath, j.sid, provider, cfg.SessionMine, label, promptBase)
				r := res{sid: j.sid, ok: err == nil && !rateLimited, err: err, rateLimited: rateLimited}
				out <- r
			}
		}()
	}

	go func() {
		for i, sid := range sessionIDs {
			in <- job{sid: sid, index: i}
		}
		close(in)
	}()

	total := 0
	rateLimited := false
	since := 0
	var batchIDs []string
	for range sessionIDs {
		r := <-out
		if r.rateLimited {
			logMsg("sync", "%s envelope: rate limited on %s — will resume next run", label, r.sid)
			rateLimited = true
			continue
		}
		if r.err != nil {
			logMsg("sync", "%s envelope: %s failed: %v", label, r.sid, r.err)
			continue
		}
		total++
		since++
		batchIDs = append(batchIDs, r.sid)
		logMsg("sync", "%s envelope: %s complete (%d/%d mined)", label, r.sid, total, len(sessionIDs))

		if since >= checkpointInterval && total < len(sessionIDs) {
			since = 0
			if err := s.rebuildAndReindex(root); err != nil {
				logMsg("sync", "checkpoint reindex error: %v", err)
			}
			committed, err := s.commitAndPush(root, fmt.Sprintf("sync: %s checkpoint (%d sessions)", label, total))
			if err != nil {
				logMsg("sync", "checkpoint commit error: %v", err)
			} else if committed {
				recordBatchOutcome(root, label, batchIDs)
				batchIDs = nil
			}
		}
	}
	// Stamp into runStats so writeRunRecord captures envelope-mode
	// session-mine activity. Key naming matches what dream/assess/deep
	// emit so a single jq filter can roll all four orchestrator paths
	// up consistently.
	if runStats == nil {
		runStats = map[string]any{}
	}
	runStats["mode"] = "envelope"
	runStats[label+"_envelope_mined"] = total
	runStats[label+"_envelope_total"] = len(sessionIDs)
	return total, rateLimited
}

// parseEnvelopeAllowEmpty mirrors parseEnvelope but tolerates an
// envelope with `"actions": []`. Used by session-mine where "no
// extractable knowledge" is a valid outcome and we still want the
// meta block (sessions_log_append) to apply.
func parseEnvelopeAllowEmpty(jsonText string) (WikiActionEnvelope, error) {
	env, err := parseEnvelope(jsonText)
	if err == nil {
		return env, nil
	}
	// Re-unmarshal raw to distinguish "no actions" from "invalid
	// JSON". parseEnvelope already rejected on JSON unmarshal
	// errors → fall through to that case below.
	var raw WikiActionEnvelope
	if jerr := json.Unmarshal([]byte(jsonText), &raw); jerr != nil {
		return raw, fmt.Errorf("unmarshal envelope: %w", jerr)
	}
	// Validate per-action shape; tolerate empty actions list.
	for i, a := range raw.Actions {
		if a.Op == "" {
			return raw, fmt.Errorf("action[%d] missing op", i)
		}
		if a.Path == "" {
			return raw, fmt.Errorf("action[%d] missing path", i)
		}
	}
	return raw, nil
}
