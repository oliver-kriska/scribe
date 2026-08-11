package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
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
		return nil, prev, pinboardError(token, "encode cursor: %v", err)
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
		return "", pinboardError(token, "posts/update decode: %v", err)
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
		return nil, pinboardError(token, "posts/all decode: %v", err)
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
		return nil, pinboardError(token, "posts/recent decode: %v", err)
	}
	return r.Posts, nil
}

// pinboardHTTPClient refuses cross-host redirects. The auth token travels in
// the query string (Pinboard's documented form), so following a redirect to
// another host would hand the credential to that host verbatim. Same-host
// redirects stay allowed. The ctx deadline set by pullSource bounds every
// request, so no client-level Timeout is needed.
var pinboardHTTPClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("refusing cross-host redirect to %q (the auth token rides in the query string)", req.URL.Host)
		}
		return nil
	},
}

// redactSecret removes an API token from any string that could reach a log
// line, a run record, or a terminal.
//
// This is not belt-and-braces: Go's net/http builds transport failures as
// *url.Error, and stripPassword only removes *userinfo* passwords — query
// parameters are preserved verbatim. So a plain DNS or dial failure yields
//
//	Get "https://api.pinboard.in/v1/posts/update?format=json&auth_token=user:HEX": dial tcp ...
//
// which would otherwise land in output/runs/*.jsonl, /tmp/scribe-pull.log, and
// `scribe doctor --section errors` output. A Pinboard token grants full
// read+write on the account, so it must never survive into any of those.
func redactSecret(s, token string) string {
	for _, term := range secretRedactionTerms(token) {
		s = strings.ReplaceAll(s, term, "REDACTED")
	}
	return s
}

// secretRedactionTerms lists every literal that must not survive redaction:
// the whole `username:HEX` token, its URL-escaped form, and the HEX half on
// its own (in case something splits the pair). Longest first, so the full
// pair is replaced before either half can match inside it. The username half
// is deliberately NOT redacted — it is not the secret, and blanking a short
// username would mangle unrelated text.
func secretRedactionTerms(token string) []string {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	terms := []string{token, url.QueryEscape(token)}
	if _, secret, ok := strings.Cut(token, ":"); ok && strings.TrimSpace(secret) != "" {
		terms = append(terms, secret, url.QueryEscape(secret))
	}
	sort.Slice(terms, func(i, j int) bool { return len(terms[i]) > len(terms[j]) })
	return terms
}

// pinboardError builds an error whose message is guaranteed free of the API
// token. The chain is deliberately FLATTENED (errors.New, not %w): keeping the
// original *url.Error reachable would keep the token reachable with it, since
// any later caller doing errors.Unwrap(err).Error() would print the raw URL.
// Nothing in the pull path does errors.Is/As on these, so no behavior is lost.
func pinboardError(token, format string, args ...any) error {
	return errors.New(redactSecret(fmt.Sprintf(format, args...), token))
}

// get issues one authenticated GET and returns the body. auth_token is kept
// literal (its `username:HEX` colon is a legal query char per RFC 3986) so it
// matches the documented form exactly; other params are URL-encoded.
//
// EVERY error path here runs through pinboardError — the request URL carries
// the token, so an unsanitized error escaping this function is a credential
// leak (see redactSecret).
func (p pinboardSource) get(ctx context.Context, token, endpoint string, q url.Values) ([]byte, error) {
	full := p.base() + endpoint + "?format=json&auth_token=" + token
	if enc := q.Encode(); enc != "" {
		full += "&" + enc
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		// url.Parse failures embed the whole URL in the message.
		return nil, pinboardError(token, "%s: build request: %v", endpoint, err)
	}
	req.Header.Set("User-Agent", "scribe-ingest/1.0")
	resp, err := pinboardHTTPClient.Do(req)
	if err != nil {
		// *url.Error — carries the token-bearing URL. Also the path a refused
		// cross-host redirect takes.
		return nil, pinboardError(token, "%s request: %v", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, pinboardError(token, "%s: rate limited (429) — try again later", endpoint)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, pinboardError(token, "%s: unauthorized (401) — check the API token", endpoint)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, pinboardError(token, "%s: status %d", endpoint, resp.StatusCode)
	}
	// Guard against a runaway body; a full Pinboard archive is a few MB.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, pinboardError(token, "%s: read body: %v", endpoint, err)
	}
	return body, nil
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
