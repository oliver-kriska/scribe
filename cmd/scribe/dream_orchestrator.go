package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// dream_orchestrator.go is the Phase 4D port of the weekly Dream
// cycle. The legacy path runs one `claude -p` for up to an hour with
// Read/Write/Edit/Glob/Grep tools; the orchestrator splits Dream into
// pure-Go phases (1 = orient, 4 = prune+index) and a single bounded
// LLM subtask covering 2.5/3/3.5 (contradictions, consolidation,
// stubs) via the V2 envelope.
//
// The Go side does the file walking, date math, link-graph analysis,
// and stale-candidate flagging. The LLM only sees a compact orient
// packet (recent log + sampled inventory + stale list + contradiction
// pairs) and emits one envelope.

// runDreamOrchestrator is the Phase 4D entry point. Called from
// DreamCmd.Run when cfg.Dream.Mode == "orchestrator". Returns the
// envelope result (or nil envelope if the cycle was a no-op).
func runDreamOrchestrator(ctx context.Context, root string, cfg *ScribeConfig, today string) error {
	logTail := dreamReadLogTail(root, 20)
	focus := dreamFocusPairs(root, "", dreamFocusMaxPairs, time.Now())
	inventory := dreamFocusInventory(root, "", focus, 40)
	stale := dreamStaleCandidates(root, "", 60)

	provider := newLLMProvider(cfg.Dream.Provider, cfg.Dream.Model, cfg.Dream.OllamaURL, root)
	promptName := promptForProvider("dream", providerNameFor(provider))
	prompt, err := loadPrompt(promptName, map[string]string{
		"KB_DIR":    root,
		"TODAY":     today,
		"LOG_TAIL":  logTail,
		"INVENTORY": inventory,
		"STALE":     stale,
		"FOCUS":     renderDreamFocus(focus),
	})
	if err != nil {
		return fmt.Errorf("load dream prompt: %w", err)
	}
	timeout := time.Duration(cfg.Dream.TimeoutMin) * time.Minute
	tagged := withOllamaNumCtx(withOpLabel(ctx, "dream"), cfg.Dream.NumCtx)
	callCtx, cancel := context.WithTimeout(tagged, timeout)
	defer cancel()
	out, err := generateMaybeJSON(callCtx, provider, prompt)
	if err != nil {
		if errors.Is(err, ErrRateLimit) || errors.Is(err, ErrDailyBudgetExhausted) {
			logMsg("dream", "rate limited / budget exhausted — cycle interrupted (%v)", err)
			return nil
		}
		return fmt.Errorf("dream LLM call: %w", err)
	}
	jsonText, ok := extractJSON(out)
	if !ok {
		return fmt.Errorf("dream: no JSON envelope in provider output (%d bytes)", len(out))
	}
	env, err := parseEnvelopeV2(jsonText, "dream")
	if err != nil {
		return fmt.Errorf("dream: parse envelope: %w", err)
	}
	// Dream runs the blindest of all: the orient packet above is article
	// *metadata* (path/title/type/updated), never a body, so any whole-file
	// content it emits is invented. entityWriterApplyOptions keeps
	// AllowOverwrite off (its only valid create is a new stub) and protects
	// provenance frontmatter. See d06cc70 (0.2.18) for the same incident
	// class on _-prefixed artifacts.
	superseded := guardDreamFocusActions(root, &env, focus)
	res, err := applyWikiActions(root, env, entityWriterApplyOptions())
	if err != nil {
		return fmt.Errorf("dream: apply actions: %w", err)
	}
	markDreamFocusReviewed(root, focus, time.Now())
	if len(res.Errors) > 0 {
		logMsg("dream", "envelope: %d applied, %d errors: %v", len(res.Applied), len(res.Errors), res.Errors)
	} else {
		logMsg("dream", "envelope: applied %d action(s)", len(res.Applied))
	}
	// Stamp orchestrator-only metrics into the run record so
	// `scribe doctor --section freshness` and ad-hoc rollups can
	// tell envelope-mode dreams apart from no-op runs. The legacy
	// monolithic path sets articles_before/after later in dream.go;
	// here we add the envelope-specific counts. maps.Copy in main.go
	// merges these into the JSONL row.
	setRunStat("mode", "orchestrator")
	setRunStat("envelope_actions_applied", len(res.Applied))
	setRunStat("envelope_actions_errored", len(res.Errors))
	setRunStat("envelope_meta_ops", len(env.Meta))
	setRunStat("dream_focus_pairs", len(focus))
	setRunStat("dream_content_actions", dreamContentActions(res.Applied))
	setRunStat("dream_superseded", superseded)
	return nil
}

// dreamReadLogTail returns the last `lines` non-empty lines of
// <root>/log.md. Empty string when the file is absent (first-run
// KBs).
func dreamReadLogTail(root string, lines int) string {
	data, err := os.ReadFile(filepath.Join(root, "log.md"))
	if err != nil {
		return ""
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	out := strings.Join(all, "\n")
	// A line count is no size bound — one pasted stack trace on a
	// single log line is enough to swamp the packet. Keep the newest
	// bytes; they are the ones the consolidation reads.
	if len(out) > dreamLogTailMaxBytes {
		out = "[… earlier log text cut …]\n" + tailBytes(out, dreamLogTailMaxBytes)
	}
	return out
}

// dreamLogTailMaxBytes bounds the log excerpt in the dream packets.
const dreamLogTailMaxBytes = 4096

// dreamArticleSample is one row of the inventory packet.
type dreamArticleSample struct {
	Path       string
	Title      string
	Type       string
	Domain     string
	Confidence string
	Updated    string
}

// dreamSampleInventory walks the wiki dirs and returns up to `maxEntries`
// articles spread across types and directories. The sample biases
// toward recently-updated articles so contradiction triage has fresh
// claims to look at; ancient articles still show up via the stale
// list.
//
// domain, when non-empty, restricts the sample to articles whose
// frontmatter domain matches exactly — used by the hot-domain mini
// consolidation (dream_hot.go) to scope the orient packet to one
// domain. Empty string preserves the full-dream behavior of sampling
// across the whole KB.
func dreamSampleInventory(root, domain string, maxEntries int) string {
	var samples []dreamArticleSample
	_ = walkArticles(root, func(path string, content []byte) error {
		fm, err := parseFrontmatter(content)
		if err != nil || fm == nil {
			return nil //nolint:nilerr // unparseable frontmatter: skip the article, keep sampling
		}
		if domain != "" && fm.Domain != domain {
			return nil
		}
		// Superseded records stay on disk on purpose (Rule 12). Showing them
		// as stale, contradiction or inventory candidates invites the model
		// to decay-mark them or reopen a contradiction already resolved.
		if isSuperseded(fm) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		samples = append(samples, dreamArticleSample{
			Path:       filepath.ToSlash(rel),
			Title:      fm.Title,
			Type:       fm.Type,
			Domain:     fm.Domain,
			Confidence: fm.Confidence,
			// stringFromAny normalises the yaml.v3 time.Time auto-conversion
			// back to a YYYY-MM-DD string. fmt.Sprintf("%v", ...) would
			// produce "2025-09-12 00:00:00 +0000 UTC" — looks like garbage
			// to the LLM.
			Updated: stringFromAny(fm.Updated),
		})
		return nil
	})
	sort.Slice(samples, func(i, j int) bool {
		// Newest first.
		return samples[i].Updated > samples[j].Updated
	})
	if len(samples) > maxEntries {
		samples = samples[:maxEntries]
	}
	var sb strings.Builder
	for _, s := range samples {
		fmt.Fprintf(&sb, "- %s | %s | %s | %s | conf=%s | upd=%s\n", s.Path, s.Title, s.Type, s.Domain, s.Confidence, s.Updated)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// dreamStaleCandidates returns article paths whose `updated:` is more
// than `days` ago. Zero-link analysis is intentionally not done here
// — it would require walking the whole graph; the LLM gets the bare
// list and decides whether to decay-mark.
//
// domain, when non-empty, restricts candidates to that domain (see
// dreamSampleInventory's doc comment for the hot-domain rationale).
func dreamStaleCandidates(root, domain string, days int) string {
	cutoff := time.Now().AddDate(0, 0, -days)
	var paths []string
	_ = walkArticles(root, func(path string, content []byte) error {
		fm, err := parseFrontmatter(content)
		if err != nil || fm == nil {
			return nil //nolint:nilerr // unparseable article: skip it, keep walking
		}
		if domain != "" && fm.Domain != domain {
			return nil
		}
		// Superseded records stay on disk on purpose (Rule 12). Showing them
		// as stale, contradiction or inventory candidates invites the model
		// to decay-mark them or reopen a contradiction already resolved.
		if isSuperseded(fm) {
			return nil
		}
		updated := stringFromAny(fm.Updated)
		if updated == "" {
			return nil
		}
		t, err := time.Parse("2006-01-02", updated)
		if err != nil {
			return nil //nolint:nilerr // unparseable article: skip it, keep walking
		}
		if t.Before(cutoff) {
			rel, _ := filepath.Rel(root, path)
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	})
	if len(paths) > 60 {
		paths = paths[:60]
	}
	return strings.Join(paths, "\n")
}

// stringFromAny is a small helper for frontmatter fields that arrive
// as either string or time.Time. Returns "" for nil / unknown shapes.
func stringFromAny(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case time.Time:
		return t.Format("2006-01-02")
	default:
		// yaml may yield a node — try to encode and trim.
		if v == nil {
			return ""
		}
		b, err := yaml.Marshal(v)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
}
