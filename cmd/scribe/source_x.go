package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// xSource pulls the authenticated user's X/Twitter bookmarks via the official
// v2 API. Like Pinboard it is a URL producer only: it queues tweet permalinks
// (with the tweet text as title + note) into output/inbox/ and lets the
// existing ingest-drain path fetch the content — scribe's fetch cascade already
// has an fxtwitter tier that renders x.com URLs, so no page content is fetched
// here and the adapter stays deterministic.
//
// Transport is the official `xurl` CLI (github.com/xdevplatform/xurl), which
// owns the whole OAuth 2.0 PKCE dance — browser flow, token storage in
// ~/.xurl, auto-refresh. scribe never implements OAuth or stores X tokens, and
// no new Go dependency is added (exec-only), matching the repo's "prefer
// shelling out to optional tools" convention.
//
// Cost model: owned reads are billed $0.001/resource, so a page of 100 costs
// $0.10. A cheap max_results=1 probe gates every run (unchanged newest id ⇒
// $0.001 no-op), and a hard 9-page cap bounds a full pull to the API's
// documented ~800-bookmark reachable window.
type xSource struct{}

func (x xSource) Name() string { return "x" }

// xurlRun executes `xurl <path>` and returns its stdout. It is a package
// variable (mirroring the newLLMProvider seam in llm.go) purely so tests can
// stub the transport without a real xurl binary or network; production code
// never reassigns it. On a non-zero exit the returned error carries xurl's
// stderr so auth/scope problems surface to the caller.
var xurlRun = func(ctx context.Context, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "xurl", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return out, nil
}

func (x xSource) Configured(cfg *ScribeConfig) (bool, string) {
	ic, ok := integrationConfig(cfg, "x")
	if !ok || !ic.Enabled {
		return false, "not enabled (set integrations.x.enabled: true in scribe.yaml)"
	}
	// No token check — xurl owns auth. A network probe would cost money, so
	// Configured stays offline: PATH presence is the gate, an auth failure
	// surfaces at Fetch time with the re-auth hint.
	if _, err := exec.LookPath("xurl"); err != nil {
		return false, "xurl not on PATH — brew install --cask xdevplatform/tap/xurl, then `xurl auth oauth2` (see README)"
	}
	// Cheap offline auth check: xurl keeps its tokens under ~/.xurl, so its
	// absence means `xurl auth oauth2` was never run — soft-skip with the hint
	// instead of hard-failing every cron pull at Fetch time. (An expired token
	// still surfaces at Fetch; presence is all that's checkable for free.)
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(home, ".xurl")); err != nil {
			return false, "xurl is not authenticated (no ~/.xurl) — run `xurl auth oauth2` (see README)"
		}
	}
	return true, ""
}

// Setup is the guided checklist behind `scribe pull x --setup`. X is the one
// integration whose setup spans a developer account, an OAuth app, prepaid
// billing, and a separate CLI — this re-checks each prerequisite in
// dependency order, prints the exact fix for the first failing step, and
// finishes with a single live /2/users/me call (one owned read, ~$0.001) to
// prove auth end-to-end. Offline checks run first so the network — and the
// meter — is only touched once everything local passes.
func (x xSource) Setup(ctx context.Context, cfg *ScribeConfig, out io.Writer) error {
	if ic, ok := integrationConfig(cfg, "x"); !ok || !ic.Enabled {
		fmt.Fprintln(out, "✗ integrations.x is not enabled — add to scribe.yaml:")
		fmt.Fprintln(out, "      integrations:")
		fmt.Fprintln(out, "        x:")
		fmt.Fprintln(out, "          enabled: true")
		return errSetupIncomplete("x")
	}
	fmt.Fprintln(out, "✓ integrations.x.enabled in scribe.yaml")

	if _, err := exec.LookPath("xurl"); err != nil {
		fmt.Fprintln(out, "✗ xurl not on PATH — install X's official API CLI:")
		fmt.Fprintln(out, "      brew install --cask xdevplatform/tap/xurl")
		fmt.Fprintln(out, "      # or: go install github.com/xdevplatform/xurl@latest")
		return errSetupIncomplete("x")
	}
	fmt.Fprintln(out, "✓ xurl on PATH")

	if home, err := os.UserHomeDir(); err == nil {
		if _, statErr := os.Stat(filepath.Join(home, ".xurl")); statErr != nil {
			fmt.Fprintln(out, "✗ no ~/.xurl — xurl has never authenticated. One-time setup:")
			fmt.Fprintln(out, "    1. developer.x.com → create a Project + App; in the app's user")
			fmt.Fprintln(out, "       authentication settings enable OAuth 2.0, redirect URI")
			fmt.Fprintln(out, "       http://localhost:8080/callback, scopes bookmark.read")
			fmt.Fprintln(out, "       tweet.read users.read offline.access")
			fmt.Fprintln(out, "    2. add a few dollars of prepaid credit (reads bill $0.001 each)")
			fmt.Fprintln(out, "    3. xurl auth apps add scribe --client-id YOUR_ID --client-secret YOUR_SECRET")
			fmt.Fprintln(out, "    4. xurl auth oauth2 --app scribe   # one-time browser consent")
			return errSetupIncomplete("x")
		}
		fmt.Fprintln(out, "✓ ~/.xurl present (xurl has authenticated before)")
	}

	fmt.Fprintln(out, "→ live check: GET /2/users/me (one owned read, ~$0.001)")
	body, err := x.get(ctx, "/2/users/me")
	if err != nil {
		fmt.Fprintf(out, "✗ live check failed: %v\n", err)
		return errSetupIncomplete("x")
	}
	var resp xUserResponse
	if err := json.Unmarshal(body, &resp); err != nil || resp.Data.ID == "" {
		fmt.Fprintf(out, "✗ live check returned no user object (decode error: %v)\n", err)
		return errSetupIncomplete("x")
	}
	fmt.Fprintf(out, "✓ authenticated as @%s (id %s)\n", resp.Data.Username, resp.Data.ID)
	fmt.Fprintln(out, "ready — next: scribe pull x -n   # dry-run: shows what would be queued")
	return nil
}

// xCursor is the opaque per-source cursor persisted between runs. user_id is
// resolved once and cached (saves a /2/users/me call per run); newest_id is the
// id of the most recent bookmark seen last run, used both for the cheap probe
// and the stop-on-seen boundary during pagination.
type xCursor struct {
	UserID   string `json:"user_id"`
	NewestID string `json:"newest_id"`
}

// xMaxBookmarkPages caps a single run at 9 pages of 100 (~800 bookmarks, the
// API's documented reachable depth). This is the money guard: each resource
// read is billed and a buggy/looping next_token could otherwise page forever,
// so the loop hard-stops here regardless of what the server returns.
const xMaxBookmarkPages = 9

func (x xSource) Fetch(ctx context.Context, _ *ScribeConfig, prev json.RawMessage, opts FetchOpts) ([]SourceItem, json.RawMessage, error) {
	var cur xCursor
	if len(prev) > 0 {
		_ = json.Unmarshal(prev, &cur) // tolerant: a malformed cursor re-resolves
	}

	// Resolve + cache the user id once. /2/users/me is a fixed, cheap call and
	// the id never changes, so later runs skip it.
	if cur.UserID == "" {
		id, err := x.resolveUserID(ctx)
		if err != nil {
			return nil, prev, err
		}
		cur.UserID = id
	}

	// Cheap probe (cost guard): fetch a single bookmark. Bookmarks are LIFO, so
	// if the newest id is unchanged since last run nothing was added — bail for
	// ~$0.001. Skipped on the first run (no newest_id yet) and under --force.
	// The probe is purely a cost optimization, so any probe failure degrades to
	// the full walk instead of failing the run — e.g. if the endpoint rejects
	// max_results=1 (minimum page size unverified), the 100-per-page walk still
	// works and stops at the boundary after one page. Real failures (auth,
	// network) repeat identically in the walk and surface there.
	if !opts.Force && cur.NewestID != "" {
		probe, err := x.fetchBookmarksPage(ctx, cur.UserID, "", 1)
		switch {
		case err != nil:
			logMsg("pull", "x: probe failed (%v) — falling back to a full page walk", err)
		case len(probe.Data) > 0 && probe.Data[0].ID == cur.NewestID:
			next, err := json.Marshal(cur)
			if err != nil {
				return nil, prev, fmt.Errorf("x: encode cursor: %w", err)
			}
			return nil, next, nil
		}
	}

	// Full incremental pull: page newest→oldest, stopping at the known id.
	items, newestID, more, err := x.collect(ctx, cur.UserID, cur.NewestID)
	if err != nil {
		return nil, prev, err
	}
	switch {
	case more && cur.NewestID != "":
		// The page cap ended the walk before the previous boundary was
		// reached: everything between the cap and the old newest_id was never
		// fetched. Advancing the cursor would skip that gap forever (the probe
		// would short-circuit every later run), so keep the old boundary — the
		// next run re-pages and the driver's seen-set absorbs the overlap.
		// Mirrors the driver's own refusal to advance a capped run's cursor.
		logMsg("pull", "x: %d-page cap hit before reaching the previous boundary — cursor not advanced, next run re-pages", xMaxBookmarkPages)
	case newestID != "":
		cur.NewestID = newestID
	}
	next, err := json.Marshal(cur)
	if err != nil {
		return nil, prev, fmt.Errorf("x: encode cursor: %w", err)
	}
	return items, next, nil
}

// collect pages bookmarks front-to-back, collecting items until it reaches the
// known id (everything at/after it was queued on a prior run — the driver's
// seen-set dedups the small overlap window too) or runs out of pages. The
// newest id seen this run is returned to become the next cursor. The page loop
// is hard-capped at xMaxBookmarkPages so a runaway next_token can't rack up
// per-read cost; more=true reports that the cap fired while the server still
// offered a next page, so the caller knows the walk has an unfetched gap and
// must not advance its boundary across it.
func (x xSource) collect(ctx context.Context, userID, knownID string) (items []SourceItem, newestID string, more bool, err error) {
	token := ""
	for range xMaxBookmarkPages {
		resp, err := x.fetchBookmarksPage(ctx, userID, token, 100)
		if err != nil {
			return nil, "", false, err
		}
		users := usernameIndex(resp.Includes.Users)
		for _, tw := range resp.Data {
			if tw.ID == "" {
				continue // malformed row: no id means no permalink, nothing to queue
			}
			if newestID == "" {
				newestID = tw.ID // first (newest) bookmark seen this run
			}
			if knownID != "" && tw.ID == knownID {
				return items, newestID, false, nil // boundary reached; rest is already seen
			}
			items = append(items, tw.toItem(users))
		}
		token = resp.Meta.NextToken
		if token == "" {
			return items, newestID, false, nil // no more pages
		}
	}
	return items, newestID, true, nil // cap fired with a live next_token
}

// resolveUserID fetches the authenticated user's numeric id via /2/users/me.
func (x xSource) resolveUserID(ctx context.Context) (string, error) {
	body, err := x.get(ctx, "/2/users/me")
	if err != nil {
		return "", err
	}
	var resp xUserResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("x: decode /2/users/me: %w", err)
	}
	if resp.Data.ID == "" {
		return "", errors.New("x: /2/users/me returned no user id")
	}
	return resp.Data.ID, nil
}

// fetchBookmarksPage fetches one page of bookmarks. expansions=author_id plus
// user.fields=username let each tweet's permalink use the author handle;
// tweet.fields=created_at carries the tweet's post time (the API doesn't
// expose when the bookmark was saved) and entities carries the tweet's
// #hashtags, which become the item's Tags so the driver's OR tag filter has
// something real to match against.
func (x xSource) fetchBookmarksPage(ctx context.Context, userID, paginationToken string, maxResults int) (xBookmarksResponse, error) {
	path := fmt.Sprintf("/2/users/%s/bookmarks?max_results=%d&expansions=author_id&tweet.fields=created_at,entities&user.fields=username", url.PathEscape(userID), maxResults)
	if paginationToken != "" {
		path += "&pagination_token=" + url.QueryEscape(paginationToken)
	}
	body, err := x.get(ctx, path)
	if err != nil {
		return xBookmarksResponse{}, err
	}
	var resp xBookmarksResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return xBookmarksResponse{}, fmt.Errorf("x: decode bookmarks page: %w", err)
	}
	return resp, nil
}

// get issues one xurl GET and returns the response body, wrapping failures with
// the endpoint path. An authentication failure appends the re-auth hint so a
// stale/absent token has an actionable fix.
func (x xSource) get(ctx context.Context, path string) ([]byte, error) {
	out, err := xurlRun(ctx, path)
	if err != nil {
		if mentionsAuth(err.Error()) {
			return nil, fmt.Errorf("x: xurl %s: %w — run `xurl auth oauth2` to (re)authenticate", path, err)
		}
		return nil, fmt.Errorf("x: xurl %s: %w", path, err)
	}
	return out, nil
}

// mentionsAuth reports whether an xurl error looks like an authentication
// failure (stale/missing token), so get can attach the re-auth hint.
func mentionsAuth(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "authenticat") || strings.Contains(s, "unauthorized") || strings.Contains(s, "401")
}

// --- response envelopes (tolerant: missing fields decode to zero, never panic) ---

// xUserResponse is the /2/users/me envelope. Only the id is used.
type xUserResponse struct {
	Data struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"data"`
}

// xBookmarksResponse is the /2/users/:id/bookmarks envelope.
type xBookmarksResponse struct {
	Data     []xTweet `json:"data"`
	Includes struct {
		Users []xUser `json:"users"`
	} `json:"includes"`
	Meta struct {
		NextToken   string `json:"next_token"`
		ResultCount int    `json:"result_count"`
	} `json:"meta"`
}

type xTweet struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	AuthorID  string `json:"author_id"`
	CreatedAt string `json:"created_at"`
	Entities  struct {
		Hashtags []struct {
			Tag string `json:"tag"`
		} `json:"hashtags"`
	} `json:"entities"`
}

type xUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// usernameIndex maps author id → username from includes.users so a tweet's
// permalink can use the handle; ids absent from the block fall back to /i/.
func usernameIndex(users []xUser) map[string]string {
	idx := make(map[string]string, len(users))
	for _, u := range users {
		if u.ID != "" {
			idx[u.ID] = u.Username
		}
	}
	return idx
}

func (t xTweet) toItem(users map[string]string) SourceItem {
	permalink := "https://x.com/i/status/" + t.ID
	if h := users[t.AuthorID]; h != "" {
		permalink = "https://x.com/" + h + "/status/" + t.ID
	}
	created, _ := time.Parse(time.RFC3339, t.CreatedAt) // zero on parse failure — fine
	// The tweet's #hashtags are the only tag-like signal a bookmark carries;
	// surfacing them as Tags is what makes a configured integrations.x.tags
	// filter match anything at all (the driver filters on Tags before queueing).
	var tags []string
	for _, h := range t.Entities.Hashtags {
		if h.Tag != "" {
			tags = append(tags, h.Tag)
		}
	}
	return SourceItem{
		URL:       permalink,
		Title:     tweetTitle(t.Text),
		Note:      t.Text, // full text survives later tweet deletion
		CreatedAt: created,
		Tags:      tags,
		ID:        t.ID,
		Unread:    false,
		// Bookmarked tweets are public content; the bookmark *list* is private
		// but that's the whole point of pulling it, so never over-skip.
		Private: false,
	}
}

// tweetTitle collapses tweet text to a single line and truncates to ~80 runes
// for a queue-entry title. The full text still rides along as the note.
func tweetTitle(text string) string {
	title := strings.Join(strings.Fields(text), " ")
	if r := []rune(title); len(r) > 80 {
		title = strings.TrimSpace(string(r[:80]))
	}
	return title
}
