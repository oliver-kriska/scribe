package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// fetchResult is what a successful URL fetch produces.
type fetchResult struct {
	Title string
	Body  string // markdown
	Via   string // fxtwitter|trafilatura|jina
	URL   string // final URL after redirects (best effort)
}

// fetchURL routes a URL through the appropriate fetcher and returns the result.
// Order: FxTwitter for X/Twitter → trafilatura (local) → Jina Reader (fallback).
// forced lets callers pin a specific fetcher ("fxtwitter", "trafilatura", "jina").
func fetchURL(ctx context.Context, rawURL, forced string) (fetchResult, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" {
		return fetchResult{}, fmt.Errorf("invalid URL: %s", rawURL)
	}

	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")

	isTwitter := host == "x.com" || host == "twitter.com" || host == "mobile.twitter.com"

	switch forced {
	case "fxtwitter":
		return fetchFxTwitter(ctx, rawURL)
	case "trafilatura":
		return fetchTrafilatura(ctx, rawURL)
	case "jina":
		return fetchJina(ctx, rawURL)
	case "", "auto":
		// fall through to auto logic
	default:
		return fetchResult{}, fmt.Errorf("unknown fetcher: %s", forced)
	}

	if isTwitter {
		if res, err := fetchFxTwitter(ctx, rawURL); err == nil {
			return res, nil
		}
		// Fall through if FxTwitter fails.
	}

	// Try trafilatura first — local, fast, works for most articles.
	if res, err := fetchTrafilatura(ctx, rawURL); err == nil && strings.TrimSpace(res.Body) != "" {
		return res, nil
	}

	// Jina Reader fallback handles JS-heavy pages.
	return fetchJina(ctx, rawURL)
}

// --- FxTwitter ---

type fxTweetResp struct {
	Code    int `json:"code"`
	Message string
	Tweet   struct {
		ID     string `json:"id"`
		URL    string `json:"url"`
		Text   string `json:"text"`
		Author struct {
			Name       string `json:"name"`
			ScreenName string `json:"screen_name"`
		} `json:"author"`
		CreatedAt string `json:"created_at"`
	} `json:"tweet"`
}

func fetchFxTwitter(ctx context.Context, rawURL string) (fetchResult, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fetchResult{}, err
	}
	// api.fxtwitter.com expects /{user}/status/{id} — just reuse the path.
	apiURL := "https://api.fxtwitter.com" + u.Path

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return fetchResult{}, err
	}
	req.Header.Set("User-Agent", "scribe-ingest/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fetchResult{}, fmt.Errorf("fxtwitter request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fetchResult{}, fmt.Errorf("fxtwitter status %d", resp.StatusCode)
	}

	var data fxTweetResp
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return fetchResult{}, fmt.Errorf("fxtwitter decode: %w", err)
	}
	if data.Code != 200 || data.Tweet.ID == "" {
		return fetchResult{}, fmt.Errorf("fxtwitter: no tweet (code=%d msg=%s)", data.Code, data.Message)
	}

	title := fmt.Sprintf("Tweet by %s (@%s)", data.Tweet.Author.Name, data.Tweet.Author.ScreenName)
	body := fmt.Sprintf("> %s\n\n— [@%s](%s) on %s\n\n[Original tweet](%s)\n",
		strings.ReplaceAll(data.Tweet.Text, "\n", "\n> "),
		data.Tweet.Author.ScreenName,
		"https://x.com/"+data.Tweet.Author.ScreenName,
		data.Tweet.CreatedAt,
		data.Tweet.URL,
	)

	return fetchResult{
		Title: title,
		Body:  body,
		Via:   "fxtwitter",
		URL:   data.Tweet.URL,
	}, nil
}

// --- trafilatura (local binary) ---

func fetchTrafilatura(ctx context.Context, rawURL string) (fetchResult, error) {
	if _, err := exec.LookPath("trafilatura"); err != nil {
		return fetchResult{}, fmt.Errorf("trafilatura not installed")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Use JSON output + --with-metadata so we get the real <title> tag and
	// author, not a guess pulled from the body text.
	cmd := exec.CommandContext(ctx, "trafilatura",
		"--URL", rawURL,
		"--no-comments",
		"--with-metadata",
		"--json",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fetchResult{}, fmt.Errorf("trafilatura: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return fetchResult{}, fmt.Errorf("trafilatura: empty output (stderr: %s)", strings.TrimSpace(stderr.String()))
	}

	var meta struct {
		Title string `json:"title"`
		Raw   string `json:"raw_text"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return fetchResult{}, fmt.Errorf("trafilatura: decode json: %w", err)
	}

	body := strings.TrimSpace(meta.Text)
	if body == "" {
		body = strings.TrimSpace(meta.Raw)
	}
	if body == "" {
		return fetchResult{}, fmt.Errorf("trafilatura: empty body")
	}

	title := strings.TrimSpace(meta.Title)
	if title == "" {
		title = firstMarkdownHeading(body)
	}

	return fetchResult{
		Title: title,
		Body:  body,
		Via:   "trafilatura",
		URL:   rawURL,
	}, nil
}

// firstMarkdownHeading returns the first '# heading' line or the first non-empty
// line (truncated). Used as a last-resort title fallback.
var h1RE = regexp.MustCompile(`(?m)^#\s+(.+)$`)

func firstMarkdownHeading(body string) string {
	if m := h1RE.FindStringSubmatch(body); len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			if len(line) > 100 {
				line = line[:100]
			}
			return line
		}
	}
	return "Untitled"
}

// --- Jina Reader ---

func fetchJina(ctx context.Context, rawURL string) (fetchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	jinaURL := "https://r.jina.ai/" + rawURL
	var raw []byte
	if err := WithRetry(ctx, defaultRetryConfig(), func() error {
		req, err := http.NewRequestWithContext(ctx, "GET", jinaURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "scribe-ingest/1.0")
		req.Header.Set("Accept", "text/markdown")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("jina request: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("jina status %d", resp.StatusCode)
		}
		raw, err = io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("jina read: %w", err)
		}
		return nil
	}); err != nil {
		return fetchResult{}, err
	}
	body := strings.TrimSpace(string(raw))
	if body == "" {
		return fetchResult{}, fmt.Errorf("jina: empty body")
	}

	// Jina's markdown header includes "Title: ..." and "URL Source: ..." lines
	// before the body — extract title and strip the preamble.
	title, body := parseJinaEnvelope(body)
	return fetchResult{
		Title: title,
		Body:  body,
		Via:   "jina",
		URL:   rawURL,
	}, nil
}

// parseJinaEnvelope extracts "Title: ..." from Jina Reader output and returns
// the remaining body with the metadata header stripped.
func parseJinaEnvelope(body string) (string, string) {
	title := "Untitled"
	lines := strings.Split(body, "\n")
	bodyStart := 0
	for i, line := range lines {
		if after, ok := strings.CutPrefix(line, "Title:"); ok {
			title = strings.TrimSpace(after)
		}
		// Metadata headers end at the first blank line.
		if line == "" {
			bodyStart = i + 1
			break
		}
	}
	rest := strings.Join(lines[bodyStart:], "\n")
	return title, strings.TrimSpace(rest)
}
