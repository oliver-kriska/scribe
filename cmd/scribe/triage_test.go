package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestMessageLimitClause documents the two independent message-count
// filters (--message-limit and --min-messages) and how they compose.
// Both together is how tier-2 runs isolate "large but bounded" sessions.
func TestMessageLimitClause(t *testing.T) {
	cases := []struct {
		name string
		cmd  TriageCmd
		want string
	}{
		{"zero", TriageCmd{}, ""},
		{"upper only", TriageCmd{MessageLimit: 300}, "AND s.message_count <= 300"},
		{"lower only", TriageCmd{MinMessages: 50}, "AND s.message_count >= 50"},
		{"both", TriageCmd{MessageLimit: 300, MinMessages: 50}, "AND s.message_count <= 300 AND s.message_count >= 50"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cmd.messageLimitClause(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOrderClause covers the --sort flag. Default (score) gates the
// top-N mining behavior; date is used when curating recent work.
func TestOrderClause(t *testing.T) {
	cases := []struct {
		sort string
		want string
	}{
		{"", "total_score DESC"},
		{"score", "total_score DESC"},
		{"date", "s.updated_at DESC"},
		{"anything-else", "total_score DESC"}, // fallback is score
	}
	for _, tc := range cases {
		t.Run(tc.sort, func(t *testing.T) {
			cmd := TriageCmd{Sort: tc.sort}
			if got := cmd.orderClause(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuildExcludeClause is important — this runs against the ccrider
// DB which is user-writable, so injection hardening matters. The
// sanitizer keeps only [a-zA-Z0-9_-]. Anything else gets dropped.
func TestBuildExcludeClause(t *testing.T) {
	t.Run("empty list", func(t *testing.T) {
		if got := buildExcludeClause(nil); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("clean ids", func(t *testing.T) {
		got := buildExcludeClause([]string{"abc-123", "def_456"})
		want := "AND s.session_id NOT IN ('abc-123','def_456')"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("sql injection stripped", func(t *testing.T) {
		// A malicious ID with semicolons, quotes, and spaces must have
		// those stripped so it lands inside a single SQL string literal
		// with no way to break out. The remaining alnum/dash/underscore
		// characters are kept as-is — they're harmless inside quotes.
		got := buildExcludeClause([]string{"abc'; DROP TABLE messages;--"})
		// No stray quotes besides the two wrapping the ID.
		if strings.Count(got, "'") != 2 {
			t.Errorf("extra quotes (injection leaked): %q", got)
		}
		// No semicolons, spaces, or parens inside the quoted value.
		quoted := strings.TrimPrefix(got, "AND s.session_id NOT IN (")
		quoted = strings.TrimSuffix(quoted, ")")
		if strings.ContainsAny(quoted[1:len(quoted)-1], "'; ") {
			t.Errorf("stray dangerous chars: %q", quoted)
		}
	})

	t.Run("unicode stripped", func(t *testing.T) {
		got := buildExcludeClause([]string{"abcčíž"})
		if !strings.Contains(got, "'abc'") {
			t.Errorf("expected unicode stripped, got %q", got)
		}
	})
}

// TestBuildProjectClause covers the --project filter's sanitizer. The
// allowed charset is broader (adds / and .) because project paths have
// those; everything else is still stripped.
func TestBuildProjectClause(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"simple", "acme", "AND s.project_path LIKE '%acme%'"},
		{"slashes ok", "work/acme", "AND s.project_path LIKE '%work/acme%'"},
		{"dots ok", "example.com", "AND s.project_path LIKE '%example.com%'"},
		{"quote stripped", "foo'; bar", "AND s.project_path LIKE '%foobar%'"}, // space, quote, ; all stripped
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildProjectClause(tc.in)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLoadProcessedSessionIDs reads the processed map from
// _sessions_log.json. This is the exclude list for triage — a bug here
// makes every session look unprocessed and sync re-extracts the world.
func TestLoadProcessedSessionIDs(t *testing.T) {
	t.Run("missing file returns nil", func(t *testing.T) {
		if got := loadProcessedSessionIDs("/nonexistent/path.json"); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("valid file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "_sessions_log.json")
		content := `{"processed": {"session-a": {"extracted": "2026-04-10"}, "session-b": true}}`
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		got := loadProcessedSessionIDs(path)
		sort.Strings(got)
		if len(got) != 2 || got[0] != "session-a" || got[1] != "session-b" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("malformed json returns nil", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bad.json")
		_ = os.WriteFile(path, []byte("{not json"), 0o644)
		if got := loadProcessedSessionIDs(path); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("empty processed map", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "empty.json")
		_ = os.WriteFile(path, []byte(`{"processed": {}}`), 0o644)
		got := loadProcessedSessionIDs(path)
		if len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})
}
