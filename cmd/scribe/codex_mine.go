package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// codex_mine.go is the C3 driver: it closes the Codex loop. Discovery
// (0.2.15) finds projects touched only via Codex; the AGENTS.md
// handshake (0.2.17) makes Codex sessions write drop files; this
// distills the Codex *sessions themselves* into the KB — the same
// triage→envelope→wiki treatment ccrider sessions get, with the only
// difference being the transcript source (fetchCodexTranscript instead
// of the ccrider DB). Everything else — provider, prompt family,
// envelope parse, applyWikiActions — is reused from session_mine.
//
// Serial on purpose: Codex session volume is low and the LLM call is
// the cost; the bounded SessionsMax + lookback window + processed-set
// keep a run cheap. Parallelism can be lifted later if it ever
// matters (mirroring mineSessionBatchesEnvelope), but YAGNI for v1.

// mineCodexSessions scans ~/.codex/ rollouts within the lookback
// window, scores each unmined transcript, and runs the session-mine
// envelope on the ones that clear MinScore (up to SessionsMax).
// Returns the number of sessions mined.
//
// Read-only contract (0.2.21): a --dry-run reports intent and writes
// nothing — no envelope, no _codex_sessions_log.json, no wiki edits.
func (s *SyncCmd) mineCodexSessions(root string, cfg *ScribeConfig) (int, error) {
	cc := cfg.Codex
	dir := cfg.CodexSessionsDir
	if dir == "" {
		return 0, nil // Codex not configured — silent no-op
	}

	processed := loadProcessedCodexIDs(root)

	type candidate struct{ path, id, cwd string }
	var cands []candidate
	walkErr := walkCodexRollouts(dir, cc.LookbackHours, func(p string, meta *codexSessionMeta, _ time.Time) {
		if meta == nil || meta.ID == "" || processed[meta.ID] {
			return
		}
		cands = append(cands, candidate{path: p, id: meta.ID, cwd: meta.Cwd})
	})
	if walkErr != nil {
		return 0, fmt.Errorf("walk codex rollouts: %w", walkErr)
	}
	if len(cands) == 0 {
		return 0, nil
	}

	if s.DryRun {
		logMsg("sync", "DRY RUN -- codex mining: %d candidate session(s) within %dh lookback (cap %d)",
			len(cands), cc.LookbackHours, cc.SessionsMax)
		return 0, nil
	}

	provider := newLLMProvider(cfg.SessionMine.Provider, cfg.SessionMine.Model, cfg.SessionMine.OllamaURL, root)
	keywords, weights := cfg.Triage.Resolve()
	timeout := time.Duration(cfg.SessionMine.TimeoutMin) * time.Minute

	logMsg("sync", "codex mining: %d candidate(s), cap %d (provider=%s)", len(cands), cc.SessionsMax, provider.Name())

	mined := 0
	for _, c := range cands {
		if mined >= cc.SessionsMax {
			break
		}

		turns, err := fetchCodexTranscript(c.path)
		if err != nil || len(turns) == 0 {
			// Empty/unreadable: record so we don't re-evaluate it
			// every run. A resumed session writes a new rollout with
			// a fresh id, so this never strands real content.
			_ = markCodexProcessed(root, c.id, c.cwd, "empty or unreadable transcript")
			continue
		}

		rendered := renderTranscriptForPrompt(turns, cfg.SessionMine.TranscriptMaxChars)
		score := scoreText(keywords, weights, rendered)
		if score < cc.MinScore {
			_ = markCodexProcessed(root, c.id, c.cwd,
				fmt.Sprintf("below MinScore (%d < %d)", score, cc.MinScore))
			continue
		}

		ctx := withOllamaNumCtx(context.Background(), cfg.SessionMine.NumCtx)
		env, err := runSessionEnvelopeOnce(ctx, provider, "codex-session", "session-extract",
			c.cwd, c.id, "", rendered, nil, timeout, false)
		if err != nil {
			if errors.Is(err, ErrRateLimit) || errors.Is(err, ErrDailyBudgetExhausted) {
				// Don't mark processed — resume cleanly next run.
				logMsg("sync", "codex mining rate/budget limited on %s — resuming next run", c.id)
				return mined, nil
			}
			// One corrective retry, same posture as mineSessionEnvelope:
			// local models occasionally wrap the envelope in prose.
			logMsg("sync", "codex envelope %s: first attempt failed (%v) — retrying with corrective prompt", c.id, err)
			env, err = runSessionEnvelopeOnce(ctx, provider, "codex-session", "session-extract",
				c.cwd, c.id, "", rendered, nil, timeout, true)
			if err != nil {
				// Leave it unmarked so a future run retries — a
				// transient model failure shouldn't lose the session.
				logMsg("sync", "codex envelope %s failed after retry: %v", c.id, err)
				continue
			}
		}

		res, applyErr := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true, SanitizeContent: true})
		if applyErr != nil {
			logMsg("sync", "codex envelope %s apply error: %v", c.id, applyErr)
			continue
		}
		if len(res.Errors) > 0 {
			logMsg("sync", "codex envelope %s: applied %d action(s), %d error(s): %v",
				c.id, len(res.Applied), len(res.Errors), res.Errors)
		} else {
			logMsg("sync", "codex envelope %s: applied %d action(s)", c.id, len(res.Applied))
		}
		_ = markCodexProcessed(root, c.id, c.cwd, "")
		mined++
	}

	logMsg("sync", "codex mining done: mined %d session(s)", mined)
	return mined, nil
}
