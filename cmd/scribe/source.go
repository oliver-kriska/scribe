package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Source is an external provider of bookmark-like items that scribe pulls
// incrementally and queues into output/inbox/ for the existing ingest-drain
// path to fetch → contextualize → absorb. Deterministic, no LLM — the same
// budget class as capture/triage/hook.
//
// Pinboard is the first implementation (source_pinboard.go). The interface is
// deliberately narrow so future sources with different pagination models — a
// timestamp cursor (Pinboard), a page token (GitHub stars), a last-id marker
// (Reddit saved) — all fit without widening it: each owns its opaque cursor.
type Source interface {
	// Name is the registry key: state-file stem, provenance tag, log prefix.
	Name() string
	// Configured reports whether the source is enabled and credentialed. The
	// reason explains a false result for the CLI/log (soft-skip, not an error).
	Configured(cfg *ScribeConfig) (ok bool, reason string)
	// Fetch returns fresh items given the opaque prior cursor, plus the cursor
	// to persist for the next run. Implementations own their cursor shape and
	// resolve their own config (scope defaults, token) off cfg. The OR tag
	// filter and skip_domains are applied generically by the driver afterward.
	Fetch(ctx context.Context, cfg *ScribeConfig, prev json.RawMessage, opts FetchOpts) (items []SourceItem, next json.RawMessage, err error)
}

// SourceItem is one normalized bookmark from any Source.
type SourceItem struct {
	URL       string
	Title     string
	Tags      []string
	Note      string    // the user's own annotation → article body
	CreatedAt time.Time // when the bookmark was saved (best-effort)
	ID        string    // stable dedup key (provider hash; falls back to URL)
	Unread    bool
}

// FetchOpts carries the per-run knobs a Source honors. Empty Scope means
// "use the configured default". Tags is the per-run override of the OR tag
// filter the driver applies (empty → use the integration's configured tags).
type FetchOpts struct {
	Scope string   // "recent+unread" | "unread" | "all" | ""
	Tags  []string // OR filter override; empty = use config
	Force bool     // bypass the source's cheap unchanged-since-last-run probe
}

// sourceRegistry maps integration name → adapter. Adding an adapter is one
// entry here plus a file implementing Source.
var sourceRegistry = map[string]Source{
	"pinboard": pinboardSource{},
}

// registeredSources returns every adapter, sorted by name for stable output.
func registeredSources() []Source {
	out := make([]Source, 0, len(sourceRegistry))
	for _, s := range sourceRegistry {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

func sourceNames() []string {
	out := make([]string, 0, len(sourceRegistry))
	for name := range sourceRegistry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// sourceState is the persisted per-source state under output/sources/<name>.json.
// output/ is gitignored, so this per-machine cursor never lands in a shared KB.
type sourceState struct {
	Cursor   json.RawMessage `json:"cursor,omitempty"` // opaque, owned by the Source
	Seen     []string        `json:"seen"`             // dedup IDs already queued
	LastPull string          `json:"last_pull,omitempty"`
}

func sourceStatePath(root, name string) string {
	return filepath.Join(root, "output", "sources", name+".json")
}

func loadSourceState(path string) (*sourceState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &sourceState{}, nil
		}
		return nil, err
	}
	var st sourceState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse state json: %w", err)
	}
	return &st, nil
}

func saveSourceState(path string, st *sourceState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// pullSource runs one adapter end-to-end: configured gate → load state →
// fetch → dedup + skip-domains → write queue entries → persist state. Returns
// the number of items queued. An unconfigured source is a soft-skip (0, nil):
// `scribe pull` over every registered source must not fail because one lacks a
// token. A per-source lock keeps a manual run and the cron run from racing the
// state file.
func pullSource(root string, src Source, opts FetchOpts, maxItems int, dryRun bool) (int, error) {
	cfg := loadConfig(root)
	if err := cfg.requireParseable(); err != nil {
		return 0, err
	}
	if ok, reason := src.Configured(cfg); !ok {
		logMsg("pull", "%s: skipped (%s)", src.Name(), reason)
		return 0, nil
	}

	if !dryRun {
		lockPath := lockPathFor(cfg.LockDir, "pull-"+src.Name(), root)
		lf, ok, err := acquireLock(lockPath)
		if err != nil {
			return 0, fmt.Errorf("lock %s: %w", lockPath, err)
		}
		if !ok {
			logMsg("pull", "%s: blocked by existing pull-%s lock", src.Name(), src.Name())
			return 0, nil
		}
		defer releaseLock(lf)
	}

	statePath := sourceStatePath(root, src.Name())
	state, err := loadSourceState(statePath)
	if err != nil {
		return 0, fmt.Errorf("load %s state: %w", src.Name(), err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	items, cursor, err := src.Fetch(ctx, cfg, state.Cursor, opts)
	if err != nil {
		return 0, fmt.Errorf("%s fetch: %w", src.Name(), err)
	}

	seen := make(map[string]bool, len(state.Seen))
	for _, id := range state.Seen {
		seen[id] = true
	}

	skip := integrationSkipDomains(cfg, src.Name())
	// OR tag filter: a per-run --tag override wins, else the integration's
	// configured tags. Empty → keep everything the scope returned.
	tagFilter := effectiveTags(opts.Tags, integrationTags(cfg, src.Name()))
	inbox := filepath.Join(root, "output", "inbox")

	queued := 0
	capped := false
	for _, it := range items {
		id := it.ID
		if id == "" {
			id = it.URL
		}
		if id == "" || seen[id] {
			continue
		}
		if it.URL == "" || shouldSkipURL(it.URL, skip) {
			// Don't mark skipped as seen — a later config change that drops
			// the skip rule should let the item through (mirrors capture).
			continue
		}
		if !tagFilter.allows(it.Tags) {
			// Same reasoning as skip: not marked seen, so widening the filter
			// later (or --force / --all-history) re-includes it.
			continue
		}
		if maxItems > 0 && queued >= maxItems {
			capped = true
			break
		}

		if dryRun {
			fmt.Printf("  WOULD QUEUE: %s (%s)\n", it.URL, it.Title)
			seen[id] = true
			queued++
			continue
		}

		if _, err := writeQueueEntry(inbox, sourceItemToQueue(src.Name(), it)); err != nil {
			logMsg("pull", "%s: queue %s failed: %v", src.Name(), it.URL, err)
			continue
		}
		seen[id] = true
		queued++
	}

	if dryRun {
		suffix := ""
		if capped {
			suffix = fmt.Sprintf(" (capped at --max %d)", maxItems)
		}
		logMsg("pull", "%s: %d item(s) would be queued%s (dry-run, state unchanged)", src.Name(), queued, suffix)
		return queued, nil
	}

	// Advance the cursor only on a complete pass. When --max capped the run,
	// keeping the old cursor lets the next `pull … --all-history` resume the
	// backfill (seen dedups what already went through) instead of the cheap
	// unchanged-probe short-circuiting it away.
	if !capped {
		state.Cursor = cursor
	}
	state.Seen = sortedKeys(seen)
	state.LastPull = time.Now().UTC().Format(time.RFC3339)
	if err := saveSourceState(statePath, state); err != nil {
		return queued, fmt.Errorf("save %s state: %w", src.Name(), err)
	}

	if capped {
		logMsg("pull", "%s: queued %d new item(s); --max %d reached, re-run to continue backfill", src.Name(), queued, maxItems)
	} else {
		logMsg("pull", "%s: queued %d new item(s)", src.Name(), queued)
	}
	return queued, nil
}

// sourceItemToQueue maps a normalized item to the shared inbox queue format.
// The source name becomes a provenance tag; unread items also get "to-read".
func sourceItemToQueue(name string, it SourceItem) queueFields {
	tags := append([]string(nil), it.Tags...)
	if it.Unread && !containsFold(tags, "to-read") {
		tags = append(tags, "to-read")
	}
	f := queueFields{
		URL:    it.URL,
		Title:  it.Title,
		Tags:   tags,
		Domain: "general",
		Note:   it.Note,
		Source: name,
	}
	if !it.CreatedAt.IsZero() {
		f.QueuedAt = it.CreatedAt
	}
	return f
}

func containsFold(ss []string, want string) bool {
	for _, s := range ss {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// integrationSkipDomains returns the skip_domains configured for an
// integration (empty when the block is absent).
func integrationSkipDomains(cfg *ScribeConfig, name string) []string {
	if cfg == nil || cfg.Integrations == nil {
		return nil
	}
	return cfg.Integrations[name].SkipDomains
}

// integrationTags returns the configured OR tag filter for an integration
// (empty when the block is absent).
func integrationTags(cfg *ScribeConfig, name string) []string {
	if cfg == nil || cfg.Integrations == nil {
		return nil
	}
	return cfg.Integrations[name].Tags
}

// tagSet is a case-insensitive OR filter over item tags. An empty set allows
// everything (no filtering).
type tagSet map[string]bool

func (ts tagSet) allows(itemTags []string) bool {
	if len(ts) == 0 {
		return true
	}
	for _, t := range itemTags {
		if ts[strings.ToLower(strings.TrimSpace(t))] {
			return true
		}
	}
	return false
}

// effectiveTags builds the OR filter for a run: the per-run override wins over
// the configured tags; empty either way disables filtering.
func effectiveTags(override, configured []string) tagSet {
	src := override
	if len(src) == 0 {
		src = configured
	}
	if len(src) == 0 {
		return nil
	}
	ts := make(tagSet, len(src))
	for _, t := range src {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			ts[t] = true
		}
	}
	return ts
}

// --- CLI ---

// PullCmd pulls bookmarks from configured integrations (Pinboard, …) into the
// ingest queue. Bare `scribe pull` pulls every configured source — that is
// what the pull-sources cron job runs.
type PullCmd struct {
	Source     string   `arg:"" optional:"" help:"Integration to pull (e.g. pinboard). Omit to pull every configured integration."`
	Scope      string   `help:"Override scope for this run: recent+unread|unread|all." enum:"recent+unread,unread,all," default:""`
	AllHistory bool     `help:"Backfill the entire archive this run (≡ --scope all). Pair with --max on a first big pull." name:"all-history"`
	Tag        []string `help:"Only ingest bookmarks carrying at least one of these tags (repeatable, OR). Overrides the integration's configured tags for this run."`
	Force      bool     `help:"Bypass the source's cheap unchanged-since-last-run probe."`
	Max        int      `help:"Cap items queued this run (0 = no limit). Useful to pace a first --all-history backfill." default:"0"`
	List       bool     `help:"List integrations and their status; pull nothing."`
	DryRun     bool     `help:"Print what would be queued without writing." short:"n"`
}

func (c *PullCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	if c.List {
		return listIntegrations(root)
	}

	scope := c.Scope
	if c.AllHistory {
		scope = "all"
	}
	opts := FetchOpts{Scope: scope, Tags: c.Tag, Force: c.Force || c.AllHistory}

	var srcs []Source
	if c.Source != "" {
		s, ok := sourceRegistry[c.Source]
		if !ok {
			return fmt.Errorf("unknown integration %q (known: %s)", c.Source, strings.Join(sourceNames(), ", "))
		}
		srcs = []Source{s}
	} else {
		srcs = registeredSources()
	}

	total := 0
	var firstErr error
	for _, s := range srcs {
		n, err := pullSource(root, s, opts, c.Max, c.DryRun)
		total += n
		if err != nil {
			logMsg("pull", "%s: %v", s.Name(), err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	logMsg("pull", "done: %d item(s) queued across %d integration(s)", total, len(srcs))
	return firstErr
}

// listIntegrations prints each registered adapter with whether it is
// configured and when it last pulled.
func listIntegrations(root string) error {
	cfg := loadConfig(root)
	for _, s := range registeredSources() {
		status := "ready"
		if ok, reason := s.Configured(cfg); !ok {
			status = "not configured: " + reason
		}
		last := "never"
		if st, err := loadSourceState(sourceStatePath(root, s.Name())); err == nil && st.LastPull != "" {
			last = st.LastPull
		}
		fmt.Printf("%-12s  %-40s  last pull: %s\n", s.Name(), status, last)
	}
	return nil
}
