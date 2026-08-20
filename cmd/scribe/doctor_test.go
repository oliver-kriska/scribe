package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCheckLocalMode_NoOllamaProvider: when pass2_provider != ollama,
// only the anthropic-ceiling INFO/WARN applies. Ollama checks must
// not run (no network calls in CI).
func TestCheckLocalMode_NoOllamaProvider(t *testing.T) {
	cfg := &ScribeConfig{
		Absorb: AbsorbConfig{Pass2Provider: "anthropic"},
		Sync:   SyncConfig{DailyAnthropicOutputTokenCeiling: 2_000_000},
	}
	out := checkLocalMode(cfg)
	if len(out) != 0 {
		t.Errorf("anthropic provider + ceiling set should produce 0 findings, got %+v", out)
	}
}

// TestCheckLocalMode_NoCeiling: ceiling=0 alone surfaces a warn so
// users know they have no backstop after the 2026-05-11 runaway.
func TestCheckLocalMode_NoCeiling(t *testing.T) {
	cfg := &ScribeConfig{
		Absorb: AbsorbConfig{Pass2Provider: "anthropic"},
		Sync:   SyncConfig{DailyAnthropicOutputTokenCeiling: 0},
	}
	out := checkLocalMode(cfg)
	if len(out) != 1 || out[0].Name != "output_token_ceiling" || out[0].Status != statusWarn {
		t.Errorf("want one warn 'output_token_ceiling', got %+v", out)
	}
}

// TestCheckLocalMode_OllamaPass2WithoutAtomicFacts: when pass-2 is
// routed through ollama but atomic_facts is off, surface a warn — the
// model fabricates [cN-fM] citations without ground-truth fact IDs.
// Skips the ollama network probe via env var so the test runs offline.
func TestCheckLocalMode_OllamaPass2WithoutAtomicFacts(t *testing.T) {
	t.Setenv("SCRIBE_DOCTOR_SKIP_OLLAMA", "1")
	cfg := &ScribeConfig{
		Absorb: AbsorbConfig{
			Pass2Provider: "ollama",
			Pass2Model:    "gemma3:27b",
			AtomicFacts:   nil,
		},
		Sync: SyncConfig{DailyAnthropicOutputTokenCeiling: 2_000_000},
	}
	out := checkLocalMode(cfg)
	gotAtomic := false
	for _, c := range out {
		if c.Name == "atomic_facts_with_ollama" && c.Status == statusWarn {
			gotAtomic = true
		}
	}
	if !gotAtomic {
		t.Errorf("expected atomic_facts_with_ollama warn, got %+v", out)
	}
}

// TestCheckLocalMode_OllamaPass2WellConfigured: ollama provider +
// atomic_facts on + ceiling set should produce zero findings (the
// ollama-daemon and pass2_model_pulled checks are skipped via env).
func TestCheckLocalMode_OllamaPass2WellConfigured(t *testing.T) {
	t.Setenv("SCRIBE_DOCTOR_SKIP_OLLAMA", "1")
	trueV := true
	cfg := &ScribeConfig{
		Absorb: AbsorbConfig{
			Pass2Provider: "ollama",
			Pass2Model:    "gemma3:27b",
			AtomicFacts:   &trueV,
		},
		Sync: SyncConfig{DailyAnthropicOutputTokenCeiling: 2_000_000},
	}
	out := checkLocalMode(cfg)
	for _, c := range out {
		if c.Status == statusWarn || c.Status == statusFail {
			t.Errorf("well-configured KB should produce no warn/fail, got %+v", c)
		}
	}
}

func TestCheckVaultScaffolding_OkWhenClean(t *testing.T) {
	dir := t.TempDir()
	out := checkVaultScaffolding(dir)
	if len(out) != 1 || out[0].Status != statusOK {
		t.Fatalf("clean KB should report 1 ok check; got %+v", out)
	}
}

func TestCheckVaultScaffolding_WarnPerStrayDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "logseq", "bak"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logseq", "config.edn"), []byte(":x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "pages"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pages", "contents.md"), []byte("-"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := checkVaultScaffolding(dir)
	warns := 0
	for _, c := range out {
		if c.Status == statusWarn {
			warns++
		}
	}
	if warns != 2 {
		t.Errorf("expected 2 warns (logseq + pages); got %d (full: %+v)", warns, out)
	}
}

// TestLoadRunRecords covers the critical path — doctor's freshness check is
// only as good as this loader. The three things it must not get wrong:
// (a) picking the newest ok record per command, (b) ignoring error records,
// (c) splitting `sync` vs `sync --sessions` into distinct keys.
func TestLoadRunRecords(t *testing.T) {
	root := t.TempDir()
	runsDir := filepath.Join(root, "output", "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Two JSONL files across two dates. Newest ok lines should win.
	day1 := filepath.Join(runsDir, "2026-04-09.jsonl")
	day2 := filepath.Join(runsDir, "2026-04-10.jsonl")

	day1Content := []string{
		`{"command":"sync","status":"ok","timestamp":"2026-04-09T10:00:00Z","args":["sync"]}`,
		`{"command":"sync","status":"error","timestamp":"2026-04-09T12:00:00Z","args":["sync"]}`,
		`{"command":"lint","status":"ok","timestamp":"2026-04-09T12:30:00Z","args":["lint"]}`,
	}
	day2Content := []string{
		`{"command":"sync","status":"ok","timestamp":"2026-04-10T08:00:00Z","args":["sync","--sessions","--sessions-max","3"]}`,
		`{"command":"sync","status":"ok","timestamp":"2026-04-10T06:00:00Z","args":["sync"]}`,
		`{"command":"dream","status":"error","timestamp":"2026-04-10T02:00:00Z","args":["dream"]}`,
		`garbage line that should be skipped`,
		`{"command":"ingest drain","status":"ok","timestamp":"2026-04-10T09:30:00Z","args":["ingest","drain"]}`,
	}
	if err := os.WriteFile(day1, []byte(joinLines(day1Content)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(day2, []byte(joinLines(day2Content)), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := loadRunRecords(root)
	if err != nil {
		t.Fatalf("loadRunRecords: %v", err)
	}

	// Newest ok `sync` is 2026-04-10T08:00:00Z (the one with --sessions args).
	syncTime := mustTime(t, "2026-04-10T08:00:00Z")
	if !got["sync"].Equal(syncTime) {
		t.Errorf("sync newest: got %v, want %v", got["sync"], syncTime)
	}
	// `sync --sessions` should only see the 08:00 record (the other sync had no --sessions flag).
	if !got["sync --sessions"].Equal(syncTime) {
		t.Errorf("sync --sessions: got %v, want %v", got["sync --sessions"], syncTime)
	}
	// `lint` — only day1 had it.
	lintTime := mustTime(t, "2026-04-09T12:30:00Z")
	if !got["lint"].Equal(lintTime) {
		t.Errorf("lint: got %v, want %v", got["lint"], lintTime)
	}
	// `dream` had only an error — must not appear.
	if _, ok := got["dream"]; ok {
		t.Errorf("dream should not appear (only error records): %v", got["dream"])
	}
	// `ingest drain` — args do not contain any --flag, so only the base key should exist.
	drainTime := mustTime(t, "2026-04-10T09:30:00Z")
	if !got["ingest drain"].Equal(drainTime) {
		t.Errorf("ingest drain: got %v, want %v", got["ingest drain"], drainTime)
	}
}

func TestLoadRunRecords_MissingDir(t *testing.T) {
	// Fresh checkout with no output/runs yet must not error.
	root := t.TempDir()
	got, err := loadRunRecords(root)
	if err != nil {
		t.Fatalf("expected nil error on missing dir, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %d entries", len(got))
	}
}

func TestClassifyFreshness(t *testing.T) {
	now := mustTime(t, "2026-04-10T12:00:00Z")
	cases := []struct {
		name    string
		lastOk  time.Time
		gap     time.Duration
		want    checkStatus
		wantSub string // substring the detail must contain
	}{
		{"never ran", time.Time{}, 6 * time.Hour, statusWarn, "never run"},
		{"fresh — within gap", now.Add(-1 * time.Hour), 6 * time.Hour, statusOK, "last run 1h ago"},
		{"right at edge", now.Add(-6 * time.Hour), 6 * time.Hour, statusOK, "last run 6h ago"},
		{"stale — over gap", now.Add(-7 * time.Hour), 6 * time.Hour, statusWarn, "expected ≤ 6h"},
		{"very stale — days", now.Add(-72 * time.Hour), 48 * time.Hour, statusWarn, "3d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, detail := classifyFreshness(tc.lastOk, now, tc.gap)
			if status != tc.want {
				t.Errorf("status: got %q, want %q", status, tc.want)
			}
			if !strings.Contains(detail, tc.wantSub) {
				t.Errorf("detail: got %q, want substring %q", detail, tc.wantSub)
			}
		})
	}
}

// TestCheckState_Parsers verifies that each state file probe correctly
// classifies a corrupt fixture as FAIL and a valid fixture as OK. The full
// checkState function is exercised against a real tmp KB root.
func TestCheckState_Parsers(t *testing.T) {
	root := t.TempDir()
	// Minimal KB layout.
	mustMkdir(t, filepath.Join(root, "scripts"))
	mustMkdir(t, filepath.Join(root, "wiki"))

	// Valid projects.json manifest.
	manifest := `{"projects":{"foo":{"path":"/tmp/foo","domain":"general","last_sha":"","last_extracted":"","last_md_scan":""}},"domain_aliases":{},"ignored_paths":[]}`
	mustWrite(t, filepath.Join(root, "scripts", "projects.json"), manifest)
	// Valid imessage-state.
	mustWrite(t, filepath.Join(root, "scripts", "imessage-state.json"), `{"last_capture":null,"captured_urls":[],"captured_count":0}`)
	// Corrupt sessions log — should FAIL.
	mustWrite(t, filepath.Join(root, "wiki", "_sessions_log.json"), `{not valid json`)
	// Valid backlinks.
	mustWrite(t, filepath.Join(root, "wiki", "_backlinks.json"), `{"Foo":["Bar"]}`)
	// Non-empty index.md and log.md.
	mustWrite(t, filepath.Join(root, "wiki", "_index.md"), "# Index\n\n- item\n")
	mustWrite(t, filepath.Join(root, "log.md"), "## 2026-04-10 init\n")

	results := checkState(root, loadConfig(root))

	findStatus := func(name string) checkStatus {
		for _, ck := range results {
			if ck.Name == name {
				return ck.Status
			}
		}
		return ""
	}

	cases := []struct {
		name string
		want checkStatus
	}{
		{"scripts/projects.json", statusOK},
		{"scripts/imessage-state.json", statusOK},
		{"wiki/_sessions_log.json", statusFail},
		{"wiki/_backlinks.json", statusOK},
		{"wiki/_index.md", statusOK},
		{"log.md", statusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findStatus(tc.name)
			if got != tc.want {
				t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestCheckState_FlagsKBAsProject asserts doctor surfaces the self-extraction
// contamination: a manifest project whose path is itself a scribe KB. This is
// how an already-affected user finds the source of their duplicate pages.
func TestCheckState_FlagsKBAsProject(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "scripts"))
	mustMkdir(t, filepath.Join(root, "wiki"))

	// A "project" that is actually a scribe KB (has scribe.yaml).
	kbProject := filepath.Join(root, "..", "some-kb")
	mustMkdir(t, kbProject)
	mustWrite(t, filepath.Join(kbProject, "scribe.yaml"), "owner_name: T\n")
	// A genuine project (no marker).
	plain := filepath.Join(root, "..", "plain-proj")
	mustMkdir(t, plain)

	manifest := fmt.Sprintf(`{"projects":{"some-kb":{"path":%q,"domain":"general"},"plain-proj":{"path":%q,"domain":"general"}},"domain_aliases":{},"ignored_paths":[]}`, kbProject, plain)
	mustWrite(t, filepath.Join(root, "scripts", "projects.json"), manifest)

	results := checkState(root, loadConfig(root))

	var found *check
	for i := range results {
		if results[i].Name == "kb-as-project" {
			found = &results[i]
			break
		}
	}
	if found == nil {
		t.Fatal("expected a kb-as-project check, got none")
	}
	if found.Status != statusWarn {
		t.Errorf("status = %q, want warn", found.Status)
	}
	if !strings.Contains(found.Detail, "some-kb") {
		t.Errorf("detail should name the offending project, got %q", found.Detail)
	}
	if strings.Contains(found.Detail, "plain-proj") {
		t.Errorf("plain project wrongly flagged: %q", found.Detail)
	}
}

// TestPrintChecksJSON sanity-checks the JSON schema so downstream consumers
// (monitoring probes) get stable keys.
func TestPrintChecksJSON(t *testing.T) {
	all := []check{
		{Section: "deps", Name: "claude", Status: statusOK, Detail: "/usr/bin/claude"},
		{Section: "cron", Name: "com.scribe.lint", Status: statusFail, Detail: "missing", Fix: "scribe cron install"},
		{Section: "freshness", Name: "lint", Status: statusWarn, Detail: "never run"},
	}

	// Capture stdout by temporarily replacing os.Stdout with a pipe.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	printChecksJSON(all, "/tmp/fake")
	_ = w.Close()
	os.Stdout = orig

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	var payload struct {
		KBRoot  string         `json:"kb_root"`
		Checks  []check        `json:"checks"`
		Summary map[string]int `json:"summary"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, string(data))
	}

	if payload.KBRoot != "/tmp/fake" {
		t.Errorf("kb_root: got %q", payload.KBRoot)
	}
	if len(payload.Checks) != 3 {
		t.Errorf("checks count: got %d, want 3", len(payload.Checks))
	}
	if payload.Summary["ok"] != 1 || payload.Summary["warn"] != 1 || payload.Summary["fail"] != 1 {
		t.Errorf("summary: %+v", payload.Summary)
	}
}

// ---- test helpers ----

func joinLines(lines []string) string {
	return strings.Join(lines, "\n") + "\n"
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return tm
}

// TestCheckState_FlagsPlaceholderArtifacts asserts doctor surfaces an
// unsubstituted {{VAR}} template placeholder that leaked into a KB path, so an
// affected user is pointed at `scribe lint --fix`.
func TestCheckState_FlagsPlaceholderArtifacts(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "scripts"))
	mustMkdir(t, filepath.Join(root, "wiki"))
	mustWrite(t, filepath.Join(root, "scripts", "projects.json"),
		`{"projects":{},"domain_aliases":{},"ignored_paths":[]}`)
	mustMkdir(t, filepath.Join(root, "projects", "{{DOMAIN}}"))
	mustWrite(t, filepath.Join(root, "projects", "{{DOMAIN}}", "scribe_analysis.md"), "leaked")

	results := checkState(root, loadConfig(root))

	var found *check
	for i := range results {
		if results[i].Name == "placeholder-artifacts" {
			found = &results[i]
			break
		}
	}
	if found == nil {
		t.Fatal("expected a placeholder-artifacts check, got none")
	}
	if found.Status != statusWarn {
		t.Errorf("status = %q, want warn", found.Status)
	}
	if !strings.Contains(found.Detail, "{{DOMAIN}}") {
		t.Errorf("detail should name the offending path, got %q", found.Detail)
	}
}

// TestCheckState_StopwordHeldSurfaced asserts doctor surfaces a stop-word
// hold match (gap 1): previously, a file the commit gate held back (or an
// already-committed article matching a hold word added later) left no
// trace beyond a transient sync-log "STOPWORD HELD" line — doctor must now
// report it so it doesn't vanish invisibly.
func TestCheckState_StopwordHeldSurfaced(t *testing.T) {
	seedStopWords(t, "")
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "scripts"))
	mustMkdir(t, filepath.Join(root, "wiki"))
	mustWrite(t, filepath.Join(root, "scripts", "projects.json"),
		`{"projects":{},"domain_aliases":{},"ignored_paths":[]}`)
	mustWrite(t, filepath.Join(root, "wiki", "held.md"), "the Project Falcon spec\n")
	mustWrite(t, filepath.Join(root, "scribe.yaml"), "stop_words:\n  hold:\n    - Project Falcon\n")

	results := checkState(root, loadConfig(root))

	var found *check
	for i := range results {
		if results[i].Name == "stopword-held-articles" {
			found = &results[i]
			break
		}
	}
	if found == nil {
		t.Fatal("doctor did not flag the stop-word hold")
	}
	if found.Status != statusWarn {
		t.Errorf("status = %q, want warn", found.Status)
	}
	if !strings.Contains(found.Detail, "wiki/held.md:1") {
		t.Errorf("detail should point at wiki/held.md:1, got %q", found.Detail)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// mustWrite lives in link_test.go — reused here.

// TestForeignScribeAgents pins the duplicate-LaunchAgent detector: a
// plist outside scribe's own job set that references the same binary or
// KB root is the 2026-06 double-run incident (a pre-rename agent set
// stayed loaded and every job fired twice for weeks).
func TestForeignScribeAgents(t *testing.T) {
	agents := t.TempDir()
	binary := "/Users/u/.local/bin/scribe"
	root := "/Users/u/Projects/kb"
	plist := func(name, body string) string {
		path := filepath.Join(agents, name)
		if err := os.WriteFile(path, []byte("<plist>"+body+"</plist>\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	ownPath := plist("com.scribe.sync-projects.plist", "<string>cd "+root+" && "+binary+" sync</string>")
	plist("com.legacy.sync-projects.plist", "<string>cd "+root+" && "+binary+" sync</string>")
	plist("com.other.kb-watcher.plist", "<string>watchthing "+root+"</string>")
	plist("com.apple.unrelated.plist", "<string>/usr/bin/true</string>")
	plist("not-a-plist.txt", "<string>"+binary+"</string>")

	got := foreignScribeAgents(agents, binary, root, map[string]bool{ownPath: true})
	want := []string{"com.legacy.sync-projects", "com.other.kb-watcher"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("foreignScribeAgents = %v, want %v", got, want)
	}

	// Missing dir (non-macOS, fresh account) must be silent, not an error.
	if got := foreignScribeAgents(filepath.Join(agents, "nope"), binary, root, nil); got != nil {
		t.Errorf("missing dir should yield nil, got %v", got)
	}
}

// TestBlockReferencesKB pins the multi-KB handshake check (#27): a
// present scribe block only counts as installed FOR THIS KB when the
// block body mentions this KB's root.
func TestBlockReferencesKB(t *testing.T) {
	block := claudeMDMarkerBegin + "\nKB lives at `/Users/u/Projects/kb-a` — query it first.\n" + claudeMDMarkerEnd
	if !blockReferencesKB(block, "/Users/u/Projects/kb-a") {
		t.Error("block referencing kb-a must pass for kb-a")
	}
	if blockReferencesKB(block, "/Users/u/Projects/kb-b") {
		t.Error("block referencing kb-a must fail for kb-b")
	}
	if blockReferencesKB("no markers at all /Users/u/Projects/kb-a", "/Users/u/Projects/kb-a") {
		t.Error("content without markers must fail")
	}
	// Root mentioned only OUTSIDE the scribe block doesn't count.
	outside := "/Users/u/Projects/kb-b is mentioned here\n" + claudeMDMarkerBegin + "\npoints at kb-a\n" + claudeMDMarkerEnd
	if blockReferencesKB(outside, "/Users/u/Projects/kb-b") {
		t.Error("root outside the block must not count")
	}
}

// TestParseCcriderTime covers the formats ccrider's updated_at has worn.
// The live DB stores Go's time.Time.String() output; RFC3339 is accepted
// defensively so a schema change doesn't silently drop every provider
// (an unparseable timestamp is skipped, which would read as "healthy").
func TestParseCcriderTime(t *testing.T) {
	for _, in := range []string{
		"2026-08-10 08:23:54.349 +0000 UTC",
		"2026-07-22 16:44:52 +0000 UTC",
		"2026-08-10T08:23:54Z",
		"2026-08-10T08:23:54.349Z",
		"2026-08-10 08:23:54",
	} {
		if _, ok := parseCcriderTime(in); !ok {
			t.Errorf("should parse %q", in)
		}
	}
	for _, in := range []string{"", "   ", "not a time", "1754812834"} {
		if _, ok := parseCcriderTime(in); ok {
			t.Errorf("should not parse %q", in)
		}
	}
}

// TestCheckProviderMining pins the staleness row and — more importantly
// — the three guards that keep it from crying wolf. The failure this
// catches is real: ccrider's Amp importer went quiet on 2026-07-22 and
// nothing surfaced it for three weeks.
func TestCheckProviderMining(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	stamp := func(d time.Duration) string {
		return now.Add(-d).Format("2006-01-02 15:04:05.999999999 -0700 MST")
	}
	// seed builds a fixture DB whose sessions carry the given provider,
	// count and newest-timestamp, then runs the check against it.
	seed := func(t *testing.T, rows []ccriderProviderStat, ages []time.Duration) []check {
		t.Helper()
		db, path := newCcriderDB(t)
		for i, r := range rows {
			for n := range r.Sessions {
				//nolint:noctx // test fixture
				if _, err := db.Exec(
					`INSERT INTO sessions (session_id, project_path, updated_at, provider) VALUES (?, ?, ?, ?)`,
					fmt.Sprintf("%s-%d", r.Provider, n), "/p", stamp(ages[i]), r.Provider); err != nil {
					t.Fatal(err)
				}
			}
		}
		return checkProviderMining(&ScribeConfig{CcriderDB: path}, now)
	}
	warnsFor := func(out []check, provider string) *check {
		for i := range out {
			if out[i].Status == statusWarn && strings.Contains(out[i].Name, provider) {
				return &out[i]
			}
		}
		return nil
	}

	// The July incident: amp frozen for 19 days while claude indexed an
	// hour ago. Both providers well past the min-session bar.
	out := seed(t,
		[]ccriderProviderStat{{Provider: "claude", Sessions: 20}, {Provider: "amp", Sessions: 42}},
		[]time.Duration{time.Hour, 19 * 24 * time.Hour})
	got := warnsFor(out, "amp")
	if got == nil {
		t.Fatalf("stale amp provider should WARN; got %+v", out)
	}
	if !strings.Contains(got.Detail, "42 sessions") || !strings.Contains(got.Detail, "stopped using it") {
		t.Errorf("detail should carry the count and both readings; got %q", got.Detail)
	}
	if !strings.Contains(got.Fix, "amp_enabled") {
		t.Errorf("amp fix should name the ccrider toggle; got %q", got.Fix)
	}
	if warnsFor(out, "claude") != nil {
		t.Error("the fresh provider must not warn")
	}

	// Guard 1: a provider with almost no history is an experiment, not a
	// habit — going quiet says nothing.
	out = seed(t,
		[]ccriderProviderStat{{Provider: "claude", Sessions: 20}, {Provider: "opencode", Sessions: 2}},
		[]time.Duration{time.Hour, 40 * 24 * time.Hour})
	if w := warnsFor(out, "opencode"); w != nil {
		t.Errorf("provider under the min-session bar must not warn; got %+v", w)
	}

	// Guard 2: no live reference clock. If ccrider indexed nothing
	// recently the machine was simply idle, and blaming one provider
	// would be noise — the whole section stays OK.
	out = seed(t,
		[]ccriderProviderStat{{Provider: "claude", Sessions: 20}, {Provider: "amp", Sessions: 42}},
		[]time.Duration{30 * 24 * time.Hour, 60 * 24 * time.Hour})
	for _, ck := range out {
		if ck.Status == statusWarn {
			t.Errorf("stale index with no active reference must not warn; got %+v", ck)
		}
	}

	// Guard 3: past the far bound the provider is abandoned, not broken.
	// Warning every run about a tool you stopped using months ago is
	// noise nobody can act on, so it drops to the inventory instead.
	out = seed(t,
		[]ccriderProviderStat{{Provider: "claude", Sessions: 20}, {Provider: "opencode", Sessions: 9}},
		[]time.Duration{time.Hour, 120 * 24 * time.Hour})
	if w := warnsFor(out, "opencode"); w != nil {
		t.Errorf("provider past the abandoned bound must not warn; got %+v", w)
	}
	if len(out) != 1 || !strings.Contains(out[0].Detail, "opencode") || !strings.Contains(out[0].Detail, "(inactive)") {
		t.Errorf("abandoned provider should still appear in the inventory, marked inactive; got %+v", out)
	}

	// Guard 4: everything fresh → one OK inventory row, no per-provider
	// noise.
	out = seed(t,
		[]ccriderProviderStat{{Provider: "claude", Sessions: 20}, {Provider: "amp", Sessions: 42}},
		[]time.Duration{time.Hour, 2 * time.Hour})
	if len(out) != 1 || out[0].Status != statusOK {
		t.Fatalf("healthy index should yield one OK row; got %+v", out)
	}
	if !strings.Contains(out[0].Detail, "claude") || !strings.Contains(out[0].Detail, "amp") {
		t.Errorf("inventory should list every provider; got %q", out[0].Detail)
	}
}

// TestCheckProviderMining_NoDB: a missing ccrider DB is checkConfig's
// FAIL to report, not this row's — it must stay silent rather than
// double-reporting the same problem in two sections.
func TestCheckProviderMining_NoDB(t *testing.T) {
	got := checkProviderMining(&ScribeConfig{CcriderDB: filepath.Join(t.TempDir(), "absent.db")}, time.Now())
	if got != nil {
		t.Errorf("missing DB should yield no rows, got %+v", got)
	}
	if got := checkProviderMining(nil, time.Now()); got != nil {
		t.Errorf("nil config should yield no rows, got %+v", got)
	}
}

// TestCheckAgentMDHandshake pins the four states the shared handshake
// row reports, and that a foreign-KB block names the right agent — the
// row is what tells a multi-KB machine which sessions are pointed where.
func TestCheckAgentMDHandshake(t *testing.T) {
	dir := t.TempDir()
	const root = "/Users/u/Projects/kb-a"

	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}
	block := func(kb string) string {
		return claudeMDMarkerBegin + "\nKB lives at `" + kb + "`.\n" + claudeMDMarkerEnd
	}

	// 1. File missing → WARN with the caller's detail, row named for the
	//    file (not "<file> block") so it reads as "no file at all".
	got := checkAgentMDHandshake(filepath.Join(dir, "absent.md"), "~/.config/amp/AGENTS.md", "Amp", "not found (Amp not set up?)", root)
	if got.Status != statusWarn || got.Detail != "not found (Amp not set up?)" {
		t.Errorf("missing file: got %+v", got)
	}
	if got.Name != "~/.config/amp/AGENTS.md" {
		t.Errorf("missing file row should name the file, got %q", got.Name)
	}

	// 2. File present, no block → WARN.
	got = checkAgentMDHandshake(write("nomarkers.md", "# my own notes\n"), "~/.config/amp/AGENTS.md", "Amp", "nf", root)
	if got.Status != statusWarn || got.Detail != "scribe block not found" {
		t.Errorf("no block: got %+v", got)
	}

	// 3. Block pointing at this KB → OK.
	got = checkAgentMDHandshake(write("ok.md", "user prose\n"+block(root)), "~/.config/amp/AGENTS.md", "Amp", "nf", root)
	if got.Status != statusOK || got.Name != "~/.config/amp/AGENTS.md block" {
		t.Errorf("in-sync block: got %+v", got)
	}

	// 4. Block pointing at another KB → WARN naming the agent (#27).
	got = checkAgentMDHandshake(write("foreign.md", block("/Users/u/Projects/kb-b")), "~/.config/amp/AGENTS.md", "Amp", "nf", root)
	if got.Status != statusWarn || !strings.Contains(got.Detail, "Amp sessions query that one") {
		t.Errorf("foreign block: got %+v", got)
	}
	if !strings.Contains(got.Fix, "--bind") {
		t.Errorf("foreign block fix should point at --bind, got %q", got.Fix)
	}
}
