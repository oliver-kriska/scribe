package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StatusCmd is a single-shot scoreboard for the KB. Shows what's ingested,
// what's pending, where the pipeline is stuck, and which LLM provider the
// user is on. Deliberately read-only — it does NOT run fetchers, the LLM,
// or qmd. A user who's wondering "what's in my KB and what will sync do?"
// should be able to answer that in <1 second.
//
// Also exposed from `scribe doctor` so doctor acts as a superset.
type StatusCmd struct{}

func (s *StatusCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	return renderStatus(os.Stdout, root)
}

// renderStatus prints the scoreboard. Broken out so `scribe doctor` can
// include it as a section without reinventing the queries. Takes io.Writer
// so tests can capture to a bytes.Buffer.
func renderStatus(w io.Writer, root string) error {
	cfg := loadConfig(root)

	fmt.Fprintln(w, "KB status")
	fmt.Fprintln(w, "─────────")
	fmt.Fprintf(w, "  root: %s\n", root)
	fmt.Fprintln(w)

	// --- raw articles by density ---
	rawDir := filepath.Join(root, "raw", "articles")
	counts := map[string]int{}
	total := 0
	noFrontmatter := 0
	entries, _ := os.ReadDir(rawDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		total++
		data, err := os.ReadFile(filepath.Join(rawDir, e.Name()))
		if err != nil {
			continue
		}
		raw, err := parseFrontmatterRaw(data)
		if err != nil {
			noFrontmatter++
			continue
		}
		d, _ := raw["density"].(string)
		if d == "" {
			counts["unknown"]++
		} else {
			counts[d]++
		}
	}
	fmt.Fprintf(w, "  raw/articles:     %d files\n", total)
	if total > 0 {
		fmt.Fprintf(w, "    density: brief=%d standard=%d dense=%d unknown=%d\n",
			counts["brief"], counts["standard"], counts["dense"], counts["unknown"])
		if noFrontmatter > 0 {
			fmt.Fprintf(w, "    %d file(s) without frontmatter\n", noFrontmatter)
		}
	}

	// --- contextualize + absorb progress ---
	cxLog := loadJSONMap(filepath.Join(root, "wiki", "_contextualized_log.json"))
	absorbLog, _ := loadAbsorbLog(filepath.Join(root, "wiki", "_absorb_log.json"))
	unContext := len(unprocessedForContextualize(root))
	unAbsorb := len(unprocessedForAbsorb(root))
	fmt.Fprintf(w, "  contextualized:   %d done, %d pending\n", len(cxLog), unContext)
	fmt.Fprintf(w, "  absorbed:         %d done, %d pending\n", len(absorbLog), unAbsorb)
	fmt.Fprintln(w)

	// --- contextualize provider ---
	cx := cfg.Absorb.Contextualize
	fmt.Fprintf(w, "  contextualize:    provider=%s  model=%s\n", cx.Provider, cx.Model)
	if strings.EqualFold(cx.Provider, "ollama") {
		if err := pingOllamaFast(cx.OllamaURL); err != nil {
			fmt.Fprintf(w, "                    ⚠ ollama unreachable at %s: %v\n", cx.OllamaURL, err)
		} else {
			fmt.Fprintf(w, "                    ✓ ollama up at %s\n", cx.OllamaURL)
		}
	} else {
		fmt.Fprintln(w, "                    tip: set provider=ollama for free local mode")
	}

	// --- proposal files (review queue) ---
	renderProposalQueue(w, root)

	// --- last sync ---
	runsDir := filepath.Join(root, "output", "runs")
	last := lastSyncSummary(runsDir)
	if last != "" {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  last sync:        %s\n", last)
	}

	// --- qmd index size ---
	fmt.Fprintln(w)
	if size, err := qmdIndexSize(); err == nil {
		fmt.Fprintf(w, "  qmd index:        %s\n", size)
	}

	return nil
}

// renderProposalQueue prints a one-line-per-file review queue so pending
// LLM proposals don't rot on disk. Pulled directly from the known
// proposal markdown paths — counts `###` section headers as the proxy
// for "items awaiting review".
func renderProposalQueue(w io.Writer, root string) {
	type qitem struct {
		label string
		path  string
	}
	items := []qitem{
		{"contradictions:  ", "wiki/_contradictions.md"},
		{"resolutions:     ", "wiki/_resolution-proposals.md"},
		{"identities:      ", "wiki/_identity-proposals.md"},
		{"unfetched-links: ", "wiki/_unfetched-links.md"},
	}
	printed := false
	for _, it := range items {
		abs := filepath.Join(root, it.path)
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		n := countProposalItems(string(data), it.path)
		if n == 0 {
			continue
		}
		if !printed {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "  review queue (hand-review, then clear the file):")
			printed = true
		}
		fmt.Fprintf(w, "    %s%3d pending   (%s)\n", it.label, n, it.path)
	}
}

// countProposalItems counts the per-entry blocks in a proposal file. Uses
// `###` as the proxy for resolve/identity files and `- ` bullets for
// contradictions/unfetched-links which are flat list format.
func countProposalItems(body, path string) int {
	if strings.Contains(path, "_contradictions.md") || strings.Contains(path, "_unfetched-links.md") {
		n := 0
		for line := range strings.SplitSeq(body, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "- ") {
				n++
			}
		}
		return n
	}
	n := 0
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, "### ") {
			n++
		}
	}
	return n
}

// pingOllamaFast does a 2-second GET /api/tags. Separate from the Generate
// path's ensureReady because we don't want the scoreboard to auto-pull a
// model — it just reports.
func pingOllamaFast(baseURL string) error {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	o := &ollamaProvider{baseURL: baseURL, model: ""}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := o.listedModels(ctx)
	return err
}

// lastSyncSummary finds the most recent JSONL entry in output/runs/ whose
// command is "sync" and returns a one-line summary.
func lastSyncSummary(runsDir string) string {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return ""
	}
	// Files are YYYY-MM-DD.jsonl — read the newest.
	var newest os.DirEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if newest == nil || e.Name() > newest.Name() {
			newest = e
		}
	}
	if newest == nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(runsDir, newest.Name()))
	if err != nil {
		return ""
	}
	// Walk lines backward to find the most recent sync entry.
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if strings.Contains(line, `"command":"sync"`) {
			// Parse just enough to pull timestamp + key counters.
			return formatRunLine(line)
		}
	}
	return ""
}

// formatRunLine extracts ts + status + key stats from a JSONL run record.
// We don't pull a full JSON parse because the record has variable shape;
// string scraping a few fields is faster and plenty for the scoreboard.
func formatRunLine(line string) string {
	ts := extractJSONField(line, "timestamp")
	status := extractJSONField(line, "status")
	abs := extractJSONField(line, "absorbed")
	ext := extractJSONField(line, "extracted")
	ses := extractJSONField(line, "sessions")
	return fmt.Sprintf("%s [%s] extracted=%s absorbed=%s sessions=%s",
		ts, status, defaultStr(ext, "0"), defaultStr(abs, "0"), defaultStr(ses, "0"))
}

func extractJSONField(line, key string) string {
	needle := fmt.Sprintf(`"%s":`, key)
	_, after, ok := strings.Cut(line, needle)
	if !ok {
		return ""
	}
	rest := after
	// Value starts with " for strings, digit for numbers.
	if strings.HasPrefix(rest, `"`) {
		end := strings.Index(rest[1:], `"`)
		if end < 0 {
			return ""
		}
		return rest[1 : 1+end]
	}
	// Number or bool — read until , or }.
	end := strings.IndexAny(rest, ",}")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// qmdIndexSize shells out to `qmd status` and grabs the reported size. Best
// effort — returns "" if qmd isn't installed so status stays useful even
// without the semantic layer.
func qmdIndexSize() (string, error) {
	out, err := runCmdErr("", "qmd", "status")
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "Size:") {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Size:")), nil
		}
	}
	return "", fmt.Errorf("size line not found")
}
