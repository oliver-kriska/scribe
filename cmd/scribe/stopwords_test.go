package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// seedStopWords isolates the personal config (so loadUserConfig() can't
// read the dev machine's real ~/.config/scribe/config.yaml) and, when
// yaml is non-empty, seeds it with a personal stop_words list.
func seedStopWords(t *testing.T, yaml string) {
	t.Helper()
	isolateUserConfig(t)
	if yaml != "" {
		writeUserCfg(t, yaml)
	}
}

// --- matcher compilation & semantics -------------------------------------

func TestStopWord_WholeWordCaseInsensitive(t *testing.T) {
	hold := compileStopWords([]string{"Falcon"})
	mask := []stopWordMatcher(nil)

	hits := []struct {
		body string
		held bool
	}{
		{"the Falcon project", true},   // exact word, mid-sentence
		{"codename FALCON here", true}, // case-insensitive
		{"falconry is unrelated", false},
		{"Falcons plural", false}, // \b after the word: "Falcons" is not "Falcon"
		{"nothing to see", false},
	}
	for _, tc := range hits {
		dec := applyStopWords([]byte(tc.body), hold, mask, defaultRedaction)
		if dec.hold != tc.held {
			t.Errorf("body %q: hold = %v, want %v", tc.body, dec.hold, tc.held)
		}
	}
}

func TestStopWord_RegexOptIn(t *testing.T) {
	hold := compileStopWords([]string{`/codename-\d+/`})
	dec := applyStopWords([]byte("ticket codename-42 shipped"), hold, nil, defaultRedaction)
	if !dec.hold {
		t.Fatal("regex entry did not match codename-42")
	}
	if dec.label != `/codename-\d+/` {
		t.Errorf("label = %q, want the raw regex entry", dec.label)
	}
	// A non-matching body for the same regex.
	if applyStopWords([]byte("codename-x"), hold, nil, defaultRedaction).hold {
		t.Error("regex matched codename-x (no digits)")
	}
}

func TestStopWord_BadRegexDropped(t *testing.T) {
	// "/[/" is an unterminated class — must be dropped, not panic.
	got := compileStopWords([]string{"/[/", "Keep"})
	if len(got) != 1 || got[0].label != "Keep" {
		t.Fatalf("bad regex not dropped cleanly: %+v", got)
	}
}

func TestStopWord_PunctuationEdgeLiteral(t *testing.T) {
	// A literal whose edges are non-word chars gets no \b there, so it
	// still matches as a span (the safest available behavior for RE2).
	hold := compileStopWords([]string{"@acme"})
	if !applyStopWords([]byte("ping @acme now"), hold, nil, defaultRedaction).hold {
		t.Error("@acme literal did not match")
	}
}

// --- applyStopWords behavior ---------------------------------------------

func TestApplyStopWords_MaskPreservesByteStructure(t *testing.T) {
	mask := compileStopWords([]string{"secret"})
	for _, tc := range []struct {
		in, want string
	}{
		{"a secret here\nno match\n", "a [redacted] here\nno match\n"},
		{"just secret", "just [redacted]"}, // no trailing newline preserved
		{"SECRET twice secret\n", "[redacted] twice [redacted]\n"},
	} {
		dec := applyStopWords([]byte(tc.in), nil, mask, defaultRedaction)
		if !dec.masked {
			t.Fatalf("in %q: not masked", tc.in)
		}
		if string(dec.content) != tc.want {
			t.Errorf("in %q: got %q, want %q", tc.in, dec.content, tc.want)
		}
	}
}

func TestApplyStopWords_HoldWinsOverMask(t *testing.T) {
	hold := compileStopWords([]string{"BLOCK"})
	mask := compileStopWords([]string{"keepout"})
	dec := applyStopWords([]byte("this BLOCK and keepout\n"), hold, mask, defaultRedaction)
	if !dec.hold {
		t.Fatal("hold word present but file not held")
	}
	if dec.masked || dec.content != nil {
		t.Error("hold must win: no masked content should be produced")
	}
}

func TestApplyStopWords_AllowMarkerExempts(t *testing.T) {
	hold := compileStopWords([]string{"Falcon"})
	mask := compileStopWords([]string{"secret"})
	// Both on allow-marked lines → no hold, no mask.
	body := "Falcon here scribe:allow\nthe secret value gitleaks:allow\n"
	dec := applyStopWords([]byte(body), hold, mask, defaultRedaction)
	if dec.hold {
		t.Error("hold fired on a scribe:allow line")
	}
	if dec.masked {
		t.Error("mask applied on a gitleaks:allow line")
	}
}

func TestApplyStopWords_MaskLabelsDistinctDeduped(t *testing.T) {
	mask := compileStopWords([]string{"alpha", "beta"})
	dec := applyStopWords([]byte("beta then alpha then beta\n"), nil, mask, defaultRedaction)
	// Labels are reported in config order and deduped (beta appears twice
	// in the text but once in the log) — deterministic regardless of where
	// in the document each word lands.
	if !slices.Equal(dec.maskedLabels, []string{"alpha", "beta"}) {
		t.Errorf("maskedLabels = %v, want [alpha beta] (config order, deduped)", dec.maskedLabels)
	}
}

func TestApplyStopWords_CustomRedaction(t *testing.T) {
	mask := compileStopWords([]string{"name"})
	dec := applyStopWords([]byte("the name\n"), nil, mask, "███")
	if string(dec.content) != "the ███\n" {
		t.Errorf("custom redaction not used: %q", dec.content)
	}
}

// --- gate end to end (real git index) ------------------------------------

func TestHoldStopWordFiles_HoldsMatching_SoloKB(t *testing.T) {
	seedStopWords(t, "")
	repo := initTestGitRepo(t, "Gate Tester")
	writeKBFile(t, repo, "wiki/leak.md", "# notes\n\nthe Project Falcon spec\n")
	gitRun(t, repo, "add", "wiki")

	// Team is FALSE — stop-words must run for solo KBs too (issue #25).
	cfg := &ScribeConfig{StopWords: StopWordsConfig{Hold: []string{"Project Falcon"}}}
	if !holdStopWordFiles(repo, cfg) {
		t.Fatal("gate reported unsafe on a clean hold")
	}
	if len(stagedMarkdown(repo)) != 0 {
		t.Errorf("held file still staged: %v", stagedMarkdown(repo))
	}
	// The file remains on disk (held, not dropped).
	if _, err := os.Stat(filepath.Join(repo, "wiki", "leak.md")); err != nil {
		t.Errorf("held file vanished from worktree: %v", err)
	}
}

func TestHoldStopWordFiles_MasksMatching(t *testing.T) {
	seedStopWords(t, "")
	repo := initTestGitRepo(t, "Gate Tester")
	writeKBFile(t, repo, "wiki/doc.md", "# doc\n\nbuilt for AcmeCorp last year\n")
	gitRun(t, repo, "add", "wiki")

	cfg := &ScribeConfig{StopWords: StopWordsConfig{Mask: []string{"AcmeCorp"}}}
	if !holdStopWordFiles(repo, cfg) {
		t.Fatal("gate reported unsafe while masking")
	}
	// Still staged (masked, not held)...
	if !slices.Contains(stagedMarkdown(repo), "wiki/doc.md") {
		t.Fatal("masked file was unstaged — it should commit redacted")
	}
	// ...and the staged blob no longer contains the word.
	blob, err := gitShowBytes(repo, ":wiki/doc.md")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "AcmeCorp") {
		t.Errorf("staged blob still contains the masked word:\n%s", blob)
	}
	if !strings.Contains(string(blob), defaultRedaction) {
		t.Errorf("staged blob missing redaction marker:\n%s", blob)
	}
	// Worktree file rewritten too (so it doesn't re-diff next run).
	disk, err := os.ReadFile(filepath.Join(repo, "wiki", "doc.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(disk), "AcmeCorp") {
		t.Errorf("worktree file still contains the masked word:\n%s", disk)
	}
}

func TestHoldStopWordFiles_PersonalConfigUnion(t *testing.T) {
	// The hold word lives ONLY in the personal config, not scribe.yaml.
	seedStopWords(t, "stop_words:\n  hold:\n    - MyPrivateName\n")
	repo := initTestGitRepo(t, "Gate Tester")
	writeKBFile(t, repo, "wiki/p.md", "MyPrivateName appears here\n")
	gitRun(t, repo, "add", "wiki")

	cfg := &ScribeConfig{} // shared list empty
	if !holdStopWordFiles(repo, cfg) {
		t.Fatal("gate reported unsafe")
	}
	if len(stagedMarkdown(repo)) != 0 {
		t.Errorf("personal-config hold word didn't hold the file: %v", stagedMarkdown(repo))
	}
}

func TestHoldStopWordFiles_EmptyConfigNoOp(t *testing.T) {
	seedStopWords(t, "")
	repo := initTestGitRepo(t, "Gate Tester")
	writeKBFile(t, repo, "wiki/x.md", "ordinary content\n")
	gitRun(t, repo, "add", "wiki")

	if !holdStopWordFiles(repo, &ScribeConfig{}) {
		t.Fatal("empty config reported unsafe")
	}
	if !slices.Contains(stagedMarkdown(repo), "wiki/x.md") {
		t.Error("empty stop-words config touched a staged file")
	}
}

func TestHoldStopWordFiles_LoadErrFailsClosed(t *testing.T) {
	seedStopWords(t, "")
	repo := initTestGitRepo(t, "Gate Tester")
	cfg := &ScribeConfig{LoadErr: os.ErrInvalid}
	if holdStopWordFiles(repo, cfg) {
		t.Error("gate must fail closed (false) on an unparseable config")
	}
}

func TestHoldStopWordFiles_AllowMarkerCommits(t *testing.T) {
	seedStopWords(t, "")
	repo := initTestGitRepo(t, "Gate Tester")
	writeKBFile(t, repo, "wiki/ok.md", "Project Falcon scribe:allow\n")
	gitRun(t, repo, "add", "wiki")

	cfg := &ScribeConfig{StopWords: StopWordsConfig{Hold: []string{"Project Falcon"}}}
	if !holdStopWordFiles(repo, cfg) {
		t.Fatal("gate reported unsafe")
	}
	if !slices.Contains(stagedMarkdown(repo), "wiki/ok.md") {
		t.Error("scribe:allow line was held anyway")
	}
}
