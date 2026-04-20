package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// IngestCmd groups URL capture operations. Default flow: queue a URL into the
// inbox for later fetching. Drain processes the inbox. --now short-circuits
// the queue and fetches synchronously.
type IngestCmd struct {
	URL   IngestURLCmd   `cmd:"" help:"Queue a URL for ingestion (writes to output/inbox/, returns fast)."`
	Drain IngestDrainCmd `cmd:"" help:"Process queued URLs from output/inbox/ into raw/articles/."`
}

// --- ingest URL ---

type IngestURLCmd struct {
	URL     string   `arg:"" help:"URL to ingest."`
	Title   string   `help:"Override the article title."`
	Tag     []string `help:"Tag to add to frontmatter (repeatable)." short:"t"`
	Domain  string   `help:"Domain tag (default: general)." default:"general"`
	Now     bool     `help:"Fetch and write immediately instead of queueing."`
	Absorb  bool     `help:"Also contextualize + absorb synchronously after ingest. Implies --now. Good for Raycast/CLI shortcuts." short:"a"`
	Fetcher string   `help:"Force a specific fetcher: auto|fxtwitter|trafilatura|jina." default:"auto"`
	DryRun  bool     `help:"Print what would happen without writing." short:"n"`
}

func (c *IngestURLCmd) Run() error {
	u, err := url.ParseRequestURI(c.URL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid URL: scheme %q not supported (need http/https)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid URL: missing host")
	}

	root, err := kbDir()
	if err != nil {
		return err
	}

	// --absorb implies --now: we can't chain contextualize+absorb against a
	// queued .url entry that hasn't been fetched yet.
	if c.Absorb {
		c.Now = true
	}

	if !c.Now {
		return ingestQueue(root, c.URL, c.Title, c.Tag, c.Domain, c.Fetcher, c.DryRun)
	}

	rawPath, err := ingestFetchAndWriteReturn(root, c.URL, c.Title, c.Tag, c.Domain, c.Fetcher, c.DryRun)
	if err != nil {
		return err
	}
	if !c.Absorb || c.DryRun {
		return nil
	}
	return contextualizeThenAbsorb(root, rawPath)
}

// contextualizeThenAbsorb runs the one-shot post-ingest pipeline for a
// single raw article: contextualize it (LLM-generated retrieval paragraph)
// then absorb it (single or two-pass depending on density). Marks the
// article as absorbed in wiki/_absorb_log.json on success, so a later
// `scribe sync` does not re-process it.
//
// Designed for interactive use (`scribe ingest url <url> --absorb`). Cron
// sync uses the batched absorbRaw path instead.
func contextualizeThenAbsorb(root, rawPath string) error {
	cfg := loadConfig(root)

	// Contextualize first so the wiki article benefits from a canonical
	// retrieval header even if absorb splits the source into multiple pages.
	cx := cfg.Absorb.Contextualize
	if cx.Enabled != nil && *cx.Enabled {
		logMsg("ingest", "contextualizing %s", filepath.Base(rawPath))
		if err := contextualizeOne(root, rawPath, cx.Model); err != nil {
			// Non-fatal: log and continue to absorb.
			logMsg("ingest", "contextualize failed (continuing): %v", err)
		} else {
			markContextualized(root, filepath.Base(rawPath))
		}
	}

	// Absorb via the same dispatch sync uses. Model defaults to sync.default_model.
	model := cfg.DefaultModel
	if model == "" {
		model = "sonnet"
	}
	sc := &SyncCmd{Model: model}
	density := readRawDensity(rawPath)
	logMsg("ingest", "absorbing %s (density=%s)", filepath.Base(rawPath), density)

	var err error
	if density == "dense" {
		err = sc.absorbDenseTwoPass(root, rawPath, filepath.Base(rawPath))
	} else {
		err = sc.absorbSinglePass(root, rawPath)
	}
	if err != nil {
		return fmt.Errorf("absorb: %w", err)
	}

	// Record in absorb log so subsequent sync runs skip this file.
	absorbLogPath := filepath.Join(root, "wiki", "_absorb_log.json")
	absorbLog := loadJSONMap(absorbLogPath)
	absorbLog[filepath.Base(rawPath)] = time.Now().UTC().Format(time.RFC3339)
	if err := saveJSONMap(absorbLogPath, absorbLog); err != nil {
		logMsg("ingest", "warn: could not update _absorb_log.json: %v", err)
	}

	logMsg("ingest", "done: %s absorbed", filepath.Base(rawPath))
	return nil
}

// markContextualized writes an entry into wiki/_contextualized_log.json
// so the batch contextualize phase doesn't redo the work on the next sync.
func markContextualized(root, rawBase string) {
	logPath := filepath.Join(root, "wiki", "_contextualized_log.json")
	m := loadJSONMap(logPath)
	m[rawBase] = time.Now().UTC().Format(time.RFC3339)
	if err := saveJSONMap(logPath, m); err != nil {
		logMsg("ingest", "warn: could not persist _contextualized_log.json: %v", err)
	}
}

// ingestQueue writes a small .url file to output/inbox/ and returns.
func ingestQueue(root, rawURL, title string, tags []string, domain, fetcher string, dryRun bool) error {
	inbox := filepath.Join(root, "output", "inbox")

	ts := time.Now()
	stamp := ts.Format("2006-01-02-150405")
	slug := slugify(rawURL)
	if len(slug) > 40 {
		slug = slug[:40]
	}
	fname := fmt.Sprintf("%s-%s.url", stamp, slug)
	path := filepath.Join(inbox, fname)

	var sb strings.Builder
	fmt.Fprintf(&sb, "url: %s\n", rawURL)
	if title != "" {
		fmt.Fprintf(&sb, "title: %s\n", title)
	}
	if len(tags) > 0 {
		fmt.Fprintf(&sb, "tags: %s\n", strings.Join(tags, ", "))
	}
	fmt.Fprintf(&sb, "domain: %s\n", domain)
	if fetcher != "" && fetcher != "auto" {
		fmt.Fprintf(&sb, "fetcher: %s\n", fetcher)
	}
	fmt.Fprintf(&sb, "queued_at: %s\n", ts.Format(time.RFC3339))

	if dryRun {
		fmt.Printf("[dry-run] would queue: %s\n", path)
		fmt.Println("---")
		fmt.Println(sb.String())
		return nil
	}

	if err := os.MkdirAll(inbox, 0o755); err != nil {
		return fmt.Errorf("create inbox: %w", err)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		return fmt.Errorf("write queue entry: %w", err)
	}

	fmt.Printf("queued: %s\n", path)
	return nil
}

// ingestFetchAndWriteReturn fetches + writes in one shot. Returns the path
// to the raw file (empty on dry-run) so callers that want to chain further
// work (contextualize, absorb) know where the article landed.
func ingestFetchAndWriteReturn(root, rawURL, overrideTitle string, tags []string, domain, fetcher string, dryRun bool) (string, error) {
	ctx := context.Background()
	res, err := fetchURL(ctx, rawURL, fetcher)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}

	title := res.Title
	if overrideTitle != "" {
		title = overrideTitle
	}
	if title == "" {
		title = "Untitled"
	}

	path, content := buildRawArticle(root, rawURL, title, res.Body, res.Via, domain, tags)

	if dryRun {
		fmt.Printf("[dry-run] would write: %s\n", path)
		fmt.Printf("  title: %s\n", title)
		fmt.Printf("  via:   %s\n", res.Via)
		fmt.Printf("  bytes: %d\n", len(content))
		return "", nil
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create raw dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write raw article: %w", err)
	}
	fmt.Printf("ingested: %s (via %s)\n", path, res.Via)
	return path, nil
}

// buildRawArticle produces the raw/articles/ path and frontmatter+body content.
// density and word_count are heuristically classified from body and embedded
// in frontmatter so absorb can branch on them without recomputing.
func buildRawArticle(root, rawURL, title, body, via, domain string, tags []string) (string, string) {
	now := time.Now()
	dateStr := now.Format("2006-01-02")
	slug := slugify(title)
	if slug == "" || slug == "untitled" {
		slug = slugify(rawURL)
	}
	if len(slug) > 60 {
		slug = slug[:60]
	}

	fname := fmt.Sprintf("%s-%s.md", dateStr, slug)
	path := filepath.Join(root, "raw", "articles", fname)

	// Ensure title is safe for YAML (no unescaped quotes).
	safeTitle := strings.ReplaceAll(title, `"`, `\"`)

	var tagLine string
	if len(tags) > 0 {
		tagLine = "[" + strings.Join(tags, ", ") + "]"
	} else {
		tagLine = "[]"
	}

	cfg := loadConfig(root)
	words, density := classifyDensityWith(body, cfg.Absorb)

	fm := fmt.Sprintf(`---
title: "%s"
source_url: "%s"
captured: %s
fetched_via: %s
type: article
domain: %s
tags: %s
word_count: %d
density: %s
---

`, safeTitle, rawURL, dateStr, via, domain, tagLine, words, density)

	return path, fm + body + "\n"
}

// classifyDensity uses the default thresholds. Equivalent to
// classifyDensityWith(body, absorbDefaults()). Kept for tests and callers
// that do not have a config in scope.
func classifyDensity(body string) (int, string) {
	return classifyDensityWith(body, absorbDefaults())
}

// classifyDensityWith counts words + H2/H3 headings in the body and returns
// (word_count, density). Thresholds come from the passed AbsorbConfig:
//   - brief: words < BriefThresholdWords AND headings <= BriefThresholdHeadings
//   - dense: words >= DenseThresholdWords OR headings >= DenseThresholdHeadings
//   - standard: everything else
//
// Heading count is a coarse proxy for distinct-topic count. Users can
// override per-file by writing `density:` into raw frontmatter manually.
func classifyDensityWith(body string, cfg AbsorbConfig) (int, string) {
	words := countWords(body)
	headings := countHeadings(body)

	if words >= cfg.DenseThresholdWords || headings >= cfg.DenseThresholdHeadings {
		return words, "dense"
	}
	if words < cfg.BriefThresholdWords && headings <= cfg.BriefThresholdHeadings {
		return words, "brief"
	}
	return words, "standard"
}

// countWords returns a whitespace-split word count.
func countWords(body string) int {
	return len(strings.Fields(body))
}

// countHeadings returns the number of H2/H3 markdown headings in the body.
// Fenced code blocks are stripped first so # inside code doesn't inflate the
// count.
var (
	fenceRE     = regexp.MustCompile("(?s)```.*?```")
	headingH2H3 = regexp.MustCompile(`(?m)^#{2,3}\s+\S`)
)

func countHeadings(body string) int {
	stripped := fenceRE.ReplaceAllString(body, "")
	return len(headingH2H3.FindAllString(stripped, -1))
}

// --- ingest drain ---

type IngestDrainCmd struct {
	Limit  int  `help:"Process at most N queue entries (0 = no limit)." default:"0"`
	DryRun bool `help:"Show what would be processed without fetching." short:"n"`
}

func (c *IngestDrainCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	return drainInbox(root, c.Limit, c.DryRun)
}

// drainInbox processes all .url files in output/inbox/. On successful fetch +
// write, the .url file is deleted. Failed entries stay for retry.
func drainInbox(root string, limit int, dryRun bool) error {
	inbox := filepath.Join(root, "output", "inbox")
	entries, err := os.ReadDir(inbox)
	if err != nil {
		if os.IsNotExist(err) {
			logMsg("ingest", "inbox empty (no directory)")
			return nil
		}
		return fmt.Errorf("read inbox: %w", err)
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".url") {
			continue
		}
		files = append(files, filepath.Join(inbox, e.Name()))
	}
	sort.Strings(files) // oldest first

	if len(files) == 0 {
		logMsg("ingest", "inbox empty")
		return nil
	}

	logMsg("ingest", "draining %d queued URL(s)", len(files))

	processed := 0
	for _, path := range files {
		if limit > 0 && processed >= limit {
			break
		}
		if err := drainOne(root, path, dryRun); err != nil {
			logMsg("ingest", "failed: %s: %v", filepath.Base(path), err)
			continue
		}
		processed++
	}
	logMsg("ingest", "drain complete: %d processed, %d remaining", processed, len(files)-processed)
	return nil
}

// drainOne reads a queue entry, fetches the URL, writes the raw article, and
// deletes the queue entry on success.
func drainOne(root, queuePath string, dryRun bool) error {
	data, err := os.ReadFile(queuePath)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	entry := parseQueueEntry(string(data))
	if entry["url"] == "" {
		return fmt.Errorf("no url in queue entry")
	}

	rawURL := entry["url"]
	title := entry["title"]
	domain := entry["domain"]
	if domain == "" {
		domain = "general"
	}
	fetcher := entry["fetcher"]
	if fetcher == "" {
		fetcher = "auto"
	}
	var tags []string
	if t := entry["tags"]; t != "" {
		for p := range strings.SplitSeq(t, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				tags = append(tags, p)
			}
		}
	}

	logMsg("ingest", "fetching %s", rawURL)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := fetchURL(ctx, rawURL, fetcher)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	finalTitle := res.Title
	if title != "" {
		finalTitle = title
	}
	if finalTitle == "" {
		finalTitle = "Untitled"
	}

	path, content := buildRawArticle(root, rawURL, finalTitle, res.Body, res.Via, domain, tags)

	if dryRun {
		logMsg("ingest", "[dry-run] would write %s (via %s, %d bytes)", filepath.Base(path), res.Via, len(content))
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := os.Remove(queuePath); err != nil {
		// Article is written; queue removal failing is a warning, not a blocker.
		logMsg("ingest", "warning: could not remove queue entry %s: %v", queuePath, err)
	}
	logMsg("ingest", "wrote %s (via %s)", filepath.Base(path), res.Via)
	return nil
}

// parseQueueEntry reads the simple "key: value\n" format.
func parseQueueEntry(s string) map[string]string {
	out := make(map[string]string)
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		before, after, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key := strings.TrimSpace(before)
		val := strings.TrimSpace(after)
		out[key] = val
	}
	return out
}

// --- utilities ---

var slugifyRE = regexp.MustCompile(`[^a-z0-9]+`)

// slugify produces a filesystem-safe slug from a title or URL.
func slugify(s string) string {
	s = strings.ToLower(s)
	// Strip common URL prefixes so slug from a raw URL is readable.
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")
	s = slugifyRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}
