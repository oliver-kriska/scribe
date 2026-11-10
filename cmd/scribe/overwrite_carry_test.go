package main

import (
	"strings"
	"testing"
)

func overwritePage(t *testing.T, oldPage, newPage string) string {
	t.Helper()
	root := t.TempDir()
	writeTestArticle(t, root, "patterns/q.md", oldPage)
	env := WikiActionEnvelope{Actions: []WikiAction{{Op: "create", Path: "patterns/q.md", Content: newPage}}}
	if _, err := applyWikiActions(root, env, ApplyOptions{AllowOverwrite: true}); err != nil {
		t.Fatal(err)
	}
	return readBack(t, root, "patterns/q.md")
}

const carryHead = "type: pattern\ndomain: general\ncreated: 2026-10-14\nupdated: 2026-10-14\nconfidence: medium\n"

func TestOverwriteKeepsTitleAndAliasesNewOne(t *testing.T) {
	got := overwritePage(t,
		"---\ntitle: \"Handoff with Ack Convention\"\n"+carryHead+"aliases: [\"Ack Handoff\"]\n---\n# Handoff with Ack Convention\n\nold\n",
		"---\ntitle: \"Handoff-with-Ack Convention\"\n"+carryHead+"---\n# Handoff-with-Ack Convention\n\nnew\n")
	for _, want := range []string{
		"title: Handoff with Ack Convention\n",
		`aliases: ["Ack Handoff", "Handoff-with-Ack Convention"]`,
		"# Handoff with Ack Convention\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if fm, err := parseFrontmatter([]byte(got)); err != nil || fm.Title != "Handoff with Ack Convention" {
		t.Errorf("frontmatter does not parse back to the old title: %v %+v", err, fm)
	}
}

func TestOverwriteKeepsOwnedScalars(t *testing.T) {
	got := overwritePage(t,
		"---\ntitle: Q\n"+carryHead+"status: superseded\ncontributor: Someone\nindex_tier: deep\n---\nold\n",
		"---\ntitle: Q\n"+carryHead+"index_tier: brief\n---\nnew\n")
	for _, want := range []string{"\nstatus: superseded\n", "\ncontributor: Someone\n", "\nindex_tier: brief\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "index_tier: deep") {
		t.Errorf("a key the new page sets must not be overridden:\n%s", got)
	}
}

func TestOverwriteKeepsUncoveredSections(t *testing.T) {
	evidence := "Three incidents between 5844 and 6041 traced to the replica pool exhausting under burst load from the nightly importer job."
	merged := "The recommended fix is to cap the importer concurrency at four workers and move it off the primary connection pool entirely."
	got := overwritePage(t,
		"---\ntitle: Q\n"+carryHead+"---\n# Q\n\n## Evidence\n\n"+evidence+"\n\n## Recommended fix\n\n"+merged+"\n\n## Stub\n\nshort\n",
		"---\ntitle: Q\n"+carryHead+"---\n# Q\n\n## Summary\n\n"+merged+"\n")
	if !strings.Contains(got, "## Evidence\n\n"+carriedMarker) || !strings.Contains(got, evidence) {
		t.Errorf("uncovered section not carried with marker:\n%s", got)
	}
	if strings.Count(got, merged) != 1 || strings.Contains(got, "## Recommended fix") {
		t.Errorf("a section merged under another heading must not be re-added:\n%s", got)
	}
	if strings.Contains(got, "## Stub") {
		t.Errorf("trivial sections are not carried:\n%s", got)
	}
	// Idempotent: overwriting again with the same new page carries the same
	// section once, not twice.
	again := overwritePage(t, got, "---\ntitle: Q\n"+carryHead+"---\n# Q\n\n## Summary\n\n"+merged+"\n")
	if strings.Count(again, "## Evidence") != 1 || strings.Count(again, carriedMarker) != 1 {
		t.Errorf("second overwrite duplicated the carried section:\n%s", again)
	}
}

func TestOverwriteSectionCarryIsCapped(t *testing.T) {
	var old strings.Builder
	old.WriteString("---\ntitle: Q\n" + carryHead + "---\n# Q\n")
	for _, h := range []string{"A", "B", "C"} {
		old.WriteString("\n## " + h + "\n\n")
		for j := range 400 {
			old.WriteString(h + "word" + strings.Repeat("x", j%7) + " ")
		}
		old.WriteString("\n")
	}
	got := overwritePage(t, old.String(), "---\ntitle: Q\n"+carryHead+"---\n# Q\n\nnew\n")
	if n := len(got); n > len("new")+carrySectionMaxBytes+500 {
		t.Errorf("carry exceeded the cap: page is %d bytes", n)
	}
	if !strings.Contains(got, "## A") {
		t.Errorf("the first section under the cap should be carried:\n%.300s", got)
	}
}

func TestFreshCreateIsUntouched(t *testing.T) {
	root := t.TempDir()
	page := "---\ntitle: Q\n" + carryHead + "---\nbody\n"
	if got := carryOnOverwrite(0, root+"/missing.md", page); got != page {
		t.Errorf("fresh create changed:\n%s", got)
	}
}

func TestOverwriteDoesNotRestoreRollingEntries(t *testing.T) {
	entry := "An entry the rolling compaction moved to the archive file on purpose, long enough to be carried otherwise."
	got := overwritePage(t,
		"---\ntitle: Q\nrolling: true\n"+carryHead+"---\n# Q\n\n## 2026-02-07 | Old entry\n\n"+entry+"\n",
		"---\ntitle: Q\nrolling: true\n"+carryHead+"---\n# Q\n\n## 2026-11-10 | New entry\n\nfresh\n")
	if strings.Contains(got, entry) {
		t.Errorf("rolling page got an archived entry back:\n%s", got)
	}
}
