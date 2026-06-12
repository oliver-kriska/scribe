package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Phase 7C: defuddle fetcher tier.
//
// Defuddle is kepano's CLI for clean web-page extraction (npm package
// `defuddle`). It tends to handle JS-heavy modern sites that the
// trafilatura tier struggles on. Slots between trafilatura and jina
// in the cascade so we keep the local-first, hosted-API-last shape:
//
//   arxiv → fxtwitter → trafilatura → defuddle → jina
//
// Optional dependency: when defuddle isn't on PATH the cascade
// silently skips this tier (same pattern as trafilatura). `scribe
// doctor` will surface "defuddle not installed" so users can opt in
// with `npm install -g defuddle`.

func fetchDefuddle(ctx context.Context, rawURL string) (fetchResult, error) {
	if _, err := exec.LookPath("defuddle"); err != nil {
		return fetchResult{}, errors.New("defuddle not installed")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// JSON output gives us title metadata alongside the markdown body.
	// `parse` is the subcommand that takes a URL; `--json` returns
	// {"title": "...", "content": "<markdown>", ...}.
	cmd := exec.CommandContext(ctx, "defuddle", "parse", rawURL, "--md", "--json")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fetchResult{}, fmt.Errorf("defuddle: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return fetchResult{}, fmt.Errorf("defuddle: empty output (stderr: %s)", strings.TrimSpace(stderr.String()))
	}

	// Defuddle's JSON shape varies a bit by version; accept the union
	// of the field names seen in 0.x — `content` (canonical), plus
	// `markdown` and `text` as fallbacks for older builds.
	var meta struct {
		Title    string `json:"title"`
		Content  string `json:"content"`
		Markdown string `json:"markdown"`
		Text     string `json:"text"`
	}
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return fetchResult{}, fmt.Errorf("defuddle: decode json: %w", err)
	}

	body := strings.TrimSpace(meta.Content)
	if body == "" {
		body = strings.TrimSpace(meta.Markdown)
	}
	if body == "" {
		body = strings.TrimSpace(meta.Text)
	}
	if body == "" {
		return fetchResult{}, errors.New("defuddle: empty body")
	}

	title := strings.TrimSpace(meta.Title)
	if title == "" {
		title = firstMarkdownHeading(body)
	}

	return fetchResult{
		Title: title,
		Body:  body,
		Via:   "defuddle",
		URL:   rawURL,
	}, nil
}
