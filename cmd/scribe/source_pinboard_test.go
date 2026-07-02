package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakePinboard stands in for api.pinboard.in. It records how many times each
// endpoint was hit so the short-circuit behavior is observable, and asserts
// the auth token + format are passed on every call.
func fakePinboard(t *testing.T, hits map[string]int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	guard := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("auth_token"); got != "user:TESTTOKEN" {
				t.Errorf("auth_token = %q, want user:TESTTOKEN", got)
			}
			if got := r.URL.Query().Get("format"); got != "json" {
				t.Errorf("format = %q, want json", got)
			}
			next(w, r)
		}
	}
	mux.HandleFunc("/posts/update", guard(func(w http.ResponseWriter, _ *http.Request) {
		hits["update"]++
		io.WriteString(w, `{"update_time":"2026-07-01T10:00:00Z"}`)
	}))
	mux.HandleFunc("/posts/all", guard(func(w http.ResponseWriter, r *http.Request) {
		hits["all"]++
		if r.URL.Query().Get("toread") == "yes" {
			io.WriteString(w, `[{"href":"https://u.com","description":"Unread","tags":"read-later","time":"2026-06-01T00:00:00Z","toread":"yes","hash":"h-unread"}]`)
			return
		}
		io.WriteString(w, `[{"href":"https://a.com","description":"A","extended":"note a","tags":"go rust","time":"2026-06-01T00:00:00Z","toread":"no","hash":"h-a"}]`)
	}))
	mux.HandleFunc("/posts/recent", guard(func(w http.ResponseWriter, _ *http.Request) {
		hits["recent"]++
		io.WriteString(w, `{"posts":[{"href":"https://r.com","description":"R","tags":"","time":"2026-06-02T00:00:00Z","toread":"no","hash":"h-r"}]}`)
	}))
	return httptest.NewServer(mux)
}

func pinboardTestCfg() *ScribeConfig {
	return &ScribeConfig{Integrations: IntegrationsConfig{"pinboard": {Enabled: true}}}
}

func TestPinboardToItem(t *testing.T) {
	post := pinboardPost{
		Href:        "https://example.com/x",
		Description: "  Title  ",
		Extended:    "my note",
		Tags:        "go  rust   elixir",
		Time:        "2026-06-01T12:00:00Z",
		ToRead:      "yes",
		Shared:      "no",
		Hash:        "abc123",
	}
	it, ok := post.toItem()
	if !ok {
		t.Fatal("toItem returned ok=false for a valid post")
	}
	if it.URL != "https://example.com/x" {
		t.Errorf("URL = %q", it.URL)
	}
	if it.Title != "Title" {
		t.Errorf("Title = %q, want trimmed", it.Title)
	}
	if it.Note != "my note" {
		t.Errorf("Note = %q", it.Note)
	}
	if len(it.Tags) != 3 || it.Tags[0] != "go" || it.Tags[2] != "elixir" {
		t.Errorf("Tags = %v, want [go rust elixir]", it.Tags)
	}
	if !it.Unread {
		t.Error("Unread = false, want true for toread=yes")
	}
	if !it.Private {
		t.Error("Private = false, want true for shared=no")
	}
	if it.ID != "abc123" {
		t.Errorf("ID = %q, want the hash", it.ID)
	}
	if it.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero, want parsed time")
	}
}

func TestPinboardToItemEmptyHrefSkipped(t *testing.T) {
	if _, ok := (pinboardPost{Href: "  "}).toItem(); ok {
		t.Error("blank href should yield ok=false")
	}
}

func TestPinboardToItemIDFallsBackToHref(t *testing.T) {
	it, _ := pinboardPost{Href: "https://h.com"}.toItem()
	if it.ID != "https://h.com" {
		t.Errorf("ID = %q, want href fallback when hash empty", it.ID)
	}
}

func TestMergePostsByHash(t *testing.T) {
	a := []pinboardPost{{Href: "https://1", Hash: "h1"}, {Href: "https://2", Hash: "h2"}}
	b := []pinboardPost{{Href: "https://2", Hash: "h2"}, {Href: "https://3", Hash: "h3"}}
	got := mergePostsByHash(a, b)
	if len(got) != 3 {
		t.Fatalf("merged len = %d, want 3 (dedup h2)", len(got))
	}
	if got[0].Hash != "h1" || got[2].Hash != "h3" {
		t.Errorf("order not preserved: %+v", got)
	}
}

func TestPinboardFetchScopeAll(t *testing.T) {
	t.Setenv("SCRIBE_PINBOARD_TOKEN", "user:TESTTOKEN")
	hits := map[string]int{}
	srv := fakePinboard(t, hits)
	defer srv.Close()
	p := pinboardSource{baseURL: srv.URL + "/"}

	items, cur, err := p.Fetch(context.Background(), pinboardTestCfg(), nil, FetchOpts{Scope: "all"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 1 || items[0].ID != "h-a" {
		t.Fatalf("items = %+v, want one h-a", items)
	}
	if hits["update"] != 1 || hits["all"] != 1 || hits["recent"] != 0 {
		t.Errorf("hits = %v, want update=1 all=1 recent=0", hits)
	}
	var pc pinboardCursor
	if err := json.Unmarshal(cur, &pc); err != nil || pc.UpdateTime != "2026-07-01T10:00:00Z" {
		t.Errorf("cursor = %s (err %v), want update_time persisted", cur, err)
	}
}

func TestPinboardUpdateShortCircuit(t *testing.T) {
	t.Setenv("SCRIBE_PINBOARD_TOKEN", "user:TESTTOKEN")
	hits := map[string]int{}
	srv := fakePinboard(t, hits)
	defer srv.Close()
	p := pinboardSource{baseURL: srv.URL + "/"}

	// Cursor already at the server's update_time → nothing changed.
	prev, err := json.Marshal(pinboardCursor{UpdateTime: "2026-07-01T10:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	items, cur, err := p.Fetch(context.Background(), pinboardTestCfg(), prev, FetchOpts{Scope: "all"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("items = %d, want 0 on unchanged account", len(items))
	}
	if hits["all"] != 0 {
		t.Errorf("posts/all hit %d times, want 0 (short-circuit before the expensive call)", hits["all"])
	}
	if string(cur) != string(prev) {
		t.Errorf("cursor changed on short-circuit: %s", cur)
	}
}

func TestPinboardForceBypassesShortCircuit(t *testing.T) {
	t.Setenv("SCRIBE_PINBOARD_TOKEN", "user:TESTTOKEN")
	hits := map[string]int{}
	srv := fakePinboard(t, hits)
	defer srv.Close()
	p := pinboardSource{baseURL: srv.URL + "/"}

	prev, err := json.Marshal(pinboardCursor{UpdateTime: "2026-07-01T10:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	items, _, err := p.Fetch(context.Background(), pinboardTestCfg(), prev, FetchOpts{Scope: "all", Force: true})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("items = %d, want 1 (force bypasses short-circuit)", len(items))
	}
	if hits["all"] != 1 {
		t.Errorf("posts/all hit %d times, want 1", hits["all"])
	}
}

func TestPinboardRecentUnreadUnions(t *testing.T) {
	t.Setenv("SCRIBE_PINBOARD_TOKEN", "user:TESTTOKEN")
	hits := map[string]int{}
	srv := fakePinboard(t, hits)
	defer srv.Close()
	p := pinboardSource{baseURL: srv.URL + "/"}

	items, _, err := p.Fetch(context.Background(), pinboardTestCfg(), nil, FetchOpts{Scope: "recent+unread"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (recent ∪ unread)", len(items))
	}
	if hits["recent"] != 1 || hits["all"] != 1 {
		t.Errorf("hits = %v, want recent=1 all=1", hits)
	}
}

func TestPinboardUnknownScopeErrors(t *testing.T) {
	t.Setenv("SCRIBE_PINBOARD_TOKEN", "user:TESTTOKEN")
	hits := map[string]int{}
	srv := fakePinboard(t, hits)
	defer srv.Close()
	p := pinboardSource{baseURL: srv.URL + "/"}

	if _, _, err := p.Fetch(context.Background(), pinboardTestCfg(), nil, FetchOpts{Scope: "bogus"}); err == nil {
		t.Error("an unknown scope should error")
	}
}

func TestPinboardConfiguredGates(t *testing.T) {
	p := pinboardSource{}

	// disabled
	if ok, _ := p.Configured(&ScribeConfig{}); ok {
		t.Error("Configured true with no integrations block")
	}
	// enabled but no token
	t.Setenv("SCRIBE_PINBOARD_TOKEN", "")
	cfg := &ScribeConfig{Integrations: IntegrationsConfig{"pinboard": {Enabled: true}}}
	if ok, reason := p.Configured(cfg); ok || reason == "" {
		t.Errorf("Configured should fail without a token, got ok=%v reason=%q", ok, reason)
	}
	// enabled + token
	t.Setenv("SCRIBE_PINBOARD_TOKEN", "user:TESTTOKEN")
	if ok, reason := p.Configured(cfg); !ok {
		t.Errorf("Configured false with token present: %q", reason)
	}
}
