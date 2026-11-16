package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeTestUserConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "scribe"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scribe", "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeCommandEnv(t *testing.T) {
	const tokenVar = "CLAUDE_CODE_OAUTH_TOKEN"

	t.Run("no token anywhere inherits the environment", func(t *testing.T) {
		writeTestUserConfig(t, "kb_dir: /tmp/kb\n")
		t.Setenv(tokenVar, "")
		t.Setenv("SCRIBE_CLAUDE_TOKEN", "")
		if env := claudeCommandEnv(); env != nil {
			t.Errorf("want nil (inherit), got %d vars", len(env))
		}
	})

	t.Run("user config token is passed to claude", func(t *testing.T) {
		writeTestUserConfig(t, "integration_tokens:\n  claude: from-config\n")
		t.Setenv(tokenVar, "")
		t.Setenv("SCRIBE_CLAUDE_TOKEN", "")
		if env := claudeCommandEnv(); !slices.Contains(env, tokenVar+"=from-config") {
			t.Errorf("token from user config missing from env")
		}
	})

	t.Run("SCRIBE_CLAUDE_TOKEN overrides the config", func(t *testing.T) {
		writeTestUserConfig(t, "integration_tokens:\n  claude: from-config\n")
		t.Setenv(tokenVar, "")
		t.Setenv("SCRIBE_CLAUDE_TOKEN", "from-env")
		if env := claudeCommandEnv(); !slices.Contains(env, tokenVar+"=from-env") {
			t.Errorf("SCRIBE_CLAUDE_TOKEN did not win")
		}
	})

	t.Run("an exported CLAUDE_CODE_OAUTH_TOKEN wins", func(t *testing.T) {
		writeTestUserConfig(t, "integration_tokens:\n  claude: from-config\n")
		t.Setenv(tokenVar, "already-set")
		if env := claudeCommandEnv(); env != nil {
			t.Errorf("want nil (inherit the exported token), got %d vars", len(env))
		}
	})
}

func TestClaudeFailureNamesTheResult(t *testing.T) {
	authEnvelope := `{"type":"result","subtype":"success","is_error":true,"result":"Failed to authenticate: OAuth session expired and could not be refreshed"}`
	env, ok := parseClaudeResult(authEnvelope)
	if !ok {
		t.Fatal("fixture envelope did not parse")
	}
	err := claudeFailure(errors.New("exit status 1"), env, ok, authEnvelope)
	first := strings.SplitN(err.Error(), "\n", 2)[0]
	if !strings.Contains(first, "OAuth session expired") {
		t.Errorf("first line %q does not name the failure", first)
	}

	multiLine, _ := parseClaudeResult(`{"type":"result","is_error":true,"result":"API Error: 400\n{\"type\":\"error\"}"}`)
	if got := claudeFailure(errors.New("exit status 1"), multiLine, true, "t").Error(); !strings.HasPrefix(got, `claude -p: exit status 1: API Error: 400 {"type":"error"}`+"\n") {
		t.Errorf("multi-line result not flattened onto the first line: %q", got)
	}

	success, _ := parseClaudeResult(`{"type":"result","is_error":false,"result":"Here is the article"}`)
	if got := claudeFailure(errors.New("exit status 1"), success, true, "t").Error(); strings.Contains(strings.SplitN(got, "\n", 2)[0], "Here is the article") {
		t.Errorf("a non-error result was reported as the failure reason: %q", got)
	}

	plain := claudeFailure(errors.New("exit status 1"), claudeResultEnvelope{}, false, "tail")
	if !strings.HasPrefix(plain.Error(), "claude -p: exit status 1\ntail") {
		t.Errorf("no-envelope error changed shape: %q", plain.Error())
	}
}
