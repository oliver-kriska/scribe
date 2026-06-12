package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Phase 6C — staleness detection.
//
// Three independent signals can mark an article stale:
//   - date    — `updated:` is older than the article-type's half-life.
//   - source  — for articles with `source_url:`, an opt-in HEAD probe
//               returned 4xx/5xx or a network error.
//   - reference — (deferred to v2; depends on Phase 6A v2 typed-edge
//                 graph populated by the LLM migrator)
//
// The ledger is a flat JSONL file at `wiki/_staleness.jsonl`, one entry
// per article that has at least one signal. Rebuilds preserve
// `first_observed_at`. The ledger is a derived artifact; gitignored.

const stalenessLedgerVersion = 1

const (
	StaleSignalDate   = "date"
	StaleSignalSource = "source"
)

// StalenessEntry is one article's record. Fields are union-typed:
// not every signal populates every field. Consumers should branch on
// `signals` and read only what's relevant.
type StalenessEntry struct {
	Version         int      `json:"version"`
	ID              string   `json:"id"`
	Path            string   `json:"path"`
	Title           string   `json:"title,omitempty"`
	Type            string   `json:"type,omitempty"`
	Signals         []string `json:"signals"`
	Updated         string   `json:"updated,omitempty"`
	AgeDays         int      `json:"age_days,omitempty"`
	HalfLifeDays    int      `json:"half_life_days,omitempty"`
	SourceURL       string   `json:"source_url,omitempty"`
	SourceStatus    int      `json:"source_status,omitempty"`
	SourceCheckedAt string   `json:"source_checked_at,omitempty"`
	FirstObservedAt string   `json:"first_observed_at"`
	LastSeenAt      string   `json:"last_seen_at"`
}

// halfLifeDays returns the date-staleness threshold per article type.
// Returning 0 disables the date signal for that article (never stale).
//
// Defaults are conservative — better to under-flag than to nag the user
// into ignoring the ledger. scribe.yaml override ships in v2.
func halfLifeDays(articleType, status string) int {
	if articleType == "research" && status == "superseded" {
		return 0
	}
	if articleType == "decision" && status == "superseded" {
		return 0
	}
	switch articleType {
	case "decision":
		return 180
	case "pattern":
		return 365
	case "solution":
		return 365
	case "research":
		return 90
	case "tool":
		return 365
	case "idea":
		return 90
	case "project":
		return 60
	default:
		return 365
	}
}

func stalenessLedgerPath(root string) string {
	return filepath.Join(root, "wiki", "_staleness.jsonl")
}

// updatedToTime accepts the YAML-parsed `updated:` value. yaml.v3 turns
// a bare YYYY-MM-DD into time.Time; quoted strings stay strings. Both
// shapes appear in the corpus.
func updatedToTime(v any) (time.Time, bool) {
	if v == nil {
		return time.Time{}, false
	}
	if t, ok := v.(time.Time); ok {
		return t.UTC(), true
	}
	if s, ok := v.(string); ok {
		for _, layout := range []string{"2006-01-02", time.RFC3339, time.DateTime} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC(), true
			}
		}
	}
	return time.Time{}, false
}

// BuildStaleOpts gates the optional URL-probing pass.
type BuildStaleOpts struct {
	CheckURLs  bool
	MaxURLs    int
	URLTimeout time.Duration
}

// StaleCounts reports per-signal counts to the caller.
type StaleCounts struct {
	Total  int
	Date   int
	Source int
}

// buildStalenessLedger walks every wiki article, computes signals, and
// writes the JSONL ledger. Idempotent: preserves `first_observed_at` for
// entries that recur. Removes the file when there are no stale entries.
//
// Skips raw/articles by walking via walkArticles + a `raw/` prefix guard
// (some KBs route raw content through wikiDirs aliases).
func buildStalenessLedger(root string, opts BuildStaleOpts, now time.Time) (StaleCounts, error) {
	now = now.UTC()
	nowStamp := now.Format(time.RFC3339)

	prior, _ := readStalenessLedger(root)
	priorByID := map[string]StalenessEntry{}
	for _, e := range prior {
		priorByID[e.ID] = e
	}

	var counts StaleCounts
	var entries []StalenessEntry

	err := walkArticles(root, func(path string, content []byte) error {
		fm, err := parseFrontmatter(content)
		if err != nil {
			return nil //nolint:nilerr // unparseable article: skip it, keep walking
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "raw/") {
			return nil
		}

		rawMap, _ := parseFrontmatterRaw(content)
		sourceURL := ""
		if rawMap != nil {
			if u, ok := rawMap["source_url"].(string); ok {
				sourceURL = u
			}
		}

		var signals []string
		var ageDays, halfLife int
		var updatedStr string

		hl := halfLifeDays(fm.Type, fm.Status)
		if hl > 0 {
			if ut, ok := updatedToTime(fm.Updated); ok {
				age := int(now.Sub(ut).Hours() / 24)
				if age >= hl {
					signals = append(signals, StaleSignalDate)
					ageDays = age
					halfLife = hl
					updatedStr = ut.Format("2006-01-02")
				}
			}
		}

		// We hold the entry even when no date signal fires *if* there's
		// a source_url to probe. Filter at the end.
		if len(signals) == 0 && (!opts.CheckURLs || sourceURL == "") {
			return nil
		}

		entries = append(entries, StalenessEntry{
			Version:      stalenessLedgerVersion,
			ID:           stalenessID(rel),
			Path:         rel,
			Title:        fm.Title,
			Type:         fm.Type,
			Signals:      signals,
			Updated:      updatedStr,
			AgeDays:      ageDays,
			HalfLifeDays: halfLife,
			SourceURL:    sourceURL,
			LastSeenAt:   nowStamp,
		})
		return nil
	})
	if err != nil {
		return counts, err
	}

	if opts.CheckURLs {
		probeStaleURLs(entries, opts.MaxURLs, opts.URLTimeout, nowStamp)
	}

	out := make([]StalenessEntry, 0, len(entries))
	for _, e := range entries {
		if len(e.Signals) == 0 {
			continue
		}
		if old, ok := priorByID[e.ID]; ok {
			e.FirstObservedAt = old.FirstObservedAt
		}
		if e.FirstObservedAt == "" {
			e.FirstObservedAt = nowStamp
		}
		out = append(out, e)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].AgeDays != out[j].AgeDays {
			return out[i].AgeDays > out[j].AgeDays
		}
		return out[i].Path < out[j].Path
	})

	for _, e := range out {
		counts.Total++
		for _, s := range e.Signals {
			switch s {
			case StaleSignalDate:
				counts.Date++
			case StaleSignalSource:
				counts.Source++
			}
		}
	}

	path := stalenessLedgerPath(root)
	if len(out) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return counts, err
		}
		return counts, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return counts, err
	}
	f, err := os.Create(path)
	if err != nil {
		return counts, err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range out {
		if err := enc.Encode(e); err != nil {
			return counts, err
		}
	}
	return counts, nil
}

func stalenessID(relPath string) string {
	return "s-" + shortHash(relPath)
}

func readStalenessLedger(root string) ([]StalenessEntry, error) {
	f, err := os.Open(stalenessLedgerPath(root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var out []StalenessEntry
	for {
		var e StalenessEntry
		if err := dec.Decode(&e); err != nil {
			if err == io.EOF {
				break
			}
			continue
		}
		if e.Version == stalenessLedgerVersion {
			out = append(out, e)
		}
	}
	return out, nil
}

// probeStaleURLs HEAD-checks source URLs in parallel (8-way) and updates
// entries in-place. Network errors and 4xx/5xx responses both register
// the source signal; only 2xx and 3xx are considered alive.
//
// `maxProbes` caps probes so a 10k-article KB doesn't fire-hose remote
// sites. The intent is "occasional weekly recompute" not "exhaustive crawl."
func probeStaleURLs(entries []StalenessEntry, maxProbes int, timeout time.Duration, nowStamp string) {
	if maxProbes <= 0 {
		maxProbes = 100
	}
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	var mu sync.Mutex

	probed := 0
	for i := range entries {
		if entries[i].SourceURL == "" || !strings.HasPrefix(entries[i].SourceURL, "http") {
			continue
		}
		if probed >= maxProbes {
			break
		}
		probed++
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "HEAD", entries[idx].SourceURL, nil)
			if err != nil {
				return
			}
			req.Header.Set("User-Agent", "scribe-stale/1.0")
			resp, err := client.Do(req)
			if err != nil {
				mu.Lock()
				entries[idx].SourceStatus = -1
				entries[idx].SourceCheckedAt = nowStamp
				entries[idx].Signals = append(entries[idx].Signals, StaleSignalSource)
				mu.Unlock()
				return
			}
			defer resp.Body.Close()
			mu.Lock()
			entries[idx].SourceStatus = resp.StatusCode
			entries[idx].SourceCheckedAt = nowStamp
			if resp.StatusCode >= 400 {
				entries[idx].Signals = append(entries[idx].Signals, StaleSignalSource)
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
}

// ---- CLI ----

type StaleCmd struct {
	Build StaleBuildCmd `cmd:"" help:"Recompute the staleness ledger from disk."`
	List  StaleListCmd  `cmd:"" help:"Print ledger entries (filterable)."`
	Show  StaleShowCmd  `cmd:"" help:"Show one entry by ID or path."`
}

type StaleBuildCmd struct {
	CheckURLs bool          `help:"Probe source_url with HEAD requests (network)."`
	MaxURLs   int           `help:"Cap on URL probes per run." default:"100"`
	Timeout   time.Duration `help:"Per-URL HEAD timeout." default:"5s"`
}

func (c *StaleBuildCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	counts, err := buildStalenessLedger(root, BuildStaleOpts{
		CheckURLs:  c.CheckURLs,
		MaxURLs:    c.MaxURLs,
		URLTimeout: c.Timeout,
	}, time.Now())
	if err != nil {
		return err
	}
	runStats = map[string]any{
		"stale_total":  counts.Total,
		"stale_date":   counts.Date,
		"stale_source": counts.Source,
	}
	logMsg("stale", "ledger build done: total=%d date=%d source=%d", counts.Total, counts.Date, counts.Source)
	return nil
}

type StaleListCmd struct {
	Signal string `help:"Filter to one signal: date | source." enum:"date,source," default:""`
	Type   string `help:"Filter to one article type."`
}

func (c *StaleListCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	entries, _ := readStalenessLedger(root)
	if len(entries) == 0 {
		fmt.Println("(empty ledger — run `scribe stale build`)")
		return nil
	}
	n := 0
	for _, e := range entries {
		if c.Signal != "" && !containsString(e.Signals, c.Signal) {
			continue
		}
		if c.Type != "" && e.Type != c.Type {
			continue
		}
		fmt.Printf("%s  %-13s  %s  [%s]\n", e.ID, strings.Join(e.Signals, ","), e.Path, staleSummary(e))
		n++
	}
	fmt.Printf("\n(%d entries)\n", n)
	return nil
}

type StaleShowCmd struct {
	Target string `arg:"" help:"Entry ID (s-...) or article path."`
}

func (c *StaleShowCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	entries, _ := readStalenessLedger(root)
	target := filepath.ToSlash(c.Target)
	for _, e := range entries {
		if e.ID == c.Target || e.Path == target {
			data, err := json.MarshalIndent(e, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(data))
			return nil
		}
	}
	return fmt.Errorf("no entry found for %q", c.Target)
}

func staleSummary(e StalenessEntry) string {
	parts := make([]string, 0, len(e.Signals))
	for _, s := range e.Signals {
		switch s {
		case StaleSignalDate:
			parts = append(parts, fmt.Sprintf("%dd > %dd", e.AgeDays, e.HalfLifeDays))
		case StaleSignalSource:
			parts = append(parts, fmt.Sprintf("HTTP %d", e.SourceStatus))
		}
	}
	return strings.Join(parts, "; ")
}

func containsString(ss []string, want string) bool {
	return slices.Contains(ss, want)
}

// checkStale is the doctor section — one warn per signal kind, summarized.
func checkStale(root string) []check {
	entries, err := readStalenessLedger(root)
	if err != nil {
		return []check{{
			Section: "stale", Name: "ledger", Status: statusWarn,
			Detail: fmt.Sprintf("read error: %v", err),
		}}
	}
	if len(entries) == 0 {
		return []check{{
			Section: "stale", Name: "ledger", Status: statusOK,
			Detail: "no stale articles",
		}}
	}
	var date, source int
	for _, e := range entries {
		for _, s := range e.Signals {
			switch s {
			case StaleSignalDate:
				date++
			case StaleSignalSource:
				source++
			}
		}
	}
	var out []check
	if date > 0 {
		out = append(out, check{
			Section: "stale", Name: "date-stale", Status: statusWarn,
			Detail: fmt.Sprintf("%d articles past their type's half-life", date),
			Fix:    "scribe stale list --signal date",
		})
	}
	if source > 0 {
		out = append(out, check{
			Section: "stale", Name: "source-stale", Status: statusWarn,
			Detail: fmt.Sprintf("%d articles with dead source URLs", source),
			Fix:    "scribe stale list --signal source",
		})
	}
	if len(out) == 0 {
		out = append(out, check{
			Section: "stale", Name: "ledger", Status: statusOK,
			Detail: "no stale articles",
		})
	}
	return out
}
