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

// dream_focus.go gives the dream a small set of article PAIRS to judge,
// with body excerpts, instead of metadata alone.
//
// Why: the orchestrator dream was a structural no-op. From ≥2026-10-26 on,
// every run applied only its log line. It was shown the 40 newest articles
// (none stale by construction) and contradiction candidates drawn from the
// whole KB (outside that inventory), as metadata only. A model that has
// never read two articles cannot tell whether they duplicate or contradict
// each other, so it correctly declined to act.
//
// Now Go picks the pairs worth a look, from two deterministic signals:
//   - near-duplicate bodies (the same detector as `scribe lint --duplicates`)
//   - articles of the same type sharing ≥2 discriminative tags, where
//     conflicting claims are possible.
//
// The pairs are inlined with bounded excerpts, and pairs already reviewed
// within dreamFocusRecheckDays are skipped, so each run looks at new ones.
// Only two outcomes are allowed, and only on a focus-pair path (see
// guardDreamFocusActions):
//   - mark a duplicate `status: superseded`, linking the keeper in `related`
//   - append a conflict note
//
// Neither deletes knowledge: superseded articles stay on disk (KB rule 12).

const (
	dreamFocusMaxPairs     = 4
	dreamFocusHotMaxPairs  = 2
	dreamFocusBodyMaxBytes = 2500
	dreamFocusRecheckDays  = 60
	// dreamFocusTagMaxDocs drops tags so common they say nothing about
	// two articles covering the same claim ("elixir", "article", ...).
	dreamFocusTagMaxDocs = 25
	dreamFocusLedgerRel  = "output/dream-focus-reviewed.json"
)

type dreamFocusDoc struct {
	Path, Title, Type, Domain, Updated, Confidence string
	Body                                           string
}

type dreamFocusPair struct {
	A, B   dreamFocusDoc
	Kind   string // "duplicate" | "overlap"
	Reason string
}

func (p dreamFocusPair) key() string {
	a, b := p.A.Path, p.B.Path
	if b < a {
		a, b = b, a
	}
	return a + "|" + b
}

// dreamFocusPairs picks up to maxPairs pairs to review. domain, when set,
// keeps only pairs whose two articles are both in that domain.
func dreamFocusPairs(root, domain string, maxPairs int, now time.Time) []dreamFocusPair {
	docs := map[string]dreamFocusDoc{}
	tags := map[string][]string{} // tag → paths
	_ = walkArticles(root, func(path string, content []byte) error {
		fm, err := parseFrontmatter(content)
		if err != nil || fm == nil || isSuperseded(fm) || fm.Rolling {
			return nil //nolint:nilerr // unparseable or out-of-scope article: skip it
		}
		if domain != "" && fm.Domain != domain {
			return nil
		}
		rel := relPath(root, path)
		docs[rel] = dreamFocusDoc{
			Path: rel, Title: fm.Title, Type: fm.Type, Domain: fm.Domain,
			Updated: stringFromAny(fm.Updated), Confidence: fm.Confidence,
			Body: bodyAfterFrontmatter(string(content)),
		}
		if list, ok := fm.Tags.([]any); ok {
			for _, t := range list {
				if s, ok := t.(string); ok && s != "" {
					tags[strings.ToLower(s)] = append(tags[strings.ToLower(s)], rel)
				}
			}
		}
		return nil
	})

	reviewed := loadDreamFocusLedger(root)
	cutoff := now.AddDate(0, 0, -dreamFocusRecheckDays).Format("2006-01-02")
	seen := map[string]bool{}
	var out []dreamFocusPair
	add := func(p dreamFocusPair) bool {
		k := p.key()
		if seen[k] || reviewed[k] >= cutoff {
			return false
		}
		seen[k] = true
		out = append(out, p)
		return len(out) >= maxPairs
	}

	// 1. Near-duplicate bodies, strongest overlap first.
	for _, d := range findNearDuplicates(collectContentDocs(root), dupDefaultOverlap) {
		a, okA := docs[d.A]
		b, okB := docs[d.B]
		if !okA || !okB {
			continue // other domain, superseded, or rolling
		}
		reason := fmt.Sprintf("near-duplicate bodies: %.0f%% of the smaller one's vocabulary is in the other (jaccard %.2f)", d.Overlap*100, d.Jaccard)
		if add(dreamFocusPair{A: a, B: b, Kind: "duplicate", Reason: reason}) {
			return out
		}
	}

	// 2. Same type, ≥2 shared discriminative tags.
	shared := map[string][]string{}
	for tag, paths := range tags {
		if len(paths) < 2 || len(paths) > dreamFocusTagMaxDocs {
			continue
		}
		sort.Strings(paths)
		for i := range paths {
			for j := i + 1; j < len(paths); j++ {
				if docs[paths[i]].Type != docs[paths[j]].Type {
					continue
				}
				k := paths[i] + "|" + paths[j]
				shared[k] = append(shared[k], tag)
			}
		}
	}
	keys := make([]string, 0, len(shared))
	for k, t := range shared {
		if len(t) >= 2 {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(shared[keys[i]]) != len(shared[keys[j]]) {
			return len(shared[keys[i]]) > len(shared[keys[j]])
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		pa, pb, _ := strings.Cut(k, "|")
		t := shared[k]
		sort.Strings(t)
		reason := "same type, shared tags: " + strings.Join(t, ", ")
		if add(dreamFocusPair{A: docs[pa], B: docs[pb], Kind: "overlap", Reason: reason}) {
			return out
		}
	}
	return out
}

// renderDreamFocus formats the pairs for the prompt.
func renderDreamFocus(pairs []dreamFocusPair) string {
	if len(pairs) == 0 {
		return "(no focus pairs this run)"
	}
	var b strings.Builder
	for i, p := range pairs {
		fmt.Fprintf(&b, "### Pair %d: %s\nWhy flagged: %s\n\n", i+1, p.Kind, p.Reason)
		for _, d := range []dreamFocusDoc{p.A, p.B} {
			fmt.Fprintf(&b, "#### %s\ntitle: %s | type: %s | updated: %s | confidence: %s\n\n", d.Path, d.Title, d.Type, d.Updated, d.Confidence)
			body := d.Body
			if len(body) > dreamFocusBodyMaxBytes {
				body = truncateBytes(body, dreamFocusBodyMaxBytes) + "\n[… cut …]"
			}
			b.WriteString(body)
			b.WriteString("\n\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// dreamFocusInventory renders the focus articles first, then fills with
// the regular newest-first sample up to maxEntries, so the inventory names
// the articles the model is actually asked about.
func dreamFocusInventory(root, domain string, pairs []dreamFocusPair, maxEntries int) string {
	var lines []string
	seen := map[string]bool{}
	for _, p := range pairs {
		for _, d := range []dreamFocusDoc{p.A, p.B} {
			if seen[d.Path] {
				continue
			}
			seen[d.Path] = true
			lines = append(lines, fmt.Sprintf("- %s | %s | %s | %s | conf=%s | upd=%s", d.Path, d.Title, d.Type, d.Domain, d.Confidence, d.Updated))
		}
	}
	for l := range strings.SplitSeq(dreamSampleInventory(root, domain, maxEntries), "\n") {
		if len(lines) >= maxEntries {
			break
		}
		path, _, _ := strings.Cut(strings.TrimPrefix(l, "- "), " | ")
		if l == "" || seen[path] {
			continue
		}
		seen[path] = true
		lines = append(lines, l)
	}
	return strings.Join(lines, "\n")
}

// guardDreamFocusActions enforces the focus contract before apply:
//   - `status: superseded` may only land on one side of a "duplicate"
//     pair. Elsewhere the key is dropped, because the model saw no body
//     that could justify it.
//   - when it lands, `related` must link the other side. If the model
//     forgot, the link is added, so the superseded page points at its keeper.
//   - superseding BOTH sides of a pair keeps only the first.
//
// Returns how many supersede marks survived.
func guardDreamFocusActions(root string, env *WikiActionEnvelope, pairs []dreamFocusPair) int {
	partner := map[string]dreamFocusDoc{}
	pairOf := map[string]string{}
	for _, p := range pairs {
		if p.Kind != "duplicate" {
			continue
		}
		partner[p.A.Path], partner[p.B.Path] = p.B, p.A
		pairOf[p.A.Path], pairOf[p.B.Path] = p.key(), p.key()
	}
	used := map[string]bool{}
	kept := 0
	for i := range env.Actions {
		a := &env.Actions[i]
		if a.Op != "update_frontmatter" || a.Frontmatter == nil {
			continue
		}
		status, ok := a.Frontmatter["status"].(string)
		if !ok || !isSupersededStatus(status) {
			continue
		}
		other, isDup := partner[a.Path]
		if !isDup || used[pairOf[a.Path]] {
			logMsg("dream", "focus guard: dropped status=superseded on %s (not an unreviewed duplicate-pair side)", a.Path)
			fm := make(map[string]any, len(a.Frontmatter))
			for k, v := range a.Frontmatter {
				if k != "status" {
					fm[k] = v
				}
			}
			a.Frontmatter = fm
			continue
		}
		used[pairOf[a.Path]] = true
		kept++
		fm := make(map[string]any, len(a.Frontmatter)+1)
		for k, v := range a.Frontmatter {
			fm[k] = v
		}
		if !relatedMentions(fm["related"], other.Title) {
			fm["related"] = appendRelated(nil, fm["related"], "[["+other.Title+"]]")
		}
		a.Frontmatter = fm
	}
	// update_frontmatter replaces related wholesale, so every proposal
	// that sets it is merged with the article's existing links: a dream
	// can add links but never drop them. Without this, a supersede whose
	// proposed related already named the keeper wiped the article's
	// other links (seen live 2026-11-10).
	for i := range env.Actions {
		a := &env.Actions[i]
		if a.Op != "update_frontmatter" || a.Frontmatter == nil {
			continue
		}
		if _, has := a.Frontmatter["related"]; !has {
			continue
		}
		fm := make(map[string]any, len(a.Frontmatter))
		for k, v := range a.Frontmatter {
			fm[k] = v
		}
		fm["related"] = appendRelated(readRelatedList(root, a.Path), fm["related"], "")
		a.Frontmatter = fm
	}
	// Drop actions the guard emptied, so they don't count as applied.
	actions := env.Actions[:0]
	for _, a := range env.Actions {
		if a.Op == "update_frontmatter" && len(a.Frontmatter) == 0 {
			continue
		}
		actions = append(actions, a)
	}
	env.Actions = actions
	return kept
}

// relatedMentions reports whether a related value already names title.
func relatedMentions(v any, title string) bool {
	switch t := v.(type) {
	case string:
		return strings.Contains(t, title)
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok && strings.Contains(s, title) {
				return true
			}
		}
	case []string:
		for _, s := range t {
			if strings.Contains(s, title) {
				return true
			}
		}
	}
	return false
}

// appendRelated merges the article's existing related list, whatever the
// model proposed, and link. update_frontmatter replaces a key wholesale,
// so the existing entries must be carried over or they would be lost.
func appendRelated(existing []string, proposed any, link string) []any {
	out := []any{}
	seen := map[string]bool{}
	push := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range existing {
		push(s)
	}
	switch t := proposed.(type) {
	case string:
		push(t)
	case []string:
		for _, s := range t {
			push(s)
		}
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok {
				push(s)
			}
		}
	}
	push(link)
	return out
}

// readRelatedList returns an article's current related entries.
func readRelatedList(root, rel string) []string {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return nil
	}
	fm, err := parseFrontmatter(data)
	if err != nil || fm == nil {
		return nil
	}
	var out []string
	switch t := fm.Related.(type) {
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	case string:
		out = append(out, t)
	}
	return out
}

// --- reviewed-pair ledger ---

func loadDreamFocusLedger(root string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(filepath.Join(root, dreamFocusLedgerRel))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	return out
}

// markDreamFocusReviewed records the pairs as reviewed today, whatever the
// model decided, so the next run moves on to other pairs. Entries older
// than the recheck window are pruned.
func markDreamFocusReviewed(root string, pairs []dreamFocusPair, now time.Time) {
	if len(pairs) == 0 {
		return
	}
	ledger := loadDreamFocusLedger(root)
	cutoff := now.AddDate(0, 0, -dreamFocusRecheckDays).Format("2006-01-02")
	for k, d := range ledger {
		if d < cutoff {
			delete(ledger, k)
		}
	}
	today := now.Format("2006-01-02")
	for _, p := range pairs {
		ledger[p.key()] = today
	}
	if err := writeJSONFile(filepath.Join(root, dreamFocusLedgerRel), ledger); err != nil {
		logMsg("dream", "focus ledger: %v", err)
	}
}

// dreamContentActions counts applied entries that changed an article,
// i.e. not meta ops such as the log line (labeled "meta:…"). This is the outcome metric the no-op went
// unnoticed without: envelope_actions_applied was 1 on every no-op run.
func dreamContentActions(applied []string) int {
	n := 0
	for _, a := range applied {
		if !strings.HasPrefix(a, "meta:") {
			n++
		}
	}
	return n
}
