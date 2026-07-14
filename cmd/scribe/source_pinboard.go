package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// pinboardSource pulls bookmarks from the Pinboard v1 API
// (https://api.pinboard.in/v1/). It is a URL producer only: it queues hrefs
// (with title, tags, and the user's `extended` note) into output/inbox/ and
// lets the existing ingest-drain path fetch the content. No page content is
// fetched here, so the adapter stays deterministic and fast.
//
// Auth is an API token of the form `username:HEXTOKEN` (from
// pinboard.in/settings/password), resolved via integrationToken("pinboard")
// — env SCRIBE_PINBOARD_TOKEN or ~/.config/scribe/config.yaml, never the
// committed scribe.yaml.
//
// Rate limits (Pinboard bans abusive clients): the cheap posts/update probe
// gates every run, so an unchanged account costs one small request; posts/all
// is capped server-side to once / 5 min and posts/recent to once / min, both
// comfortably within an hourly cron cadence.
type pinboardSource struct {
	// baseURL overrides the API base in tests (httptest). Empty → production.
	baseURL string
}

const pinboardAPIBase = "https://api.pinboard.in/v1/"

func (p pinboardSource) Name() string { return "pinboard" }

func (p pinboardSource) base() string {
	if p.baseURL != "" {
		return p.baseURL
	}
	return pinboardAPIBase
}

func (p pinboardSource) Configured(cfg *ScribeConfig) (bool, string) {
	ic, ok := integrationConfig(cfg, "pinboard")
	if !ok || !ic.Enabled {
		return false, "not enabled (set integrations.pinboard.enabled: true in scribe.yaml)"
	}
	if integrationToken("pinboard") == "" {
		return false, "no token (set SCRIBE_PINBOARD_TOKEN or integration_tokens.pinboard in ~/.config/scribe/config.yaml)"
	}
	if s := resolvePinboardScope(ic, FetchOpts{}); !validPinboardScope(s) {
		return false, fmt.Sprintf("unknown scope %q (want recent+unread | unread | all)", s)
	}
	return true, ""
}

func validPinboardScope(s string) bool {
	switch s {
	case "recent+unread", "unread", "all":
		return true
	}
	return false
}

// pinboardCursor is the opaque per-source cursor persisted between runs. It is
// just the last-seen posts/update timestamp, which lets Fetch short-circuit
// when nothing changed.
type pinboardCursor struct {
	UpdateTime string `json:"update_time"`
}

func (p pinboardSource) Fetch(ctx context.Context, cfg *ScribeConfig, prev json.RawMessage, opts FetchOpts) ([]SourceItem, json.RawMessage, error) {
	token := integrationToken("pinboard")
	if token == "" {
		return nil, prev, errors.New("no token")
	}
	ic, _ := integrationConfig(cfg, "pinboard")
	scope := resolvePinboardScope(ic, opts)
	if !validPinboardScope(scope) {
		return nil, prev, fmt.Errorf("unknown scope %q (want recent+unread | unread | all)", scope)
	}

	var cur pinboardCursor
	if len(prev) > 0 {
		_ = json.Unmarshal(prev, &cur)
	}

	// Cheap change-probe first. Short-circuit an unchanged account unless the
	// caller forced a run or this is the first pull (no cursor yet).
	upd, err := p.postsUpdate(ctx, token)
	if err != nil {
		return nil, prev, err
	}
	if !opts.Force && cur.UpdateTime != "" && upd != "" && upd == cur.UpdateTime {
		return nil, prev, nil
	}

	posts, err := p.fetchPosts(ctx, token, scope)
	if err != nil {
		return nil, prev, err
	}

	items := make([]SourceItem, 0, len(posts))
	for _, b := range posts {
		if it, ok := b.toItem(); ok {
			items = append(items, it)
		}
	}

	next, err := json.Marshal(pinboardCursor{UpdateTime: upd})
	if err != nil {
		return nil, prev, fmt.Errorf("encode cursor: %w", err)
	}
	return items, next, nil
}

// resolvePinboardScope resolves the effective scope: a per-run override wins,
// then the configured value, then the recent+unread default.
func resolvePinboardScope(ic IntegrationConfig, opts FetchOpts) string {
	return firstNonEmpty(opts.Scope, ic.Scope, "recent+unread")
}

// fetchPosts selects the endpoint(s) for a scope.
//   - all:            posts/all (whole archive)
//   - unread:         posts/all?toread=yes
//   - recent+unread:  posts/recent (last 100) ∪ posts/all?toread=yes
//
// Tag filtering is NOT done here — Pinboard's server-side tag filter is AND
// (and capped at 3 tags), while the user-facing filter is OR. The generic
// driver applies the OR filter over SourceItem.Tags after the fetch, so the
// behavior is identical for every adapter.
//
// The driver's seen-set dedups across runs, so re-returning already-queued
// posts is harmless; only genuinely new hrefs get queued.
func (p pinboardSource) fetchPosts(ctx context.Context, token, scope string) ([]pinboardPost, error) {
	switch scope {
	case "all":
		return p.postsAll(ctx, token, url.Values{})
	case "unread":
		return p.postsAll(ctx, token, url.Values{"toread": {"yes"}})
	case "recent+unread":
		recent, err := p.postsRecent(ctx, token, 100)
		if err != nil {
			return nil, err
		}
		unread, err := p.postsAll(ctx, token, url.Values{"toread": {"yes"}})
		if err != nil {
			return nil, err
		}
		return mergePostsByHash(recent, unread), nil
	default:
		return nil, fmt.Errorf("unknown scope %q", scope)
	}
}

// pinboardPost mirrors a bookmark object in the v1 JSON API.
type pinboardPost struct {
	Href        string `json:"href"`
	Description string `json:"description"` // the title the user gave
	Extended    string `json:"extended"`    // longer note / annotation
	Tags        string `json:"tags"`        // space-separated
	Time        string `json:"time"`        // ISO 8601
	ToRead      string `json:"toread"`      // "yes" | "no"
	Shared      string `json:"shared"`      // "yes" (public) | "no" (private)
	Hash        string `json:"hash"`        // stable per-URL id
}

func (b pinboardPost) toItem() (SourceItem, bool) {
	href := strings.TrimSpace(b.Href)
	if href == "" {
		return SourceItem{}, false
	}
	id := b.Hash
	if id == "" {
		id = href
	}
	ts, _ := time.Parse(time.RFC3339, b.Time) // zero on parse failure — fine
	return SourceItem{
		URL:       href,
		Title:     strings.TrimSpace(b.Description),
		Tags:      strings.Fields(b.Tags),
		Note:      b.Extended,
		CreatedAt: ts,
		ID:        id,
		Unread:    b.ToRead == "yes",
		// Pinboard omits `shared` in some payloads; treat only an explicit
		// "no" as private so a missing field defaults to public (no over-skip).
		Private: b.Shared == "no",
	}, true
}

// mergePostsByHash unions bookmark lists, deduping on hash (href fallback),
// preserving first-seen order.
func mergePostsByHash(lists ...[]pinboardPost) []pinboardPost {
	seen := map[string]bool{}
	var out []pinboardPost
	for _, list := range lists {
		for _, b := range list {
			key := b.Hash
			if key == "" {
				key = b.Href
			}
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, b)
		}
	}
	return out
}

// --- API calls ---

func (p pinboardSource) postsUpdate(ctx context.Context, token string) (string, error) {
	body, err := p.get(ctx, token, "posts/update", url.Values{})
	if err != nil {
		return "", err
	}
	var r struct {
		UpdateTime string `json:"update_time"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("posts/update decode: %w", err)
	}
	return r.UpdateTime, nil
}

func (p pinboardSource) postsAll(ctx context.Context, token string, q url.Values) ([]pinboardPost, error) {
	body, err := p.get(ctx, token, "posts/all", q)
	if err != nil {
		return nil, err
	}
	var posts []pinboardPost
	if err := json.Unmarshal(body, &posts); err != nil {
		return nil, fmt.Errorf("posts/all decode: %w", err)
	}
	return posts, nil
}

func (p pinboardSource) postsRecent(ctx context.Context, token string, count int) ([]pinboardPost, error) {
	body, err := p.get(ctx, token, "posts/recent", url.Values{"count": {strconv.Itoa(count)}})
	if err != nil {
		return nil, err
	}
	var r struct {
		Posts []pinboardPost `json:"posts"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("posts/recent decode: %w", err)
	}
	return r.Posts, nil
}

// get issues one authenticated GET and returns the body. auth_token is kept
// literal (its `username:HEX` colon is a legal query char per RFC 3986) so it
// matches the documented form exactly; other params are URL-encoded.
func (p pinboardSource) get(ctx context.Context, token, endpoint string, q url.Values) ([]byte, error) {
	full := p.base() + endpoint + "?format=json&auth_token=" + token
	if enc := q.Encode(); enc != "" {
		full += "&" + enc
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "scribe-ingest/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s request: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%s: rate limited (429) — try again later", endpoint)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%s: unauthorized (401) — check the API token", endpoint)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d", endpoint, resp.StatusCode)
	}
	// Guard against a runaway body; a full Pinboard archive is a few MB.
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
