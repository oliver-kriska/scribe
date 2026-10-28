package main

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestOrphanReports_AgreeAcrossCommands(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(root+"/scribe.yaml", []byte("domains: [acme]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCRIBE_KB", root)
	head := func(title, extra string) string {
		return "---\ntitle: \"" + title + "\"\ntype: solution\ndomain: general\n" + extra + "---\n\n"
	}
	writeKBFile(t, root, "wiki/a.md", head("A", "aliases: [Alpha]\n"))
	writeKBFile(t, root, "wiki/b.md", head("B", "")+"[[alpha]] [[Ghost]] [[Go Modules]] [[go modules]] [[GO MODULES]]\n")
	writeKBFile(t, root, "wiki/e.md", head("E", "")+"[[e]]\n")
	writeKBFile(t, root, "wiki/r.md", head("R", "rolling: true\n"))
	writeKBFile(t, root, "wiki/s.md", head("S", "status: superseded\n"))
	writeKBFile(t, root, "wiki/dup1.md", head("Dup", ""))
	writeKBFile(t, root, "wiki/dup2.md", head("Dup", ""))
	writeKBFile(t, root, "wiki/gm1.md", head("Go Modules", ""))
	writeKBFile(t, root, "wiki/gm2.md", head("go modules", ""))

	wantOrphans := []string{"B", "Dup", "E"}

	var err error
	out := captureLintStdout(t, func() { err = (&OrphansCmd{JSON: true}).Run() })
	if err != nil {
		t.Fatal(err)
	}
	var report orphanReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out)
	}
	if !slices.Equal(report.Orphans, wantOrphans) || !slices.Equal(report.Missing, []string{"Ghost"}) {
		t.Errorf("orphans cmd = %+v", report)
	}

	lintOut := captureLintStdout(t, func() {
		lintOrphans(newLintReport(os.Stdout, false, false), root)
	})
	for _, want := range []string{"3 orphan articles", "1 missing pages", "1 ambiguous links"} {
		if !strings.Contains(lintOut, want) {
			t.Errorf("lint output missing %q:\n%s", want, lintOut)
		}
	}

	g, err := buildLinkGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := graphOrphans(g); !slices.Equal(got, wantOrphans) {
		t.Errorf("graphOrphans = %v, want %v", got, wantOrphans)
	}
}
