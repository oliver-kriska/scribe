package main

import (
	"fmt"
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

// TestBuildKBExcludeClause covers the session-side guard that keeps work
// done inside the KB out of the mining pipeline (the session twin of the
// KB-extracts-itself loop).
func TestBuildKBExcludeClause(t *testing.T) {
	t.Run("empty root yields no clause", func(t *testing.T) {
		if got := buildKBExcludeClause(""); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("excludes root and nested cwds", func(t *testing.T) {
		got := buildKBExcludeClause("/Users/x/kb")
		want := "AND s.project_path != '/Users/x/kb' AND substr(s.project_path, 1, 12) != '/Users/x/kb/'"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("single quotes in path are escaped", func(t *testing.T) {
		got := buildKBExcludeClause("/Users/o'brien/kb")
		// Each literal must double its single quote so the path can't
		// break out of the SQL string. Two literals → four quote-chars
		// from escaping plus the four wrapping quotes = even, balanced.
		if strings.Contains(got, "o'brien") {
			t.Errorf("single quote not escaped: %q", got)
		}
		if !strings.Contains(got, "o''brien") {
			t.Errorf("expected doubled quote, got %q", got)
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

// TestScoreText covers the pure C3 triage scorer. It must agree with
// the resolved default keyword/weight config and be a presence-
// weighted gate (one weight per category that has any hit), case-
// insensitive, quoted-phrase aware.
func TestScoreText(t *testing.T) {
	kw, w := (TriageConfig{}).Resolve()

	t.Run("empty text scores zero", func(t *testing.T) {
		if s := scoreText(kw, w, "   \n\t "); s != 0 {
			t.Errorf("empty text = %d, want 0", s)
		}
	})

	t.Run("no keywords scores zero", func(t *testing.T) {
		if s := scoreText(kw, w, "the quick brown fox jumped over the lazy dog"); s != 0 {
			t.Errorf("keyword-free text = %d, want 0", s)
		}
	})

	t.Run("single category adds its weight once", func(t *testing.T) {
		// "decided" → decision (weight 3). Repeated mentions must not
		// multiply — it's a presence gate.
		got := scoreText(kw, w, "We decided X. Then we decided Y. We decided again.")
		if got != w["decision"] {
			t.Errorf("got %d, want %d (decision weight, counted once)", got, w["decision"])
		}
	})

	t.Run("case-insensitive", func(t *testing.T) {
		if scoreText(kw, w, "we DECIDED to ship") != w["decision"] {
			t.Error("scorer must be case-insensitive")
		}
	})

	t.Run("quoted phrase keyword matches", func(t *testing.T) {
		// architecture category includes the quoted phrase
		// "design pattern".
		if scoreText(kw, w, "this introduces a new design pattern") < w["architecture"] {
			t.Error("quoted-phrase keyword should match")
		}
	})

	t.Run("multiple categories sum", func(t *testing.T) {
		// decision (decided=3) + research (benchmark=3) + code_pattern
		// (GenServer=1) = 7.
		text := "We decided to run a benchmark on the GenServer hot path."
		want := w["decision"] + w["research"] + w["code_pattern"]
		if got := scoreText(kw, w, text); got != want {
			t.Errorf("got %d, want %d", got, want)
		}
	})
}

func TestTriageKeywordTerms(t *testing.T) {
	got := triageKeywordTerms(`architecture OR "design pattern" OR strategy`)
	want := []string{"architecture", "design pattern", "strategy"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("term[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if triageKeywordTerms("   ") != nil {
		t.Error("blank keyword string should yield nil")
	}
}

// The starvation regression. A project can be approved in the manifest and
// still be undrainable by the miner: isIgnored wants >=4 non-empty path
// segments, so every repo sitting directly under a mount root
// (/Volumes/CaseSensitive/dabrain — 3) is dropped as too shallow. Those
// drops are silent and leave the session UNMARKED, so it keeps its rank.
//
// Before --in-scope the miner asked triage for SessionsMax*3 rows and
// filtered afterwards, so a cluster of shallow-path sessions ranked at the
// top consumed the whole admission window every run. Observed live: 10 of
// the top 11, `no normal sessions to mine`, and a 72-session backlog that
// never moved. keepInScope drops them while picking instead.
func TestKeepInScope_BlockersDoNotConsumeAdmissionSlots(t *testing.T) {
	root := sessionsTestKB(t, "")
	deep := "/home/dev/projects/alpha"  // 4 segments — admissible
	shallow := "/Volumes/CaseSensitive" // 2 segments — always too shallow

	manifest := `{"projects": {"alpha": {"path": ` + jsonQuote(deep) + `, "status": "approved"}}}`
	writeKBFile(t, root, "scripts/projects.json", manifest)
	cfg := loadConfig(root)

	// Ten blockers outranking three admissible sessions, mirroring the
	// live ordering (blockers score high: they are long, dense sessions).
	results := make([]triageResult, 0, 13)
	for i := range 10 {
		results = append(results, triageResult{
			SessionID: fmt.Sprintf("blocker-%d", i), Score: 100 - i, rawPath: shallow,
		})
	}
	for i := range 3 {
		results = append(results, triageResult{
			SessionID: fmt.Sprintf("good-%d", i), Score: 50 - i, rawPath: deep,
		})
	}

	t.Run("in-scope drops blockers and fills the window", func(t *testing.T) {
		cmd := &TriageCmd{Top: 3, InScope: true}
		got := cmd.keepInScope(root, cfg, results)
		if len(got) != 3 {
			t.Fatalf("got %d rows, want 3 — blockers still consuming slots", len(got))
		}
		for _, r := range got {
			if r.rawPath != deep {
				t.Errorf("admitted %s from %s, want only %s", r.SessionID, r.rawPath, deep)
			}
		}
	})

	t.Run("without the flag nothing is filtered", func(t *testing.T) {
		cmd := &TriageCmd{Top: 3, InScope: false}
		got := cmd.keepInScope(root, cfg, results)
		if len(got) != len(results) {
			t.Errorf("got %d rows, want %d — plain `scribe triage` must still show blockers", len(got), len(results))
		}
	})

	t.Run("trims to Top even when everything is admissible", func(t *testing.T) {
		onlyGood := results[10:]
		cmd := &TriageCmd{Top: 2, InScope: true}
		if got := cmd.keepInScope(root, cfg, onlyGood); len(got) != 2 {
			t.Errorf("got %d rows, want 2", len(got))
		}
	})
}

// scanLimit must over-fetch under --in-scope (the predicate is Go-side, so
// blockers have to be read before they can be dropped) and must not change
// the row count a plain `scribe triage --top N` reads.
func TestScanLimit_OverfetchesOnlyForInScope(t *testing.T) {
	if got := (&TriageCmd{Top: 9}).scanLimit(); got != 9 {
		t.Errorf("plain triage scanLimit = %d, want 9 (unchanged)", got)
	}
	if got := (&TriageCmd{Top: 9, InScope: true}).scanLimit(); got != 9*inScopeOverfetch {
		t.Errorf("in-scope scanLimit = %d, want %d", got, 9*inScopeOverfetch)
	}
	// The cap bounds the OVER-FETCH, not the request: at Top=200 the 20x
	// would be 4000, so the cap applies.
	if got := (&TriageCmd{Top: 200, InScope: true}).scanLimit(); got != inScopeOverfetchCap {
		t.Errorf("capped scanLimit = %d, want %d", got, inScopeOverfetchCap)
	}
	// But --all sets Top to 99999, and scanning fewer rows than Top would
	// silently truncate what was explicitly asked for — the floor wins.
	if got := (&TriageCmd{Top: 99999, InScope: true}).scanLimit(); got != 99999 {
		t.Errorf("--all scanLimit = %d, want 99999 (floor beats cap)", got)
	}
}

// sessionDropReason is shared by keepInScope and preFilterSessions; if they
// disagree the window starves again. Pins each reason, including the
// approved-but-too-shallow case that caused the live deadlock.
func TestSessionDropReason(t *testing.T) {
	root := sessionsTestKB(t, "")
	deep := "/home/dev/projects/alpha"
	pendingProj := "/home/dev/projects/beta"
	manifest := `{"projects": {` +
		`"alpha": {"path": ` + jsonQuote(deep) + `, "status": "approved"},` +
		`"beta": {"path": ` + jsonQuote(pendingProj) + `, "status": "pending"}}}`
	writeKBFile(t, root, "scripts/projects.json", manifest)
	cfg := loadConfig(root)
	m, err := loadManifest(root)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}

	cases := []struct{ name, path, want string }{
		{"approved and deep enough", deep, ""},
		{"approved but too shallow", "/Volumes/CaseSensitive", dropReasonScope},
		{"repo directly under a mount root", "/Volumes/CaseSensitive/dabrain", dropReasonScope},
		{"session run inside the KB", root, dropReasonKB},
		{"project not approved yet", pendingProj, dropReasonPending},
		{"no provenance recorded", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sessionDropReason(cfg, m, root, c.path); got != c.want {
				t.Errorf("sessionDropReason(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}
