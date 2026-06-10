package main

import (
	"strings"
	"testing"
)

// Fixture tokens are assembled at runtime (Claude Code's join-trick)
// so no full credential-shaped literal ever sits in this repo — that
// would trip GitHub push protection and scribe's own scanner.
func fakeAWSKey() string      { return "AKIA" + "ABCDEFGHIJKLMNOP" }
func fakeGitHubToken() string { return "ghp" + "_" + strings.Repeat("a1b2", 9) }
func fakeAnthropicKey() string {
	return strings.Join([]string{"sk", "ant", "api03"}, "-") + "-" + strings.Repeat("a", 93) + "AA"
}

func fakeJWT() string {
	return "ey" + strings.Repeat("J1abc", 4) + "." + "ey" + strings.Repeat("K2def", 4) + "." + strings.Repeat("L3", 6)
}

func scanLine(t *testing.T, line string, generic bool) []secretHit {
	t.Helper()
	return scanContentForSecrets([]byte(line+"\n"), generic)
}

func TestScanDetectsKnownTokenShapes(t *testing.T) {
	tests := []struct {
		name string
		line string
		rule string
	}{
		{"aws key id", "the key was " + fakeAWSKey() + " in the env", "aws-access-key-id"},
		{"github token", "export GH_TOKEN=" + fakeGitHubToken(), "github-token"},
		{"anthropic key", "set it to " + fakeAnthropicKey() + " and run", "anthropic-api-key"},
		{"pem header", "-----BEGIN RSA PRIVATE KEY-----", "private-key-pem"},
		{"jwt", "Authorization: Bearer " + fakeJWT(), "jwt"},
		{"url password", "conn: postgres://admin:s3cretPa55@db.internal:5432/app", "url-userinfo-password"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hits := scanLine(t, tt.line, false)
			if len(hits) == 0 {
				t.Fatalf("no hit for %s", tt.name)
			}
			found := false
			for _, h := range hits {
				if h.RuleID == tt.rule {
					found = true
					if h.Line != 1 {
						t.Errorf("line = %d, want 1", h.Line)
					}
				}
			}
			if !found {
				t.Errorf("rule %s did not fire; got %+v", tt.rule, hits)
			}
		})
	}
}

func TestScanIgnoresPlaceholdersAndAllows(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{"canonical AWS doc key", "use AKIAIOSFODNN7EXAMPLE in docs"},
		{"xxx password in url", "postgres://user:xxxx@localhost/db"},
		{"env var in url", "https://user:${DB_PASS}@host.example.com"},
		{"angle placeholder", "https://api:<your-token-here>@example.com"},
		{"scribe allow marker", "real-looking " + fakeAWSKey() + " <!-- scribe:allow -->"},
		{"gitleaks allow marker", "real-looking " + fakeAWSKey() + " # gitleaks:allow"},
		{"prose without tokens", "Set api_key in scribe.yaml before the first sync run."},
		{"git sha is not a secret", "fixed in commit 3f2a9c81d4e7b6a05c9f1e8d2b7a4c6e9f0d3b5a"},
		{"setext heading", "Heading\n======="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if hits := scanLine(t, tt.line, false); len(hits) != 0 {
				t.Errorf("false positive %+v on %q", hits, tt.name)
			}
		})
	}
}

func TestScanDedupesPerRule(t *testing.T) {
	content := "first " + fakeAWSKey() + "\nsecond " + "AKIA" + "QRSTUVWXYZ234567" + "\n"
	hits := scanContentForSecrets([]byte(content), false)
	if len(hits) != 1 {
		t.Errorf("got %d hits, want 1 (dedupe per rule): %+v", len(hits), hits)
	}
}

func TestGenericRuleIsOptIn(t *testing.T) {
	line := `api_key = "x9K2mP8qL5nR3vT7wY1zB6cD4"`
	if hits := scanLine(t, line, false); len(hits) != 0 {
		t.Errorf("generic rule fired while disabled: %+v", hits)
	}
	hits := scanLine(t, line, true)
	if len(hits) != 1 || hits[0].RuleID != "generic-credential-assignment" {
		t.Errorf("generic rule should fire when enabled: %+v", hits)
	}
	// All-letters value: never a machine secret.
	if hits := scanLine(t, "password = secretvaluewithoutanydigits", true); len(hits) != 0 {
		t.Errorf("no-digit value fired: %+v", hits)
	}
}

func TestShannonEntropy(t *testing.T) {
	if e := shannonEntropy("aaaa"); e != 0 {
		t.Errorf("entropy(aaaa) = %f, want 0", e)
	}
	if e := shannonEntropy("abcd"); e != 2 {
		t.Errorf("entropy(abcd) = %f, want 2", e)
	}
	if e := shannonEntropy(""); e != 0 {
		t.Errorf("entropy(empty) = %f, want 0", e)
	}
}

func TestHoldSecretFiles(t *testing.T) {
	setup := func(t *testing.T) string {
		t.Helper()
		repo := initTestGitRepo(t, "Gate Tester")
		writeKBFile(t, repo, "wiki/leaky.md", "---\ntitle: Leaky\n---\n\nkey: "+fakeAWSKey()+"\n")
		writeKBFile(t, repo, "wiki/clean.md", "---\ntitle: Clean\n---\n\nnothing here\n")
		writeKBFile(t, repo, "raw/articles/absorbed.md", "# Absorbed page\n\ntoken: "+fakeGitHubToken()+"\n")
		gitRun(t, repo, "add", "wiki", "raw")
		return repo
	}
	stagedSet := func(repo string) map[string]bool {
		out := map[string]bool{}
		for _, f := range stagedKBMarkdown(repo) {
			out[f] = true
		}
		return out
	}

	// Team mode: leaky wiki + raw files held, clean stays.
	repo := setup(t)
	holdSecretFiles(repo, &ScribeConfig{Team: true})
	staged := stagedSet(repo)
	if staged["wiki/leaky.md"] {
		t.Error("leaky file still staged in team mode")
	}
	if staged["raw/articles/absorbed.md"] {
		t.Error("leaky raw/ file still staged in team mode")
	}
	if !staged["wiki/clean.md"] {
		t.Error("clean file was unstaged")
	}

	// Solo KB: gate inactive.
	repo = setup(t)
	holdSecretFiles(repo, &ScribeConfig{})
	if !stagedSet(repo)["wiki/leaky.md"] {
		t.Error("solo KB should not hold files")
	}

	// Explicitly disabled.
	repo = setup(t)
	holdSecretFiles(repo, &ScribeConfig{Team: true, SecretScan: SecretScanConfig{Disable: true}})
	if !stagedSet(repo)["wiki/leaky.md"] {
		t.Error("disabled gate should not hold files")
	}

	// allow_paths exemption.
	repo = setup(t)
	holdSecretFiles(repo, &ScribeConfig{Team: true, SecretScan: SecretScanConfig{AllowPaths: []string{"wiki"}}})
	if !stagedSet(repo)["wiki/leaky.md"] {
		t.Error("allow_paths exemption ignored")
	}
}

func TestDoctorWarnsOnSecretsTeamOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	root := t.TempDir()
	token := fakeAWSKey()
	writeKBFile(t, root, "wiki/leaky.md", "---\ntitle: Leaky\n---\n\nkey: "+token+"\n")

	// Solo KB: no secrets check at all.
	for _, c := range checkState(root) {
		if c.Name == "secrets-in-articles" {
			t.Fatalf("solo KB got secrets check: %+v", c)
		}
	}

	// Team KB: WARN, naming rule + location but NEVER the value.
	writeKBFile(t, root, "scribe.yaml", "team: true\n")
	var found *check
	for _, c := range checkState(root) {
		if c.Name == "secrets-in-articles" {
			found = &c
			break
		}
	}
	if found == nil {
		t.Fatal("team KB missing secrets WARN")
	}
	if found.Status != statusWarn {
		t.Errorf("status = %q, want WARN", found.Status)
	}
	if !strings.Contains(found.Detail, "wiki/leaky.md:5 [AWS Access Key ID]") {
		t.Errorf("detail %q missing file:line [label]", found.Detail)
	}
	if strings.Contains(found.Detail, token) {
		t.Fatalf("detail leaked the secret value: %q", found.Detail)
	}
}
