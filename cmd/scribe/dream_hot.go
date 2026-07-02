// dream_hot.go — the daily hot-domain mini consolidation (issue #24).
//
// scribe dream --hot auto-selects the single most-touched domain since
// the last real consolidation (full or hot) and runs a small, bounded
// EnvelopeV2 LLM pass scoped to just that domain — the same orchestrator
// machinery the weekly dream uses (dream_orchestrator.go), just filtered
// to one domain and gated so it stays cheap and self-scheduling. See
// docs/issue-24-hot-domain-consolidation-plan.md for the full design.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// hotDomainTouchCounts tallies, per domain, how many distinct articles
// under wikiDirs were added or modified in git history since `since`.
// Deletions and renames-as-D+A don't count as "churn to consolidate."
// Each touched file counts once no matter how many times it was
// committed in the window — otherwise an hourly auto-commit habit on
// one article would dominate the signal (same dedupe shape as
// gitDigestActivity's fileSet pattern in digest.go).
//
// Only domains present in cfg.AllDomains() are tallied; a touched file
// whose frontmatter domain is empty or unrecognized is excluded
// entirely, not bucketed under a catch-all.
func hotDomainTouchCounts(root string, cfg *ScribeConfig, since time.Time) map[string]int {
	args := make([]string, 0, 6+len(wikiDirs))
	args = append(args,
		"log", "--since="+since.Format(time.RFC3339),
		"--pretty=format:", "--name-status", "--no-renames", "--")
	args = append(args, wikiDirs...)
	out := runCmd(root, "git", args...)
	if out == "" {
		return map[string]int{}
	}

	allowed := map[string]bool{}
	for _, d := range cfg.AllDomains() {
		allowed[d] = true
	}

	touched := map[string]bool{}
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		status, path, ok := strings.Cut(line, "\t")
		if !ok || !strings.HasSuffix(path, ".md") {
			continue
		}
		if strings.HasPrefix(filepath.Base(path), "_") {
			continue
		}
		// Only added/modified count as churn to consolidate.
		if !strings.HasPrefix(status, "A") && !strings.HasPrefix(status, "M") {
			continue
		}
		touched[path] = true
	}

	counts := map[string]int{}
	for path := range touched {
		domain := articleDomain(root, path)
		if domain == "" || !allowed[domain] {
			continue
		}
		counts[domain]++
	}
	return counts
}

// selectHotDomain picks the domain with the highest touch count, ties
// broken alphabetically for determinism. ok is false when the winning
// count is below minTouches — "no meaningful churn."
func selectHotDomain(counts map[string]int, minTouches int) (domain string, touches int, ok bool) {
	domains := make([]string, 0, len(counts))
	for d := range counts {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	best := ""
	bestCount := -1
	for _, d := range domains {
		if counts[d] > bestCount {
			best = d
			bestCount = counts[d]
		}
	}
	if best == "" || bestCount < minTouches {
		return "", bestCount, false
	}
	return best, bestCount, true
}

// dreamRunHistory scans output/runs/*.jsonl for the newest successful
// `dream` invocation of each kind: lastFull is the newest run whose args
// did NOT include --hot; lastHot is the newest --hot run that actually
// reached consolidation (runStats["mode"] == "hot" was merged into the
// row — see runHotDream). A self-gated --hot skip never sets that field,
// so it correctly does not advance lastHot — see
// docs/issue-24-hot-domain-consolidation-plan.md §3 for why that matters
// (without this filter, a skip would advance the churn anchor and the
// domain could never cross the threshold).
func dreamRunHistory(root string) (lastFull, lastHot time.Time) {
	runsDir := filepath.Join(root, "output", "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return time.Time{}, time.Time{}
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(runsDir, e.Name()))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			var row map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
				continue
			}
			if s, _ := row["status"].(string); s != "ok" {
				continue
			}
			if c, _ := row["command"].(string); c != "dream" {
				continue
			}
			ts, err := time.Parse(time.RFC3339, fmt.Sprint(row["timestamp"]))
			if err != nil {
				continue
			}
			isHotInvocation := false
			if argsAny, ok := row["args"].([]any); ok {
				for _, a := range argsAny {
					if s, _ := a.(string); s == "--hot" {
						isHotInvocation = true
						break
					}
				}
			}
			if !isHotInvocation {
				if ts.After(lastFull) {
					lastFull = ts
				}
				continue
			}
			if mode, _ := row["mode"].(string); mode == "hot" && ts.After(lastHot) {
				lastHot = ts
			}
		}
		_ = f.Close()
	}
	return lastFull, lastHot
}

// hotDomainSince returns the anchor for the churn-count git-log window:
// the more recent of the last successful full dream and the last
// successful hot dream that actually did work, falling back to
// now - cfg.Dream.HotLookbackDays when neither has ever run.
func hotDomainSince(cfg *ScribeConfig, lastFull, lastHot, now time.Time) time.Time {
	anchor := lastFull
	if lastHot.After(anchor) {
		anchor = lastHot
	}
	if anchor.IsZero() {
		return now.AddDate(0, 0, -cfg.Dream.HotLookbackDays)
	}
	return anchor
}

// hotSkipIfFullWithin is gate 1 (§2.3): the hot pass must not run when
// the full weekly dream just covered the whole KB. 24h matches the
// daily hot cadence exactly.
const hotSkipIfFullWithin = 24 * time.Hour

// runHotDream is the `scribe dream --hot` entry point. It auto-selects
// the most-touched domain since the last real consolidation (or uses
// domainOverride when set), self-gates when there's nothing to do, and
// — once past both gates — runs the same envelope machinery the full
// dream orchestrator uses, scoped to one domain, then shares the full
// dream's post-LLM commit tail via commitDreamCycle.
func runHotDream(root string, cfg *ScribeConfig, domainOverride string, dryRun bool) error {
	now := time.Now()
	lastFull, lastHot := dreamRunHistory(root)

	var domain string
	var touches int
	if domainOverride != "" {
		// --domain bypasses BOTH self-gating checks — it's an explicit
		// human/cron override. It still goes through the lock, the team
		// lease, and the budget ceiling below; those are hard safety
		// constraints, never bypassable by a flag.
		domain = domainOverride
		touches = -1
	} else {
		if !lastFull.IsZero() && now.Sub(lastFull) < hotSkipIfFullWithin {
			logMsg("dream", "hot: full dream ran %s ago — skipping (weekly cycle just covered the whole KB)", shortDuration(now.Sub(lastFull)))
			return nil
		}
		since := hotDomainSince(cfg, lastFull, lastHot, now)
		counts := hotDomainTouchCounts(root, cfg, since)
		d, t, ok := selectHotDomain(counts, cfg.Dream.HotMinTouches)
		if !ok {
			logMsg("dream", "hot: no domain crossed the churn threshold (%d touches) since %s — nothing to do", cfg.Dream.HotMinTouches, since.Format("2006-01-02"))
			return nil
		}
		domain, touches = d, t
	}

	logMsg("dream", "hot: selected domain=%s touches=%d", domain, touches)
	if dryRun {
		logMsg("dream", "DRY RUN — would run hot-domain consolidation on domain=%s", domain)
		return nil
	}

	// Same lock name ("dream") and same team lease as the full dream —
	// duplicated here rather than shared via a helper so the working
	// full-dream lock/lease block in dream.go stays untouched. A hot
	// pass and a full dream can therefore never run concurrently on one
	// machine, and on a team KB only one machine's dream activity (full
	// or hot) proceeds at a time.
	lockPath := lockPathFor(cfg.LockDir, "dream", root)
	lf, ok, lerr := acquireLock(lockPath)
	if lerr != nil {
		return fmt.Errorf("lock %s: %w", lockPath, lerr)
	}
	if !ok {
		logMsg("dream", "hot: another dream cycle is running — exiting")
		return nil
	}
	defer releaseLock(lf)

	if cfg.Team {
		acquired, holder := acquireDreamLease(root, now)
		if !acquired {
			logMsg("dream", "hot: dream lease held by %s — skipping this cycle", holder)
			return nil
		}
		defer releaseDreamLease(root)
	}

	today := now.Format("2006-01-02")
	preCount := countArticles(root)
	ctx := context.Background()

	logTail := dreamReadLogTail(root, 20)
	inventory := dreamSampleInventory(root, domain, 40)
	stale := dreamStaleCandidates(root, domain, 60)
	contradictions := dreamContradictionCandidates(root, domain)

	provider := newLLMProvider(cfg.Dream.Provider, cfg.Dream.Model, cfg.Dream.OllamaURL, root)
	promptName := promptForProvider("dream-hot", providerNameFor(provider))
	prompt, err := loadPrompt(promptName, map[string]string{
		"KB_DIR":         root,
		"TODAY":          today,
		"DOMAIN":         domain,
		"LOG_TAIL":       logTail,
		"INVENTORY":      inventory,
		"STALE":          stale,
		"CONTRADICTIONS": contradictions,
	})
	if err != nil {
		return fmt.Errorf("load dream-hot prompt: %w", err)
	}

	timeout := time.Duration(cfg.Dream.TimeoutMin) * time.Minute
	tagged := withOllamaNumCtx(withOpLabel(ctx, "dream-hot"), cfg.Dream.NumCtx)
	callCtx, cancel := context.WithTimeout(tagged, timeout)
	defer cancel()

	// Stamp mode/domain BEFORE the LLM call: this is the "real work
	// started" marker dreamRunHistory keys off. A skip above never
	// reaches this line, so a self-gated --hot invocation never
	// advances lastHot (see dreamRunHistory's doc comment).
	if runStats == nil {
		runStats = map[string]any{}
	}
	runStats["mode"] = "hot"
	runStats["hot_domain"] = domain
	runStats["hot_domain_touches"] = touches

	out, err := generateMaybeJSON(callCtx, provider, prompt)
	if err != nil {
		if errors.Is(err, ErrRateLimit) || errors.Is(err, ErrDailyBudgetExhausted) {
			// This happens AFTER runStats["mode"]="hot" was set, so a
			// rate-limited hot run still advances lastHot next time
			// dreamRunHistory runs — intentional, mirrors the full
			// dream's identical rate-limit handling (dream.go): "we
			// tried and got rate-limited" is treated the same as "we
			// tried and found nothing to change."
			logMsg("dream", "hot: rate limited / budget exhausted — cycle interrupted (%v)", err)
			return nil
		}
		return fmt.Errorf("dream-hot LLM call: %w", err)
	}
	jsonText, ok := extractJSON(out)
	if !ok {
		return fmt.Errorf("dream-hot: no JSON envelope in provider output (%d bytes)", len(out))
	}
	env, err := parseEnvelopeV2(jsonText, "dream-hot")
	if err != nil {
		return fmt.Errorf("dream-hot: parse envelope: %w", err)
	}
	res, err := applyWikiActions(root, env, entityWriterApplyOptions())
	if err != nil {
		return fmt.Errorf("dream-hot: apply actions: %w", err)
	}
	runStats["envelope_actions_applied"] = len(res.Applied)
	runStats["envelope_actions_errored"] = len(res.Errors)
	if len(res.Errors) > 0 {
		logMsg("dream", "hot: envelope: %d applied, %d errors: %v", len(res.Applied), len(res.Errors), res.Errors)
	} else {
		logMsg("dream", "hot: envelope: applied %d action(s)", len(res.Applied))
	}

	return commitDreamCycle(root, today, "dream-hot domain="+domain, preCount)
}
