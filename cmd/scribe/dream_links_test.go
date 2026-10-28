package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var linkReportToday = time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)

func oldArticle(path, title string) graphArticle {
	return graphArticle{Path: path, Title: title, Updated: "2026-01-01"}
}

func TestDreamLinkReport_Sections(t *testing.T) {
	g := linkGraph{
		Articles: []graphArticle{oldArticle("wiki/a.md", "Alpha")},
		Broken:   []brokenLink{{Raw: "alpha", Canonical: "Alpha", SourcePath: "wiki/b.md"}},
		Missing: map[string][]linkEdge{
			"Ghost": {{SourcePath: "wiki/1.md"}, {SourcePath: "wiki/2.md"}, {SourcePath: "wiki/3.md"}},
		},
	}
	got := dreamLinkReport(g, linkReportToday, 60)
	for _, want := range []string{
		"### Broken links (fix: change [[Raw]] to [[Canonical]], or [[Canonical|Raw]] to keep the wording)\n- wiki/b.md: [[alpha]] → [[Alpha]]\n",
		"### Decay candidates (no links from other articles, no outbound links, updated more than 60 days ago)\n- wiki/a.md | Alpha | updated 2026-01-01\n",
		"### Wanted pages (linked from 3 or more articles, no page exists)\n- [[Ghost]] — 3 articles: wiki/1.md, wiki/2.md, wiki/3.md",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q in:\n%s", want, got)
		}
	}
}

func TestDreamLinkReport_IndexLinksDoNotSaveFromDecay(t *testing.T) {
	g := linkGraph{
		Articles: []graphArticle{oldArticle("wiki/a.md", "Alpha")},
		Inbound:  map[string][]linkEdge{"Alpha": {{SourcePath: "wiki/_index.md", Generated: true}}},
	}
	if got := dreamLinkReport(g, linkReportToday, 60); !strings.Contains(got, "- wiki/a.md | Alpha") {
		t.Errorf("index-only article should be a decay candidate:\n%s", got)
	}
}

func TestDreamLinkReport_OutboundOrRecentOrRollingExcluded(t *testing.T) {
	withOutbound := oldArticle("wiki/o.md", "Out")
	withOutbound.Outbound = 1
	recent := graphArticle{Path: "wiki/r.md", Title: "Recent", Updated: "2026-10-20"}
	rolling := oldArticle("wiki/l.md", "Rolling")
	rolling.Rolling = true
	g := linkGraph{Articles: []graphArticle{withOutbound, recent, rolling}}
	got := dreamLinkReport(g, linkReportToday, 60)
	for _, path := range []string{"wiki/o.md", "wiki/r.md", "wiki/l.md"} {
		if strings.Contains(got, path) {
			t.Errorf("%s must not be listed:\n%s", path, got)
		}
	}
}

func TestDreamLinkReport_WantedNeedsThreeArticles(t *testing.T) {
	g := linkGraph{Missing: map[string][]linkEdge{
		"Ghost": {{SourcePath: "wiki/1.md"}, {SourcePath: "wiki/2.md"}, {SourcePath: "wiki/_index.md", Generated: true}},
	}}
	if got := dreamLinkReport(g, linkReportToday, 60); strings.Contains(got, "[[Ghost]]") {
		t.Errorf("two articles plus an index is not wanted:\n%s", got)
	}
}

func TestDreamLinkReport_WantedGroupsBySlug(t *testing.T) {
	g := linkGraph{Missing: map[string][]linkEdge{
		"Ghost": {{SourcePath: "wiki/1.md"}, {SourcePath: "wiki/2.md"}},
		"ghost": {{SourcePath: "wiki/3.md"}},
	}}
	got := dreamLinkReport(g, linkReportToday, 60)
	if !strings.Contains(got, "- [[Ghost]] — 3 articles: wiki/1.md, wiki/2.md, wiki/3.md") {
		t.Errorf("want one grouped line labeled Ghost:\n%s", got)
	}
	if strings.Contains(got, "[[ghost]]") {
		t.Errorf("variant must not get its own line:\n%s", got)
	}
}

func TestDreamLinkReport_CapsAreExact(t *testing.T) {
	broken := make([]brokenLink, 0, 45)
	for i := range 45 {
		broken = append(broken, brokenLink{Raw: "x", Canonical: "X", SourcePath: fmt.Sprintf("wiki/%02d.md", i)})
	}
	got := dreamLinkReport(linkGraph{Broken: broken}, linkReportToday, 60)
	if n := strings.Count(got, "- wiki/"); n != dreamLinkListMax {
		t.Errorf("listed %d broken lines, want %d", n, dreamLinkListMax)
	}
	if !strings.Contains(got, "- (+5 more not shown)") {
		t.Errorf("missing exact overflow line:\n%s", got)
	}
}

func TestDreamLinkReport_EmptyListsSayNone(t *testing.T) {
	got := dreamLinkReport(linkGraph{}, linkReportToday, 60)
	if n := strings.Count(got, "- none"); n != 3 {
		t.Errorf("want 3 '- none' lines, got %d:\n%s", n, got)
	}
}

func TestDreamLinkReport_SkippedNote(t *testing.T) {
	got := dreamLinkReport(linkGraph{Unparsed: 2, Unreadable: 1}, linkReportToday, 60)
	want := "Note: 3 files could not be read or parsed; links in them are missing from this report."
	if !strings.HasPrefix(got, want) {
		t.Errorf("want prefix %q, got:\n%s", want, got)
	}
}

func TestDreamLinkReport_EscapesBraces(t *testing.T) {
	g := linkGraph{Articles: []graphArticle{
		oldArticle("wiki/a.md", "{{DATE}} plan"),
		oldArticle("wiki/b.md", "{{{X}}}"),
		oldArticle("wiki/c.md", "{{{{"),
	}}
	got := dreamLinkReport(g, linkReportToday, 60)
	if !strings.Contains(got, "{ {DATE}} plan") {
		t.Errorf("double braces not split:\n%s", got)
	}
	if strings.Contains(got, "{{") {
		t.Errorf("a brace run survived:\n%s", got)
	}
}
