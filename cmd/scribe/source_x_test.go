package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func xTestCfg() *ScribeConfig {
	return &ScribeConfig{Integrations: IntegrationsConfig{"x": {Enabled: true}}}
}

// stubXurl swaps the xurlRun transport seam for the duration of a test, so no
// real xurl binary or network is ever touched.
func stubXurl(t *testing.T, fn func(ctx context.Context, path string) ([]byte, error)) {
	t.Helper()
	orig := xurlRun
	t.Cleanup(func() { xurlRun = orig })
	xurlRun = fn
}

// fakeXurlOnPath drops an executable named `xurl` into a temp dir and points
// PATH at it, so exec.LookPath("xurl") succeeds without a real install.
func fakeXurlOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "xurl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestXConfiguredGates(t *testing.T) {
	x := xSource{}

	// disabled / no integrations block
	if ok, reason := x.Configured(&ScribeConfig{}); ok || reason == "" {
		t.Errorf("Configured true with no integrations block (reason %q)", reason)
	}

	// enabled but xurl missing from PATH
	t.Setenv("PATH", t.TempDir()) // empty dir → LookPath fails
	if ok, reason := x.Configured(xTestCfg()); ok || !strings.Contains(reason, "install") {
		t.Errorf("Configured = (%v, %q), want false with an install hint", ok, reason)
	}

	// enabled + xurl on PATH, but never authenticated (no ~/.xurl) → soft-skip
	// with an auth hint rather than a hard Fetch error on every cron pull.
	fakeXurlOnPath(t)
	t.Setenv("HOME", t.TempDir()) // empty home → no ~/.xurl
	if ok, reason := x.Configured(xTestCfg()); ok || !strings.Contains(reason, "xurl auth oauth2") {
		t.Errorf("Configured = (%v, %q), want false with an auth hint when ~/.xurl is missing", ok, reason)
	}

	// enabled + xurl on PATH + ~/.xurl present
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".xurl"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if ok, reason := x.Configured(xTestCfg()); !ok {
		t.Errorf("Configured false with xurl present and authenticated: %q", reason)
	}
}

func TestXFetchResolvesAndCachesUserID(t *testing.T) {
	calls := 0
	stubXurl(t, func(_ context.Context, path string) ([]byte, error) {
		calls++
		switch {
		case strings.HasPrefix(path, "/2/users/me"):
			return []byte(`{"data":{"id":"u1","username":"me"}}`), nil
		case strings.Contains(path, "/bookmarks"):
			return []byte(`{"data":[{"id":"t9","text":"hello","author_id":"a1","created_at":"2026-07-01T00:00:00Z"}],"includes":{"users":[{"id":"a1","username":"alice"}]},"meta":{"result_count":1}}`), nil
		}
		return nil, fmt.Errorf("unexpected path %q", path)
	})

	items, cur, err := xSource{}.Fetch(context.Background(), xTestCfg(), nil, FetchOpts{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 1 || items[0].URL != "https://x.com/alice/status/t9" {
		t.Fatalf("items = %+v, want one alice/t9", items)
	}
	var xc xCursor
	if err := json.Unmarshal(cur, &xc); err != nil {
		t.Fatalf("cursor unmarshal: %v", err)
	}
	if xc.UserID != "u1" || xc.NewestID != "t9" {
		t.Errorf("cursor = %+v, want user_id=u1 newest_id=t9", xc)
	}
	if calls != 2 {
		t.Errorf("xurl calls = %d, want 2 (me + one page)", calls)
	}
}

func TestXFetchCheapProbeShortCircuit(t *testing.T) {
	prev, err := json.Marshal(xCursor{UserID: "u1", NewestID: "t9"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	stubXurl(t, func(_ context.Context, path string) ([]byte, error) {
		calls++
		if !strings.Contains(path, "/bookmarks") || !strings.Contains(path, "max_results=1&") {
			t.Errorf("unexpected call %q, want a single-item probe", path)
		}
		return []byte(`{"data":[{"id":"t9","text":"same","author_id":"a1"}],"includes":{"users":[{"id":"a1","username":"alice"}]},"meta":{"result_count":1}}`), nil
	})

	items, cur, err := xSource{}.Fetch(context.Background(), xTestCfg(), prev, FetchOpts{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("items = %d, want 0 when newest id unchanged", len(items))
	}
	if calls != 1 {
		t.Errorf("xurl calls = %d, want 1 (probe only, no full page)", calls)
	}
	var xc xCursor
	if err := json.Unmarshal(cur, &xc); err != nil || xc.NewestID != "t9" {
		t.Errorf("cursor = %s (err %v), want newest_id preserved", cur, err)
	}
}

func TestXFetchForceBypassesProbe(t *testing.T) {
	prev, err := json.Marshal(xCursor{UserID: "u1", NewestID: "t9"})
	if err != nil {
		t.Fatal(err)
	}
	sawProbe := false
	stubXurl(t, func(_ context.Context, path string) ([]byte, error) {
		if strings.Contains(path, "max_results=1&") {
			sawProbe = true
		}
		// A --force run pages the full archive; return one new tweet, no next.
		return []byte(`{"data":[{"id":"t10","text":"new","author_id":"a1"}],"includes":{"users":[{"id":"a1","username":"alice"}]},"meta":{"result_count":1}}`), nil
	})

	items, _, err := xSource{}.Fetch(context.Background(), xTestCfg(), prev, FetchOpts{Force: true})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if sawProbe {
		t.Error("--force should skip the cheap max_results=1 probe")
	}
	if len(items) != 1 || items[0].ID != "t10" {
		t.Errorf("items = %+v, want the new t10", items)
	}
}

func TestXFetchPaginationStopsAtKnownID(t *testing.T) {
	prev, err := json.Marshal(xCursor{UserID: "u1", NewestID: "old"})
	if err != nil {
		t.Fatal(err)
	}
	var pages []string
	stubXurl(t, func(_ context.Context, path string) ([]byte, error) {
		pages = append(pages, path)
		switch {
		case strings.Contains(path, "pagination_token=t2"):
			return []byte(`{"data":[{"id":"n3","text":"c","author_id":"a1"},{"id":"old","text":"o","author_id":"a1"},{"id":"n4","text":"d","author_id":"a1"}],"includes":{"users":[{"id":"a1","username":"alice"}]},"meta":{"next_token":"t3","result_count":3}}`), nil
		case strings.Contains(path, "/bookmarks"):
			return []byte(`{"data":[{"id":"n1","text":"a","author_id":"a1"},{"id":"n2","text":"b","author_id":"a1"}],"includes":{"users":[{"id":"a1","username":"alice"}]},"meta":{"next_token":"t2","result_count":2}}`), nil
		}
		return nil, fmt.Errorf("unexpected path %q", path)
	})

	items, cur, err := xSource{}.Fetch(context.Background(), xTestCfg(), prev, FetchOpts{Force: true})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	gotIDs := make([]string, 0, len(items))
	for _, it := range items {
		gotIDs = append(gotIDs, it.ID)
	}
	if strings.Join(gotIDs, ",") != "n1,n2,n3" {
		t.Errorf("collected ids = %v, want [n1 n2 n3] (stop at known 'old')", gotIDs)
	}
	if len(pages) != 2 {
		t.Errorf("fetched %d pages, want 2 (stop after the page holding 'old')", len(pages))
	}
	var xc xCursor
	if err := json.Unmarshal(cur, &xc); err != nil || xc.NewestID != "n1" {
		t.Errorf("cursor = %s (err %v), want newest_id=n1", cur, err)
	}
}

func TestXFetchHardPageCap(t *testing.T) {
	// A never-ending next_token must not page forever: the loop hard-stops at
	// xMaxBookmarkPages. user_id is cached and no newest_id is set, so every
	// call here is a bookmark page (no /2/users/me, no probe).
	prev, err := json.Marshal(xCursor{UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	stubXurl(t, func(_ context.Context, path string) ([]byte, error) {
		calls++
		if !strings.Contains(path, "/bookmarks") {
			t.Errorf("unexpected non-bookmark call %q", path)
		}
		body := fmt.Sprintf(`{"data":[{"id":"t%d","text":"x","author_id":"a1"}],"includes":{"users":[{"id":"a1","username":"alice"}]},"meta":{"next_token":"nt%d","result_count":1}}`, calls, calls)
		return []byte(body), nil
	})

	items, _, err := xSource{}.Fetch(context.Background(), xTestCfg(), prev, FetchOpts{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if calls != xMaxBookmarkPages {
		t.Errorf("xurl bookmark calls = %d, want the %d-page hard cap", calls, xMaxBookmarkPages)
	}
	if len(items) != xMaxBookmarkPages {
		t.Errorf("items = %d, want %d (one per capped page)", len(items), xMaxBookmarkPages)
	}
}

func TestXToItemURL(t *testing.T) {
	users := map[string]string{"a1": "alice"}

	// author username present → handle permalink, full text as note.
	it := xTweet{ID: "123", Text: "hello world", AuthorID: "a1", CreatedAt: "2026-07-01T00:00:00Z"}.toItem(users)
	if it.URL != "https://x.com/alice/status/123" {
		t.Errorf("URL = %q, want handle permalink", it.URL)
	}
	if it.Note != "hello world" {
		t.Errorf("Note = %q, want full tweet text", it.Note)
	}
	if it.ID != "123" || it.Private || it.Unread {
		t.Errorf("item flags off: %+v", it)
	}
	if it.CreatedAt.IsZero() {
		t.Error("CreatedAt zero, want parsed time")
	}

	// author absent from includes.users → /i/ fallback permalink.
	orphan := xTweet{ID: "456", Text: "orphan", AuthorID: "ghost"}.toItem(users)
	if orphan.URL != "https://x.com/i/status/456" {
		t.Errorf("URL = %q, want /i/ fallback when username missing", orphan.URL)
	}
}

func TestXTweetTitle(t *testing.T) {
	// multi-line / multi-space text collapses to one line.
	if got := tweetTitle("line one\n\nline   two"); got != "line one line two" {
		t.Errorf("tweetTitle collapse = %q", got)
	}
	// long text truncates to ~80 runes and stays single-line.
	long := strings.Repeat("a", 200)
	got := tweetTitle(long)
	if n := len([]rune(got)); n > 80 {
		t.Errorf("tweetTitle len = %d runes, want <= 80", n)
	}
	if strings.ContainsAny(got, "\n") {
		t.Errorf("tweetTitle still multi-line: %q", got)
	}
}

func TestXFetchMalformedBookmarksJSON(t *testing.T) {
	prev, err := json.Marshal(xCursor{UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	stubXurl(t, func(_ context.Context, _ string) ([]byte, error) {
		return []byte("not json{{{"), nil
	})
	if _, _, err := (xSource{}).Fetch(context.Background(), xTestCfg(), prev, FetchOpts{Force: true}); err == nil {
		t.Error("malformed bookmarks JSON should error, not panic")
	}
}

func TestXFetchMalformedUserJSON(t *testing.T) {
	stubXurl(t, func(_ context.Context, _ string) ([]byte, error) {
		return []byte("}{"), nil
	})
	if _, _, err := (xSource{}).Fetch(context.Background(), xTestCfg(), nil, FetchOpts{}); err == nil {
		t.Error("malformed /2/users/me JSON should error, not panic")
	}
}

func TestXToItemHashtagsBecomeTags(t *testing.T) {
	// #hashtags are the only tag-like signal a bookmark carries; they must land
	// in Tags or the driver's OR tag filter can never match an X item.
	var tw xTweet
	if err := json.Unmarshal([]byte(`{"id":"1","text":"post #Go #KB","author_id":"a1","entities":{"hashtags":[{"tag":"Go"},{"tag":"KB"}]}}`), &tw); err != nil {
		t.Fatal(err)
	}
	it := tw.toItem(nil)
	if strings.Join(it.Tags, ",") != "Go,KB" {
		t.Errorf("Tags = %v, want [Go KB] from entities.hashtags", it.Tags)
	}

	// no entities block → no tags, no panic.
	bare := xTweet{ID: "2", Text: "plain"}.toItem(nil)
	if len(bare.Tags) != 0 {
		t.Errorf("Tags = %v, want none without hashtags", bare.Tags)
	}
}

func TestXFetchRequestsEntitiesAndExpansions(t *testing.T) {
	// The permalink handle, save time, and hashtag tags all depend on these
	// request params — a regression here degrades items silently (fallback /i/
	// URLs, zero times, empty tags), so pin the wire format.
	prev, err := json.Marshal(xCursor{UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	stubXurl(t, func(_ context.Context, path string) ([]byte, error) {
		for _, param := range []string{"expansions=author_id", "tweet.fields=created_at,entities", "user.fields=username"} {
			if !strings.Contains(path, param) {
				t.Errorf("bookmarks path %q missing %q", path, param)
			}
		}
		return []byte(`{"data":[{"id":"t1","text":"post #go","author_id":"a1","entities":{"hashtags":[{"tag":"go"}]}}],"includes":{"users":[{"id":"a1","username":"alice"}]},"meta":{"result_count":1}}`), nil
	})

	items, _, err := xSource{}.Fetch(context.Background(), xTestCfg(), prev, FetchOpts{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 1 || strings.Join(items[0].Tags, ",") != "go" {
		t.Errorf("items = %+v, want one item tagged [go]", items)
	}
}

func TestXFetchErrorMidPaginationPreservesCursor(t *testing.T) {
	// A failure on page 2 must not advance the cursor past bookmarks that were
	// never queued: Fetch returns the previous cursor untouched so the next run
	// re-pages from the same boundary (the driver's seen-set absorbs the
	// re-fetched overlap).
	prev, err := json.Marshal(xCursor{UserID: "u1", NewestID: "old"})
	if err != nil {
		t.Fatal(err)
	}
	stubXurl(t, func(_ context.Context, path string) ([]byte, error) {
		if strings.Contains(path, "pagination_token=t2") {
			return nil, errors.New("500 server error")
		}
		return []byte(`{"data":[{"id":"n1","text":"a","author_id":"a1"}],"includes":{"users":[{"id":"a1","username":"alice"}]},"meta":{"next_token":"t2","result_count":1}}`), nil
	})

	items, cur, err := xSource{}.Fetch(context.Background(), xTestCfg(), prev, FetchOpts{Force: true})
	if err == nil {
		t.Fatal("Fetch should surface the page-2 error")
	}
	if len(items) != 0 {
		t.Errorf("items = %d, want none on a failed run", len(items))
	}
	if !bytes.Equal(cur, prev) {
		t.Errorf("cursor advanced on error: %s, want previous %s", cur, prev)
	}
}

func TestXFetchEmptyAccount(t *testing.T) {
	// First run against an account with zero bookmarks: no items, no error, and
	// the cursor caches the user id with no newest_id (so the next run probes
	// nothing and pages from the top again).
	stubXurl(t, func(_ context.Context, path string) ([]byte, error) {
		if strings.HasPrefix(path, "/2/users/me") {
			return []byte(`{"data":{"id":"u1","username":"me"}}`), nil
		}
		return []byte(`{"data":[],"meta":{"result_count":0}}`), nil
	})

	items, cur, err := xSource{}.Fetch(context.Background(), xTestCfg(), nil, FetchOpts{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("items = %d, want 0 for an empty account", len(items))
	}
	var xc xCursor
	if err := json.Unmarshal(cur, &xc); err != nil || xc.UserID != "u1" || xc.NewestID != "" {
		t.Errorf("cursor = %s (err %v), want cached user_id and empty newest_id", cur, err)
	}
}

func TestXFetchDuplicateIDsAcrossPages(t *testing.T) {
	// The API's pagination has reported overlap bugs; a tweet repeated across
	// pages must pass through without tripping the boundary logic — dedup is
	// the driver's job (seen-set), not the adapter's. The id-less row on page 2
	// exercises the malformed-row guard: it must be skipped, not queued as a
	// garbage /i/status/ permalink.
	prev, err := json.Marshal(xCursor{UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	stubXurl(t, func(_ context.Context, path string) ([]byte, error) {
		if strings.Contains(path, "pagination_token=t2") {
			return []byte(`{"data":[{"id":"n1","text":"dup","author_id":"a1"},{"id":"","text":"malformed"},{"id":"n2","text":"b","author_id":"a1"}],"includes":{"users":[{"id":"a1","username":"alice"}]},"meta":{"result_count":3}}`), nil
		}
		return []byte(`{"data":[{"id":"n1","text":"a","author_id":"a1"}],"includes":{"users":[{"id":"a1","username":"alice"}]},"meta":{"next_token":"t2","result_count":1}}`), nil
	})

	items, cur, err := xSource{}.Fetch(context.Background(), xTestCfg(), prev, FetchOpts{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	gotIDs := make([]string, 0, len(items))
	for _, it := range items {
		gotIDs = append(gotIDs, it.ID)
	}
	if strings.Join(gotIDs, ",") != "n1,n1,n2" {
		t.Errorf("collected ids = %v, want duplicates passed through (and the id-less row skipped) as [n1 n1 n2]", gotIDs)
	}
	var xc xCursor
	if err := json.Unmarshal(cur, &xc); err != nil || xc.NewestID != "n1" {
		t.Errorf("cursor = %s (err %v), want newest_id=n1", cur, err)
	}
}

func TestXFetchCapDoesNotAdvanceCursorPastGap(t *testing.T) {
	// If the page cap fires while the server still offers a next page AND the
	// previous boundary was never reached, there is an unfetched gap between
	// the cap and the old newest_id. Advancing the cursor would make that gap
	// permanently unreachable (the probe would short-circuit every later run),
	// so the cursor must stay at the old boundary. Items fetched before the
	// cap are still returned — the driver's seen-set dedups the re-walk.
	prev, err := json.Marshal(xCursor{UserID: "u1", NewestID: "old"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	stubXurl(t, func(_ context.Context, path string) ([]byte, error) {
		calls++
		if !strings.Contains(path, "/bookmarks") {
			t.Errorf("unexpected non-bookmark call %q", path)
		}
		// Endless pages that never contain "old".
		body := fmt.Sprintf(`{"data":[{"id":"t%d","text":"x","author_id":"a1"}],"includes":{"users":[{"id":"a1","username":"alice"}]},"meta":{"next_token":"nt%d","result_count":1}}`, calls, calls)
		return []byte(body), nil
	})

	items, cur, err := xSource{}.Fetch(context.Background(), xTestCfg(), prev, FetchOpts{Force: true})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != xMaxBookmarkPages {
		t.Errorf("items = %d, want %d (one per capped page)", len(items), xMaxBookmarkPages)
	}
	var xc xCursor
	if err := json.Unmarshal(cur, &xc); err != nil || xc.NewestID != "old" {
		t.Errorf("cursor = %s (err %v), want newest_id kept at 'old' when capped before the boundary", cur, err)
	}
}

func TestXFetchProbeErrorFallsBackToWalk(t *testing.T) {
	// The probe is a cost optimization, not a gate: if the single-item probe
	// fails (e.g. the endpoint rejects max_results=1 — minimum page size is
	// unverified), the run degrades to the normal 100-per-page walk instead of
	// erroring out of every cron pull.
	prev, err := json.Marshal(xCursor{UserID: "u1", NewestID: "old"})
	if err != nil {
		t.Fatal(err)
	}
	stubXurl(t, func(_ context.Context, path string) ([]byte, error) {
		if strings.Contains(path, "max_results=1&") {
			return nil, errors.New("400 Bad Request: max_results below minimum")
		}
		return []byte(`{"data":[{"id":"n1","text":"a","author_id":"a1"},{"id":"old","text":"o","author_id":"a1"}],"includes":{"users":[{"id":"a1","username":"alice"}]},"meta":{"result_count":2}}`), nil
	})

	items, cur, err := xSource{}.Fetch(context.Background(), xTestCfg(), prev, FetchOpts{})
	if err != nil {
		t.Fatalf("Fetch should degrade past a probe failure, got: %v", err)
	}
	if len(items) != 1 || items[0].ID != "n1" {
		t.Errorf("items = %+v, want the one new n1 from the fallback walk", items)
	}
	var xc xCursor
	if err := json.Unmarshal(cur, &xc); err != nil || xc.NewestID != "n1" {
		t.Errorf("cursor = %s (err %v), want newest_id=n1", cur, err)
	}
}

func TestXFetchAuthErrorHint(t *testing.T) {
	stubXurl(t, func(_ context.Context, _ string) ([]byte, error) {
		return nil, errors.New("401 Unauthorized")
	})
	_, _, err := xSource{}.Fetch(context.Background(), xTestCfg(), nil, FetchOpts{})
	if err == nil || !strings.Contains(err.Error(), "xurl auth oauth2") {
		t.Errorf("err = %v, want a `xurl auth oauth2` re-auth hint", err)
	}
}
