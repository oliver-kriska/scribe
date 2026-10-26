package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const supersededStubArticle = "---\ntitle: \"Old Approach\"\ntype: pattern\ndomain: general\nstatus: superseded\n---\n\nSuperseded. See [[New Approach]].\n"

// supersededTestKB builds a KB with:
//
//	wiki/new.md   "New Approach" (live)  → links to [[Old Approach]]
//	wiki/other.md "Other Note"   (live)  → links to [[New Approach]]
//	wiki/old.md   "Old Approach" (status: superseded) → links to [[New Approach]]
func supersededTestKB(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("domains: [acme]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCRIBE_KB", root)
	writeKBFile(t, root, "wiki/new.md",
		"---\ntitle: \"New Approach\"\ntype: pattern\ndomain: general\n---\n\nThe current way. Replaces [[Old Approach]].\n")
	writeKBFile(t, root, "wiki/other.md",
		"---\ntitle: \"Other Note\"\ntype: solution\ndomain: general\n---\n\nUses [[New Approach]].\n")
	writeKBFile(t, root, "wiki/old.md", supersededStubArticle)
	return root
}

func TestIsSuperseded(t *testing.T) {
	cases := []struct {
		name string
		fm   *Frontmatter
		want bool
	}{
		{"nil", nil, false},
		{"no status", &Frontmatter{}, false},
		{"superseded", &Frontmatter{Status: "superseded"}, true},
		{"other status", &Frontmatter{Status: "decided"}, false},
		{"case differs", &Frontmatter{Status: "Superseded"}, false},
	}
	for _, c := range cases {
		if got := isSuperseded(c.fm); got != c.want {
			t.Errorf("%s: isSuperseded = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIndexOmitsSupersededArticles(t *testing.T) {
	root := supersededTestKB(t)
	var err error
	captureLintStdout(t, func() { err = (&IndexCmd{}).Run() })
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "wiki", "_index.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if strings.Contains(content, "- [[Old Approach]]") {
		t.Errorf("superseded article listed in the index:\n%s", content)
	}
	if !strings.Contains(content, "- [[New Approach]]") {
		t.Errorf("live article missing from the index:\n%s", content)
	}
	if !strings.Contains(content, "article_count: 2") {
		t.Errorf("article_count should count only live articles (2):\n%s", content)
	}
}

// Superseded articles drop out of the index, so the consistency lint must
// not count them on disk either, or a KB with 4+ of them warns about drift
// that isn't there.
func TestLintIndexConsistencyIgnoresSupersededArticles(t *testing.T) {
	root := supersededTestKB(t)
	for i, title := range []string{"Old B", "Old C", "Old D"} {
		writeKBFile(t, root, filepath.Join("wiki", "old-"+string(rune('b'+i))+".md"),
			strings.Replace(supersededStubArticle, "Old Approach", title, 1))
	}
	writeKBFile(t, root, "wiki/_index.md", "- [[New Approach]] -- s\n- [[Other Note]] -- s\n")
	var warnings int
	out := captureLintStdout(t, func() {
		rep := newLintReport(os.Stdout, false, false)
		lintIndexConsistency(rep, root)
		warnings = rep.warnings
	})
	if warnings != 0 {
		t.Errorf("warnings = %d, want 0 with 4 superseded articles on disk\noutput:\n%s", warnings, out)
	}
	if !strings.Contains(out, "index: 2 entries, disk: 2 articles") {
		t.Errorf("disk count should exclude superseded articles:\n%s", out)
	}
}

func readBacklinksJSON(t *testing.T, root string) map[string][]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "wiki", "_backlinks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string][]string
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("_backlinks.json is not a title→sources map: %v", err)
	}
	return m
}

func runBacklinks(t *testing.T) {
	t.Helper()
	var err error
	captureLintStdout(t, func() { err = (&BacklinksCmd{}).Run() })
	if err != nil {
		t.Fatal(err)
	}
}

func TestBacklinksDropSupersededArticlesAsTargetsAndSources(t *testing.T) {
	root := supersededTestKB(t)
	runBacklinks(t)
	bl := readBacklinksJSON(t, root)
	if _, ok := bl["Old Approach"]; ok {
		t.Errorf("superseded article kept as a backlinks target: %v", bl["Old Approach"])
	}
	if slices.Contains(bl["New Approach"], "Old Approach") {
		t.Errorf("superseded article counted as an inbound source: %v", bl["New Approach"])
	}
	if !slices.Contains(bl["New Approach"], "Other Note") {
		t.Errorf("live inbound link lost: %v", bl["New Approach"])
	}
}

// A leftover duplicate often keeps the survivor's exact title. Marking it
// superseded must not wipe the live article's backlinks.
func TestBacklinksKeepLiveArticleThatSharesASupersededTitle(t *testing.T) {
	root := supersededTestKB(t)
	writeKBFile(t, root, "wiki/new-dup.md",
		"---\ntitle: \"New Approach\"\ntype: pattern\ndomain: general\nstatus: superseded\n---\n\nDuplicate. See [[Other Note]].\n")
	runBacklinks(t)
	bl := readBacklinksJSON(t, root)
	if !slices.Contains(bl["New Approach"], "Other Note") {
		t.Errorf("live article's backlinks wiped by a superseded duplicate with the same title: %v", bl["New Approach"])
	}
	got, err := computeBacklinksFor(root, "New Approach")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "Other Note") {
		t.Errorf("computeBacklinksFor wiped the live article's backlinks: %v", got)
	}
}

// debug --fresh recomputes one title's backlinks; for every title it must
// equal what scribe backlinks writes. graphTestKB covers a self-link
// (B Article), a hub source (wiki/_index.md) and a ghost target.
func TestComputeBacklinksForMatchesBacklinksFile(t *testing.T) {
	root := graphTestKB(t)
	writeKBFile(t, root, "wiki/old.md", supersededStubArticle)
	runBacklinks(t)
	bl := readBacklinksJSON(t, root)
	titles := []string{"A Article", "B Article", "C Tool", "Ghost Page", "Old Approach", "New Approach"}
	for k := range bl {
		if !slices.Contains(titles, k) {
			titles = append(titles, k)
		}
	}
	for _, title := range titles {
		got, err := computeBacklinksFor(root, title)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 && len(bl[title]) == 0 {
			continue
		}
		if !slices.Equal(got, bl[title]) {
			t.Errorf("computeBacklinksFor(%q) = %v, _backlinks.json has %v", title, got, bl[title])
		}
	}
}

// orphanFixture is supersededTestKB with the survivor's link to the stub
// removed: now nothing links to "Old Approach" (superseded) or to "Other
// Note" (live). Only "Other Note" may be reported as an orphan.
func orphanFixture(t *testing.T) string {
	t.Helper()
	root := supersededTestKB(t)
	writeKBFile(t, root, "wiki/new.md",
		"---\ntitle: \"New Approach\"\ntype: pattern\ndomain: general\n---\n\nThe current way.\n")
	return root
}

// The generated index links every listed article, which is what kept stubs
// from being orphans. With superseded articles out of the index, the orphan
// checks must skip them, or lint warns forever and `link --apply` writes
// See Also links to dead stubs into live articles.
func TestLintOrphansSkipsSupersededArticles(t *testing.T) {
	root := orphanFixture(t)
	out := captureLintStdout(t, func() {
		rep := newLintReport(os.Stdout, false, false)
		lintOrphans(rep, root)
	})
	if !strings.Contains(out, "1 orphan articles") {
		t.Errorf("want exactly 1 orphan (Other Note), not the superseded article:\n%s", out)
	}
}

func TestOrphansCmdSkipsSupersededArticles(t *testing.T) {
	orphanFixture(t)
	runBacklinks(t)
	var err error
	out := captureLintStdout(t, func() { err = (&OrphansCmd{JSON: true}).Run() })
	if err != nil {
		t.Fatal(err)
	}
	var report orphanReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out)
	}
	if !slices.Equal(report.Orphans, []string{"Other Note"}) {
		t.Errorf("orphans = %v, want [Other Note]", report.Orphans)
	}
	if len(report.Missing) != 0 {
		t.Errorf("missing = %v, want none (superseded titles stay known)", report.Missing)
	}
}

func TestOrphansSkipSupersededAliases(t *testing.T) {
	root := orphanFixture(t)
	writeKBFile(t, root, "wiki/old2.md",
		"---\ntitle: \"Old Two\"\naliases: [old-two-name]\ntype: pattern\ndomain: general\nstatus: superseded\n---\n\nStub.\n")
	runBacklinks(t)

	out := captureLintStdout(t, func() {
		rep := newLintReport(os.Stdout, false, false)
		lintOrphans(rep, root)
	})
	if !strings.Contains(out, "1 orphan articles") {
		t.Errorf("want exactly 1 orphan (Other Note):\n%s", out)
	}
	for _, name := range []string{"Old Two", "old-two-name"} {
		if strings.Contains(out, name) {
			t.Errorf("lint reported superseded %q as orphan:\n%s", name, out)
		}
	}

	var err error
	jsonOut := captureLintStdout(t, func() { err = (&OrphansCmd{JSON: true}).Run() })
	if err != nil {
		t.Fatal(err)
	}
	var report orphanReport
	if err := json.Unmarshal([]byte(jsonOut), &report); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, jsonOut)
	}
	if !slices.Equal(report.Orphans, []string{"Other Note"}) {
		t.Errorf("orphans = %v, want [Other Note]", report.Orphans)
	}
}

func TestLinkOrphanFlagRejectsSupersededArticle(t *testing.T) {
	supersededTestKB(t)
	runBacklinks(t)
	err := (&LinkCmd{Orphan: "Old Approach"}).Run()
	if err == nil || !strings.Contains(err.Error(), "status: superseded") {
		t.Errorf("err = %v, want superseded rejection", err)
	}
	err = (&LinkCmd{Orphan: "No Such Title"}).Run()
	if err == nil || !strings.Contains(err.Error(), "not found by title") {
		t.Errorf("err = %v, want not-found error", err)
	}
}

func TestLinkNeverLoadsSupersededArticles(t *testing.T) {
	root := supersededTestKB(t)
	articles, err := loadArticles(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range articles {
		if a.Title == "Old Approach" {
			t.Error("loadArticles returned a superseded article; link would offer it as an orphan or a host")
		}
	}
}

// The duplicate report tells users to mark merged-away articles
// status: superseded; once they do, those must stop being reported.
func TestCollectContentDocsSkipsSupersededArticles(t *testing.T) {
	root := t.TempDir()
	wiki := filepath.Join(root, "wiki")
	body := toks("alpha", 1, 60)
	writeArticle(t, wiki, "survivor.md", "title: Survivor\ntype: pattern\n", body)
	writeArticle(t, wiki, "stub.md", "title: Stub\ntype: pattern\nstatus: superseded\n", body)
	docs := collectContentDocs(root)
	if len(docs) != 1 || docs[0].rel != filepath.Join("wiki", "survivor.md") {
		t.Errorf("collectContentDocs = %+v, want only wiki/survivor.md", docs)
	}
}
