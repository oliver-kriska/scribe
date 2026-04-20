package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// stubParkedHeader is the top of wiki/_unfetched-links.md. We keep it
// append-only so repeat syncs don't churn the file.
const stubParkedHeader = `# Unfetched links

Links captured from iMessage (or other surfaces) where the page body
could not be fetched. Review manually — open each, paste the title and
a one-line summary, or delete the entry if it's not worth keeping.

Re-run ` + "`scribe capture --refetch`" + ` to retry fetching before
parking here.

`

// thinBodyWordFloor is the minimum word count a raw article's body must
// exceed to be treated as "absorbable". Anything under this is a link
// shell — an X/Twitter URL where the fetcher returned 403, a GitHub gist
// reference without the gist body, etc. Picked empirically: the stub
// template is 30 words; a real tweet usually produces 80+ words after
// FxTwitter JSON expands author/date/text; genuinely short news blurbs
// hit 200+ once trafilatura adds headline + dek + lede.
const thinBodyWordFloor = 60

// rawArticleIsStub returns true when a raw article carries no useful
// content for absorb. Covers three cases:
//
//  1. fetched_via: stub — capture.go wrote a URL-only shell (fetch off
//     or failed).
//  2. body under thinBodyWordFloor — fetcher succeeded but got a login
//     wall, rate-limit blurb, or a 30-word "content unknown" echo.
//  3. body that is effectively just the source URL echoed back.
//
// All three get routed to wiki/_unfetched-links.md for manual review
// instead of wasting an absorb LLM call on empty text.
func rawArticleIsStub(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	raw, err := parseFrontmatterRaw(data)
	if err != nil {
		return false
	}
	if via, _ := raw["fetched_via"].(string); strings.EqualFold(via, "stub") {
		return true
	}
	body := stripFrontmatter(string(data))
	words := countWords(body)
	if words < thinBodyWordFloor {
		return true
	}
	// Body that is essentially a URL echo: < ~20 non-URL words.
	bodyNoURLs := strings.Join(strings.Fields(stripURLs(body)), " ")
	return countWords(bodyNoURLs) < 20
}

// stripURLs removes http(s) URLs from a string so we can compute a
// URL-free word count. Used as a "is the body just a link echo" check.
func stripURLs(s string) string {
	return captureURLTextRE.ReplaceAllString(s, " ")
}

// rawArticleSourceURL returns the source_url field from a raw article's
// frontmatter, or "" when absent. Used by the link-parking flow to record
// the original URL in the parked-links file.
func rawArticleSourceURL(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	raw, err := parseFrontmatterRaw(data)
	if err != nil {
		return ""
	}
	if u, ok := raw["source_url"].(string); ok {
		return u
	}
	return ""
}

// parkStubLink appends the source URL of an unfetched stub article to
// wiki/_unfetched-links.md. Returns true on a successful write, false on
// any error (missing URL, unwriteable file). The original stub file is
// left on disk — idempotency comes from the absorb log, and the user
// may still want to manually enrich it before deleting.
func parkStubLink(root, rawPath string) bool {
	url := rawArticleSourceURL(rawPath)
	if url == "" {
		return false
	}
	path := filepath.Join(root, "wiki", "_unfetched-links.md")
	// Idempotency — don't duplicate entries for the same URL.
	if existing, err := os.ReadFile(path); err == nil && strings.Contains(string(existing), url) {
		return false
	}
	var content string
	if existing, err := os.ReadFile(path); err == nil && len(existing) > 0 {
		content = string(existing)
	} else {
		content = stubParkedHeader
	}
	today := time.Now().UTC().Format("2006-01-02")
	line := fmt.Sprintf("- %s — %s (from %s)\n", today, url, filepath.Base(rawPath))
	return os.WriteFile(path, []byte(content+line), 0o644) == nil
}

// CaptureRefetchCmd retries fetching every raw article whose frontmatter
// still says `fetched_via: stub`. On success the article body is
// replaced; on failure the URL is appended to wiki/_unfetched-links.md
// so a person can handle it manually (copy-paste, delete, or annotate).
type CaptureRefetchCmd struct {
	Max    int  `help:"Max stubs to refetch per run (0 = no limit)." default:"20"`
	DryRun bool `help:"List stubs without fetching or writing." short:"n"`
	Park   bool `help:"On fetch failure, park the URL in wiki/_unfetched-links.md (default: true)." default:"true" negatable:""`
}

func (c *CaptureRefetchCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	rawDir := filepath.Join(root, "raw", "articles")
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		return fmt.Errorf("read raw/articles: %w", err)
	}

	refetched, parked, scanned := 0, 0, 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if c.Max > 0 && (refetched+parked) >= c.Max {
			break
		}
		rawPath := filepath.Join(rawDir, e.Name())
		if !rawArticleIsStub(rawPath) {
			continue
		}
		scanned++
		url := rawArticleSourceURL(rawPath)
		if url == "" {
			logMsg("capture", "skip %s: no source_url in frontmatter", e.Name())
			continue
		}

		if c.DryRun {
			logMsg("capture", "would refetch %s: %s", e.Name(), url)
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		res, ferr := fetchURL(ctx, url, "auto")
		cancel()
		if ferr != nil || strings.TrimSpace(res.Body) == "" {
			if c.Park && parkStubLink(root, rawPath) {
				parked++
				logMsg("capture", "parked %s (fetch failed: %v)", e.Name(), ferr)
			} else {
				logMsg("capture", "fetch failed for %s: %v", e.Name(), ferr)
			}
			continue
		}

		if err := rewriteRawArticleBody(rawPath, res); err != nil {
			logMsg("capture", "rewrite failed for %s: %v", e.Name(), err)
			continue
		}
		refetched++
		logMsg("capture", "refetched %s (via %s)", e.Name(), res.Via)
		time.Sleep(1 * time.Second)
	}

	logMsg("capture", "refetch pass done: scanned=%d refetched=%d parked=%d", scanned, refetched, parked)
	return nil
}

// rewriteRawArticleBody replaces the body of a stub raw article with a
// freshly fetched one and updates `fetched_via:` in the frontmatter so
// the article stops registering as a stub on subsequent passes.
func rewriteRawArticleBody(path string, res fetchResult) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	s := string(data)
	if !strings.HasPrefix(s, "---") {
		return fmt.Errorf("no frontmatter")
	}
	end := strings.Index(s[3:], "\n---")
	if end < 0 {
		return fmt.Errorf("no closing frontmatter")
	}
	fmBlock := s[3 : end+3]

	// Replace fetched_via line.
	lines := strings.Split(fmBlock, "\n")
	foundVia := false
	for i, line := range lines {
		key, _, ok := splitFrontmatterLine(line)
		if !ok {
			continue
		}
		if key == "fetched_via" {
			lines[i] = "fetched_via: " + res.Via
			foundVia = true
			break
		}
	}
	if !foundVia {
		lines = append(lines, "fetched_via: "+res.Via)
	}
	newFM := strings.Join(lines, "\n")

	newContent := "---" + newFM + "\n---\n\n" + res.Body + "\n"
	return os.WriteFile(path, []byte(newContent), 0o644)
}
