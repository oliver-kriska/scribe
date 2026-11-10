package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func focusArticle(title, typ, domain, tags, body, extra string) string {
	return fmt.Sprintf("---\ntitle: %q\ntype: %s\ndomain: %s\ntags: [%s]\nupdated: 2026-01-01\n%s---\n\n%s\n", title, typ, domain, tags, extra, body)
}

// uniqueWords returns n distinct made-up words, so near-duplicate fixtures
// clear the detector's minimum token count with discriminative vocabulary.
func uniqueWords(prefix string, n int) string {
	w := make([]string, n)
	for i := range n {
		w[i] = fmt.Sprintf("%sword%03d", prefix, i)
	}
	return strings.Join(w, " ")
}

func setupFocusKB(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	shared := uniqueWords("dup", 80)
	writeTestArticle(t, root, "patterns/retry-a.md", focusArticle("Retry With Backoff", "pattern", "general", "go", shared+" extra alpha", ""))
	writeTestArticle(t, root, "patterns/retry-b.md", focusArticle("Retry Backoff Pattern", "pattern", "general", "go", shared+" extra beta", "related: [\"[[Existing Link]]\"]\n"))
	writeTestArticle(t, root, "decisions/cache-x.md", focusArticle("Cache In Redis", "decision", "general", "cache, redis, infra", uniqueWords("x", 60), ""))
	writeTestArticle(t, root, "decisions/cache-y.md", focusArticle("Cache In Postgres", "decision", "general", "cache, redis, infra", uniqueWords("y", 60), ""))
	// Same tags but a different type: never paired.
	writeTestArticle(t, root, "tools/redis.md", focusArticle("Redis", "tool", "general", "cache, redis", uniqueWords("z", 60), ""))
	// Superseded copy of the duplicate: never offered again.
	writeTestArticle(t, root, "patterns/retry-old.md", focusArticle("Retry Old", "pattern", "general", "go", shared+" extra gamma", "status: superseded\n"))
	// Other domain, same tags as the decisions.
	writeTestArticle(t, root, "decisions/other.md", focusArticle("Other Domain Cache", "decision", "work", "cache, redis", uniqueWords("o", 60), ""))
	return root
}

func TestDreamFocusPairs(t *testing.T) {
	root := setupFocusKB(t)
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)

	pairs := dreamFocusPairs(root, "general", 4, now)
	if len(pairs) != 2 {
		t.Fatalf("pairs = %d, want 2 (one duplicate, one tag overlap): %+v", len(pairs), pairs)
	}
	if pairs[0].Kind != "duplicate" || pairs[0].key() != "patterns/retry-a.md|patterns/retry-b.md" {
		t.Errorf("first pair should be the near-duplicate, got %s %s", pairs[0].Kind, pairs[0].key())
	}
	if pairs[1].Kind != "overlap" || pairs[1].key() != "decisions/cache-x.md|decisions/cache-y.md" {
		t.Errorf("second pair should be the same-type tag overlap, got %s %s", pairs[1].Kind, pairs[1].key())
	}
	if !strings.Contains(pairs[1].Reason, "cache, infra, redis") {
		t.Errorf("reason should list shared tags: %q", pairs[1].Reason)
	}

	// Reviewed pairs are skipped for the recheck window, then return.
	markDreamFocusReviewed(root, pairs[:1], now)
	again := dreamFocusPairs(root, "general", 4, now.AddDate(0, 0, 1))
	if len(again) != 1 || again[0].Kind != "overlap" {
		t.Errorf("reviewed duplicate pair should be skipped: %+v", again)
	}
	later := dreamFocusPairs(root, "general", 4, now.AddDate(0, 0, dreamFocusRecheckDays+1))
	if len(later) != 2 {
		t.Errorf("pair should return after the recheck window, got %d", len(later))
	}

	if capped := dreamFocusPairs(root, "", 1, now.AddDate(0, 0, dreamFocusRecheckDays+1)); len(capped) != 1 {
		t.Errorf("maxPairs not honored: %d", len(capped))
	}
}

func TestRenderDreamFocusBoundsBodies(t *testing.T) {
	long := strings.Repeat("x", dreamFocusBodyMaxBytes*3)
	out := renderDreamFocus([]dreamFocusPair{{
		A: dreamFocusDoc{Path: "a.md", Title: "A", Body: long}, B: dreamFocusDoc{Path: "b.md", Title: "B", Body: "short"},
		Kind: "duplicate", Reason: "r",
	}})
	if len(out) > 2*dreamFocusBodyMaxBytes+600 {
		t.Errorf("packet not bounded: %d bytes", len(out))
	}
	if !strings.Contains(out, "[… cut …]") || !strings.Contains(out, "#### b.md") {
		t.Errorf("render missing cut marker or second doc:\n%s", out[:200])
	}
	if renderDreamFocus(nil) == "" {
		t.Error("empty focus must still render a placeholder line")
	}
}

func TestGuardDreamFocusActions(t *testing.T) {
	root := setupFocusKB(t)
	pairs := dreamFocusPairs(root, "general", 4, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))

	env := WikiActionEnvelope{Actions: []WikiAction{
		// Allowed: duplicate side, no related → keeper link added, existing kept.
		{Op: "update_frontmatter", Path: "patterns/retry-b.md", Frontmatter: map[string]any{"status": "superseded"}},
		// Second supersede in the same pair: status dropped, action emptied → removed.
		{Op: "update_frontmatter", Path: "patterns/retry-a.md", Frontmatter: map[string]any{"status": "superseded"}},
		// Overlap pair is not a duplicate pair: status dropped, other keys kept.
		{Op: "update_frontmatter", Path: "decisions/cache-x.md", Frontmatter: map[string]any{"status": "superseded", "updated": "2026-11-09"}},
		// Unrelated ops pass through untouched.
		{Op: "append", Path: "decisions/cache-y.md", Content: "\n\n> **Conflict noted**\n"},
	}}
	kept := guardDreamFocusActions(root, &env, pairs)
	if kept != 1 {
		t.Errorf("kept = %d, want 1", kept)
	}
	if len(env.Actions) != 3 {
		t.Fatalf("actions = %d, want 3 (emptied one removed): %+v", len(env.Actions), env.Actions)
	}
	rel, ok := env.Actions[0].Frontmatter["related"].([]any)
	if !ok || len(rel) != 2 || rel[0] != "[[Existing Link]]" || rel[1] != "[[Retry With Backoff]]" {
		t.Errorf("related = %#v, want existing link kept + keeper link added", env.Actions[0].Frontmatter["related"])
	}
	if _, has := env.Actions[1].Frontmatter["status"]; has || env.Actions[1].Frontmatter["updated"] != "2026-11-09" {
		t.Errorf("non-duplicate supersede not stripped correctly: %#v", env.Actions[1].Frontmatter)
	}
}

// The model's proposed related already names the keeper: the article's
// existing links must still survive (live regression 2026-11-10, where
// update_frontmatter's wholesale replace wiped them).
func TestGuardDreamFocusKeepsExistingRelated(t *testing.T) {
	root := setupFocusKB(t)
	pairs := dreamFocusPairs(root, "general", 4, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
	writeTestArticle(t, root, "tools/redis.md", focusArticle("Redis", "tool", "general", "cache, redis", uniqueWords("z", 60), "related: [\"[[Keep Me]]\"]\n"))

	env := WikiActionEnvelope{Actions: []WikiAction{
		{Op: "update_frontmatter", Path: "patterns/retry-b.md", Frontmatter: map[string]any{
			"status": "superseded", "related": []any{"[[Retry With Backoff]]"},
		}},
		// Not a focus article: related is still merged, never replaced.
		{Op: "update_frontmatter", Path: "tools/redis.md", Frontmatter: map[string]any{"related": []string{"[[New Link]]"}}},
	}}
	if kept := guardDreamFocusActions(root, &env, pairs); kept != 1 {
		t.Fatalf("kept = %d, want 1", kept)
	}
	want := [][]any{
		{"[[Existing Link]]", "[[Retry With Backoff]]"},
		{"[[Keep Me]]", "[[New Link]]"},
	}
	for i, w := range want {
		got, _ := env.Actions[i].Frontmatter["related"].([]any)
		if len(got) != len(w) {
			t.Errorf("action %d related = %#v, want %#v", i, got, w)
			continue
		}
		for j := range w {
			if got[j] != w[j] {
				t.Errorf("action %d related = %#v, want %#v", i, got, w)
				break
			}
		}
	}
}

func TestDreamContentActions(t *testing.T) {
	if n := dreamContentActions([]string{"meta:log_append", "patterns/a.md", "decisions/b.md"}); n != 2 {
		t.Errorf("content actions = %d, want 2", n)
	}
	if n := dreamContentActions([]string{"meta:log_append"}); n != 0 {
		t.Errorf("a log-only run must count 0, got %d", n)
	}
}

func TestDreamFocusInventoryListsFocusFirst(t *testing.T) {
	root := setupFocusKB(t)
	pairs := dreamFocusPairs(root, "", 1, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
	inv := dreamFocusInventory(root, "", pairs, 3)
	lines := strings.Split(inv, "\n")
	if len(lines) != 3 {
		t.Fatalf("inventory lines = %d, want 3:\n%s", len(lines), inv)
	}
	if !strings.HasPrefix(lines[0], "- patterns/retry-a.md") || !strings.HasPrefix(lines[1], "- patterns/retry-b.md") {
		t.Errorf("focus articles should lead the inventory:\n%s", inv)
	}
}
