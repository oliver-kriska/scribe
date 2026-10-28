package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	dreamLinkListMax = 40
	dreamLinkLineMax = 200
	// wantedPageMinArticles is the linking-article count at which a dangling
	// target becomes a Phase 3.5 stub candidate.
	wantedPageMinArticles = 3
	wantedPagePathsShown  = 3
	dreamLinkDateLayout   = "2006-01-02"
)

// dreamLinkReport renders the resolved link graph as the bounded text the
// monolithic dream prompt embeds. Every `{{` becomes `{ {` so loadPrompt's
// placeholder strip cannot rewrite KB text.
func dreamLinkReport(g linkGraph, today time.Time, staleDays int) string {
	var sb strings.Builder
	if skipped := g.Unparsed + g.Unreadable; skipped > 0 {
		fmt.Fprintf(&sb, "Note: %d files could not be read or parsed; links in them are missing from this report.\n\n", skipped)
	}
	sb.WriteString("### Broken links (fix: change [[Raw]] to [[Canonical]], or [[Canonical|Raw]] to keep the wording)\n")
	sb.WriteString(renderCappedList(brokenLinkLines(g)))
	fmt.Fprintf(&sb, "\n### Decay candidates (no links from other articles, no outbound links, updated more than %d days ago)\n", staleDays)
	sb.WriteString(renderCappedList(decayCandidateLines(g, today, staleDays)))
	fmt.Fprintf(&sb, "\n### Wanted pages (linked from %d or more articles, no page exists)\n", wantedPageMinArticles)
	sb.WriteString(renderCappedList(wantedPageLines(g)))
	return escapeBraceRuns(sb.String())
}

// escapeBraceRuns splits every `{{`, including runs of 3 or more, so no
// placeholder-shaped text reaches loadPrompt. Each pass shortens a run.
func escapeBraceRuns(text string) string {
	for strings.Contains(text, "{{") {
		text = strings.ReplaceAll(text, "{{", "{ {")
	}
	return text
}

// renderCappedList cuts each line, keeps the first dreamLinkListMax and
// says exactly how many were dropped.
func renderCappedList(lines []string) string {
	if len(lines) == 0 {
		return "- none\n"
	}
	var sb strings.Builder
	for _, line := range lines[:min(len(lines), dreamLinkListMax)] {
		sb.WriteString(truncateBytes(line, dreamLinkLineMax) + "\n")
	}
	if extra := len(lines) - dreamLinkListMax; extra > 0 {
		fmt.Fprintf(&sb, "- (+%d more not shown)\n", extra)
	}
	return sb.String()
}

func brokenLinkLines(g linkGraph) []string {
	broken := slices.Clone(g.Broken)
	slices.SortFunc(broken, func(a, b brokenLink) int {
		return cmp.Or(cmp.Compare(a.SourcePath, b.SourcePath), cmp.Compare(a.Raw, b.Raw))
	})
	lines := make([]string, 0, len(broken))
	for _, link := range broken {
		lines = append(lines, fmt.Sprintf("- %s: [[%s]] → [[%s]]", link.SourcePath, link.Raw, link.Canonical))
	}
	return lines
}

func hasNonGeneratedEdge(edges []linkEdge) bool {
	return slices.ContainsFunc(edges, func(e linkEdge) bool { return !e.Generated })
}

func decayCandidateLines(g linkGraph, today time.Time, staleDays int) []string {
	cutoff := today.AddDate(0, 0, -staleDays)
	var candidates []graphArticle
	for _, article := range g.Articles {
		if article.Rolling || article.Outbound != 0 || hasNonGeneratedEdge(g.Inbound[article.Title]) {
			continue
		}
		updated, err := time.Parse(dreamLinkDateLayout, article.Updated)
		if err != nil || !updated.Before(cutoff) {
			continue
		}
		candidates = append(candidates, article)
	}
	slices.SortFunc(candidates, func(a, b graphArticle) int {
		return cmp.Or(cmp.Compare(a.Updated, b.Updated), cmp.Compare(a.Path, b.Path))
	})
	lines := make([]string, 0, len(candidates))
	for _, article := range candidates {
		lines = append(lines, fmt.Sprintf("- %s | %s | updated %s", article.Path, article.Title, article.Updated))
	}
	return lines
}

// wantedGroup collects the dangling spellings that slugify to one page.
type wantedGroup struct {
	spellingLinks map[string]int // raw spelling -> distinct linking articles
	sources       map[string]bool
}

func wantedPageLines(g linkGraph) []string {
	groups := map[string]*wantedGroup{}
	for raw, edges := range g.Missing {
		key := slugify(raw)
		if key == "" {
			key = "\x00" + raw // empty slug: its own group per key
		}
		group := groups[key]
		if group == nil {
			group = &wantedGroup{spellingLinks: map[string]int{}, sources: map[string]bool{}}
			groups[key] = group
		}
		spellingSources := map[string]bool{}
		for _, edge := range edges {
			if !edge.Generated {
				spellingSources[edge.SourcePath] = true
				group.sources[edge.SourcePath] = true
			}
		}
		group.spellingLinks[raw] = len(spellingSources)
	}
	type wantedLine struct {
		label string
		count int
		text  string
	}
	var wanted []wantedLine
	for _, group := range groups {
		if len(group.sources) < wantedPageMinArticles {
			continue
		}
		label := mostLinkedSpelling(group.spellingLinks)
		paths := make([]string, 0, len(group.sources))
		for path := range group.sources {
			paths = append(paths, path)
		}
		slices.Sort(paths)
		shown := paths[:min(len(paths), wantedPagePathsShown)]
		wanted = append(wanted, wantedLine{
			label: label,
			count: len(paths),
			text:  fmt.Sprintf("- [[%s]] — %d articles: %s", label, len(paths), strings.Join(shown, ", ")),
		})
	}
	slices.SortFunc(wanted, func(a, b wantedLine) int {
		return cmp.Or(cmp.Compare(b.count, a.count), cmp.Compare(a.label, b.label))
	})
	lines := make([]string, 0, len(wanted))
	for _, line := range wanted {
		lines = append(lines, line.text)
	}
	return lines
}

// mostLinkedSpelling picks the spelling with the most linking articles;
// ties go to the lowest string.
func mostLinkedSpelling(spellingLinks map[string]int) string {
	best, bestCount := "", -1
	for spelling, count := range spellingLinks {
		if count > bestCount || (count == bestCount && spelling < best) {
			best, bestCount = spelling, count
		}
	}
	return best
}

const dreamDecayStaleDays = 60

const dreamLinkReportUnavailable = "Link report unavailable this cycle; check links with Grep as described above."

// dreamLinkReportText builds the report for the prompt. A failed scan
// degrades the run but never blocks the dream.
func dreamLinkReportText(root string, now time.Time) string {
	g, err := buildLinkGraph(root)
	if err != nil {
		recordDegraded("dream_link_report", err)
		logMsg("dream", "link report unavailable: %v", err)
		return dreamLinkReportUnavailable
	}
	if g.Unreadable > 0 {
		recordDegradedMsg("dream_link_report", fmt.Sprintf("%d unreadable files in link scan", g.Unreadable))
	}
	return dreamLinkReport(g, now, dreamDecayStaleDays)
}
