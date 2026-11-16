package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mergeBothWays runs mergeKBFile in both argument orders and fails the
// test unless they agree: two machines merging the same conflict from
// opposite sides must converge on one result.
func mergeBothWays(t *testing.T, rel, base, a, b string) (string, specialFileClass, bool) {
	t.Helper()
	ab, class, okAB := mergeKBFile(rel, []byte(base), []byte(a), []byte(b))
	ba, _, okBA := mergeKBFile(rel, []byte(base), []byte(b), []byte(a))
	if okAB != okBA {
		t.Fatalf("%s: resolved differs by order: ab=%v ba=%v", rel, okAB, okBA)
	}
	if okAB && !bytes.Equal(ab, ba) {
		t.Fatalf("%s: merge is not symmetric\n--- a,b ---\n%s\n--- b,a ---\n%s", rel, ab, ba)
	}
	return string(ab), class, okAB
}

func TestSpecialFileFor_Classes(t *testing.T) {
	cases := []struct {
		rel  string
		want specialFileClass
		ok   bool
	}{
		{"decisions/x.md", classArticle, true},
		{"wiki/sub/deep.md", classArticle, true},
		{"projects/alpha/overview.md", classArticle, true},
		{"projects/alpha/learnings.md", classAppendOnly, true},
		{"projects/alpha/decisions-log.md", classAppendOnly, true},
		{"wiki/_unfetched-links.md", classAppendOnly, true},
		{"log.md", classAppendOnly, true},
		{"raw/articles/2026-11-09-x.md", classRawArticle, true},
		{"wiki/_index.md", classDerivedRegenerable, true},
		{"scripts/extraction-ledger.json", classSemanticMerge, true},
		{"scripts/projects.json", classMachineLocal, true},
		{"README.md", "", false},
		{"decisions/x.json", "", false},
		{"raw/articles/sub/x.md", "", false},
	}
	for _, c := range cases {
		spec, ok := specialFileFor(c.rel)
		if ok != c.ok || (ok && spec.Class != c.want) {
			t.Errorf("specialFileFor(%q) = (%q, %v), want (%q, %v)", c.rel, spec.Class, ok, c.want, c.ok)
		}
	}
}

func TestMergeArticle(t *testing.T) {
	base := "---\ntitle: X\nupdated: 2026-11-01\nrelated: [\"[[A]]\"]\n---\n# X\n\nline one\nline two\n"
	cases := []struct {
		name, base, a, b string
		want             []string // substrings that must appear
		absent           []string
	}{
		{
			name: "related lists union, base order first, wikilinks deduped",
			base: base,
			a:    "---\ntitle: X\nupdated: 2026-11-01\nrelated: [\"[[A]]\", \"[[B]]\"]\n---\n# X\n\nline one\nline two\n",
			b:    "---\ntitle: X\nupdated: 2026-11-01\nrelated: [\"[[A]]\", \"[[c]]\", \"[[b]]\"]\n---\n# X\n\nline one\nline two\n",
			want: []string{"[[A]]", "[[B]]", "[[c]]"},
		},
		{
			name: "later updated wins",
			base: base,
			a:    strings.Replace(base, "2026-11-01", "2026-11-09", 1),
			b:    strings.Replace(base, "2026-11-01", "2026-11-12", 1) + "extra\n",
			want: []string{"updated: 2026-11-12", "extra"},
		},
		{
			name: "scalar changed on one side wins",
			base: base,
			a:    strings.Replace(base, "title: X", "title: Y", 1),
			b:    base + "tail from b\n",
			want: []string{"title: Y", "tail from b"},
		},
		{
			name:   "both changed a scalar: lexical pick",
			base:   base,
			a:      strings.Replace(base, "title: X", "title: Zed", 1),
			b:      strings.Replace(base, "title: X", "title: Alpha", 1),
			want:   []string{"title: Alpha"},
			absent: []string{"Zed"},
		},
		{
			name: "bodies edited in different places merge cleanly",
			base: base,
			a:    strings.Replace(base, "line one", "line ONE", 1),
			b:    strings.Replace(base, "line two", "line TWO", 1),
			want: []string{"line ONE", "line TWO"},
		},
		{
			name:   "same line edited both sides: keep one, append the other as a dated section",
			base:   base,
			a:      strings.Replace(base, "line one", "alice wrote this", 1),
			b:      strings.Replace(base, "line one", "bob wrote this", 1),
			want:   []string{"alice wrote this", "bob wrote this", "## Merged from another machine ("},
			absent: []string{"<<<<<<<", ">>>>>>>"},
		},
		{
			name:   "no frontmatter at all still merges bodies",
			base:   "base\n",
			a:      "alice version\n",
			b:      "bob version\n",
			want:   []string{"alice version", "bob version", "## Merged from another machine ("},
			absent: []string{"<<<<<<<"},
		},
		{
			name: "add/add with empty base",
			base: "",
			a:    "---\ntitle: X\n---\nsame\n",
			b:    "---\ntitle: X\n---\nsame\n",
			want: []string{"same"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, class, ok := mergeBothWays(t, "decisions/x.md", c.base, c.a, c.b)
			if !ok || class != classArticle {
				t.Fatalf("not resolved as article: ok=%v class=%q", ok, class)
			}
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, w := range c.absent {
				if strings.Contains(got, w) {
					t.Errorf("unexpected %q in:\n%s", w, got)
				}
			}
		})
	}
}

func TestMergeArticle_ListUnionDedupesNormalizedLinks(t *testing.T) {
	base := "---\nrelated:\n  - \"[[A]]\"\n---\nbody\n"
	a := "---\nrelated:\n  - \"[[A]]\"\n  - \"[[New One]]\"\n---\nbody\n"
	b := "---\nrelated:\n  - \"[[A]]\"\n  - \"[[new one]]\"\n  - \"[[Other]]\"\n---\nbody\n"
	got, _, _ := mergeBothWays(t, "decisions/x.md", base, a, b)
	if n := strings.Count(strings.ToLower(got), "[[new one]]"); n != 1 {
		t.Errorf("normalized duplicate kept %d times:\n%s", n, got)
	}
	if !strings.Contains(got, "[[Other]]") {
		t.Errorf("union lost [[Other]]:\n%s", got)
	}
	if strings.Index(got, "[[A]]") > strings.Index(strings.ToLower(got), "[[new one]]") {
		t.Errorf("base item must come first:\n%s", got)
	}
}

func TestMergeArticle_DatedSectionNotDuplicated(t *testing.T) {
	base := "base line\n"
	a := "alice line\n"
	b := "bob line\n"
	once, _, _ := mergeBothWays(t, "decisions/x.md", base, a, b)
	// Merging the merged result against b again must not append b twice.
	twice, _, _ := mergeBothWays(t, "decisions/x.md", base, once, b)
	if strings.Count(twice, "bob line") != 1 {
		t.Errorf("other side's text appended twice:\n%s", twice)
	}
}

func TestMergeRawArticle(t *testing.T) {
	fm := "---\ntitle: R\nsource: https://example.com\n---\n"
	body := "# R\n\nraw body\n"
	withAuto := func(p string) string {
		out, err := insertRetrievalContext(fm+body, p)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	t.Run("only the auto block differs: block that sorts first wins", func(t *testing.T) {
		got, class, ok := mergeBothWays(t, "raw/articles/r.md", fm+body, withAuto("Zulu context."), withAuto("Alpha context."))
		if !ok || class != classRawArticle {
			t.Fatalf("not resolved as raw article: ok=%v class=%q", ok, class)
		}
		if got != withAuto("Alpha context.") {
			t.Errorf("got:\n%s\nwant:\n%s", got, withAuto("Alpha context."))
		}
	})
	t.Run("one side added the block, the other edited the body", func(t *testing.T) {
		edited := fm + "# R\n\nraw body, fixed\n"
		got, _, ok := mergeBothWays(t, "raw/articles/r.md", fm+body, withAuto("Ctx."), edited)
		want, _ := insertRetrievalContext(edited, "Ctx.")
		if !ok || got != want {
			t.Errorf("got:\n%s\nwant:\n%s", got, want)
		}
	})
}

func TestMergeAppendOnly(t *testing.T) {
	got, class, ok := mergeBothWays(t, "projects/alpha/learnings.md", "# L\n", "# L\n- alice\n", "# L\n- bob\n")
	if !ok || class != classAppendOnly {
		t.Fatalf("ok=%v class=%q", ok, class)
	}
	for _, w := range []string{"- alice", "- bob"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q:\n%s", w, got)
		}
	}
}

func TestMergeKBFile_UnclassifiedIsUnresolved(t *testing.T) {
	if _, _, ok := mergeKBFile("README.md", []byte("b\n"), []byte("x\n"), []byte("y\n")); ok {
		t.Error("an unclassified path must stay unresolved")
	}
	if _, _, ok := mergeKBFile("scripts/projects.json", []byte("{}"), []byte("{\"a\":1}"), []byte("{\"b\":1}")); ok {
		t.Error("machine-local files are never merged")
	}
}

func TestMergeFileCmd_WritesResultAndLeavesMarkersWhenUnresolved(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cmd := MergeFileCmd{Base: write("o", "base\n"), Current: write("a", "alice\n"), Other: write("b", "bob\n"), Path: "decisions/x.md"}
	if err := cmd.Run(); err != nil {
		t.Fatalf("article merge must resolve: %v", err)
	}
	got, _ := os.ReadFile(cmd.Current)
	if !strings.Contains(string(got), "bob") || !strings.Contains(string(got), "alice") {
		t.Errorf("current not overwritten with the merge:\n%s", got)
	}

	cmd = MergeFileCmd{Base: write("o2", "base\n"), Current: write("a2", "alice\n"), Other: write("b2", "bob\n"), Path: "README.md"}
	if err := cmd.Run(); err == nil {
		t.Fatal("unclassified path must return an error (exit 1)")
	}
	got, _ = os.ReadFile(cmd.Current)
	if firstConflictMarkerLine(got) == 0 {
		t.Errorf("unresolved merge must leave git's usual conflict markers:\n%s", got)
	}
}

func TestStripRetrievalContext_InvertsInsert(t *testing.T) {
	orig := "---\ntitle: R\n---\n# R\n\nbody\n"
	with, err := insertRetrievalContext(orig, "Some context.")
	if err != nil {
		t.Fatal(err)
	}
	para, stripped := splitRetrievalContext(with)
	if stripped != orig || para != "Some context." {
		t.Errorf("strip(insert(x)) != x\npara=%q\nstripped=%q", para, stripped)
	}
	if p, s := splitRetrievalContext(orig); p != "" || s != orig {
		t.Errorf("no block: got (%q, %q)", p, s)
	}
}

func TestAbsorbContentSHA_IgnoresAutoBlock(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "a.md")
	ctx := filepath.Join(dir, "b.md")
	orig := "---\ntitle: R\n---\nbody\n"
	with, _ := insertRetrievalContext(orig, "Ctx.")
	_ = os.WriteFile(plain, []byte(orig), 0o644)
	_ = os.WriteFile(ctx, []byte(with), 0o644)
	full, stripped := absorbContentSHAs(ctx)
	plainFull, plainStripped := absorbContentSHAs(plain)
	if stripped != plainFull || plainStripped != plainFull {
		t.Errorf("stripped sha must equal the sha of the file without the block")
	}
	if full == stripped {
		t.Errorf("full sha must include the block")
	}
	log := AbsorbLog{"b.md": {SHA: plainFull}}
	if d := checkAbsorbDecisionDual(log, "b.md", full, stripped); d != absorbDecisionSkipSameContent {
		t.Errorf("an added auto block must not trigger re-absorb, got %v", d)
	}
	log = AbsorbLog{"b.md": {SHA: full}}
	if d := checkAbsorbDecisionDual(log, "b.md", full, stripped); d != absorbDecisionSkipSameContent {
		t.Errorf("a legacy full-file sha must still match, got %v", d)
	}
}

func TestEnsureMergeDriver_Idempotent(t *testing.T) {
	repo := initTestGitRepo(t, "Tester")
	defer stubMergeDriverCommand("/opt/scribe merge-file %O %A %B %P")()
	for range 2 {
		if err := ensureMergeDriver(repo); err != nil {
			t.Fatal(err)
		}
	}
	if got := runCmd(repo, "git", "config", "--get-all", "merge.scribe.driver"); got != "/opt/scribe merge-file %O %A %B %P" {
		t.Errorf("driver config = %q", got)
	}
}

func TestEnsureGitattributes_Idempotent(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("*.png binary\n"), 0o644)
	for range 2 {
		if _, err := ensureGitattributes(root); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(filepath.Join(root, ".gitattributes"))
	s := string(data)
	if !strings.HasPrefix(s, "*.png binary\n") {
		t.Errorf("existing lines must be kept:\n%s", s)
	}
	if strings.Count(s, "*.md merge=scribe") != 1 || !strings.Contains(s, "scripts/extraction-ledger.json merge=scribe") {
		t.Errorf("unexpected .gitattributes:\n%s", s)
	}
	if strings.Contains(s, "dream-lease.json") {
		t.Errorf("the dream lease must stay with the rebase fallback (remote wins):\n%s", s)
	}
	if strings.Contains(s, "projects.json") {
		t.Errorf("machine-local files must not get the driver:\n%s", s)
	}
}

func TestGitAddWiki_NeverStagesConflictMarkers(t *testing.T) {
	root := initTestGitRepo(t, "Tester")
	writeKBFile(t, root, "decisions/ok.md", "---\ntitle: ok\n---\nfine\n")
	writeKBFile(t, root, "decisions/bad.md", "<<<<<<< HEAD\na\n=======\nb\n>>>>>>> x\n")
	gitAddWiki(root)
	staged := runCmd(root, "git", "diff", "--cached", "--name-only")
	if !strings.Contains(staged, "decisions/ok.md") || strings.Contains(staged, "decisions/bad.md") {
		t.Errorf("staged = %q", staged)
	}
}

func TestMergeDriverCheck(t *testing.T) {
	defer stubMergeDriverCommand("/opt/scribe merge-file %O %A %B %P")()
	repo := initTestGitRepo(t, "Tester")
	if _, ok := mergeDriverCheck(repo, false); ok {
		t.Error("solo KB without merge=scribe lines gets no row")
	}
	if row, _ := mergeDriverCheck(repo, true); row.Status != statusWarn {
		t.Errorf("team KB without .gitattributes lines: %+v", row)
	}
	if _, err := ensureGitattributes(repo); err != nil {
		t.Fatal(err)
	}
	if row, _ := mergeDriverCheck(repo, true); row.Status != statusWarn || !strings.Contains(row.Detail, "not set") {
		t.Errorf("driver not configured: %+v", row)
	}
	if err := ensureMergeDriver(repo); err != nil {
		t.Fatal(err)
	}
	if row, _ := mergeDriverCheck(repo, true); row.Status != statusOK {
		t.Errorf("configured: %+v", row)
	}
}

func TestWithoutMergeDriverLines(t *testing.T) {
	in := "Rebasing (1/1)\rmerge-file: article x.md resolved\nerror: could not apply abc\n"
	if got := withoutMergeDriverLines(in); got != "Rebasing (1/1)\nerror: could not apply abc" {
		t.Errorf("got %q", got)
	}
}
