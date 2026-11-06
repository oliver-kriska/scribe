package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrokenRewriteFor(t *testing.T) {
	resolver := newLinkResolver([]resolverEntry{
		{Title: "Go Modules", Path: "tools/go-modules.md"},
		{Title: "Kafka", Path: "tools/kafka.md"},
		{Title: "C#", Path: "tools/c-sharp.md"},
		{Title: "Kafka Streams (KStreams)", Path: "tools/kafka-streams.md"},
		{Title: "Foo: Bar", Path: "wiki/foo-bar.md"},
		{Title: "Notes in CLAUDE.md", Path: "wiki/notes.md"},
		{Title: "Twin", Path: "wiki/twin-a.md"},
		{Title: "twin", Path: "wiki/twin-b.md"},
	})
	cases := []struct {
		inner, want string
		verdict     brokenVerdict
	}{
		{"go modules", "Go Modules", linkSafeRewrite},
		{"go_modules", "Go Modules", linkSafeRewrite},
		{"go-modules", "Go Modules", linkSafeRewrite},
		{"Go-Modules.md", "Go Modules", linkSafeRewrite},
		{"kafka.md", "Kafka", linkSafeRewrite},
		{"kafka#Setup", "Kafka#Setup", linkSafeRewrite},
		{"kafka|the broker", "Kafka|the broker", linkSafeRewrite},
		{`kafka\|label`, `Kafka\|label`, linkSafeRewrite},
		{"c#", "C#", linkSafeRewrite},
		{"notes in claude.md", "Notes in CLAUDE.md", linkSafeRewrite},
		{"Kafka", "", linkNotBroken},
		{"Ghost", "", linkNotBroken},
		{"TWIN", "", linkNotBroken},            // ambiguous
		{"kafka-streams", "", linkNeedsReview}, // file stem, title differs in words
		{"foo bar", "", linkNeedsReview},       // punctuation differs
		{"tools/kafka", "", linkNeedsReview},   // path match
		{"go modules|Go modules", "Go Modules|Go modules", linkSafeRewrite},
	}
	for _, c := range cases {
		got, _, verdict := brokenRewriteFor(resolver, c.inner)
		if verdict != c.verdict || got != c.want {
			t.Errorf("brokenRewriteFor(%q) = %q, %v; want %q, %v", c.inner, got, verdict, c.want, c.verdict)
		}
	}
}

func fixBrokenKB(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	head := func(title, extra string) string {
		return "---\ntitle: \"" + title + "\"\ntype: tool\ndomain: general\n" + extra + "---\n\n"
	}
	writeKBFile(t, root, "tools/go-modules.md", head("Go Modules", "")+"body\n")
	writeKBFile(t, root, "tools/kafka-streams.md", head("Kafka Streams (KStreams)", "")+"body\n")
	writeKBFile(t, root, "wiki/user.md", head("User",
		"related: [\"[[go_modules]]\", \"[[Kafka Streams (KStreams)]]\"]\nsupersedes: \"[[go-modules]]\"\n")+
		"See [[go modules#Setup|modules]] and [[kafka-streams]].\n"+
		"Code `[[go modules]]` stays.\n```\n[[go modules]]\n```\n")
	writeKBFile(t, root, "wiki/_index.md", "- [[go modules]]\n")
	writeKBFile(t, root, "wiki/old.md", head("Old", "status: superseded\n")+"[[go modules]]\n")
	return root
}

func TestRunFixBroken_DryRunThenApply(t *testing.T) {
	root := fixBrokenKB(t)
	user := filepath.Join(root, "wiki", "user.md")
	before, _ := os.ReadFile(user)

	out := captureLintStdout(t, func() {
		if err := runFixBroken(root, true); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{
		"wiki/user.md: [[go_modules]] → [[Go Modules]]\n",
		"wiki/user.md: [[go modules#Setup|modules]] → [[Go Modules#Setup|modules]]\n",
		"would rewrite 2 link(s) in 1 file(s); 2 broken link(s) need review",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run missing %q:\n%s", want, out)
		}
	}
	if after, _ := os.ReadFile(user); !bytes.Equal(after, before) {
		t.Fatal("dry run wrote the file")
	}

	captureLintStdout(t, func() {
		if err := runFixBroken(root, false); err != nil {
			t.Fatal(err)
		}
	})
	after, _ := os.ReadFile(user)
	want := strings.NewReplacer(
		`"[[go_modules]]"`, `"[[Go Modules]]"`,
		"[[go modules#Setup|modules]]", "[[Go Modules#Setup|modules]]",
	).Replace(string(before))
	if string(after) != want {
		t.Errorf("applied file:\n%s\nwant:\n%s", after, want)
	}
	for _, rel := range []string{"wiki/_index.md", "wiki/old.md"} {
		data, _ := os.ReadFile(filepath.Join(root, rel))
		if strings.Contains(string(data), "Go Modules]]") {
			t.Errorf("%s was rewritten:\n%s", rel, data)
		}
	}
}

// The dream report keeps only the broken links Go cannot settle, and says
// how many it left out.
func TestDreamLinkReport_LeavesOutSafeBroken(t *testing.T) {
	root := fixBrokenKB(t)
	g := buildTestGraph(t, root)
	plan, err := planBrokenLinkFixes(root)
	if err != nil {
		t.Fatal(err)
	}
	got := dreamLinkReport(withoutSettledBroken(g, plan), linkReportToday, 60)
	if strings.Contains(got, "[[go_modules]]") || strings.Contains(got, "[[go modules#Setup]]") {
		t.Errorf("safe link still listed:\n%s", got)
	}
	// [[go-modules]] sits in supersedes:, which Go never edits.
	for _, want := range []string{
		"- wiki/user.md: [[go-modules]] → [[Go Modules]]",
		"- wiki/user.md: [[kafka-streams]] → [[Kafka Streams (KStreams)]]",
		"2 more differ from their title only by case or separators",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q:\n%s", want, got)
		}
	}
}
