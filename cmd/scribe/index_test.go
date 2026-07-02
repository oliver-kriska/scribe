package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTruncateOutsideWikilink(t *testing.T) {
	tests := []struct {
		name  string
		s     string
		limit int
		want  string
	}{
		{"short string unchanged", "hello world", 80, "hello world"},
		{"plain cut adds ellipsis", strings.Repeat("a", 100), 10, strings.Repeat("a", 7) + "..."},
		{
			name:  "cut mid-wikilink backs up to before the opener",
			s:     "see the [[Very Long Article Title Goes Here]] for details and more words after",
			limit: 20,
			want:  "see the...",
		},
		{
			name:  "closed wikilink inside head survives",
			s:     "[[Short]] then a lot of extra text that goes well past the cut limit here",
			limit: 30,
			want:  "[[Short]] then a lot of ext...",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncateOutsideWikilink(tt.s, tt.limit); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractBody(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"with frontmatter", "---\ntitle: x\n---\n\nThe body.\n", "The body."},
		{"no frontmatter", "Just text.", "Just text."},
		{"unclosed frontmatter", "---\ntitle: x\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractBody([]byte(tt.content)); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFirstSentence(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"empty", "", ""},
		{"skips headings", "# Heading\n\nReal content here.\n", "Real content here."},
		{"first sentence only", "One sentence. Second sentence.", "One sentence."},
		{"short line as-is", "No terminal period here", "No terminal period here"},
		{"only headings", "# A\n## B\n", ""},
		{
			name: "long line truncated outside wikilink",
			body: strings.Repeat("x", 100) + " [[Some Linked Article Title That Is Long]] tail",
			want: strings.Repeat("x", 100) + "...",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstSentence(tt.body); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// graphTestKB builds a tiny linked KB:
//
//	wiki/a.md      "A Article" → links to [[B Article]] and [[Ghost Page]]
//	wiki/b.md      "B Article" → links to itself (must not count)
//	tools/c.md     "C Tool"    → no links in or out (orphan)
//	wiki/_index.md hub         → links to [[A Article]]
func graphTestKB(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("domains: [acme]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCRIBE_KB", root)
	writeKBFile(t, root, "wiki/a.md",
		"---\ntitle: \"A Article\"\ntype: solution\ndomain: general\n---\n\nFirst sentence about A. More.\n\nSee [[B Article]] and [[Ghost Page]].\n")
	writeKBFile(t, root, "wiki/b.md",
		"---\ntitle: \"B Article\"\ntype: solution\ndomain: general\n---\n\nB body refers to [[B Article]] itself.\n")
	writeKBFile(t, root, "tools/c.md",
		"---\ntitle: \"C Tool\"\ntype: tool\ndomain: general\n---\n\nA tool nobody links to.\n")
	writeKBFile(t, root, "wiki/_index.md", "- [[A Article]] -- s\n")
	return root
}

func TestIndexCmdRun(t *testing.T) {
	root := graphTestKB(t)

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
	if !strings.Contains(content, "article_count: 3") {
		t.Errorf("article_count wrong:\n%s", content)
	}
	for _, want := range []string{
		"## tools/",
		"## wiki/",
		"- [[A Article]] -- First sentence about A. (solution, general)",
		"- [[C Tool]] -- A tool nobody links to. (tool, general)",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("missing %q in:\n%s", want, content)
		}
	}
	// Groups are sorted: tools/ section before wiki/.
	if strings.Index(content, "## tools/") > strings.Index(content, "## wiki/") {
		t.Errorf("directory groups not sorted:\n%s", content)
	}
	if strings.Contains(content, "%d") {
		t.Errorf("count placeholder not substituted:\n%s", content)
	}
}

func TestIndexCmdDryRun(t *testing.T) {
	root := graphTestKB(t)
	// Remove the seeded index so dry-run hits the would-create path.
	if err := os.Remove(filepath.Join(root, "wiki", "_index.md")); err != nil {
		t.Fatal(err)
	}

	var err error
	out := captureLintStdout(t, func() { err = (&IndexCmd{DryRun: true}).Run() })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "would create _index.md with 3 articles") {
		t.Errorf("unexpected dry-run output:\n%s", out)
	}
	if fileExists(filepath.Join(root, "wiki", "_index.md")) {
		t.Error("dry run wrote the index")
	}

	// Write the real index, then dry-run again: up to date.
	captureLintStdout(t, func() { err = (&IndexCmd{}).Run() })
	if err != nil {
		t.Fatal(err)
	}
	out = captureLintStdout(t, func() { err = (&IndexCmd{DryRun: true}).Run() })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "up to date") {
		t.Errorf("expected up-to-date message:\n%s", out)
	}
}

// TestIndexCmdMasksSynopsisSecrets pins issue #5: a credential-shaped
// string in an article's first sentence must not resurface unmasked in
// the regenerated wiki/_index.md synopsis line, but only in team mode
// with the secret gate active — solo KBs and explicitly-disabled gates
// see today's unmasked behavior, matching holdSecretFiles/findSecretsInKB.
func TestIndexCmdMasksSynopsisSecrets(t *testing.T) {
	awsKey := fakeAWSKey()

	tests := []struct {
		name         string
		yaml         string
		sentence     string
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "team mode masks a real secret",
			yaml:         "team: true\n",
			sentence:     "Uses key " + awsKey + " for auth.",
			wantContains: []string{defaultRedaction},
			wantAbsent:   []string{awsKey},
		},
		{
			name:         "solo KB (default) leaves it unmasked",
			yaml:         "domains: [acme]\n",
			sentence:     "Uses key " + awsKey + " for auth.",
			wantContains: []string{awsKey},
		},
		{
			name:         "secret_scan.disable leaves it unmasked",
			yaml:         "team: true\nsecret_scan:\n  disable: true\n",
			sentence:     "Uses key " + awsKey + " for auth.",
			wantContains: []string{awsKey},
		},
		{
			name:         "canonical example key stays visible end-to-end",
			yaml:         "team: true\n",
			sentence:     "See AKIAIOSFODNN7EXAMPLE in the docs.",
			wantContains: []string{"AKIAIOSFODNN7EXAMPLE"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SCRIBE_KB", root)
			writeKBFile(t, root, "wiki/a.md",
				"---\ntitle: \"A Article\"\ntype: solution\ndomain: general\n---\n\n"+tt.sentence+"\n")

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
			for _, sub := range tt.wantContains {
				if !strings.Contains(content, sub) {
					t.Errorf("missing %q in:\n%s", sub, content)
				}
			}
			for _, sub := range tt.wantAbsent {
				if strings.Contains(content, sub) {
					t.Errorf("unexpected %q leaked into:\n%s", sub, content)
				}
			}
		})
	}
}

// TestIndexCmdMasksSynopsisSecretsBeforeTruncating pins D5: masking must
// run on desc before the 80-char truncation, so a raw secret can never
// be cut mid-value and partially survive into the synopsis.
func TestIndexCmdMasksSynopsisSecretsBeforeTruncating(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("team: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCRIBE_KB", root)

	awsKey := fakeAWSKey()
	// No ". " in the first 120 chars, and total length is between 80 and
	// 120, so firstSentence returns the raw line unmodified (its own
	// truncation only kicks in past 120 chars) — masking then runs on
	// this full raw desc before index.go's own 80-char truncation.
	sentence := awsKey + " " + strings.Repeat("x", 79)
	writeKBFile(t, root, "wiki/a.md",
		"---\ntitle: \"A Article\"\ntype: solution\ndomain: general\n---\n\n"+sentence+"\n")

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
	if strings.Contains(content, awsKey) {
		t.Fatalf("raw key survived masking+truncation:\n%s", content)
	}
	if !strings.Contains(content, defaultRedaction) {
		t.Fatalf("expected masked marker in:\n%s", content)
	}
	found := false
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "- [[A Article]]") {
			continue
		}
		found = true
		// Isolate the masked+truncated desc: everything between "-- "
		// and the trailing "(type, domain)" suffix appended by index.go.
		desc, ok := strings.CutPrefix(line, "- [[A Article]] -- ")
		if !ok {
			t.Fatalf("unexpected synopsis line shape: %q", line)
		}
		desc, ok = strings.CutSuffix(desc, " (solution, general)")
		if !ok {
			t.Fatalf("unexpected synopsis line shape: %q", line)
		}
		if len(desc) > 83 { // 80-char truncation limit + "..." ellipsis
			t.Errorf("masked+truncated desc too long (%d bytes): %q", len(desc), desc)
		}
		if strings.Contains(desc, "[[") {
			t.Errorf("synopsis desc has a dangling wikilink opener: %q", desc)
		}
	}
	if !found {
		t.Fatalf("A Article entry missing from:\n%s", content)
	}
}

func TestBacklinksCmdRun(t *testing.T) {
	root := graphTestKB(t)

	var err error
	captureLintStdout(t, func() { err = (&BacklinksCmd{}).Run() })
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(root, "wiki", "_backlinks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var backlinks map[string][]string
	if err := json.Unmarshal(data, &backlinks); err != nil {
		t.Fatal(err)
	}

	if got := backlinks["B Article"]; len(got) != 1 || got[0] != "A Article" {
		t.Errorf("B Article backlinks = %v, want [A Article]", got)
	}
	// Self-link from b.md must not appear.
	for _, src := range backlinks["B Article"] {
		if src == "B Article" {
			t.Error("self-link counted as backlink")
		}
	}
	// Hub _index.md links are attributed by relative path.
	if got := backlinks["A Article"]; len(got) != 1 || got[0] != "wiki/_index.md" {
		t.Errorf("A Article backlinks = %v, want [wiki/_index.md]", got)
	}
	// Ghost Page is linked even though it doesn't exist — backlinks
	// records inbound edges regardless of target existence.
	if got := backlinks["Ghost Page"]; len(got) != 1 || got[0] != "A Article" {
		t.Errorf("Ghost Page backlinks = %v", got)
	}
}

func TestBacklinksCmdJSONAndDryRun(t *testing.T) {
	root := graphTestKB(t)

	var err error
	out := captureLintStdout(t, func() { err = (&BacklinksCmd{JSON: true}).Run() })
	if err != nil {
		t.Fatal(err)
	}
	var backlinks map[string][]string
	if err := json.Unmarshal([]byte(out), &backlinks); err != nil {
		t.Fatalf("stdout not JSON: %v\n%s", err, out)
	}
	if fileExists(filepath.Join(root, "wiki", "_backlinks.json")) {
		t.Error("--json must not write the file")
	}

	out = captureLintStdout(t, func() { err = (&BacklinksCmd{DryRun: true}).Run() })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "would create _backlinks.json") {
		t.Errorf("dry-run output:\n%s", out)
	}
	if fileExists(filepath.Join(root, "wiki", "_backlinks.json")) {
		t.Error("dry run wrote the file")
	}
}

func TestOrphansCmdRun(t *testing.T) {
	root := graphTestKB(t)
	_ = root

	var err error
	out := captureLintStdout(t, func() { err = (&OrphansCmd{JSON: true}).Run() })
	if err != nil {
		t.Fatal(err)
	}
	var report orphanReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out)
	}

	// C Tool has no inbound links; A Article is linked from the hub
	// index; B Article is linked from A.
	if len(report.Orphans) != 1 || report.Orphans[0] != "C Tool" {
		t.Errorf("orphans = %v, want [C Tool]", report.Orphans)
	}
	if len(report.Missing) != 1 || report.Missing[0] != "Ghost Page" {
		t.Errorf("missing = %v, want [Ghost Page]", report.Missing)
	}
}

func TestOrphansCmdFilters(t *testing.T) {
	graphTestKB(t)

	t.Run("orphans only", func(t *testing.T) {
		var err error
		out := captureLintStdout(t, func() {
			err = (&OrphansCmd{JSON: true, OrphansOnly: true}).Run()
		})
		if err != nil {
			t.Fatal(err)
		}
		var report orphanReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		if len(report.Orphans) != 1 || report.Missing != nil {
			t.Errorf("orphans-only report = %+v", report)
		}
	})

	t.Run("missing only", func(t *testing.T) {
		var err error
		out := captureLintStdout(t, func() {
			err = (&OrphansCmd{JSON: true, MissingOnly: true}).Run()
		})
		if err != nil {
			t.Fatal(err)
		}
		var report orphanReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		if len(report.Missing) != 1 || report.Orphans != nil {
			t.Errorf("missing-only report = %+v", report)
		}
	})

	t.Run("text output lists both sections", func(t *testing.T) {
		var err error
		out := captureLintStdout(t, func() { err = (&OrphansCmd{}).Run() })
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "Orphan articles (1)") || !strings.Contains(out, "C Tool") {
			t.Errorf("orphan section wrong:\n%s", out)
		}
		if !strings.Contains(out, "Missing pages (1)") || !strings.Contains(out, "[[Ghost Page]]") {
			t.Errorf("missing section wrong:\n%s", out)
		}
	})
}

func TestOrphansCmd_AliasCountsAsTitle(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("domains: [acme]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCRIBE_KB", root)
	writeKBFile(t, root, "wiki/a.md",
		"---\ntitle: \"A Article\"\n---\n\nLinks to [[Bee]].\n")
	writeKBFile(t, root, "wiki/b.md",
		"---\ntitle: \"B Article\"\naliases: [Bee]\n---\n\nLinks to [[A Article]].\n")

	var err error
	out := captureLintStdout(t, func() { err = (&OrphansCmd{JSON: true}).Run() })
	if err != nil {
		t.Fatal(err)
	}
	var report orphanReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 0 {
		t.Errorf("alias link reported missing: %v", report.Missing)
	}
}
