package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Phase 5B: tiered index hint.
//
// Every article in the KB carries an `index_tier` so qmd ranking can
// weight long-form research above tweet stubs and link previews. A
// 10-word fxtwitter quote and a 200-line research note used to land
// in the same retrieval bucket; the tier breaks that tie at search
// time.
//
// Five tiers, closed set:
//
//	stub      — capture-only or extremely short; excluded from search
//	brief     — short (single-tweet, link preview); rarely worth surfacing
//	standard  — ordinary curated article
//	deep      — long-form or richly-sectioned reference material
//	reference — explicit canonical artifact (decisions, policies)
//
// scribe computes the tier on every absorb/lint pass. Frontmatter may
// carry `index_tier_override:` to pin a specific value (e.g. tag a
// short article as `reference`). The override is preserved across
// recomputes; the computed `index_tier:` is overwritten freely.

const (
	TierStub      = "stub"
	TierBrief     = "brief"
	TierStandard  = "standard"
	TierDeep      = "deep"
	TierReference = "reference"
)

// validIndexTiers is the closed set lint and validate.go gate against.
var validIndexTiers = map[string]bool{
	TierStub:      true,
	TierBrief:     true,
	TierStandard:  true,
	TierDeep:      true,
	TierReference: true,
}

// Word-count thresholds. Picked empirically against scriptorium's
// 1700-article distribution: 80 words is the upper bound of a typical
// fxtwitter capture; 200 is the lower bound of a meaningful curated
// article; 2000 is roughly where solutions/patterns get long enough
// to need section navigation (and where Phase 5A sidecars start
// carrying real value).
const (
	tierStubWordMax     = 80
	tierBriefWordMax    = 199
	tierStandardWordMax = 1999
	tierDeepSectionMin  = 5
)

// computeIndexTier returns the computed tier for an article based on
// its body and frontmatter metadata. The override (when set on the
// frontmatter) wins unconditionally as long as it's a valid tier
// value — invalid overrides fall through to computation so a typo
// doesn't permanently mis-shelve an article.
//
// sections is a section count (typically from the sidecar); pass 0
// when the sidecar is absent or empty. The tier still resolves
// without a sidecar — sections only promote `standard` to `deep`,
// they're not required for the lower buckets.
func computeIndexTier(fm *Frontmatter, body []byte, sections int) string {
	if fm != nil {
		if v := strings.TrimSpace(fm.IndexTierOverride); v != "" && validIndexTiers[v] {
			return v
		}
	}

	via := ""
	if fm != nil {
		// fetched_via lives in raw-article frontmatter only and is not on the
		// Frontmatter struct; pull it from rawmap-based callers via a
		// dedicated helper instead. Here we have no via signal.
		_ = via
	}

	words := countBodyWords(body)

	switch {
	case words <= tierStubWordMax:
		return TierStub
	case words <= tierBriefWordMax:
		return TierBrief
	case words <= tierStandardWordMax:
		if sections >= tierDeepSectionMin {
			return TierDeep
		}
		return TierStandard
	default:
		return TierDeep
	}
}

// computeIndexTierForRaw is the variant for raw articles where we
// have access to fetched_via. fxtwitter captures are clamped to
// brief regardless of length (a 250-word tweet thread is still a
// tweet for retrieval purposes), and fetched_via=stub forces stub.
func computeIndexTierForRaw(fetchedVia string, fm *Frontmatter, body []byte, sections int) string {
	if fm != nil {
		if v := strings.TrimSpace(fm.IndexTierOverride); v != "" && validIndexTiers[v] {
			return v
		}
	}
	via := strings.ToLower(strings.TrimSpace(fetchedVia))
	if via == "stub" {
		return TierStub
	}
	if via == "fxtwitter" {
		return TierBrief
	}
	return computeIndexTier(fm, body, sections)
}

// countBodyWords approximates a word count by splitting on whitespace.
// Cheap and good enough — the tier thresholds have built-in slack so
// off-by-a-few words at boundaries don't cause flapping.
func countBodyWords(body []byte) int {
	return len(strings.Fields(string(body)))
}

// IndexTierForArticle is the public entry point used by lint and the
// CLI. Reads the article from disk, parses frontmatter, looks up
// section count from the sidecar (when present), and returns the
// computed tier. Falls back to TierStandard on any error so a broken
// article doesn't crash the calling command.
func IndexTierForArticle(root, articlePath string) (string, error) {
	content, err := os.ReadFile(articlePath)
	if err != nil {
		return TierStandard, err
	}
	fm, _ := parseFrontmatter(content)
	body := articleBody(content)
	sections := 0
	if sc, _ := readSectionsSidecar(root, articlePath); sc != nil {
		sections = len(sc.Sections)
	}
	if isRawArticle(articlePath) {
		via := rawArticleFetchedVia(content)
		return computeIndexTierForRaw(via, fm, body, sections), nil
	}
	return computeIndexTier(fm, body, sections), nil
}

// isRawArticle reports whether a path points into the raw inbox tree.
// Raw articles live under raw/articles/ regardless of KB layout
// (scribe init plants both `raw/articles/` and the wiki dirs at root).
func isRawArticle(path string) bool {
	p := filepath.ToSlash(path)
	return strings.Contains(p, "/raw/articles/") || strings.HasPrefix(p, "raw/articles/")
}

// rawArticleFetchedVia reads the `fetched_via:` line from raw-article
// frontmatter. Empty string when absent.
func rawArticleFetchedVia(content []byte) string {
	raw, err := parseFrontmatterRaw(content)
	if err != nil {
		return ""
	}
	if v, ok := raw["fetched_via"].(string); ok {
		return v
	}
	return ""
}

// TierCmd is the CLI surface for Phase 5B.
//
//	scribe tier list [--missing] [--tier <t>]   show tier per article
//	scribe tier compute <article>               recompute one article's tier
//	scribe tier set <article> <tier>            pin an override
//	scribe tier write [--all] [--missing-only]  persist computed tier into
//	                                            frontmatter for every article
type TierCmd struct {
	List    TierListCmd    `cmd:"" help:"List tier (computed) for every wiki article."`
	Compute TierComputeCmd `cmd:"" help:"Recompute tier for one article and print the result."`
	Set     TierSetCmd     `cmd:"" help:"Pin index_tier_override on one article."`
	Write   TierWriteCmd   `cmd:"" help:"Persist computed index_tier into every article's frontmatter."`
}

// TierListCmd prints "<tier>  <path>" lines so the output greps and
// pipes well. --missing filters to articles that don't yet have a
// stored tier in frontmatter (useful for the v0.2.5 backfill).
// --tier <name> filters to one bucket.
type TierListCmd struct {
	Missing bool   `help:"Show only articles whose stored tier is empty."`
	Tier    string `help:"Filter to one computed tier."`
}

func (l *TierListCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	if l.Tier != "" && !validIndexTiers[l.Tier] {
		return errors.New("--tier must be one of: stub, brief, standard, deep, reference")
	}
	counts := map[string]int{}
	err = walkArticles(root, func(path string, content []byte) error {
		fm, _ := parseFrontmatter(content)
		body := articleBody(content)
		sections := 0
		if sc, _ := readSectionsSidecar(root, path); sc != nil {
			sections = len(sc.Sections)
		}
		tier := computeIndexTier(fm, body, sections)
		stored := ""
		if fm != nil {
			stored = strings.TrimSpace(fm.IndexTier)
		}
		if l.Missing && stored != "" {
			return nil
		}
		if l.Tier != "" && tier != l.Tier {
			return nil
		}
		counts[tier]++
		marker := " "
		if stored != tier && stored != "" {
			marker = "*"
		}
		fmt.Printf("%-9s%s  %s\n", tier, marker, relPath(root, path))
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr)
	for _, t := range []string{TierStub, TierBrief, TierStandard, TierDeep, TierReference} {
		if counts[t] > 0 {
			fmt.Fprintf(os.Stderr, "  %-9s %d\n", t, counts[t])
		}
	}
	return nil
}

// TierComputeCmd prints the computed tier (and the stored value, if
// any) for a single article. Useful for sanity-checking the
// classifier on edge cases without rewriting the file.
type TierComputeCmd struct {
	Article string `arg:"" help:"Article path or title."`
}

func (c *TierComputeCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	path, err := resolveArticleArg(root, c.Article)
	if err != nil {
		return err
	}
	tier, err := IndexTierForArticle(root, path)
	if err != nil {
		return err
	}
	content, _ := os.ReadFile(path)
	fm, _ := parseFrontmatter(content)
	stored := ""
	override := ""
	if fm != nil {
		stored = fm.IndexTier
		override = fm.IndexTierOverride
	}
	body := articleBody(content)
	fmt.Printf("article : %s\n", relPath(root, path))
	fmt.Printf("computed: %s\n", tier)
	if stored != "" {
		fmt.Printf("stored  : %s\n", stored)
	}
	if override != "" {
		fmt.Printf("override: %s\n", override)
	}
	fmt.Printf("words   : %d\n", countBodyWords(body))
	if sc, _ := readSectionsSidecar(root, path); sc != nil {
		fmt.Printf("sections: %d\n", len(sc.Sections))
	}
	return nil
}

// TierSetCmd writes index_tier_override into frontmatter so the human
// pin survives recomputes. Empty value clears the override.
type TierSetCmd struct {
	Article string `arg:"" help:"Article path or title."`
	Tier    string `arg:"" help:"One of: stub, brief, standard, deep, reference. Empty string clears the override."`
}

func (s *TierSetCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	path, err := resolveArticleArg(root, s.Article)
	if err != nil {
		return err
	}
	tier := strings.TrimSpace(s.Tier)
	if tier != "" && !validIndexTiers[tier] {
		return fmt.Errorf("invalid tier: %q (want stub|brief|standard|deep|reference, or empty to clear)", tier)
	}
	return updateFrontmatterField(path, "index_tier_override", tier)
}

// TierWriteCmd backfills computed `index_tier:` into frontmatter for
// every article that lacks one (--missing-only) or for everything
// (--all). Without either flag, the command exits with a usage hint
// to avoid silent mass writes.
type TierWriteCmd struct {
	All         bool `help:"Write computed tier for every article, overwriting existing values."`
	MissingOnly bool `help:"Write computed tier only for articles that don't have one yet."`
	DryRun      bool `help:"Print what would change without modifying files." short:"n"`
}

func (w *TierWriteCmd) Run() error {
	if !w.All && !w.MissingOnly {
		return errors.New("pick one: --all (rewrite every article) or --missing-only (only articles without a stored tier)")
	}
	root, err := kbDir()
	if err != nil {
		return err
	}
	wrote, skipped, scanned := 0, 0, 0
	err = walkArticles(root, func(path string, content []byte) error {
		scanned++
		fm, _ := parseFrontmatter(content)
		body := articleBody(content)
		sections := 0
		if sc, _ := readSectionsSidecar(root, path); sc != nil {
			sections = len(sc.Sections)
		}
		tier := computeIndexTier(fm, body, sections)

		stored := ""
		if fm != nil {
			stored = strings.TrimSpace(fm.IndexTier)
		}
		if w.MissingOnly && stored != "" {
			skipped++
			return nil
		}
		if stored == tier {
			skipped++
			return nil
		}
		if w.DryRun {
			fmt.Printf("WOULD: %-9s -> %s (was %q)\n", tier, relPath(root, path), stored)
			wrote++
			return nil
		}
		if err := updateFrontmatterField(path, "index_tier", tier); err != nil {
			logMsg("tier", "write failed for %s: %v", relPath(root, path), err)
			return nil
		}
		wrote++
		return nil
	})
	if err != nil {
		return err
	}
	logMsg("tier", "tier write done: scanned=%d wrote=%d skipped=%d (dry_run=%v)",
		scanned, wrote, skipped, w.DryRun)
	runStats = map[string]any{
		"tier_scanned": scanned,
		"tier_wrote":   wrote,
		"tier_skipped": skipped,
	}
	return nil
}

// updateFrontmatterField writes (or removes) one frontmatter key in a
// markdown file. Existing key with same value → no-op. Empty value →
// the key is removed entirely (so `tier set <article> ""` cleanly
// clears an override). New keys land just before the closing `---`.
//
// Intentionally minimal — no full YAML round-trip — to preserve the
// human-authored ordering and any incidental whitespace. Mirrors the
// approach in lint_fix.go's normalize* helpers.
func updateFrontmatterField(path, key, value string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	s := string(data)
	if !strings.HasPrefix(s, "---") {
		return errors.New("no frontmatter delimiter")
	}
	end := strings.Index(s[3:], "\n---")
	if end < 0 {
		return errors.New("no closing frontmatter delimiter")
	}
	fmBlock := s[3 : end+3]
	rest := s[end+7:] // skip `\n---\n`

	lines := strings.Split(fmBlock, "\n")
	keyFound := false
	out := make([]string, 0, len(lines)+1)
	for _, line := range lines {
		k, _, ok := splitFrontmatterLine(line)
		if !ok {
			out = append(out, line)
			continue
		}
		if k != key {
			out = append(out, line)
			continue
		}
		keyFound = true
		if value == "" {
			// Drop the line entirely (clears the field).
			continue
		}
		out = append(out, key+": "+value)
	}
	if !keyFound && value != "" {
		// Insert just before the trailing blank line, if any.
		insertAt := len(out)
		for insertAt > 0 && strings.TrimSpace(out[insertAt-1]) == "" {
			insertAt--
		}
		newLine := key + ": " + value
		out = append(out[:insertAt], append([]string{newLine}, out[insertAt:]...)...)
	}

	newFM := strings.Join(out, "\n")
	newContent := "---" + newFM + "\n---" + rest
	if newContent == s {
		return nil
	}
	// Bump updated: when we touched the file, mirroring scribe's
	// elsewhere-used convention. Skip when only updating the override
	// to avoid noise on humans who pin a value.
	if key == "index_tier" {
		newContent = bumpUpdatedTo(newContent, time.Now().UTC().Format("2006-01-02"))
	}
	return os.WriteFile(path, []byte(newContent), 0o644)
}

// bumpUpdatedTo replaces the `updated:` line in frontmatter with the
// given date. No-op when the field is absent or already current. Used
// by tier write so a sweep over a 1700-article KB doesn't silently
// change file mtimes without recording the touch.
func bumpUpdatedTo(content, date string) string {
	if !strings.HasPrefix(content, "---") {
		return content
	}
	end := strings.Index(content[3:], "\n---")
	if end < 0 {
		return content
	}
	fmBlock := content[3 : end+3]
	rest := content[end+7:]
	lines := strings.Split(fmBlock, "\n")
	for i, line := range lines {
		k, v, ok := splitFrontmatterLine(line)
		if !ok || k != "updated" {
			continue
		}
		if strings.Trim(v, `"' `) == date {
			return content
		}
		lines[i] = "updated: " + date
		return "---" + strings.Join(lines, "\n") + "\n---" + rest
	}
	return content
}
