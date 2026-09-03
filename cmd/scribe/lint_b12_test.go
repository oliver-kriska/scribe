package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestChangedWikiFiles_SkipsDeleted: `git diff --name-only HEAD` lists a
// deleted article too, and it reached validateFile as "cannot read".
func TestChangedWikiFiles_SkipsDeleted(t *testing.T) {
	root := lintTestKB(t)
	writeKBFile(t, root, "wiki/gone.md", lintValidArticle("Gone", 20))
	writeKBFile(t, root, "wiki/kept.md", lintValidArticle("Kept", 20))
	gitQuick(t, root, "init")
	gitQuick(t, root, "add", ".")
	gitQuick(t, root, "commit", "-m", "seed")
	if err := os.Remove(filepath.Join(root, "wiki", "gone.md")); err != nil {
		t.Fatal(err)
	}
	writeKBFile(t, root, "wiki/kept.md", lintValidArticle("Kept", 21))

	files, err := changedWikiFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || relPath(root, files[0]) != "wiki/kept.md" {
		t.Errorf("want only wiki/kept.md, got %v", files)
	}
}

// TestLintRun_ExplicitFileStaysLocal: `scribe lint wiki/a.md` used to walk
// the whole KB for phases 3-6 and fail on articles nobody asked about.
// Conflict markers follow the scope, since they are a per-file property.
func TestLintRun_ExplicitFileStaysLocal(t *testing.T) {
	root := lintTestKB(t)
	writeKBFile(t, root, "wiki/a.md", lintValidArticle("A Article", 14))
	writeKBFile(t, root, "wiki/broken.md", lintValidArticle("Broken", 14)+"<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> origin/main\n")

	full := &LintCmd{}
	var err error
	captureLintStdout(t, func() { err = full.Run() })
	if err == nil {
		t.Fatal("full scan must fail on the conflict marker in wiki/broken.md")
	}

	scoped := &LintCmd{Files: []string{filepath.Join(root, "wiki", "a.md")}}
	out := captureLintStdout(t, func() { err = scoped.Run() })
	if err != nil {
		t.Fatalf("lint on one clean file must not fail on another file: %v\n%s", err, out)
	}
	if strings.Contains(out, "Phase 3") {
		t.Errorf("phases 3-5 must not run for an explicit file:\n%s", out)
	}

	scoped = &LintCmd{Files: []string{filepath.Join(root, "wiki", "broken.md")}}
	captureLintStdout(t, func() { err = scoped.Run() })
	if err == nil {
		t.Fatal("conflict marker inside the scoped file must still fail")
	}
}

func TestFirstConflictMarkerLine_Escapes(t *testing.T) {
	cases := map[string]int{
		"a\n<<<<<<< HEAD\nb\n":                            2,
		"a\n```diff\n<<<<<<< HEAD\n>>>>>>> x\n```\nb\n":   0,
		"a\n~~~\n<<<<<<< HEAD\n~~~\n<<<<<<< HEAD\n":       5,
		"a\n<<<<<<< HEAD <!-- scribe:allow -->\nb\n":      0,
		"a\n  ```\n<<<<<<< HEAD\n  ```\n":                 0,
		"a\n```\nunclosed fence\n<<<<<<< HEAD\n":          0,
		"<<<<<<< HEAD\n":                                  1,
		"a\n>>>>>>> feature scribe:allow\n<<<<<<< HEAD\n": 3,
	}
	for in, want := range cases {
		if got := firstConflictMarkerLine([]byte(in)); got != want {
			t.Errorf("%q: line %d, want %d", in, got, want)
		}
	}
}

func TestCoerceScalarListField_KeepsCommasInsideURLs(t *testing.T) {
	got, ok := coerceScalarListField([]string{"sources: https://x.test/a,b, https://y.test/c"}, "sources")
	if !ok {
		t.Fatal("expected coercion")
	}
	if strings.Count(got[0], "http") != 2 || !strings.Contains(got[0], "https://x.test/a,b") {
		t.Errorf("URL split on its own comma: %q", got[0])
	}
	got, _ = coerceScalarListField([]string{"tags: a,b, c"}, "tags")
	if got[0] != "tags: [a, b, c]" {
		t.Errorf("plain scalars still split on every comma: %q", got[0])
	}
}

const b12FullFM = "title: \"X\"\ntype: decision\ncreated: 2026-05-16\nupdated: 2026-05-16\ndomain: general\nconfidence: high\ntags: []\nrelated: []\nsources: []\n"

func TestAutoFixArticle_TrailingSpaceBeforeCR(t *testing.T) {
	in := "---\r\n" + strings.ReplaceAll(strings.Replace(b12FullFM, "title: \"X\"", "title: \"X\"   ", 1), "\n", "\r\n") + "---\r\n\r\nBody.\r\n"
	changes, out, err := autoFixArticle("", "decisions/x.md", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "\"X\"   \r") {
		t.Errorf("trailing spaces before CR not stripped:\n%q", out)
	}
	if !strings.Contains(string(out), "title: \"X\"\r\n") {
		t.Errorf("CR must survive the strip:\n%q", out)
	}
	if !strings.Contains(strings.Join(changes, "|"), "trailing whitespace") {
		t.Errorf("changes = %v", changes)
	}
}

func TestAutoFixArticle_BOMAndOddOpenings(t *testing.T) {
	changes, out, err := autoFixArticle("", "decisions/x.md", []byte("\uFEFF---\n"+b12FullFM+"---\n\nBody.\n"))
	if err != nil {
		t.Fatalf("BOM must be a repair, not a skip: %v", err)
	}
	if strings.HasPrefix(string(out), "\uFEFF") || !strings.Contains(strings.Join(changes, "|"), "byte-order mark") {
		t.Errorf("BOM not removed: %v\n%q", changes, out[:8])
	}
	if _, perr := parseFrontmatter(out); perr != nil {
		t.Errorf("repaired file does not validate: %v", perr)
	}

	_, _, err = autoFixArticle("", "decisions/x.md", []byte("----\ntitle: X\n---\n"))
	if err == nil || !strings.Contains(err.Error(), "unrecognized opening fence") {
		t.Errorf("a ---- opening must SKIP with a reason, got %v", err)
	}
	changes, out, err = autoFixArticle("", "decisions/x.md", []byte("Just a body-only stub.\n"))
	if err != nil || out != nil || changes != nil {
		t.Errorf("body-only stubs stay a silent no-op: %v %v %q", changes, err, out)
	}
}

// TestFrontmatterSpan_Agreement: validator, fixer and duplicate scanner
// read the same fence rule.
func TestFrontmatterSpan_Agreement(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		validOK  bool
		fixErr   bool
		fixNotes string // substring expected in the fixer's change list
		body     string // stripFrontmatterBody result
	}{
		{"bare", "---\n" + b12FullFM + "---\n\nBody.\n", true, false, "", "\nBody.\n"},
		{"trailing ws close", "---\n" + b12FullFM + "--- \n\nBody.\n", true, false, "normalized closing", "\nBody.\n"},
		{"trailing ws open", "--- \n" + b12FullFM + "---\n\nBody.\n", true, false, "normalized opening", "\nBody.\n"},
		{"crlf", "---\r\n" + strings.ReplaceAll(b12FullFM, "\n", "\r\n") + "---\r\n\r\nBody.\r\n", true, false, "", "\r\nBody.\r\n"},
		{"fence at EOF", "---\n" + b12FullFM + "---", true, false, "", ""},
		{"hr in body", "---\n" + b12FullFM + "---\n\nBody.\n\n---\n\nMore.\n", true, false, "", "\nBody.\n\n---\n\nMore.\n"},
		{"five dashes", "---\n" + b12FullFM + "-----\n\nBody.\n", false, false, "normalized closing", ""},
		{"dash x", "---\n" + b12FullFM + "--- x\n\nBody.\n", false, false, "normalized closing", ""},
		{"no close", "---\n" + b12FullFM + "\nBody.\n", false, true, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, verr := parseFrontmatter([]byte(c.in))
			if (verr == nil) != c.validOK {
				t.Errorf("validator: err=%v, want ok=%v", verr, c.validOK)
			}
			if _, rerr := parseFrontmatterRaw([]byte(c.in)); (rerr == nil) != c.validOK {
				t.Errorf("parseFrontmatterRaw disagrees with parseFrontmatter: %v", rerr)
			}
			if verr != nil && !c.fixErr && classifyFrontmatterError(verr.Error()) != errKindFixable {
				t.Errorf("a fence the fixer repairs must classify as fixable: %v", verr)
			}
			changes, out, ferr := autoFixArticle("", "decisions/x.md", []byte(c.in))
			if (ferr != nil) != c.fixErr {
				t.Errorf("fixer: err=%v, want err=%v", ferr, c.fixErr)
			}
			if c.fixNotes != "" && !strings.Contains(strings.Join(changes, "|"), c.fixNotes) {
				t.Errorf("fixer changes %v lack %q", changes, c.fixNotes)
			}
			if ferr == nil && out != nil {
				if _, perr := parseFrontmatter(out); perr != nil {
					t.Errorf("fixer output does not validate: %v\n%s", perr, out)
				}
			}
			body := stripFrontmatterBody([]byte(c.in))
			if c.validOK && body != c.body {
				t.Errorf("stripFrontmatterBody = %q, want %q", body, c.body)
			}
			if !c.validOK && body != c.in {
				t.Errorf("unparseable frontmatter must leave the content whole")
			}
		})
	}
}
