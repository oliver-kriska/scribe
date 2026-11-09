package main

import (
	"context"
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

// envelopeEmptyCheckSuffix is appended to the one retry a session gets
// when the model returned an envelope with no actions. Sessions reach the
// miner only after triage scored them as knowledge-rich, and an empty
// reply is usually the cheap way out, not a reading of the transcript:
// every scriptorium session re-tested after an empty mine yielded 2-4
// articles. A session that truly has nothing still comes back empty.
const envelopeEmptyCheckSuffix = "\n\n## CHECK\n\nYour previous response extracted nothing from this session. Sessions reach you only after a triage pass scored them as knowledge-rich. Re-read the transcript: if it shows any decision with its reasoning, lesson, failure trace, measured result or reusable pattern, extract it now. If it truly has none, return the envelope again with an empty actions list.\n"

// sessionEnvelopeSchema constrains session-mine replies on schema-capable
// hosted providers. Unlike pass-2, an empty actions list is legal (a
// session can hold nothing reusable), so the guard against the "{}"
// escape is meta minItems:1: the sessions_log_append op every reply must
// carry. Providers without json_schema degrade to json_object or plain
// text; parseEnvelopeV2's bare-empty check covers them.
//
// domains, when non-empty, becomes an enum on meta[].domain: the KB's
// allow-list, which applyMetaRollingAppend enforces. Without it the model
// guessed ("writing", "claude-code") or left the field out, and the
// rolling_memory_append op of most sessions was dropped.
func sessionEnvelopeSchema(domains []string) jsonSchemaSpec {
	domain := map[string]any{"type": "string"}
	if len(domains) > 0 {
		enum := make([]any, len(domains))
		for i, d := range domains {
			enum[i] = d
		}
		domain["enum"] = enum
	}
	return jsonSchemaSpec{
		Name: "WikiActionEnvelope",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"version": map[string]any{"type": "integer"},
				"entity":  map[string]any{"type": "string"},
				"actions": map[string]any{
					"type":  "array",
					"items": wikiActionItemSchema(),
				},
				"meta": map[string]any{
					"type":     "array",
					"minItems": 1,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"op":         map[string]any{"type": "string"},
							"session_id": map[string]any{"type": "string"},
							"domain":     domain,
							"target":     map[string]any{"type": "string"},
							"content":    map[string]any{"type": "string"},
						},
						"required": []any{"op"},
					},
				},
			},
			"required": []any{"version", "entity", "actions", "meta"},
		},
	}
}

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
// suffix is appended to the prompt on a retry (envelopeCorrectiveSuffix
// after a parse failure, envelopeEmptyCheckSuffix after an empty reply),
// so the retry is never the identical input the first attempt failed on.
func runSessionEnvelopeOnce(parent context.Context, provider llmProviderGenerator, opLabel, promptBase, projectPath, sessionID, relatedSessions, transcript string, extraVars map[string]string, timeout time.Duration, suffix string) (WikiActionEnvelope, error) {
	root := mustKBRoot()
	domains := kbDomainList(root)
	vars := map[string]string{
		"KB_DIR":           root,
		"DOMAINS":          strings.Join(domains, ", "),
		"SESSION_ID":       sessionID,
		"SESSION_ID_LIST":  sessionID,
		"PROJECT_PATH":     projectPath,
		"RELATED_SESSIONS": relatedSessions,
		"TRANSCRIPT":       transcript,
		"TODAY":            time.Now().UTC().Format("2006-01-02"),
		// Filled only on a re-mine (envelopeTranscript); empty keeps the
		// placeholder guard quiet on first mines.
		"CONTINUATION": "",
	}
	for k, v := range extraVars {
		vars[k] = v
	}
	promptName := promptForProvider(promptBase, providerNameFor(provider))
	prompt, err := loadPrompt(promptName, vars)
	if err != nil {
		return WikiActionEnvelope{}, fmt.Errorf("load %s prompt: %w", promptBase, err)
	}
	prompt += suffix
	callCtx, cancel := context.WithTimeout(withOpLabel(parent, opLabel), timeout)
	defer cancel()
	out, err := generateWithSchema(callCtx, provider, prompt, sessionEnvelopeSchema(domains))
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

// runSessionEnvelopeWithRetry wraps runSessionEnvelopeOnce in the two
// retries every session miner gets: one corrective retry when the reply
// does not parse (including the bare "{}"), and one check retry when it
// parses with no actions. A rate-limit or budget error is returned at
// once so the caller leaves the session queued. A failed check retry
// keeps the first, empty answer.
func runSessionEnvelopeWithRetry(ctx context.Context, provider llmProviderGenerator, opLabel, promptBase, projectPath, sessionID, relatedSessions, transcript string, extraVars map[string]string, timeout time.Duration) (WikiActionEnvelope, error) {
	once := func(suffix string) (WikiActionEnvelope, error) {
		return runSessionEnvelopeOnce(ctx, provider, opLabel, promptBase, projectPath, sessionID, relatedSessions, transcript, extraVars, timeout, suffix)
	}
	env, err := once("")
	if err != nil {
		if isBudgetStop(err) {
			return env, err
		}
		logMsg("sync", "%s envelope %s: first attempt failed (%v) — retrying with corrective prompt", opLabel, sessionID, err)
		addRunStat("session_envelope_corrective_retries", 1)
		if env, err = once(envelopeCorrectiveSuffix); err != nil {
			return env, err
		}
	}
	if len(env.Actions) > 0 {
		return env, nil
	}
	if opLabel == remineLabel {
		// A delta past the watermark often holds nothing new, and the
		// check suffix ("sessions reach you only after triage scored
		// them knowledge-rich… extract it now") would push the model to
		// re-extract what earlier mines already wrote.
		return env, nil
	}
	logMsg("sync", "%s envelope %s: no actions — retrying once with an extraction check", opLabel, sessionID)
	addRunStat("session_envelope_empty_retries", 1)
	checked, err := once(envelopeEmptyCheckSuffix)
	switch {
	case err != nil && isBudgetStop(err):
		return checked, err
	case err != nil:
		logMsg("sync", "%s envelope %s: check retry failed (%v) — keeping the empty answer", opLabel, sessionID, err)
		return env, nil
	case len(checked.Actions) > 0:
		addRunStat("session_envelope_empty_recovered", 1)
	}
	return checked, nil
}

func isBudgetStop(err error) bool {
	return errors.Is(err, ErrRateLimit) || errors.Is(err, ErrDailyBudgetExhausted)
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
	transcript, continuation, mark := envelopeTranscript(root, dbPath, sessionID, cfg.TranscriptMaxChars)
	if transcript == "" {
		// Empty / unreadable transcript (or nothing new past the
		// watermark): record the session as processed so the next run
		// doesn't re-queue it. Skip the LLM entirely.
		recordSessionProcessed(root, sessionID, mark)
		return false, nil
	}
	projectPath := lookupSessionProjectPath(dbPath, sessionID)
	timeout := time.Duration(cfg.TimeoutMin) * time.Minute
	env, err := runSessionEnvelopeWithRetry(ctx, provider, opLabel, promptBase, projectPath, sessionID, formatRelatedSessions(related), transcript, map[string]string{"CONTINUATION": continuation}, timeout)
	if err != nil {
		return isBudgetStop(err), err
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
	// Go owns wiki/_sessions_log.json: the model's sessions_log_append is
	// accepted but never applied (see applyMetaAction), so the session
	// that was actually mined is recorded here, unconditionally.
	recordSessionProcessed(root, sessionID, mark)
	return false, nil
}

// envelopeTranscript returns the transcript to inline, the continuation
// block for the prompt's {{CONTINUATION}} slot ("" on a first mine), and
// the watermark the transcript reaches. A session with no watermark is a
// first mine: the whole transcript, head/tail-truncated as before, with
// the watermark at the end of the head half when the middle was dropped.
// A session with a watermark is a re-mine: only the turns after it, as a
// prefix that fits the cap (no middle drop — what does not fit stays for
// the next run).
func envelopeTranscript(root, dbPath, sessionID string, maxChars int) (transcript, continuation string, mark mineMark) {
	view, err := loadSessionsLogView(root)
	if err != nil {
		// Without the log we cannot tell a first mine from a re-mine,
		// and guessing "first" would re-mine a whole session. Skip; the
		// caller records nothing new (a zero mark never lowers an entry).
		logMsg("sync", "sessions log unreadable, skipping %s: %v", sessionID, err)
		return "", "", mineMark{}
	}
	prev, remine := entryThroughSeq(view.processed[sessionID])
	turns, err := fetchSessionTranscript(dbPath, sessionID)
	if err != nil {
		// Unreadable: same outcome as before watermarks — the caller
		// records the session and skips the LLM.
		logMsg("sync", "transcript fetch %s: %v", sessionID, err)
		return "", "", mineMark{ThroughSeq: prev}
	}
	if !remine {
		return renderTranscriptForPrompt(turns, maxChars), "", renderedThroughSeq(turns, maxChars)
	}
	delta := turnsAfter(turns, prev)
	if len(delta) == 0 {
		return "", "", mineMark{ThroughSeq: prev}
	}
	continuation = remineContext(firstUserText(turns), pagesFromSession(root, sessionID))
	body, mark := renderTranscriptPrefix(delta, max(maxChars-len(continuation), maxChars/2))
	return body, continuation, mark
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

// lookupSessionProjectPath returns the project_path column for a
// session, or "" on error. Used to populate the prompt's
// {{PROJECT_PATH}} variable so the model has the same orientation
// info the legacy MCP path got from get_session_messages.
func lookupSessionProjectPath(dbPath, sessionID string) string {
	db, err := openSQLiteRO(dbPath)
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
			logPhaseDegraded("sync", "session mining", "%s envelope: %s failed: %v", label, r.sid, r.err)
			continue
		}
		total++
		since++
		batchIDs = append(batchIDs, r.sid)
		logMsg("sync", "%s envelope: %s complete (%d/%d mined)", label, r.sid, total, len(sessionIDs))

		if since >= checkpointInterval && total < len(sessionIDs) {
			since = 0
			if err := s.rebuildAndReindex(root); err != nil {
				logPhaseFailure("sync", "checkpoint reindex", err)
			}
			committed, err := s.commitAndPush(root, fmt.Sprintf("sync: %s checkpoint (%d sessions)", label, total))
			if err != nil {
				logPhaseFailure("sync", "checkpoint commit", err)
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
	setRunStat("mode", "envelope")
	setRunStat(label+"_envelope_mined", total)
	setRunStat(label+"_envelope_total", len(sessionIDs))
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
