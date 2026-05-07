package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Phase 6B: contradiction ledger.
//
// scribe used to surface contradictions only as a free-text markdown
// report (`wiki/_contradictions.md`) emitted by `scribe lint
// --contradictions`. The Phase 6B ledger adds a structured layer on
// top: every `contradicts:` typed edge (Phase 6A) materializes as one
// JSONL entry in `wiki/_contradictions.jsonl`. Each entry is keyed by
// the unordered pair (subject, object) so an A↔B contradiction
// produces exactly one ledger row regardless of which article you
// stand on.
//
// v1 source: typed `contradicts:` edges from frontmatter. v2 (future)
// will also import contradictions surfaced by the LLM pass-2 scan
// into the same ledger so the resolution surface is uniform.
//
// Resolution: an article that gains a `superseded_by:` edge pointing
// at the new winner — or a contradicts edge that gets removed —
// causes the next ledger build to mark the entry resolved (or drop it).
// Manual resolution via `scribe contradictions resolve <id>` writes a
// timestamped `resolved_at` to the ledger entry.

const contradictionsLedgerVersion = 1

// ContradictionEntry is one row in wiki/_contradictions.jsonl.
type ContradictionEntry struct {
	Version int `json:"version"`
	// ID is a stable hash of the (sorted) pair, so the same A↔B
	// contradiction collapses to one row regardless of which article
	// declared it first.
	ID string `json:"id"`
	// Pair lists the two articles in canonical sorted order. Use this
	// when displaying; the order is stable across rebuilds.
	Pair [2]string `json:"pair"`
	// Sources points at the frontmatter declaration(s) that produced
	// this entry. When both A and B carry `contradicts: [[...]]`
	// pointing at each other, both paths are listed. Single-direction
	// declarations record only the declaring side.
	Sources []string `json:"sources"`
	// FirstObservedAt is when this contradiction first showed up in
	// the ledger (preserved across rebuilds via merge with prior
	// ledger contents). RFC3339 UTC.
	FirstObservedAt string `json:"first_observed_at"`
	// LastSeenAt is the most recent build pass that confirmed this
	// pair still has live `contradicts:` edges. RFC3339 UTC.
	LastSeenAt string `json:"last_seen_at"`
	// ResolvedAt is set by `scribe contradictions resolve <id>` to
	// the time the human marked it handled. The next rebuild keeps
	// resolved entries in the ledger as a paper trail; they're
	// excluded from `scribe doctor` and `scribe contradictions list`
	// by default.
	ResolvedAt string `json:"resolved_at,omitempty"`
	// ResolutionNote is freeform text from the human at resolution time.
	ResolutionNote string `json:"resolution_note,omitempty"`
}

// contradictionsLedgerPath returns the JSONL ledger location.
func contradictionsLedgerPath(root string) string {
	return filepath.Join(root, "wiki", "_contradictions.jsonl")
}

// buildContradictionLedger walks every wiki article, collects typed
// `contradicts:` edges, and writes the ledger as JSONL. Idempotent:
// preserves `first_observed_at` and resolution state from the prior
// ledger when an entry's pair is unchanged.
func buildContradictionLedger(root string) (int, int, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	prior, _ := readContradictionLedger(root)
	priorByID := map[string]ContradictionEntry{}
	for _, e := range prior {
		priorByID[e.ID] = e
	}

	titleToPath := map[string]string{}
	type rawEdge struct {
		from string
		to   string
		path string
	}
	var raw []rawEdge

	err := walkArticles(root, func(path string, content []byte) error {
		fm, err := parseFrontmatter(content)
		if err != nil || fm.Title == "" {
			return nil //nolint:nilerr
		}
		titleToPath[fm.Title] = path
		for _, e := range edgesFromFrontmatter(fm) {
			if e.Kind == RelContradicts {
				raw = append(raw, rawEdge{from: fm.Title, to: e.Target, path: path})
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}

	// Collapse to canonical pairs. (A, B) and (B, A) → one entry.
	type pairKey struct{ a, b string } // canonicalized: a < b
	type accumulator struct {
		sources []string
	}
	pairs := map[pairKey]*accumulator{}
	for _, e := range raw {
		a, b := e.from, e.to
		if b < a {
			a, b = b, a
		}
		key := pairKey{a: a, b: b}
		acc := pairs[key]
		if acc == nil {
			acc = &accumulator{}
			pairs[key] = acc
		}
		acc.sources = append(acc.sources, e.path)
	}

	out := make([]ContradictionEntry, 0, len(pairs))
	for k, acc := range pairs {
		id := contradictionPairID(k.a, k.b)
		entry := ContradictionEntry{
			Version:    contradictionsLedgerVersion,
			ID:         id,
			Pair:       [2]string{k.a, k.b},
			Sources:    dedupSorted(acc.sources),
			LastSeenAt: now,
		}
		if old, ok := priorByID[id]; ok {
			entry.FirstObservedAt = old.FirstObservedAt
			entry.ResolvedAt = old.ResolvedAt
			entry.ResolutionNote = old.ResolutionNote
		}
		if entry.FirstObservedAt == "" {
			entry.FirstObservedAt = now
		}
		out = append(out, entry)
	}

	// Sort for stable on-disk output (newest-observed first within
	// unresolved, resolved at the end).
	sort.Slice(out, func(i, j int) bool {
		ri := out[i].ResolvedAt != ""
		rj := out[j].ResolvedAt != ""
		if ri != rj {
			return !ri // unresolved first
		}
		if out[i].FirstObservedAt != out[j].FirstObservedAt {
			return out[i].FirstObservedAt > out[j].FirstObservedAt
		}
		return out[i].ID < out[j].ID
	})

	path := contradictionsLedgerPath(root)
	if len(out) == 0 {
		// No contradictions; remove a stale ledger if present.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return 0, 0, err
		}
		return 0, 0, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, 0, err
	}
	f, err := os.Create(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range out {
		if err := enc.Encode(e); err != nil {
			return 0, 0, err
		}
	}

	unresolved := 0
	for _, e := range out {
		if e.ResolvedAt == "" {
			unresolved++
		}
	}
	return len(out), unresolved, nil
}

// readContradictionLedger reads the JSONL file. Empty / missing →
// empty slice, no error. Used by the build pass for merge and by the
// CLI list/show/resolve subcommands.
func readContradictionLedger(root string) ([]ContradictionEntry, error) {
	path := contradictionsLedgerPath(root)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []ContradictionEntry
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e ContradictionEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue //nolint:nilerr // skip unparseable rows; build pass overwrites
		}
		if e.Version == contradictionsLedgerVersion {
			out = append(out, e)
		}
	}
	return out, nil
}

// contradictionPairID returns a stable ID for a sorted pair. We hash
// rather than concatenate so pair IDs survive title renames if we
// later decide to keep aliases — not because the current rename
// strategy needs it.
func contradictionPairID(a, b string) string {
	return "c-" + shortHash(a+"||"+b)
}

func shortHash(s string) string {
	const fnvOffset = 1469598103934665603
	const fnvPrime = 1099511628211
	h := uint64(fnvOffset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime
	}
	return fmt.Sprintf("%016x", h)
}

func dedupSorted(s []string) []string {
	if len(s) <= 1 {
		return s
	}
	seen := make(map[string]bool, len(s))
	out := make([]string, 0, len(s))
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// resolveContradiction marks one ledger entry resolved by ID,
// preserving the entry as a paper trail. Returns an error when the ID
// doesn't exist in the current ledger.
func resolveContradiction(root, id, note string) error {
	entries, err := readContradictionLedger(root)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("ledger empty (run `scribe contradictions build` first)")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	found := false
	for i := range entries {
		if entries[i].ID == id {
			entries[i].ResolvedAt = now
			entries[i].ResolutionNote = note
			found = true
		}
	}
	if !found {
		return fmt.Errorf("no ledger entry with id %s", id)
	}
	path := contradictionsLedgerPath(root)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}

// ContradictionsCmd is the kong CLI for Phase 6B.
//
//	scribe contradictions build           rebuild the ledger from typed contradicts: edges
//	scribe contradictions list [--all]    list unresolved (default) or all entries
//	scribe contradictions show <id>       full detail for one entry
//	scribe contradictions resolve <id>    mark resolved with optional note
type ContradictionsCmd struct {
	Build   ContradictionsBuildCmd   `cmd:"" help:"Rebuild wiki/_contradictions.jsonl from typed contradicts: edges."`
	List    ContradictionsListCmd    `cmd:"" help:"List ledger entries (unresolved by default)."`
	Show    ContradictionsShowCmd    `cmd:"" help:"Show full detail of one ledger entry."`
	Resolve ContradictionsResolveCmd `cmd:"" help:"Mark a ledger entry resolved (preserves the row as a paper trail)."`
}

type ContradictionsBuildCmd struct{}

func (b *ContradictionsBuildCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	total, unresolved, err := buildContradictionLedger(root)
	if err != nil {
		return err
	}
	logMsg("contradictions", "ledger build done: total=%d unresolved=%d",
		total, unresolved)
	runStats = map[string]any{
		"contradictions_total":      total,
		"contradictions_unresolved": unresolved,
	}
	return nil
}

type ContradictionsListCmd struct {
	All bool `help:"Include resolved entries."`
}

func (l *ContradictionsListCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	entries, err := readContradictionLedger(root)
	if err != nil {
		return err
	}
	shown := 0
	for _, e := range entries {
		if !l.All && e.ResolvedAt != "" {
			continue
		}
		marker := " "
		if e.ResolvedAt != "" {
			marker = "✓"
		}
		fmt.Printf("%s %s  [[%s]] ↔ [[%s]]  (first %s)\n",
			marker, e.ID, e.Pair[0], e.Pair[1], e.FirstObservedAt)
		shown++
	}
	if shown == 0 {
		if len(entries) == 0 {
			fmt.Println("(empty ledger — run `scribe contradictions build` to rebuild)")
		} else {
			fmt.Println("(no unresolved contradictions; pass --all to see resolved)")
		}
	}
	return nil
}

type ContradictionsShowCmd struct {
	ID string `arg:"" help:"Ledger entry ID (e.g. c-deadbeef...)."`
}

func (s *ContradictionsShowCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	entries, err := readContradictionLedger(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.ID == s.ID {
			data, _ := json.MarshalIndent(e, "", "  ")
			fmt.Println(string(data))
			return nil
		}
	}
	return fmt.Errorf("no ledger entry with id %s", s.ID)
}

// checkContradictions is the doctor section. Reports unresolved
// ledger entries as warnings; reports a build error as a soft warn
// (the ledger may simply not have been built yet on a fresh KB).
//
// Each unresolved entry contributes one warn-level check, named with
// the pair so the doctor scoreboard groups sensibly. The detail line
// surfaces the canonical pair so the user can grep further.
func checkContradictions(root string) []check {
	entries, err := readContradictionLedger(root)
	if err != nil {
		return []check{{
			Section: "contradictions",
			Name:    "ledger",
			Status:  statusWarn,
			Detail:  fmt.Sprintf("read ledger: %v", err),
			Fix:     "run `scribe contradictions build`",
		}}
	}
	if len(entries) == 0 {
		return []check{{
			Section: "contradictions",
			Name:    "ledger",
			Status:  statusOK,
			Detail:  "no contradictions on file",
		}}
	}

	unresolved := 0
	out := make([]check, 0, len(entries)+1)
	for _, e := range entries {
		if e.ResolvedAt != "" {
			continue
		}
		unresolved++
		out = append(out, check{
			Section: "contradictions",
			Name:    e.ID,
			Status:  statusWarn,
			Detail:  fmt.Sprintf("[[%s]] ↔ [[%s]] (first %s)", e.Pair[0], e.Pair[1], e.FirstObservedAt),
			Fix:     fmt.Sprintf("scribe contradictions resolve %s -m \"<reason>\"", e.ID),
		})
	}
	if unresolved == 0 {
		return []check{{
			Section: "contradictions",
			Name:    "ledger",
			Status:  statusOK,
			Detail:  fmt.Sprintf("%d entry(ies), all resolved", len(entries)),
		}}
	}
	return out
}

type ContradictionsResolveCmd struct {
	ID   string `arg:"" help:"Ledger entry ID."`
	Note string `help:"Optional resolution note (recorded with timestamp)." short:"m"`
}

func (r *ContradictionsResolveCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	if err := resolveContradiction(root, r.ID, r.Note); err != nil {
		return err
	}
	logMsg("contradictions", "resolved: %s", r.ID)
	return nil
}
