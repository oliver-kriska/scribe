package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// insertFixtureToolMessage inserts an assistant-type messages row with
// content and sequence set (the columns newCcriderDB's fixture didn't
// previously exercise — see adoption plan Step 1). msgType is almost
// always "assistant" in real ccrider data — see adoption.go's file-level
// comment for why "tool"/"tool_use" never appears as a messages.type
// value.
func insertFixtureToolMessage(t *testing.T, db *sql.DB, sessionRowID int64, msgType, content string, sequence int) {
	t.Helper()
	//nolint:noctx // test fixture
	_, err := db.Exec(`INSERT INTO messages (session_id, type, content, sequence) VALUES (?, ?, ?, ?)`,
		sessionRowID, msgType, content, sequence)
	if err != nil {
		t.Fatal(err)
	}
}

// Realistic compact-JSON content fixtures, mirroring the exact shape
// verified against ccrider's real DB (adoption.go file comment point 4):
// no space after "type"/"name" colons.
const (
	qmdMCPContent    = `{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"mcp__plugin_qmd_qmd__query","input":{"searches":[{"type":"lex","query":"auth flow"}]}}]}`
	qmdAliasContent  = `{"role":"assistant","content":[{"type":"tool_use","id":"toolu_2","name":"mcp__qmd__query","input":{}}]}`
	qmdBashContent   = `{"role":"assistant","content":[{"type":"text","text":"Checking the KB first."},{"type":"tool_use","id":"toolu_3","name":"Bash","input":{"command":"qmd query \"auth flow\""}}]}`
	editContent      = `{"role":"assistant","content":[{"type":"tool_use","id":"toolu_4","name":"Edit","input":{"file_path":"/x/y.go"}}]}`
	writeContent     = `{"role":"assistant","content":[{"type":"tool_use","id":"toolu_5","name":"Write","input":{"file_path":"/x/z.go"}}]}`
	multiEditContent = `{"role":"assistant","content":[{"type":"tool_use","id":"toolu_6","name":"MultiEdit","input":{"file_path":"/x/w.go"}}]}`
	grepContent      = `{"role":"assistant","content":[{"type":"tool_use","id":"toolu_7","name":"Grep","input":{"pattern":"foo"}}]}`
	readContent      = `{"role":"assistant","content":[{"type":"tool_use","id":"toolu_8","name":"Read","input":{"file_path":"/x/y.go"}}]}`
)

func TestClassifyToolContent(t *testing.T) {
	tests := []struct {
		name         string
		content      string
		textContent  string
		wantQMD      bool
		wantDecision bool
	}{
		{"qmd MCP tool use", qmdMCPContent, "", true, false},
		{"qmd MCP under a different local alias", qmdAliasContent, "", true, false},
		{"Bash qmd CLI form", qmdBashContent, "", true, false},
		{"Edit", editContent, "", false, true},
		{"Write", writeContent, "", false, true},
		{"MultiEdit", multiEditContent, "", false, true},
		{"unrelated tool Grep", grepContent, "", false, false},
		{"unrelated tool Read", readContent, "", false, false},
		{"empty content falls back to text_content", "", `mentions "name":"Edit" here`, false, true},
		{"both empty — no panic", "", "", false, false},
		// Documents the D2 false-positive risk: a tool_result payload
		// whose echoed output text happens to contain a decision marker
		// still gets flagged by this function — proving why the SQL
		// type='assistant' filter (not this function) is what keeps
		// tool_result rows out of the scan in the first place (see
		// adoption.go's file comment point 3: tool_result blocks only
		// ever live inside type='user' rows in real ccrider data).
		{
			// Note: a raw (backtick) Go string literal passes `"` through
			// unescaped — no backslashes needed to embed the literal
			// `"name":"Edit"` marker substring here.
			"tool_result echo contains a decision marker substring",
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_4","content":[{"type":"text","text":"grep match: "name":"Edit""}]}]}`,
			"",
			false, true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotQMD, gotDecision := classifyToolContent(tt.content, tt.textContent)
			if gotQMD != tt.wantQMD {
				t.Errorf("isQMD = %v, want %v", gotQMD, tt.wantQMD)
			}
			if gotDecision != tt.wantDecision {
				t.Errorf("isDecision = %v, want %v", gotDecision, tt.wantDecision)
			}
		})
	}
}

// sqliteNow formats a time.Time the way ccrider's `datetime('now', ...)`
// comparisons expect (SQLite's default TEXT datetime format), mirroring
// TestWatchScan_MultiKBQueuing's convention in watch_multikb_test.go.
func sqliteNow(daysAgo int) string {
	return time.Now().UTC().AddDate(0, 0, -daysAgo).Format("2006-01-02 15:04:05")
}

func TestScopedClaudeSessionIDs(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	root := t.TempDir()

	approved := filepath.Join(t.TempDir(), "Projects", "approved")
	pending := filepath.Join(t.TempDir(), "Projects", "pending")

	writeStatusManifest(t, root, map[string]map[string]string{
		"approved": {"path": approved, "domain": "general"},
		"pending":  {"path": pending, "domain": "general", "status": statusPending},
	})

	sApproved := insertFixtureSession(t, db, "s-approved", approved, 10, "", sqliteNow(1), "in approved")
	insertFixtureSession(t, db, "s-pending", pending, 10, "", sqliteNow(1), "in pending project")
	insertFixtureSession(t, db, "s-unknown", "/somewhere/else", 10, "", sqliteNow(1), "no manifest entry")
	insertFixtureSession(t, db, "s-in-kb", root, 10, "", sqliteNow(1), "session about the KB itself")
	sOld := insertFixtureSession(t, db, "s-old", approved, 10, "", sqliteNow(10), "outside the 7d window")

	// A codex session in the SAME approved project, otherwise identical —
	// must be excluded (v1 is Claude-only scope).
	insertFixtureSession(t, db, "s-codex", approved, 10, "", sqliteNow(1), "codex, same project")
	setSessionProvider(t, db, "s-codex", "codex")

	openDB, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer openDB.Close()

	t.Run("7 day window", func(t *testing.T) {
		ids, err := scopedClaudeSessionIDs(openDB, root, 7)
		if err != nil {
			t.Fatal(err)
		}
		if !containsID(ids, sApproved) {
			t.Errorf("expected approved-project session in scope, got %v", ids)
		}
		if containsID(ids, sOld) {
			t.Errorf("expected session older than the 7d window excluded, got %v", ids)
		}
		if len(ids) != 1 {
			t.Errorf("scoped ids = %v, want exactly [%d] (pending/unknown/in-KB/codex all excluded)", ids, sApproved)
		}
	})

	t.Run("30 day window includes the older session", func(t *testing.T) {
		ids, err := scopedClaudeSessionIDs(openDB, root, 30)
		if err != nil {
			t.Fatal(err)
		}
		if !containsID(ids, sOld) {
			t.Errorf("expected session inside the 30d window included, got %v", ids)
		}
	})
}

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestComputeAdoptionWindow(t *testing.T) {
	db, dbPath := newCcriderDB(t)
	root := t.TempDir()
	proj := filepath.Join(t.TempDir(), "Projects", "acme")
	writeStatusManifest(t, root, map[string]map[string]string{
		"acme": {"path": proj, "domain": "general"},
	})

	sKBFirst := insertFixtureSession(t, db, "kb-first", proj, 10, "", sqliteNow(1), "queried then edited")
	insertFixtureToolMessage(t, db, sKBFirst, "assistant", qmdMCPContent, 2)
	insertFixtureToolMessage(t, db, sKBFirst, "assistant", editContent, 5)

	sEditFirst := insertFixtureSession(t, db, "edit-first", proj, 10, "", sqliteNow(1), "edited then queried")
	insertFixtureToolMessage(t, db, sEditFirst, "assistant", editContent, 2)
	insertFixtureToolMessage(t, db, sEditFirst, "assistant", qmdMCPContent, 5)

	sOnlyQMD := insertFixtureSession(t, db, "only-qmd", proj, 10, "", sqliteNow(1), "researched, never decided")
	insertFixtureToolMessage(t, db, sOnlyQMD, "assistant", qmdMCPContent, 2)

	sOnlyEdit := insertFixtureSession(t, db, "only-edit", proj, 10, "", sqliteNow(1), "no KB query at all")
	insertFixtureToolMessage(t, db, sOnlyEdit, "assistant", editContent, 2)

	// Scoped session with zero tool-type rows at all.
	insertFixtureSession(t, db, "no-tool-rows", proj, 10, "", sqliteNow(1), "zero tool-type rows")

	sBeyondCap := insertFixtureSession(t, db, "beyond-cap", proj, 10, "", sqliteNow(1), "edit past the scan cap")
	insertFixtureToolMessage(t, db, sBeyondCap, "assistant", qmdMCPContent, 1)
	insertFixtureToolMessage(t, db, sBeyondCap, "assistant", editContent, 301)

	sTie := insertFixtureSession(t, db, "tied-sequence", proj, 10, "", sqliteNow(1), "qmd and edit share a sequence number")
	insertFixtureToolMessage(t, db, sTie, "assistant", qmdMCPContent, 3) // inserted (and thus id-ordered) first
	insertFixtureToolMessage(t, db, sTie, "assistant", editContent, 3)

	openDB, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer openDB.Close()

	result, err := computeAdoptionWindow(openDB, root, 7)
	if err != nil {
		t.Fatal(err)
	}

	// beyond-cap's Edit lands past adoptionSequenceCap (300), so it does
	// NOT count as a decision session — the accepted D4 limitation.
	wantDecisionSessions := 4 // kb-first, edit-first, only-edit, tied-sequence
	if result.DecisionSessions != wantDecisionSessions {
		t.Errorf("DecisionSessions = %d, want %d", result.DecisionSessions, wantDecisionSessions)
	}
	wantKBFirst := 2 // kb-first, tied-sequence (qmd row inserted/id-ordered before the tied Edit row)
	if result.KBFirstSessions != wantKBFirst {
		t.Errorf("KBFirstSessions = %d, want %d", result.KBFirstSessions, wantKBFirst)
	}
	// Excluded-no-decision: only-qmd, no-tool-rows, beyond-cap.
	wantExcluded := 3
	if result.ExcludedNoDecision != wantExcluded {
		t.Errorf("ExcludedNoDecision = %d, want %d", result.ExcludedNoDecision, wantExcluded)
	}

	if got := (adoptionWindowResult{}).Ratio(); got != 0 {
		t.Errorf("Ratio() on zero DecisionSessions = %v, want 0 (no divide-by-zero panic)", got)
	}
}

func TestComputeAdoptionMetrics_Degrades(t *testing.T) {
	root := t.TempDir()
	cfg := loadConfig(root)

	t.Run("empty CcriderDB", func(t *testing.T) {
		cfg.CcriderDB = ""
		results, err := computeAdoptionMetrics(root, cfg)
		if err != nil {
			t.Errorf("err = %v, want nil", err)
		}
		if results != nil {
			t.Errorf("results = %v, want nil", results)
		}
	})

	t.Run("nonexistent CcriderDB path", func(t *testing.T) {
		cfg.CcriderDB = filepath.Join(t.TempDir(), "does-not-exist.db")
		results, err := computeAdoptionMetrics(root, cfg)
		if err != nil {
			t.Errorf("err = %v, want nil", err)
		}
		if results != nil {
			t.Errorf("results = %v, want nil", results)
		}
	})
}

func TestAdoptionRunStatsFields(t *testing.T) {
	results := []adoptionWindowResult{
		{Days: 7, DecisionSessions: 4, KBFirstSessions: 3},
		{Days: 30, DecisionSessions: 10, KBFirstSessions: 5},
	}
	fields := adoptionRunStatsFields(results)
	if fields["adoption_kb_first_7d_num"] != 3 || fields["adoption_kb_first_7d_den"] != 4 {
		t.Errorf("7d fields = %v", fields)
	}
	if fields["adoption_kb_first_30d_ratio"] != 0.5 {
		t.Errorf("30d ratio = %v, want 0.5", fields["adoption_kb_first_30d_ratio"])
	}
}
