package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Stop-words commit gate (issue #25). A user-defined list of words/
// patterns vetted at commit time, the same seam and fail-closed posture
// as the secret scanner (secrets.go) — but driven by the user's own
// list, and active for solo KBs too, not just team mode. Two modes:
//
//   - hold:  a staged markdown file containing the word is held back
//     from the commit, exactly like SECRET HELD — it stays local and
//     uncommitted, never silently dropped, so the knowledge doesn't
//     vanish invisibly. The conservative default.
//   - mask:  every occurrence of the word is redacted in place and the
//     now-sanitized file commits. Use when the document is worth keeping
//     minus one noun (a client name, an internal codename).
//
// Match semantics (per issue #25, "plain substring is a footgun"):
//   - a bare entry is matched WHOLE-WORD and CASE-INSENSITIVELY
//     ("Falcon" hits "the Falcon project" but not "falconry")
//   - an entry wrapped in slashes is a regex opt-in: "/codename-\d+/"
//     compiles as a case-insensitive Go regexp (embed (?-i) for case
//     sensitivity). The matched span is what gets masked.
//
// `scribe:allow` / `gitleaks:allow` on a line suppresses the gate for
// that line, same marker the secret scanner honors.

// StopWordsConfig is one half of the stop-words list — it appears both
// in the shared scribe.yaml (team policy) and in the per-machine
// ~/.config/scribe/config.yaml (personal words, never committed); the
// gate unions the two. Deliberately NOT part of the trust-locked
// sensitiveConfig: the list evolves (members add words constantly) and
// trust-locking would force every teammate to re-trust on each addition.
// The sovereign guarantee lives in the personal config, which a push can
// never touch — the shared list is convenience team policy on top.
type StopWordsConfig struct {
	// Hold lists words/patterns that hold the whole document out of the
	// KB when matched. Bare = whole-word, case-insensitive; "/re/" = regex.
	Hold []string `yaml:"hold"`
	// Mask lists words/patterns whose every occurrence is replaced with
	// Redaction in place; the sanitized document still commits.
	Mask []string `yaml:"mask"`
	// Redaction is the replacement token for masked words. Empty defaults
	// to "[redacted]".
	Redaction string `yaml:"redaction"`
}

const defaultRedaction = "[redacted]"

// stopWordMatcher is one compiled list entry. Label is the original
// entry text (never any matched content) for the gate's log line.
type stopWordMatcher struct {
	re    *regexp.Regexp
	label string
}

// stopWordRules unions the shared (scribe.yaml) and personal
// (~/.config/scribe/config.yaml) lists, compiles each entry, and dedupes
// by label so the same word configured in both places logs once. A bad
// regex entry is logged and dropped — one typo must not wedge the gate.
func stopWordRules(cfg *ScribeConfig) (hold, mask []stopWordMatcher, redaction string) {
	user := loadUserConfig()
	shared := StopWordsConfig{}
	if cfg != nil {
		shared = cfg.StopWords
	}

	redaction = defaultRedaction
	switch {
	case shared.Redaction != "":
		redaction = shared.Redaction
	case user.StopWords.Redaction != "":
		redaction = user.StopWords.Redaction
	}

	hold = compileStopWords(append(append([]string{}, shared.Hold...), user.StopWords.Hold...))
	mask = compileStopWords(append(append([]string{}, shared.Mask...), user.StopWords.Mask...))
	return hold, mask, redaction
}

// compileStopWords turns raw entries into matchers, skipping blanks and
// duplicates (by trimmed label) and dropping (with a log line) entries
// whose regex form fails to compile.
func compileStopWords(entries []string) []stopWordMatcher {
	var out []stopWordMatcher
	seen := map[string]bool{}
	for _, raw := range entries {
		label := strings.TrimSpace(raw)
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		re, ok := compileStopWord(label)
		if !ok {
			logMsg("config", "stop_words: dropping unparseable entry %q (bad regex)", label)
			continue
		}
		out = append(out, stopWordMatcher{re: re, label: label})
	}
	return out
}

// compileStopWord builds the regexp for one entry. "/.../ " is a
// case-insensitive regex; anything else is a literal whole-word match.
func compileStopWord(entry string) (*regexp.Regexp, bool) {
	if len(entry) >= 2 && strings.HasPrefix(entry, "/") && strings.HasSuffix(entry, "/") {
		re, err := regexp.Compile("(?i)" + entry[1:len(entry)-1])
		if err != nil {
			return nil, false
		}
		return re, true
	}
	re, err := regexp.Compile(literalWholeWordPattern(entry))
	if err != nil {
		return nil, false
	}
	return re, true
}

// literalWholeWordPattern quotes a literal and fences it with word
// boundaries — but only on an edge whose boundary character is an ASCII
// word char, since Go's \b is ASCII-only. A literal that starts or ends
// with punctuation ("@acme", "v2.") gets no \b on that side, which is
// the correct (non-substring-eating) behavior for RE2.
func literalWholeWordPattern(w string) string {
	var b strings.Builder
	b.WriteString("(?i)")
	if r, _ := utf8.DecodeRuneInString(w); isASCIIWordChar(r) {
		b.WriteString(`\b`)
	}
	b.WriteString(regexp.QuoteMeta(w))
	if r, _ := utf8.DecodeLastRuneInString(w); isASCIIWordChar(r) {
		b.WriteString(`\b`)
	}
	return b.String()
}

func isASCIIWordChar(r rune) bool {
	return r == '_' ||
		(r >= '0' && r <= '9') ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z')
}

// stopWordDecision is the per-file verdict from applyStopWords.
type stopWordDecision struct {
	hold         bool
	line         int    // first hold line, for the log
	label        string // the hold entry that fired
	masked       bool
	content      []byte   // masked content (only when masked)
	maskedLabels []string // distinct mask entries that fired, in first-seen order
}

var stopWordAllowMarkers = [][]byte{[]byte("scribe:allow"), []byte("gitleaks:allow")}

// applyStopWords scans content line-by-line. Hold wins: if any hold
// matcher fires on a non-exempt line, the whole file is held and no
// masking is reported (a partially masked file must not commit while it
// still carries a held word). Otherwise every mask match on a non-exempt
// line is replaced with redaction, byte structure (newlines, trailing
// newline) preserved exactly.
func applyStopWords(content []byte, hold, mask []stopWordMatcher, redaction string) stopWordDecision {
	var dec stopWordDecision
	var out bytes.Buffer
	out.Grow(len(content))
	maskedSeen := map[string]bool{}
	changed := false

	lineNo := 0
	for start := 0; start < len(content); {
		end := bytes.IndexByte(content[start:], '\n')
		var line []byte
		hasNL := false
		if end < 0 {
			line = content[start:]
			start = len(content)
		} else {
			line = content[start : start+end]
			start += end + 1
			hasNL = true
		}
		lineNo++

		processed := line
		if !lineHasAllowMarker(line) {
			for i := range hold {
				if hold[i].re.Match(line) && !dec.hold {
					dec.hold = true
					dec.line = lineNo
					dec.label = hold[i].label
				}
			}
			for i := range mask {
				if mask[i].re.Match(processed) {
					processed = mask[i].re.ReplaceAll(processed, []byte(redaction))
					changed = true
					if !maskedSeen[mask[i].label] {
						maskedSeen[mask[i].label] = true
						dec.maskedLabels = append(dec.maskedLabels, mask[i].label)
					}
				}
			}
		}
		out.Write(processed)
		if hasNL {
			out.WriteByte('\n')
		}
	}

	if dec.hold {
		return stopWordDecision{hold: true, line: dec.line, label: dec.label}
	}
	if changed {
		return stopWordDecision{masked: true, content: out.Bytes(), maskedLabels: dec.maskedLabels}
	}
	return stopWordDecision{}
}

func lineHasAllowMarker(line []byte) bool {
	for _, m := range stopWordAllowMarkers {
		if bytes.Contains(line, m) {
			return true
		}
	}
	return false
}

// holdStopWordFiles is the stop-words commit gate, run for every KB
// (solo and team) alongside holdSecretFiles. Returns false when a file
// that needed holding could not be held — the caller then skips the
// commit, exactly like the secret gate, so the staged change rolls over
// to the next run rather than committing unfiltered.
func holdStopWordFiles(root string, cfg *ScribeConfig) bool {
	if cfg != nil && cfg.LoadErr != nil {
		// An unparseable scribe.yaml means an unknowable stop-word list;
		// the secret gate already fails the commit closed here, mirror it
		// so a direct caller is safe too.
		return false
	}
	hold, mask, redaction := stopWordRules(cfg)
	if len(hold) == 0 && len(mask) == 0 {
		return true // nothing configured — zero cost for the common case
	}

	safe := true
	for _, rel := range stagedMarkdown(root) {
		data, err := gitShowBytes(root, ":"+rel)
		if err != nil {
			// A staged blob we can't read can't be proven clean. Fail
			// closed: hold it unscanned, the next run rescans.
			logMsg("git", "STOPWORD GATE: %s — staged content unreadable (%v), holding unscanned", rel, err)
			safe = unstageHeld(root, rel) && safe
			continue
		}
		dec := applyStopWords(data, hold, mask, redaction)
		switch {
		case dec.hold:
			if !unstageHeld(root, rel) {
				safe = false
				continue
			}
			logMsg("git", "STOPWORD HELD: %s:%d [%s] — file held back from commit; remove the word or add 'scribe:allow' to the line", rel, dec.line, dec.label)
		case dec.masked:
			if !restageMasked(root, rel, dec.content) {
				// Couldn't persist the masked version — the unmasked blob
				// is still staged. Hold it rather than commit unredacted.
				safe = unstageHeld(root, rel) && safe
				continue
			}
			logMsg("git", "STOPWORD MASKED: %s — redacted in place before commit: %s", rel, strings.Join(dec.maskedLabels, ", "))
		}
	}
	return safe
}

// restageMasked writes the redacted content over the worktree file and
// re-stages it, so the masked version is what commits AND what sits on
// disk (otherwise the worktree would re-show the unmasked copy as a diff
// every run). Preserves the existing file mode. Reports success.
func restageMasked(root, rel string, content []byte) bool {
	abs := filepath.Join(root, rel)
	perm := os.FileMode(0o644)
	if fi, err := os.Stat(abs); err == nil {
		perm = fi.Mode().Perm()
	}
	if err := os.WriteFile(abs, content, perm); err != nil {
		logMsg("git", "STOPWORD GATE: %s — could not write masked content (%v); holding instead", rel, err)
		return false
	}
	if _, err := runCmdErr(root, "git", "add", "--", rel); err != nil {
		logMsg("git", "STOPWORD GATE: %s — could not re-stage masked content (%v); holding instead", rel, err)
		return false
	}
	return true
}

// commitGate runs every staged-markdown gate before a commit: the
// team-mode secret scanner (secrets.go) then the stop-words filter. Both
// run unconditionally and in sequence (not short-circuited) so a file
// caught by one still gets logged by the other, and either returning
// false makes the caller skip the commit. Secrets runs first because it
// may unstage a file the stop-words pass would otherwise rescan.
func commitGate(root string, cfg *ScribeConfig) bool {
	secretsOK := holdSecretFiles(root, cfg)
	stopOK := holdStopWordFiles(root, cfg)
	return secretsOK && stopOK
}
