package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readSingleRunRecord returns the one JSONL line writeRunRecord appended.
func readSingleRunRecord(t *testing.T, root string) string {
	t.Helper()
	runsDir := filepath.Join(root, "output", "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		t.Fatalf("no run records written: %v", err)
	}
	var lines []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(runsDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if strings.TrimSpace(ln) != "" {
				lines = append(lines, ln)
			}
		}
	}
	if len(lines) != 1 {
		t.Fatalf("want exactly 1 run record, got %d: %v", len(lines), lines)
	}
	return lines[0]
}

// The failure-mode coverage for the Pinboard adapter. The happy-path tests
// live in source_pinboard_test.go; everything here drives an ERROR path,
// because that is where the token-leak bug (B1) hid: no test ever looked at
// what an error message contained.
//
// Every test is offline — httptest servers or a closed listener. Nothing here
// contacts api.pinboard.in.

const (
	testPinboardUser   = "oliver"
	testPinboardSecret = "DEADBEEFCAFE1234"
	testPinboardToken  = testPinboardUser + ":" + testPinboardSecret
)

// assertNoTokenLeak fails when a string that is about to reach a log line, a
// run record, or a terminal still carries the credential in any form.
func assertNoTokenLeak(t *testing.T, what, s string) {
	t.Helper()
	for _, bad := range []string{testPinboardToken, testPinboardSecret, "auth_token=" + testPinboardToken} {
		if strings.Contains(s, bad) {
			t.Errorf("%s leaks the credential (%q) — full text:\n%s", what, bad, s)
		}
	}
}

// pinboardErrorFrom drives one Fetch against a handler and returns the error.
func pinboardErrorFrom(t *testing.T, h http.HandlerFunc) error {
	t.Helper()
	t.Setenv("SCRIBE_PINBOARD_TOKEN", testPinboardToken)
	srv := httptest.NewServer(h)
	defer srv.Close()
	p := pinboardSource{baseURL: srv.URL + "/"}
	_, _, err := p.Fetch(context.Background(), pinboardTestCfg(), nil, FetchOpts{Scope: "all"})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	return err
}

// TestPinboardTransportErrorRedactsToken is the B1 regression. A dial failure
// produces a *url.Error whose message embeds the full request URL, and Go only
// strips userinfo passwords — never query parameters. Before the fix this
// error reached output/runs/*.jsonl, /tmp/scribe-pull.log, and `scribe doctor`
// with `auth_token=oliver:DEADBEEFCAFE1234` intact.
func TestPinboardTransportErrorRedactsToken(t *testing.T) {
	t.Setenv("SCRIBE_PINBOARD_TOKEN", testPinboardToken)

	// A server that is already closed → guaranteed connection-refused, no
	// network egress and no dependency on DNS.
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL + "/"
	srv.Close()

	p := pinboardSource{baseURL: base}
	_, _, err := p.Fetch(context.Background(), pinboardTestCfg(), nil, FetchOpts{Scope: "all"})
	if err == nil {
		t.Fatal("expected a transport error against a closed server")
	}
	assertNoTokenLeak(t, "transport error", err.Error())

	// The flattened chain must not smuggle the raw *url.Error back either.
	for e := err; e != nil; {
		assertNoTokenLeak(t, "wrapped error in chain", e.Error())
		next, ok := e.(interface{ Unwrap() error })
		if !ok {
			break
		}
		e = next.Unwrap()
	}
	if !strings.Contains(err.Error(), "REDACTED") {
		t.Errorf("expected a REDACTED marker in %q", err.Error())
	}
}

// TestPinboardHTTPStatusErrorsRedactToken covers the documented failure
// statuses. None of these embedded the token before the fix, but they share
// the get() error path, so they must stay clean as it evolves.
func TestPinboardHTTPStatusErrorsRedactToken(t *testing.T) {
	cases := []struct {
		name string
		code int
		want string
	}{
		{"rate limited", http.StatusTooManyRequests, "rate limited (429)"},
		{"unauthorized", http.StatusUnauthorized, "unauthorized (401)"},
		{"server error", http.StatusInternalServerError, "status 500"},
		{"gateway", http.StatusBadGateway, "status 502"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pinboardErrorFrom(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
			})
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			assertNoTokenLeak(t, tc.name+" error", err.Error())
		})
	}
}

// TestPinboardMalformedResponseErrors covers a truncated / non-JSON body —
// an upstream proxy serving an HTML error page, say. It must fail cleanly
// rather than panic or silently yield zero items.
func TestPinboardMalformedResponseErrors(t *testing.T) {
	cases := map[string]string{
		"html error page": "<html><body>502 Bad Gateway</body></html>",
		"truncated json":  `[{"href":"https://a.com","desc`,
		"empty body":      "",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			err := pinboardErrorFrom(t, func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, body)
			})
			if !strings.Contains(err.Error(), "decode") {
				t.Errorf("error = %q, want a decode error", err)
			}
			assertNoTokenLeak(t, name+" decode error", err.Error())
		})
	}
}

// TestPinboardRefusesCrossHostRedirect is the B2 regression: the token rides
// in the query string, so a redirect to another host would hand the
// credential over. Same-host redirects stay allowed.
func TestPinboardRefusesCrossHostRedirect(t *testing.T) {
	t.Setenv("SCRIBE_PINBOARD_TOKEN", testPinboardToken)

	var attackerSawToken string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerSawToken = r.URL.Query().Get("auth_token")
		io.WriteString(w, `{"update_time":"2026-07-01T10:00:00Z"}`)
	}))
	defer attacker.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+r.URL.Path, http.StatusFound)
	}))
	defer redirector.Close()

	p := pinboardSource{baseURL: redirector.URL + "/"}
	_, _, err := p.Fetch(context.Background(), pinboardTestCfg(), nil, FetchOpts{Scope: "all"})
	if err == nil {
		t.Fatal("a cross-host redirect must be refused, got nil error")
	}
	if attackerSawToken != "" {
		t.Errorf("token was forwarded to the redirect target: %q", attackerSawToken)
	}
	if !strings.Contains(err.Error(), "cross-host redirect") {
		t.Errorf("error = %q, want it to name the refused redirect", err)
	}
	assertNoTokenLeak(t, "redirect refusal error", err.Error())
}

// TestPinboardAllowsSameHostRedirect guards against over-correcting: the
// refusal must not break a legitimate same-host redirect.
func TestPinboardAllowsSameHostRedirect(t *testing.T) {
	t.Setenv("SCRIBE_PINBOARD_TOKEN", testPinboardToken)

	mux := http.NewServeMux()
	mux.HandleFunc("/posts/update", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/real/update?"+r.URL.RawQuery, http.StatusFound)
	})
	mux.HandleFunc("/real/update", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"update_time":"2026-07-01T10:00:00Z"}`)
	})
	mux.HandleFunc("/posts/all", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `[{"href":"https://a.com","description":"A","hash":"h-a"}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := pinboardSource{baseURL: srv.URL + "/"}
	items, _, err := p.Fetch(context.Background(), pinboardTestCfg(), nil, FetchOpts{Scope: "all"})
	if err != nil {
		t.Fatalf("same-host redirect should be followed: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("items = %d, want 1", len(items))
	}
}

// TestRedactSecretCoversEveryForm pins the redaction helper itself.
func TestRedactSecretCoversEveryForm(t *testing.T) {
	cases := []struct {
		name, in string
	}{
		{"raw pair", `Get "https://api.pinboard.in/v1/posts/all?auth_token=` + testPinboardToken + `": dial tcp`},
		{"escaped pair", "https://x/?auth_token=oliver%3ADEADBEEFCAFE1234"},
		{"bare secret", "token was " + testPinboardSecret + " somewhere"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertNoTokenLeak(t, tc.name, redactSecret(tc.in, testPinboardToken))
		})
	}

	// The username half is NOT a secret and must survive, or short usernames
	// would mangle unrelated text.
	if got := redactSecret("user oliver hit a wall", testPinboardToken); got != "user oliver hit a wall" {
		t.Errorf("username should not be redacted, got %q", got)
	}
	// An empty token must be a no-op, not a replace-everything.
	if got := redactSecret("nothing to hide", ""); got != "nothing to hide" {
		t.Errorf("empty token should be a no-op, got %q", got)
	}
}

// TestFetchErrorsPropagateThroughPullSource proves the sanitized message
// survives the full path a cron run takes: Fetch → pullSource → the error the
// CLI logs and hands to writeRunRecord.
func TestFetchErrorsPropagateThroughPullSource(t *testing.T) {
	t.Setenv("SCRIBE_PINBOARD_TOKEN", testPinboardToken)
	root := testKB(t, "integrations:\n  pinboard:\n    enabled: true\n    scope: all\n")

	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL + "/"
	srv.Close()

	n, err := pullSource(root, pinboardSource{baseURL: base}, FetchOpts{}, 0, false)
	if err == nil {
		t.Fatal("expected the transport failure to surface")
	}
	if n != 0 {
		t.Errorf("queued %d on a failed fetch, want 0", n)
	}
	assertNoTokenLeak(t, "pullSource error", err.Error())
	// And the run-record redaction is the second line of defense.
	assertNoTokenLeak(t, "run-record error", redactURLToken(err.Error()))
}

// TestRedactURLTokenMatchesPrefixedParams pins the main.go regex fix. Before
// it, `auth_token=` matched nothing: `auth` needs `=` right after it and
// `token` needs `?`/`&` right before it.
func TestRedactURLTokenMatchesPrefixedParams(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://x/?auth_token=oliver:HEX", "https://x/?auth_token=REDACTED"},
		{"https://x/?format=json&auth_token=oliver:HEX", "https://x/?format=json&auth_token=REDACTED"},
		{"https://x/?access_token=abc", "https://x/?access_token=REDACTED"},
		{"https://x/?api_key=abc", "https://x/?api_key=REDACTED"},
		{"https://x/?private_token=abc", "https://x/?private_token=REDACTED"},
		{"https://x/?token=abc", "https://x/?token=REDACTED"},
		// The value stops at a quote so the rest of a url.Error stays readable.
		{`Get "https://x/?auth_token=oliver:HEX": dial tcp`, `Get "https://x/?auth_token=REDACTED": dial tcp`},
		// Non-secret params must be left alone.
		{"https://x/?format=json&count=100", "https://x/?format=json&count=100"},
	}
	for _, tc := range cases {
		if got := redactURLToken(tc.in); got != tc.want {
			t.Errorf("redactURLToken(%q)\n  got  %q\n  want %q", tc.in, got, tc.want)
		}
	}
}

// TestWriteRunRecordRedactsErrorToken drives the real writeRunRecord and reads
// the JSONL back: this is the file `scribe doctor --section errors` prints.
func TestWriteRunRecordRedactsErrorToken(t *testing.T) {
	root := testKB(t, "")
	t.Setenv("SCRIBE_KB", root)

	leaky := errors.New(`pinboard fetch: Get "https://api.pinboard.in/v1/posts/all?format=json&auth_token=` +
		testPinboardToken + `": dial tcp: no such host`)
	writeRunRecord("pull", time.Now(), leaky)

	data := readSingleRunRecord(t, root)
	var rec struct {
		Command string `json:"command"`
		Status  string `json:"status"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &rec); err != nil {
		t.Fatalf("run record is not valid JSON: %v\n%s", err, data)
	}
	if rec.Command != "pull" || rec.Status != "error" {
		t.Errorf("unexpected record: %+v", rec)
	}
	assertNoTokenLeak(t, "run record error field", rec.Error)
	assertNoTokenLeak(t, "run record raw line", data)
}
