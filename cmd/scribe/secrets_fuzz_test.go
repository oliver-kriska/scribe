package main

// Native Go fuzz target for the secret scanner. The scanner runs over
// LLM-written article content on every team-KB commit, so arbitrary
// bytes (minified HTML, invalid UTF-8, megabyte lines) are its normal
// diet — a panic wedges the commit gate on cron.
//
// Credential-shaped seed bytes are assembled AT RUNTIME via the same
// join-trick the unit tests use (fakeAWSKey etc. in secrets_test.go) so
// no token-shaped literal ever sits in this repo.
//
// Runs as a plain unit test (seeds only) under `make test`. No network,
// no user paths.

import (
	"strings"
	"testing"
)

// FuzzScanContentForSecrets checks the scanner invariants:
//
//  1. Never panics, on any byte sequence, generic rule on or off.
//  2. Every finding's line number is within [1, line count].
//  3. A hit never lands on a line carrying a scribe:allow /
//     gitleaks:allow marker — and stamping the marker onto EVERY line
//     of the same content silences the scanner completely.
//  4. At most one hit per rule per content (dedupe contract), and every
//     hit carries a non-empty RuleID + Label (no secret bytes — the hit
//     struct has nowhere to put them, but the labels must be real).
//
// Run longer:
//
//	go test ./cmd/scribe -tags sqlite_fts5 -run '^$' -fuzz '^FuzzScanContentForSecrets$' -fuzztime 5m
func FuzzScanContentForSecrets(f *testing.F) {
	seeds := [][]byte{
		// Real token shapes, assembled at runtime (secrets_test.go helpers).
		[]byte("the key was " + fakeAWSKey() + " in the env\n"),
		[]byte("export GH_TOKEN=" + fakeGitHubToken()),
		[]byte("set it to " + fakeAnthropicKey() + " and run\n"),
		[]byte("Authorization: Bearer " + fakeJWT() + "\n"),
		[]byte("-----BEGIN RSA PRIVATE KEY-----\nMII...\n-----END RSA PRIVATE KEY-----\n"),
		[]byte("conn: postgres://admin:s3cretPa55@db.internal:5432/app\n"),
		// Allow markers.
		[]byte("real-looking " + fakeAWSKey() + " <!-- scribe:allow -->\n"),
		[]byte("real-looking " + fakeAWSKey() + " # gitleaks:allow\n"),
		// Placeholders / stopwords that must never fire.
		[]byte("use AKIAIOSFODNN7EXAMPLE in docs\n"),
		[]byte("https://user:${DB_PASS}@host.example.com\n"),
		[]byte("https://api:<your-token-here>@example.com\n"),
		// Generic assignment (entropy-gated, opt-in).
		[]byte(`api_key = "x9K2mP8qL5nR3vT7wY1zB6cD4"`),
		[]byte("password = secretvaluewithoutanydigits\n"),
		// Over-long single line (minified absorb shape) with a token head.
		[]byte("prefix " + fakeAWSKey() + " " + strings.Repeat("x", 70000) + "\n"),
		// Multi-line mix, no trailing newline, CRLF, empty lines.
		[]byte("clean line\n\nkey: " + fakeAWSKey() + "\r\nlast line no newline"),
		// Invalid UTF-8 around a token shape.
		append(append([]byte{0xff, 0xfe, 0x00}, []byte(fakeGitHubToken())...), 0x80, 0xbf),
		// Empty and whitespace-only.
		[]byte(""), []byte("\n\n\n"), []byte("   \t  \n"),
	}
	for _, s := range seeds {
		f.Add(s, false)
		f.Add(s, true)
	}
	f.Fuzz(func(t *testing.T, content []byte, generic bool) {
		hits := scanContentForSecrets(content, generic)

		lines := strings.Split(string(content), "\n")
		seenRule := make(map[string]bool, len(hits))
		for _, h := range hits {
			if h.RuleID == "" || h.Label == "" {
				t.Fatalf("hit with empty rule id/label: %+v", h)
			}
			if seenRule[h.RuleID] {
				t.Fatalf("rule %s fired twice in one content (dedupe contract)", h.RuleID)
			}
			seenRule[h.RuleID] = true
			if h.Line < 1 || h.Line > len(lines) {
				t.Fatalf("finding line %d out of bounds [1, %d]", h.Line, len(lines))
			}
			line := lines[h.Line-1]
			if strings.Contains(line, "scribe:allow") || strings.Contains(line, "gitleaks:allow") {
				t.Fatalf("rule %s fired on an allow-marked line %d: the marker must always suppress", h.RuleID, h.Line)
			}
		}

		// Suppression invariant: the same content with scribe:allow
		// stamped onto every line must produce zero findings.
		marked := make([]string, len(lines))
		for i, l := range lines {
			marked[i] = l + " scribe:allow"
		}
		if got := scanContentForSecrets([]byte(strings.Join(marked, "\n")), generic); len(got) != 0 {
			t.Fatalf("scribe:allow on every line did not suppress all findings: %+v", got)
		}
	})
}
