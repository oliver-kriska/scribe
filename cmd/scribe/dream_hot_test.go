// dream_hot_test.go — tests for the daily hot-domain mini consolidation
// (issue #24): pure gating functions (selectHotDomain, hotDomainSince),
// the run-history JSONL scanner (dreamRunHistory), the git-log churn
// tally (hotDomainTouchCounts), the domain-scoping regression on the
// shared orchestrator samplers, the commitDreamCycle runStats-merge
// fix, config defaults, and full runHotDream integration against the
// stub LLM harness (llm_stub_test.go). See
// docs/issue-24-hot-domain-consolidation-plan.md §5 for the test plan
// this file implements.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- 5.1 pure-function tests ----

func TestSelectHotDomain_PicksHighestCountAboveThreshold(t *testing.T) {
	counts := map[string]int{"tools": 5, "personal": 2}
	domain, touches, ok := selectHotDomain(counts, 3)
	if !ok || domain != "tools" || touches != 5 {
		t.Fatalf("selectHotDomain = (%q, %d, %v), want (tools, 5, true)", domain, touches, ok)
	}
}

func TestSelectHotDomain_BelowThresholdReturnsNotOK(t *testing.T) {
	counts := map[string]int{"tools": 2}
	_, _, ok := selectHotDomain(counts, 3)
	if ok {
		t.Fatal("selectHotDomain: ok = true, want false (below threshold)")
	}
}

func TestSelectHotDomain_TiesBreakAlphabetically(t *testing.T) {
	counts := map[string]int{"tools": 3, "personal": 3}
	domain, touches, ok := selectHotDomain(counts, 3)
	if !ok || domain != "personal" || touches != 3 {
		t.Fatalf("selectHotDomain = (%q, %d, %v), want (personal, 3, true)", domain, touches, ok)
	}
}

func TestSelectHotDomain_EmptyCountsReturnsNotOK(t *testing.T) {
	_, _, ok := selectHotDomain(map[string]int{}, 1)
	if ok {
		t.Fatal("selectHotDomain on empty counts: ok = true, want false")
	}
}

func TestHotDomainSince_UsesMostRecentOfFullOrHot(t *testing.T) {
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	cfg := &ScribeConfig{Dream: DreamConfig{HotLookbackDays: 14}}
	recent := now.AddDate(0, 0, -2)
	older := now.AddDate(0, 0, -5)

	cases := []struct {
		name              string
		lastFull, lastHot time.Time
		want              time.Time
	}{
		{"both zero falls back to lookback", time.Time{}, time.Time{}, now.AddDate(0, 0, -14)},
		{"only full set", recent, time.Time{}, recent},
		{"only hot set", time.Time{}, recent, recent},
		{"both set, hot newer", older, recent, recent},
		{"both set, full newer", recent, older, recent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hotDomainSince(cfg, c.lastFull, c.lastHot, now)
			if !got.Equal(c.want) {
				t.Errorf("hotDomainSince(%v, %v) = %v, want %v", c.lastFull, c.lastHot, got, c.want)
			}
		})
	}
}

// ---- 5.7 config defaults ----

func TestApplyDreamDefaults_HotFieldsDefaultAndAreOverridable(t *testing.T) {
	cfg := DreamConfig{}
	applyDreamDefaults(&cfg, LLMConfig{})
	if cfg.HotMinTouches != 3 {
		t.Errorf("HotMinTouches = %d, want 3", cfg.HotMinTouches)
	}
	if cfg.HotLookbackDays != 14 {
		t.Errorf("HotLookbackDays = %d, want 14", cfg.HotLookbackDays)
	}

	preset := DreamConfig{HotMinTouches: 10, HotLookbackDays: 30}
	applyDreamDefaults(&preset, LLMConfig{})
	if preset.HotMinTouches != 10 {
		t.Errorf("preset HotMinTouches = %d, want 10 (untouched)", preset.HotMinTouches)
	}
	if preset.HotLookbackDays != 30 {
		t.Errorf("preset HotLookbackDays = %d, want 30 (untouched)", preset.HotLookbackDays)
	}
}

// ---- 5.2 dreamRunHistory — synthetic JSONL fixtures, no git needed ----

func TestDreamRunHistory_SplitsFullVsHotAndIgnoresSkippedHotRuns(t *testing.T) {
	root := t.TempDir()
	runsDir := filepath.Join(root, "output", "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		// 1. full dream (no mode field — matches legacy monolithic path).
		`{"command":"dream","status":"ok","timestamp":"2026-07-01T02:00:00Z","args":["dream"]}`,
		// 2. a self-gated --hot skip — no mode field, since runHotDream
		//    never reached the stamp line. Must NOT advance lastHot.
		`{"command":"dream","status":"ok","timestamp":"2026-07-01T03:10:00Z","args":["dream","--hot"]}`,
		// 3. a real hot run.
		`{"command":"dream","status":"ok","timestamp":"2026-07-02T03:10:00Z","args":["dream","--hot"],"mode":"hot","hot_domain":"tools"}`,
		// 4. errored — must be excluded, status != "ok".
		`{"command":"dream","status":"error","timestamp":"2026-07-03T03:10:00Z","args":["dream","--hot"],"mode":"hot"}`,
	}
	if err := os.WriteFile(filepath.Join(runsDir, "2026-07-01.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lastFull, lastHot := dreamRunHistory(root)
	wantFull, _ := time.Parse(time.RFC3339, "2026-07-01T02:00:00Z")
	wantHot, _ := time.Parse(time.RFC3339, "2026-07-02T03:10:00Z")
	if !lastFull.Equal(wantFull) {
		t.Errorf("lastFull = %v, want %v", lastFull, wantFull)
	}
	if !lastHot.Equal(wantHot) {
		t.Errorf("lastHot = %v, want %v (not the skip at 03:10 on day 1, not the errored row on day 3)", lastHot, wantHot)
	}
}

func TestDreamRunHistory_EmptyRunsDirReturnsZeroTimes(t *testing.T) {
	root := t.TempDir()
	lastFull, lastHot := dreamRunHistory(root)
	if !lastFull.IsZero() || !lastHot.IsZero() {
		t.Errorf("dreamRunHistory on fresh KB = (%v, %v), want both zero", lastFull, lastHot)
	}
}

// ---- 5.3 hotDomainTouchCounts — real git fixture ----

func TestHotDomainTouchCounts_DedupesFilesAndBucketsByCurrentDomain(t *testing.T) {
	root := initTestGitRepo(t, "Alice")
	writeKBFile(t, root, "wiki/a.md", "---\ntitle: A\ndomain: tools\n---\n\nbody\n")
	writeKBFile(t, root, "wiki/b.md", "---\ntitle: B\ndomain: personal\n---\n\nbody\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-q", "-m", "seed")
	since := time.Now().Add(-time.Hour)

	// a.md touched twice (should count once), b.md touched once.
	writeKBFile(t, root, "wiki/a.md", "---\ntitle: A\ndomain: tools\n---\n\nv2\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-q", "-m", "edit a")
	writeKBFile(t, root, "wiki/a.md", "---\ntitle: A\ndomain: tools\n---\n\nv3\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-q", "-m", "edit a again")
	writeKBFile(t, root, "wiki/b.md", "---\ntitle: B\ndomain: personal\n---\n\nv2\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-q", "-m", "edit b")

	cfg := &ScribeConfig{Domains: []string{"tools"}} // personal comes from universalDomains
	counts := hotDomainTouchCounts(root, cfg, since)
	if counts["tools"] != 1 {
		t.Errorf("tools count = %d, want 1 (a.md counted once despite 2 edits)", counts["tools"])
	}
	if counts["personal"] != 1 {
		t.Errorf("personal count = %d, want 1", counts["personal"])
	}
	if len(counts) != 2 {
		t.Errorf("counts = %v, want exactly 2 domains", counts)
	}
}

func TestHotDomainTouchCounts_ExcludesUnrecognizedDomain(t *testing.T) {
	root := initTestGitRepo(t, "Alice")
	writeKBFile(t, root, "wiki/a.md", "---\ntitle: A\ndomain: tools\n---\n\nbody\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-q", "-m", "seed")
	since := time.Now().Add(-time.Hour)

	// domain: toolz is a typo — never in cfg.AllDomains().
	writeKBFile(t, root, "wiki/c.md", "---\ntitle: C\ndomain: toolz\n---\n\nbody\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-q", "-m", "typo domain")

	cfg := &ScribeConfig{Domains: []string{"tools"}}
	counts := hotDomainTouchCounts(root, cfg, since)
	if _, ok := counts["toolz"]; ok {
		t.Errorf("counts contains unrecognized domain %q: %v", "toolz", counts)
	}
	if _, ok := counts[""]; ok {
		t.Errorf("counts contains empty-string catch-all bucket: %v", counts)
	}
}

func TestHotDomainTouchCounts_IgnoresCommitsBeforeSince(t *testing.T) {
	root := initTestGitRepo(t, "Alice")
	writeKBFile(t, root, "wiki/a.md", "---\ntitle: A\ndomain: tools\n---\n\nbody\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-q", "-m", "old commit")

	since := time.Now().Add(time.Hour) // future — nothing should count
	cfg := &ScribeConfig{Domains: []string{"tools"}}
	counts := hotDomainTouchCounts(root, cfg, since)
	if len(counts) != 0 {
		t.Errorf("counts = %v, want empty (since is in the future)", counts)
	}
}

// ---- 5.5 domain-scoping regression on the shared orchestrator samplers ----

func TestDreamSamplers_DomainScoping(t *testing.T) {
	root := t.TempDir()
	writeKBFile(t, root, "wiki/tools-a.md", "---\ntitle: Tools A\ntype: research\ndomain: tools\ntags: [shared]\n---\n\nbody\n")
	writeKBFile(t, root, "wiki/tools-b.md", "---\ntitle: Tools B\ntype: research\ndomain: tools\ntags: [shared]\nupdated: 2020-01-01\n---\n\nbody\n")
	writeKBFile(t, root, "wiki/personal-a.md", "---\ntitle: Personal A\ntype: research\ndomain: personal\ntags: [shared]\nupdated: 2020-01-01\n---\n\nbody\n")

	// dreamSampleInventory
	toolsInv := dreamSampleInventory(root, "tools", 10)
	if !strings.Contains(toolsInv, "Tools A") || !strings.Contains(toolsInv, "Tools B") {
		t.Errorf("tools inventory missing entries: %q", toolsInv)
	}
	if strings.Contains(toolsInv, "Personal A") {
		t.Errorf("tools inventory leaked personal domain: %q", toolsInv)
	}
	allInv := dreamSampleInventory(root, "", 10)
	for _, want := range []string{"Tools A", "Tools B", "Personal A"} {
		if !strings.Contains(allInv, want) {
			t.Errorf("full inventory (domain=\"\") missing %q: %q", want, allInv)
		}
	}

	// dreamStaleCandidates
	toolsStale := dreamStaleCandidates(root, "tools", 60)
	if !strings.Contains(toolsStale, "wiki/tools-b.md") {
		t.Errorf("tools stale missing wiki/tools-b.md: %q", toolsStale)
	}
	if strings.Contains(toolsStale, "wiki/personal-a.md") {
		t.Errorf("tools stale leaked personal domain: %q", toolsStale)
	}
	allStale := dreamStaleCandidates(root, "", 60)
	if !strings.Contains(allStale, "wiki/personal-a.md") {
		t.Errorf("full stale (domain=\"\") missing personal-a.md: %q", allStale)
	}

	// dreamContradictionCandidates
	toolsContra := dreamContradictionCandidates(root, "tools")
	if !strings.Contains(toolsContra, "Tools A") || !strings.Contains(toolsContra, "Tools B") {
		t.Errorf("tools contradictions missing entries: %q", toolsContra)
	}
	if strings.Contains(toolsContra, "Personal A") {
		t.Errorf("tools contradictions leaked personal domain: %q", toolsContra)
	}
	allContra := dreamContradictionCandidates(root, "")
	if !strings.Contains(allContra, "Personal A") {
		t.Errorf("full contradictions (domain=\"\") missing Personal A: %q", allContra)
	}
}

// ---- 5.6 commitDreamCycle runStats regression ----

func TestCommitDreamCycle_PreservesRunStatsSetBeforeCall(t *testing.T) {
	root := stubHarnessKB(t, "kb_name: stubkb\n")
	writeKBFile(t, root, "wiki/seed.md", "---\ntitle: Seed\ntype: research\ndomain: general\n---\n\nbody\n")
	gitInitHotDreamFixture(t, root)

	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "qmd"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake qmd: %v", err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SCRIBE_SKIP_REINDEX", "1")

	origStats := runStats
	t.Cleanup(func() { runStats = origStats })
	runStats = map[string]any{"mode": "orchestrator", "envelope_actions_applied": 4}

	preCount := countArticles(root)
	if err := commitDreamCycle(root, "2026-07-02", "dream", preCount); err != nil {
		t.Fatalf("commitDreamCycle: %v", err)
	}
	if runStats["mode"] != "orchestrator" {
		t.Errorf("runStats[mode] = %v, want orchestrator (must survive commitDreamCycle's additive merge)", runStats["mode"])
	}
	if runStats["articles_before"] != preCount {
		t.Errorf("runStats[articles_before] = %v, want %d", runStats["articles_before"], preCount)
	}
}

// ---- 5.4 runHotDream integration tests — stubHarnessKB + installStubLLM ----

const hotDreamYAMLTemplate = `kb_name: stubkb
lock_dir: %s
domains:
  - tools
dream:
  provider: anthropic
  model: haiku
`

// hotDreamKB scaffolds a stubHarnessKB, git-initializes it, and sandboxes
// the environment so a full runHotDream call (through commitDreamCycle's
// git commit + self-invoked backlinks/index rebuild) is safe under `go
// test`:
//
//   - lock_dir is isolated to a tempdir (never the shared /tmp default —
//     lock tests must not contend with a real cron dream/sync).
//   - PATH gets a fake no-op `qmd` prepended (real git stays reachable)
//     so the post-commit qmd reindex never touches a real index.
//   - SCRIBE_SKIP_REINDEX=1 skips commitDreamCycle's self-invoked
//     `scribe backlinks`/`scribe index` — os.Executable() resolves to
//     the compiled test binary under `go test`, and running it with
//     "backlinks"/"index" as argv would re-enter the whole test suite
//     instead of the production CLI (see rebuildIndexAndBacklinks in
//     sync.go for the established precedent of this exact guard).
//   - runStats is reset to nil and restored in t.Cleanup, matching the
//     isolation pattern deepKB uses in deep_run_test.go.
func hotDreamKB(t *testing.T, extraYAML string) string {
	t.Helper()
	root := stubHarnessKB(t, fmt.Sprintf(hotDreamYAMLTemplate, t.TempDir())+extraYAML)

	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "qmd"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake qmd: %v", err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SCRIBE_SKIP_REINDEX", "1")

	origStats := runStats
	runStats = nil
	t.Cleanup(func() { runStats = origStats })

	gitInitHotDreamFixture(t, root)
	return root
}

// gitInitHotDreamFixture git-initializes an existing stubHarnessKB root
// and commits whatever's already on disk. stubHarnessKB itself is not a
// git repo, but hotDomainTouchCounts needs git history and
// gitAddWiki/gitCommit (inside commitDreamCycle) need a repo to commit
// into. Deliberately separate from initTestGitRepo, which creates its
// own tempdir rather than git-initializing an existing root.
func gitInitHotDreamFixture(t *testing.T, root string) {
	t.Helper()
	gitRun(t, root, "init", "-q")
	gitRun(t, root, "config", "user.name", "Hot Dream Tester")
	gitRun(t, root, "config", "user.email", "test@example.com")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-q", "-m", "seed")
}

// hotEnvelopeJSON renders a WikiActionEnvelope creating one article at
// path with a configurable domain in its frontmatter — like the shared
// envelopeJSON helper (llm_stub_test.go), but that helper hardcodes
// domain: general, and these tests need to assert the hot pass's
// per-domain frontmatter contract.
func hotEnvelopeJSON(t *testing.T, entity, path, domain, body string) string {
	t.Helper()
	content := fmt.Sprintf(`---
title: %s
type: research
domain: %s
created: 2026-07-02
updated: 2026-07-02
confidence: medium
tags: []
---

%s
`, entity, domain, body)
	env := WikiActionEnvelope{
		Version: 2,
		Entity:  entity,
		Actions: []WikiAction{{Op: "create", Path: path, Content: content}},
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(b)
}

func TestRunHotDream_SkipsWhenFullDreamRanRecently(t *testing.T) {
	root := hotDreamKB(t, "")

	runsDir := filepath.Join(root, "output", "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	row := map[string]any{
		"command":   "dream",
		"status":    "ok",
		"timestamp": now.Add(-time.Hour).UTC().Format(time.RFC3339),
		"args":      []string{"dream"},
	}
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	dayFile := filepath.Join(runsDir, now.UTC().Format("2006-01-02")+".jsonl")
	if err := os.WriteFile(dayFile, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	stub := &stubJSONLLM{}
	installStubLLM(t, stub)

	beforeLog := runCmd(root, "git", "log", "--oneline")

	cfg := loadConfig(root)
	if err := runHotDream(root, cfg, "", false); err != nil {
		t.Fatalf("runHotDream: %v", err)
	}
	if len(stub.Calls()) != 0 {
		t.Errorf("stub LLM called %d times, want 0 (full dream ran recently)", len(stub.Calls()))
	}

	afterLog := runCmd(root, "git", "log", "--oneline")
	if beforeLog != afterLog {
		t.Errorf("git log changed: before=%q after=%q (gate 1 must skip before any commit)", beforeLog, afterLog)
	}
}

func TestRunHotDream_SkipsWhenChurnBelowThreshold(t *testing.T) {
	root := hotDreamKB(t, "")
	writeKBFile(t, root, "wiki/tools-a.md", "---\ntitle: Tools A\ntype: research\ndomain: tools\n---\n\nbody\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-q", "-m", "one touch")

	stub := &stubJSONLLM{}
	installStubLLM(t, stub)

	cfg := loadConfig(root)
	if err := runHotDream(root, cfg, "", false); err != nil {
		t.Fatalf("runHotDream: %v", err)
	}
	if len(stub.Calls()) != 0 {
		t.Errorf("stub LLM called %d times, want 0 (churn below hot_min_touches default of 3)", len(stub.Calls()))
	}
}

func TestRunHotDream_AppliesEnvelopeScopedToSelectedDomain(t *testing.T) {
	root := hotDreamKB(t, "")
	writeKBFile(t, root, "wiki/tools-a.md", "---\ntitle: Tools A\ntype: research\ndomain: tools\n---\n\nbody a\n")
	writeKBFile(t, root, "wiki/tools-b.md", "---\ntitle: Tools B\ntype: research\ndomain: tools\n---\n\nbody b\n")
	writeKBFile(t, root, "wiki/tools-c.md", "---\ntitle: Tools C\ntype: research\ndomain: tools\n---\n\nbody c\n")
	writeKBFile(t, root, "wiki/personal-a.md", "---\ntitle: Personal A\ntype: research\ndomain: personal\n---\n\nbody p\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-q", "-m", "churn")

	envelope := hotEnvelopeJSON(t, "dream-hot-tools", "wiki/tools-stub.md", "tools", "Stub body.")
	stub := &stubJSONLLM{stubLLM: stubLLM{Rules: []*stubRule{{MatchOp: "dream-hot", Reply: envelope}}}}
	installStubLLM(t, stub)

	cfg := loadConfig(root)
	if err := runHotDream(root, cfg, "", false); err != nil {
		t.Fatalf("runHotDream: %v", err)
	}

	calls := stub.CallsWithOp("dream-hot")
	if len(calls) != 1 {
		t.Fatalf("dream-hot calls = %d, want 1", len(calls))
	}
	prompt := calls[0].Prompt
	for _, want := range []string{"tools-a.md", "tools-b.md", "tools-c.md"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "personal-a.md") {
		t.Errorf("prompt leaked personal-a.md (should be scoped to tools):\n%s", prompt)
	}

	created := filepath.Join(root, "wiki", "tools-stub.md")
	data, err := os.ReadFile(created)
	if err != nil {
		t.Fatalf("stub article not created: %v", err)
	}
	if !strings.Contains(string(data), "domain: tools") {
		t.Errorf("stub article frontmatter missing domain: tools:\n%s", data)
	}

	commitMsg := runCmd(root, "git", "log", "-1", "--pretty=format:%s")
	if !strings.HasPrefix(commitMsg, "dream-hot domain=tools: ") {
		t.Errorf("commit message = %q, want prefix %q", commitMsg, "dream-hot domain=tools: ")
	}
}

func TestRunHotDream_ExplicitDomainOverrideBypassesChurnGate(t *testing.T) {
	root := hotDreamKB(t, "") // zero churn beyond the fixture's initial seed commit

	envelope := hotEnvelopeJSON(t, "dream-hot-personal", "wiki/personal-stub.md", "personal", "Stub body.")
	stub := &stubJSONLLM{stubLLM: stubLLM{Rules: []*stubRule{{MatchOp: "dream-hot", Reply: envelope}}}}
	installStubLLM(t, stub)

	cfg := loadConfig(root)
	if err := runHotDream(root, cfg, "personal", false); err != nil {
		t.Fatalf("runHotDream: %v", err)
	}

	calls := stub.CallsWithOp("dream-hot")
	if len(calls) != 1 {
		t.Fatalf("dream-hot calls = %d, want 1 (--domain override bypasses the churn gate)", len(calls))
	}
	if !strings.Contains(calls[0].Prompt, "domain `personal`") && !strings.Contains(calls[0].Prompt, "domain=personal") {
		t.Errorf("prompt does not show DOMAIN=personal substitution:\n%s", calls[0].Prompt)
	}
}

func TestRunHotDream_RateLimitReturnsNilAndStampsMode(t *testing.T) {
	root := hotDreamKB(t, "")
	writeKBFile(t, root, "wiki/tools-a.md", "---\ntitle: Tools A\ntype: research\ndomain: tools\n---\n\nbody\n")
	writeKBFile(t, root, "wiki/tools-b.md", "---\ntitle: Tools B\ntype: research\ndomain: tools\n---\n\nbody\n")
	writeKBFile(t, root, "wiki/tools-c.md", "---\ntitle: Tools C\ntype: research\ndomain: tools\n---\n\nbody\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-q", "-m", "churn")

	stub := &stubJSONLLM{stubLLM: stubLLM{Rules: []*stubRule{{MatchOp: "dream-hot", Err: ErrRateLimit}}}}
	installStubLLM(t, stub)

	cfg := loadConfig(root)
	if err := runHotDream(root, cfg, "", false); err != nil {
		t.Fatalf("runHotDream returned error on rate limit, want nil: %v", err)
	}
	if runStats["mode"] != "hot" {
		t.Errorf("runStats[mode] = %v, want hot (stamped before the LLM call, mirrors the full dream's rate-limit contract)", runStats["mode"])
	}
}

func TestRunHotDream_DryRunMakesNoLLMCallsOrCommits(t *testing.T) {
	root := hotDreamKB(t, "")
	writeKBFile(t, root, "wiki/tools-a.md", "---\ntitle: Tools A\ntype: research\ndomain: tools\n---\n\nbody\n")
	writeKBFile(t, root, "wiki/tools-b.md", "---\ntitle: Tools B\ntype: research\ndomain: tools\n---\n\nbody\n")
	writeKBFile(t, root, "wiki/tools-c.md", "---\ntitle: Tools C\ntype: research\ndomain: tools\n---\n\nbody\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-q", "-m", "churn")

	stub := &stubJSONLLM{}
	installStubLLM(t, stub)

	beforeLog := runCmd(root, "git", "log", "--oneline")

	cfg := loadConfig(root)
	if err := runHotDream(root, cfg, "", true); err != nil {
		t.Fatalf("runHotDream dry-run: %v", err)
	}
	if len(stub.Calls()) != 0 {
		t.Errorf("stub LLM called %d times, want 0 (dry run)", len(stub.Calls()))
	}

	afterLog := runCmd(root, "git", "log", "--oneline")
	if beforeLog != afterLog {
		t.Errorf("git log changed under --dry-run: before=%q after=%q", beforeLog, afterLog)
	}
}
