package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// brokenRewrite is one link rewrite, as the text between [[ and ]].
type brokenRewrite struct{ Old, New string }

// brokenFileFix is the rewritten content of one file.
type brokenFileFix struct {
	Rel      string
	Original []byte
	Content  []byte
	Rewrites []brokenRewrite
}

// brokenKey names a Broken entry: its source path and cleaned raw target.
type brokenKey struct{ Path, Raw string }

// brokenFixPlan is what `scribe link --fix-broken` rewrites, and which of
// the link graph's Broken entries that settles.
type brokenFixPlan struct {
	Files    []brokenFileFix
	fixed    map[brokenKey]bool
	declined map[brokenKey]bool
}

// settles reports whether every occurrence of b is rewritten by the plan.
func (p brokenFixPlan) settles(b brokenLink) bool {
	key := brokenKey{Path: b.SourcePath, Raw: b.Raw}
	return p.fixed[key] && !p.declined[key]
}

type brokenVerdict int

const (
	linkNotBroken brokenVerdict = iota
	linkSafeRewrite
	linkNeedsReview
)

// linkFoldKey lowercases s and collapses runs of spaces, tabs, hyphens and
// underscores into one space.
func linkFoldKey(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == ' ' || r == '\t' || r == '-' || r == '_'
	}), " ")
}

// separatorCaseOnly reports whether target and canonical differ only by
// case, whitespace, hyphen/underscore separators, or a trailing .md on the
// target.
func separatorCaseOnly(target, canonical string) bool {
	want := linkFoldKey(canonical)
	if linkFoldKey(target) == want {
		return true
	}
	stem, ok := strings.CutSuffix(strings.ToLower(target), ".md")
	return ok && linkFoldKey(stem) == want
}

// brokenRewriteFor classifies the link text inner (between [[ and ]]) the
// way the link graph does. A Broken link whose target differs from its
// canonical title only by case or separators is a safe rewrite: newInner
// names the canonical title and keeps any #anchor, |label and table-escape
// backslash. raw is the cleaned target the graph records as Broken.Raw.
func brokenRewriteFor(resolver linkResolver, inner string) (newInner, raw string, verdict brokenVerdict) {
	targetPart, label := inner, ""
	if idx := strings.Index(inner, "|"); idx > 0 {
		targetPart, label = inner[:idx], inner[idx:]
	}
	raw = cleanLinkTarget(targetPart)
	if raw == "" {
		return "", "", linkNotBroken
	}
	target, canonical, status := resolver.resolveTarget(strings.TrimSpace(targetPart))
	if status != linkFuzzy || collapseSpace(canonical) == collapseSpace(target) {
		return "", raw, linkNotBroken
	}
	anchor, ok := strings.CutPrefix(raw, target)
	if !ok || (anchor != "" && !strings.HasPrefix(strings.TrimLeft(anchor, " \t"), "#")) {
		return "", raw, linkNeedsReview
	}
	if !separatorCaseOnly(target, canonical) || strings.ContainsAny(canonical, "|[]\n\r") {
		return "", raw, linkNeedsReview
	}
	newTarget := canonical + anchor
	if _, again, againStatus := resolver.resolveTarget(newTarget); againStatus != linkExact || again != canonical {
		return "", raw, linkNeedsReview
	}
	if strings.HasSuffix(strings.TrimSpace(targetPart), `\`) {
		newTarget += `\`
	}
	return newTarget + label, raw, linkSafeRewrite
}

// relatedByteRange returns the byte range of the top-level related: value
// lines inside content's frontmatter, and the end of the frontmatter.
func relatedByteRange(content []byte) (related byteRange, fmEnd int) {
	span, err := frontmatterSpan(string(content))
	if err != nil {
		return byteRange{}, 0
	}
	lines := strings.Split(string(content[span.YAMLStart:span.YAMLEnd]), "\n")
	start, end := relatedRegion(lines)
	if start < 0 {
		return byteRange{}, span.BodyStart
	}
	offset := span.YAMLStart
	for _, line := range lines[:start] {
		offset += len(line) + 1
	}
	length := 0
	for _, line := range lines[start:end] {
		length += len(line) + 1
	}
	return byteRange{Start: offset, End: offset + length}, span.BodyStart
}

// linkEdit is one planned rewrite of a link's inner text.
type linkEdit struct {
	inner     byteRange
	newInner  string
	raw       string
	inRelated bool
}

// rewriteBrokenLinks plans the safe rewrites in one file. Links in code,
// in frontmatter outside related:, or whose rewrite would need quoting in
// related: are declined, as are links that are broken but not safe.
func rewriteBrokenLinks(content []byte, resolver linkResolver) (fix brokenFileFix, fixed, declined []string) {
	code := codeRanges(content)
	related, fmEnd := relatedByteRange(content)
	var edits []linkEdit
	for _, m := range wikilinkRE.FindAllSubmatchIndex(content, -1) {
		if overlapsAny(byteRange{Start: m[0], End: m[1]}, code) {
			continue
		}
		newInner, raw, verdict := brokenRewriteFor(resolver, string(content[m[2]:m[3]]))
		if verdict == linkNotBroken {
			continue
		}
		inRelated := m[0] >= related.Start && m[1] <= related.End && related.End > 0
		inOtherFrontmatter := m[0] < fmEnd && !inRelated
		if verdict == linkNeedsReview || inOtherFrontmatter || (inRelated && strings.ContainsAny(newInner, `"'\`)) {
			declined = append(declined, raw)
			continue
		}
		edits = append(edits, linkEdit{inner: byteRange{Start: m[2], End: m[3]}, newInner: newInner, raw: raw, inRelated: inRelated})
	}
	if len(edits) == 0 {
		return brokenFileFix{}, nil, declined
	}
	out := applyLinkEdits(content, edits)
	if fmEnd > 0 {
		if _, err := parseFrontmatter(out); err != nil {
			// A related: rewrite broke the YAML: keep the body edits only.
			var bodyEdits []linkEdit
			for _, e := range edits {
				if e.inRelated {
					declined = append(declined, e.raw)
				} else {
					bodyEdits = append(bodyEdits, e)
				}
			}
			edits = bodyEdits
			if len(edits) == 0 {
				return brokenFileFix{}, nil, declined
			}
			out = applyLinkEdits(content, edits)
		}
	}
	fix = brokenFileFix{Original: content, Content: out}
	for _, e := range edits {
		fix.Rewrites = append(fix.Rewrites, brokenRewrite{Old: string(content[e.inner.Start:e.inner.End]), New: e.newInner})
		fixed = append(fixed, e.raw)
	}
	return fix, fixed, declined
}

func applyLinkEdits(content []byte, edits []linkEdit) []byte {
	var buf bytes.Buffer
	buf.Grow(len(content))
	pos := 0
	for _, e := range edits {
		buf.Write(content[pos:e.inner.Start])
		buf.WriteString(e.newInner)
		pos = e.inner.End
	}
	buf.Write(content[pos:])
	return buf.Bytes()
}

// planBrokenLinkFixes scans the same sources as the link graph (no
// generated _ files, no superseded articles) and plans every safe rewrite.
func planBrokenLinkFixes(root string) (brokenFixPlan, error) {
	scan, err := scanArticles(root)
	if err != nil {
		return brokenFixPlan{}, err
	}
	resolver := newLinkResolver(scan.entries)
	plan := brokenFixPlan{fixed: map[brokenKey]bool{}, declined: map[brokenKey]bool{}}
	_, err = walkMarkdownCounting(root, true, func(path string, content []byte) error {
		rel := filepath.ToSlash(relPath(root, path))
		if scan.superseded[rel] {
			return nil
		}
		fix, fixed, declined := rewriteBrokenLinks(content, resolver)
		for _, raw := range fixed {
			plan.fixed[brokenKey{Path: rel, Raw: raw}] = true
		}
		for _, raw := range declined {
			plan.declined[brokenKey{Path: rel, Raw: raw}] = true
		}
		if len(fix.Rewrites) > 0 {
			fix.Rel = rel
			plan.Files = append(plan.Files, fix)
		}
		return nil
	})
	return plan, err
}

// withoutSettledBroken drops the Broken entries the plan rewrites and
// counts them in SafeBroken.
func withoutSettledBroken(g linkGraph, plan brokenFixPlan) linkGraph {
	kept := make([]brokenLink, 0, len(g.Broken))
	for _, b := range g.Broken {
		if plan.settles(b) {
			g.SafeBroken++
			continue
		}
		kept = append(kept, b)
	}
	g.Broken = kept
	return g
}

// brokenFixResult counts what applyBrokenFixPlan did.
type brokenFixResult struct{ Links, Files, Stale, Review int }

// applyBrokenFixPlan writes every planned file, or with dryRun only counts
// them. A file that changed since the scan is left alone and counted Stale.
// onRewrite, when set, is called for each rewrite before its file is written.
func applyBrokenFixPlan(root string, plan brokenFixPlan, dryRun bool, onRewrite func(rel string, r brokenRewrite)) (brokenFixResult, error) {
	res := brokenFixResult{Review: len(plan.declined)}
	for _, f := range plan.Files {
		if onRewrite != nil {
			for _, r := range f.Rewrites {
				onRewrite(f.Rel, r)
			}
		}
		if !dryRun {
			wrote, err := writeBrokenFix(root, f)
			if err != nil {
				return res, err
			}
			if !wrote {
				res.Stale++
				continue
			}
		}
		res.Links += len(f.Rewrites)
		res.Files++
	}
	return res, nil
}

// fixSafeBrokenLinks rewrites the safe Broken links before a dream, so its
// edits land in the dream commit and the link report keeps its bounded list
// for the links that need judgment. Failures degrade the run, never block it.
func fixSafeBrokenLinks(root string) {
	plan, err := planBrokenLinkFixes(root)
	if err == nil {
		var res brokenFixResult
		res, err = applyBrokenFixPlan(root, plan, false, nil)
		if res.Links > 0 || res.Stale > 0 {
			logMsg("dream", "fixed %d broken link(s) in %d file(s) (%d file(s) changed mid-scan, left alone)", res.Links, res.Files, res.Stale)
		}
		setRunStat("broken_rewritten", res.Links)
	}
	if err != nil {
		recordDegraded("dream_fix_broken", err)
		logMsg("dream", "broken-link fix skipped: %v", err)
	}
}

// runFixBroken is `scribe link --fix-broken`: rewrite every safe Broken
// link in place, or with dryRun print what would change.
func runFixBroken(root string, dryRun bool) error {
	plan, err := planBrokenLinkFixes(root)
	if err != nil {
		return fmt.Errorf("plan broken-link fixes: %w", err)
	}
	res, err := applyBrokenFixPlan(root, plan, dryRun, func(rel string, r brokenRewrite) {
		fmt.Printf("%s: [[%s]] → [[%s]]\n", rel, r.Old, r.New)
	})
	if err != nil {
		return err
	}
	verb := "rewrote"
	if dryRun {
		verb = "would rewrite"
	}
	fmt.Printf("%s %d link(s) in %d file(s); %d broken link(s) need review", verb, res.Links, res.Files, res.Review)
	if res.Stale > 0 {
		fmt.Printf("; %d file(s) changed during the scan were left alone", res.Stale)
	}
	fmt.Println()
	mergeRunStats(map[string]any{
		"broken_rewritten": res.Links,
		"broken_files":     res.Files,
		"broken_review":    res.Review,
		"broken_stale":     res.Stale,
		"dry_run":          dryRun,
	})
	return nil
}

// writeBrokenFix writes one file atomically, unless it changed since the
// scan read it.
func writeBrokenFix(root string, f brokenFileFix) (bool, error) {
	path := filepath.Join(root, filepath.FromSlash(f.Rel))
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false, nil //nolint:nilerr // vanished or replaced since the scan: leave it
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, f.Original) {
		return false, nil //nolint:nilerr // changed since the scan: leave it
	}
	if err := writeFileAtomic(path, f.Content, info.Mode().Perm()); err != nil {
		return false, fmt.Errorf("write %s: %w", f.Rel, err)
	}
	return true, nil
}
