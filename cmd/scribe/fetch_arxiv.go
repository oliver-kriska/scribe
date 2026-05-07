package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// arxiv URLs land on /abs/<id>, /pdf/<id>, /pdf/<id>.pdf, or /html/<id>v<n>.
// /abs is the abstract page (what jina/trafilatura currently scrape — abstract
// only). The full paper lives at /html/<id>v1 (HTML, not 100% coverage,
// experimental) or /pdf/<id> (universal). This tier rewrites any arxiv URL
// to the richest form available, optionally enriching frontmatter with the
// arxiv API's clean title/authors/abstract.
//
// arxiv ID forms:
//   - new: 2605.00424, 2605.00424v1, 2605.00424v2 (4-or-5-digit category, dot, 4-or-5-digit sequence)
//   - old: hep-th/0608109, hep-th/0608109v2 (single category, slash, 7-digit sequence)
var arxivIDRE = regexp.MustCompile(`(?:abs|pdf|html|e-print)/(\d{4}\.\d{4,5}|[a-z\-]+/\d{7})(?:v\d+)?(?:\.pdf)?`)

// minHTMLBodyChars is the cutoff under which we treat /html/<id>v1 as a stub
// (e.g., template page that says "HTML version not available") and fall
// through to the PDF tier. The arxiv abstract page itself runs ~600 chars
// after trafilatura, and a real paper is several thousand.
const minHTMLBodyChars = 1500

// arxivAPIQueryURL is the Atom-XML metadata endpoint. id_list accepts the
// versioned or unversioned id; we always pass unversioned to get the latest.
const arxivAPIQueryURL = "https://export.arxiv.org/api/query?id_list="

func isArxivURL(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	return host == "arxiv.org" || host == "export.arxiv.org"
}

// arxivIDFromURL pulls the canonical (unversioned) arxiv ID out of a URL path.
// Returns "" for non-arxiv hosts or paths that don't match a known route.
func arxivIDFromURL(u *url.URL) string {
	if !isArxivURL(u) {
		return ""
	}
	m := arxivIDRE.FindStringSubmatch(u.Path)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// arxivMeta is the subset of the arxiv Atom feed we use for frontmatter.
type arxivMeta struct {
	ID         string
	Title      string
	Authors    []string
	Abstract   string
	Published  string // YYYY-MM-DD if parseable
	Categories []string
}

// atomFeed mirrors just enough of the arxiv API response to extract metadata.
// The real schema is much larger (links, comments, doi, journal_ref, …) but
// we only persist what survives a round-trip through frontmatter.
type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID         string       `xml:"id"`
	Title      string       `xml:"title"`
	Summary    string       `xml:"summary"`
	Published  string       `xml:"published"`
	Authors    []atomAuthor `xml:"author"`
	Categories []atomCat    `xml:"category"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

type atomCat struct {
	Term string `xml:"term,attr"`
}

// fetchArxivMetadata is best-effort. Errors are non-fatal — callers should
// proceed with whatever body they could fetch even if metadata fails.
//
// arxiv recommends ≥3s between requests; rapid back-to-back fetches return
// 429. We honor a Retry-After header (or use 5s) and try once more before
// giving up. export.arxiv.org also sporadically takes >15s on cold queries,
// so the per-attempt timeout is 30s.
func fetchArxivMetadata(ctx context.Context, id string) (arxivMeta, error) {
	for range 2 {
		meta, retryAfter, err := arxivMetadataAttempt(ctx, id)
		if err == nil {
			return meta, nil
		}
		if retryAfter <= 0 {
			return arxivMeta{}, err
		}
		// Honor server-provided Retry-After (or default 5s) before our one retry.
		select {
		case <-ctx.Done():
			return arxivMeta{}, ctx.Err()
		case <-time.After(retryAfter):
		}
	}
	return arxivMeta{}, fmt.Errorf("arxiv api: rate-limited after retry")
}

// arxivMetadataAttempt returns the parsed metadata, a non-zero retryAfter
// duration when the caller should wait and try again, or a terminal error.
func arxivMetadataAttempt(ctx context.Context, id string) (arxivMeta, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", arxivAPIQueryURL+id, nil)
	if err != nil {
		return arxivMeta{}, 0, err
	}
	req.Header.Set("User-Agent", "scribe-ingest/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return arxivMeta{}, 0, fmt.Errorf("arxiv api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		wait := 5 * time.Second
		if hdr := resp.Header.Get("Retry-After"); hdr != "" {
			if secs, perr := time.ParseDuration(hdr + "s"); perr == nil && secs > 0 {
				wait = secs
			}
		}
		return arxivMeta{}, wait, fmt.Errorf("arxiv api status 429")
	}
	if resp.StatusCode != 200 {
		return arxivMeta{}, 0, fmt.Errorf("arxiv api status %d", resp.StatusCode)
	}

	var feed atomFeed
	if err := xml.NewDecoder(resp.Body).Decode(&feed); err != nil {
		return arxivMeta{}, 0, fmt.Errorf("arxiv api decode: %w", err)
	}
	if len(feed.Entries) == 0 {
		return arxivMeta{}, 0, fmt.Errorf("arxiv api: no entries for %s", id)
	}

	e := feed.Entries[0]
	meta := arxivMeta{
		ID:       id,
		Title:    collapseWhitespace(e.Title),
		Abstract: collapseWhitespace(e.Summary),
	}
	for _, a := range e.Authors {
		if name := strings.TrimSpace(a.Name); name != "" {
			meta.Authors = append(meta.Authors, name)
		}
	}
	for _, c := range e.Categories {
		if c.Term != "" {
			meta.Categories = append(meta.Categories, c.Term)
		}
	}
	if t, err := time.Parse(time.RFC3339, e.Published); err == nil {
		meta.Published = t.UTC().Format("2006-01-02")
	}
	return meta, 0, nil
}

// collapseWhitespace flattens the multi-line / multi-space layout the arxiv
// API uses in its title and summary fields ("Title:\n  My Paper") into a
// single clean string.
var wsRunRE = regexp.MustCompile(`\s+`)

func collapseWhitespace(s string) string {
	return strings.TrimSpace(wsRunRE.ReplaceAllString(s, " "))
}

// stripFirstH1 removes only the first '# heading' line plus any blank lines
// that immediately follow it. Other h1s (often section headings in marker
// output) are kept.
func stripFirstH1(body string) string {
	loc := h1RE.FindStringIndex(body)
	if loc == nil {
		return body
	}
	end := loc[1]
	for end < len(body) && (body[end] == '\n' || body[end] == '\r') {
		end++
	}
	return body[:loc[0]] + body[end:]
}

// fetchArxiv routes any arxiv URL through the richest available source: HTML
// version first, PDF + marker as fallback. Metadata enrichment runs *after*
// the body fetch so a slow or rate-limited arxiv API can never block the
// critical path — capture would rather have a body without authors than wait
// 60s for an API that's about to return 429.
func fetchArxiv(ctx context.Context, rawURL string) (fetchResult, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fetchResult{}, fmt.Errorf("parse arxiv URL: %w", err)
	}
	id := arxivIDFromURL(u)
	if id == "" {
		return fetchResult{}, fmt.Errorf("not an arxiv URL: %s", rawURL)
	}

	body, via, err := fetchArxivBody(ctx, rawURL, id)
	if err != nil {
		return fetchResult{}, err
	}

	// Best-effort metadata enrichment. If this fails (rate limit, slow API,
	// network), we still ship the body — title falls back to the body's H1.
	meta, metaErr := fetchArxivMetadata(ctx, id)
	if metaErr != nil {
		logMsg("arxiv", "metadata failed for %s: %v (shipping body without enrichment)", id, metaErr)
	}

	return assembleArxivResult(rawURL, id, meta, body, via), nil
}

// fetchArxivBody returns the markdown body and the via-tag for a paper, or an
// error if every source failed. HTML → PDF+marker → jina, in that order.
func fetchArxivBody(ctx context.Context, rawURL, id string) (string, string, error) {
	htmlURL := "https://arxiv.org/html/" + id + "v1"
	if res, err := fetchTrafilatura(ctx, htmlURL); err == nil && len(strings.TrimSpace(res.Body)) >= minHTMLBodyChars {
		return res.Body, "arxiv-html", nil
	}

	if _, err := exec.LookPath("marker_single"); err != nil {
		logMsg("arxiv", "marker_single not installed; falling back to jina (abstract-only)")
		res, jerr := fetchJina(ctx, rawURL)
		if jerr != nil {
			return "", "", fmt.Errorf("arxiv: html unavailable, marker missing, jina failed: %w", jerr)
		}
		return res.Body, "arxiv-jina", nil
	}

	body, err := fetchArxivPDF(ctx, id)
	if err != nil {
		return "", "", fmt.Errorf("arxiv pdf: %w", err)
	}
	return body, "arxiv-pdf", nil
}

// fetchArxivPDF downloads the PDF to a temp file and runs marker_single over
// it. Returns the converted markdown body. The temp file is cleaned up on
// return regardless of outcome.
func fetchArxivPDF(ctx context.Context, id string) (string, error) {
	pdfURL := "https://arxiv.org/pdf/" + id

	dlCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(dlCtx, "GET", pdfURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "scribe-ingest/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download pdf: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("pdf download status %d", resp.StatusCode)
	}

	tmpDir, err := os.MkdirTemp("", "scribe-arxiv-")
	if err != nil {
		return "", fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Filename matters — marker uses the stem to name its output. Using the
	// arxiv id (with '/' → '_' for old-style ids) makes the temp output
	// predictable.
	stem := strings.ReplaceAll(id, "/", "_")
	pdfPath := filepath.Join(tmpDir, stem+".pdf")
	f, err := os.Create(pdfPath)
	if err != nil {
		return "", fmt.Errorf("create pdf: %w", err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return "", fmt.Errorf("write pdf: %w", err)
	}
	f.Close()

	// convertWithMarker shells out to marker_single, which has its own
	// internal timeout (defaultMarkerTimeoutSec). It doesn't accept a context
	// today, so the outer ctx is intentionally not propagated here — adding
	// ctx through marker is a separate refactor.
	body, _, err := convertWithMarker(pdfPath, "") //nolint:contextcheck // marker has internal timeout, no ctx surface
	if err != nil {
		return "", fmt.Errorf("marker: %w", err)
	}
	return body, nil
}

// assembleArxivResult layers arxiv metadata on top of a fetched body. The
// metadata is rendered as a frontmatter-style preamble inside the body
// (callers like buildCaptureArticle already produce real YAML frontmatter
// around it; this preamble lives below that and is what readers will see
// at the top of the article).
func assembleArxivResult(originalURL, id string, meta arxivMeta, body, via string) fetchResult {
	title := meta.Title
	if title == "" {
		title = firstMarkdownHeading(body)
	}
	if title == "" {
		title = "arxiv:" + id
	}

	var preamble strings.Builder
	preamble.WriteString("# " + title + "\n\n")
	if len(meta.Authors) > 0 {
		fmt.Fprintf(&preamble, "**Authors:** %s\n\n", strings.Join(meta.Authors, ", "))
	}
	if meta.Published != "" {
		fmt.Fprintf(&preamble, "**Published:** %s\n\n", meta.Published)
	}
	if len(meta.Categories) > 0 {
		fmt.Fprintf(&preamble, "**Categories:** %s\n\n", strings.Join(meta.Categories, ", "))
	}
	fmt.Fprintf(&preamble, "**arxiv:** [%s](https://arxiv.org/abs/%s) · [pdf](https://arxiv.org/pdf/%s) · [html](https://arxiv.org/html/%sv1)\n\n", id, id, id, id)
	if meta.Abstract != "" {
		preamble.WriteString("## Abstract\n\n")
		preamble.WriteString(meta.Abstract)
		preamble.WriteString("\n\n## Full text\n\n")
	} else {
		preamble.WriteString("---\n\n")
	}

	// If the fetched body already starts with the title as an H1, drop only
	// that first heading line (not all h1s — the paper itself may use h1 for
	// section headings) to avoid duplicating the title under our preamble.
	bodyTrimmed := strings.TrimSpace(body)
	if title != "" {
		if h := firstMarkdownHeading(bodyTrimmed); h != "" && strings.EqualFold(strings.TrimSpace(h), strings.TrimSpace(title)) {
			bodyTrimmed = strings.TrimSpace(stripFirstH1(bodyTrimmed))
		}
	}

	return fetchResult{
		Title: title,
		Body:  preamble.String() + bodyTrimmed + "\n",
		Via:   via,
		URL:   originalURL,
	}
}
