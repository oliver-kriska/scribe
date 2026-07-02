package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestStampPlistRoundTrip pins the core guarantee behind issue #54: stamping
// unstamped content the binary just rendered and then classifying that
// stamped content against the SAME unstamped content must report "current"
// (okState) — the trivial case every other classification builds on.
func TestStampPlistRoundTrip(t *testing.T) {
	raw := renderPlist(scribeJobs("/usr/local/bin/scribe")[0])

	stamped := stampPlist(raw)
	if stamped == raw {
		t.Fatal("stampPlist did not change the content")
	}
	digest, ok := extractPlistDigest(stamped)
	if !ok || digest == "" {
		t.Fatalf("stamped plist has no extractable digest: %q", stamped)
	}
	if !strings.Contains(stamped, "<?xml") {
		t.Fatalf("stamped plist lost its XML declaration:\n%s", stamped)
	}
	// The digest line must land right after the XML declaration (first
	// line), not buried somewhere inside <dict> where a naive XML/plist
	// parser might trip on it.
	lines := strings.SplitN(stamped, "\n", 3)
	if len(lines) < 2 || !strings.HasPrefix(lines[1], "<!-- scribe:digest:sha256:") {
		t.Fatalf("digest comment is not the second line:\n%s", stamped)
	}

	if state := plistStampState(stamped, raw); state != okState {
		t.Errorf("plistStampState(stamped, raw) = %v, want okState", state)
	}

	// Re-stamping stamped content (idempotent normalize) must still
	// verify clean against the same raw expected content.
	restamped := stampPlist(stamped)
	if state := plistStampState(restamped, raw); state != okState {
		t.Errorf("plistStampState(restamped, raw) = %v, want okState", state)
	}
}

// TestPlistStampStateStale confirms a scribe-authored, un-edited plist
// whose content has drifted from what the current binary would generate
// (e.g. binary moved, schedule changed) is reported stale, not hand-edited
// — the whole point of the stamp is that this case is provably safe to
// rewrite without --force.
func TestPlistStampStateStale(t *testing.T) {
	before := renderPlist(cronJob{Name: "auto-commit", Command: "/usr/local/bin/scribe each -- commit"})
	after := renderPlist(cronJob{Name: "auto-commit", Command: "/opt/homebrew/bin/scribe each -- commit"})

	installed := stampPlist(before)
	if state := plistStampState(installed, after); state != staleState {
		t.Fatalf("plistStampState = %v, want staleState", state)
	}
	// The reverse direction (content matches) must be ok.
	if state := plistStampState(installed, before); state != okState {
		t.Fatalf("plistStampState against its own source = %v, want okState", state)
	}
}

// TestPlistStampStateHandEdited covers both hand-edited flavors named in
// issue #54: a stamp that doesn't match its own content (one byte flipped
// after stamping) and a plist with no stamp at all (pre-#54 legacy, or
// truly hand-authored). Neither may ever be classified as safe-to-rewrite.
func TestPlistStampStateHandEdited(t *testing.T) {
	raw := renderPlist(scribeJobs("/usr/local/bin/scribe")[0])
	stamped := stampPlist(raw)

	t.Run("stamp_present_but_wrong", func(t *testing.T) {
		// Flip a byte inside the <dict> body — after the stamp line, so
		// the digest comment itself is untouched but no longer matches.
		idx := strings.Index(stamped, "<dict>")
		if idx < 0 {
			t.Fatal("fixture plist has no <dict> to corrupt")
		}
		corrupted := stamped[:idx] + "<!-- hand edit -->\n" + stamped[idx:]
		if state := plistStampState(corrupted, raw); state != handEditedState {
			t.Errorf("corrupted stamped content: got %v, want handEditedState", state)
		}
	})

	t.Run("no_stamp_at_all", func(t *testing.T) {
		if state := plistStampState(raw, raw); state != handEditedState {
			t.Errorf("unstamped content: got %v, want handEditedState", state)
		}
	})

	t.Run("digest_line_present_but_forged", func(t *testing.T) {
		forged := strings.Replace(stamped, extractDigestOrFatal(t, stamped), "0000000000000000000000000000000000000000000000000000000000000000", 1)
		if state := plistStampState(forged, raw); state != handEditedState {
			t.Errorf("forged digest: got %v, want handEditedState", state)
		}
	})
}

func extractDigestOrFatal(t *testing.T, content string) string {
	t.Helper()
	digest, ok := extractPlistDigest(content)
	if !ok {
		t.Fatalf("no digest found in %q", content)
	}
	return digest
}

// TestCronInstallDecision table-tests the install-time dispatch: missing
// file or --force always (over)writes; an up-to-date stamped file is
// skipped silently; a stale stamped file refreshes WITHOUT --force; an
// unstamped or hand-edited file is skipped and needs --force.
func TestCronInstallDecision(t *testing.T) {
	raw := renderPlist(cronJob{Name: "lint", Command: "/usr/local/bin/scribe each -- lint"})
	staleRaw := renderPlist(cronJob{Name: "lint", Command: "/opt/homebrew/bin/scribe each -- lint"})
	stampedCurrent := stampPlist(raw)
	stampedStale := stampPlist(staleRaw) // authored against staleRaw, so it's stale relative to raw
	unstamped := raw

	cases := []struct {
		name     string
		existing string
		exists   bool
		force    bool
		want     installAction
	}{
		{"missing_file", "", false, false, actionWrite},
		{"missing_file_force_still_write", "", false, true, actionWrite},
		{"force_overrides_up_to_date", stampedCurrent, true, true, actionWrite},
		{"force_overrides_hand_edited", unstamped, true, true, actionWrite},
		{"up_to_date_no_force", stampedCurrent, true, false, actionSkipUpToDate},
		{"stale_no_force_refreshes", stampedStale, true, false, actionRefresh},
		{"unstamped_no_force_skips", unstamped, true, false, actionSkipHandEdited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cronInstallDecision(tc.existing, tc.exists, raw, tc.force)
			if got != tc.want {
				t.Errorf("cronInstallDecision(...) = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAnyScribeAgentInstalled pins the pure-file-read signal `cron install
// --if-installed` uses: false on an empty (or absent) LaunchAgents dir,
// true as soon as one com.scribe.*.plist exists, and unmoved by
// non-scribe or non-plist files sharing the directory.
func TestAnyScribeAgentInstalled(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	if anyScribeAgentInstalled() {
		t.Fatal("no LaunchAgents dir at all: want false")
	}

	agents := filepath.Join(fakeHome, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	if anyScribeAgentInstalled() {
		t.Fatal("empty LaunchAgents dir: want false")
	}

	if err := os.WriteFile(filepath.Join(agents, "com.apple.something.plist"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, "com.scribe.notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if anyScribeAgentInstalled() {
		t.Fatal("only foreign/non-plist files present: want false")
	}

	if err := os.WriteFile(filepath.Join(agents, "com.scribe.auto-commit.plist"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !anyScribeAgentInstalled() {
		t.Fatal("com.scribe.*.plist present: want true")
	}
}

// TestCronInstallIfInstalledNoOp is the brew post_install contract: on a
// machine that never opted into cron (no com.scribe.* plist anywhere),
// `cron install --if-installed` must exit 0 without ever calling kbDir()
// or touching global state — post_install runs from an arbitrary cwd, not
// a KB checkout, so reaching kbDir() at all would be a regression (it
// would error "not inside a scribe KB checkout" and fail the brew step).
func TestCronInstallIfInstalledNoOp(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	// Deliberately do NOT set SCRIBE_KB and do NOT chdir into a KB — if
	// the no-op check didn't short-circuit before kbDir(), this would
	// fail with "not inside a scribe KB checkout" instead of the
	// expected silent success.
	t.Setenv("SCRIBE_KB", "")

	c := &CronInstallCmd{IfInstalled: true}
	if err := c.Run(); err != nil {
		t.Fatalf("--if-installed with nothing installed: want nil error, got %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(fakeHome, "Library", "LaunchAgents"))
	if len(entries) != 0 {
		t.Errorf("--if-installed no-op still wrote %d file(s)", len(entries))
	}
}

// TestCronInstallSkipsAllHandEdited drives the real Run() end to end (no
// --force) against a LaunchAgents dir where every expected job's plist
// already exists but is hand-edited (unstamped). Every job must be
// skipped — and critically, Run() must never call writeGlobalState for
// any of them, so it succeeds even though SCRIBE_KB here is a throwaway
// t.TempDir() path that writeGlobalState would otherwise refuse. That
// makes this test a genuine end-to-end proof that the hand-edited branch
// short-circuits before any write is attempted, not just a decision-table
// check.
func TestCronInstallSkipsAllHandEdited(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("LaunchAgent install is darwin-only")
	}
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	kb := t.TempDir() // throwaway on purpose — proves no write is attempted
	t.Setenv("SCRIBE_KB", kb)
	if err := os.WriteFile(filepath.Join(kb, "scribe.yaml"), []byte("kb_name: t\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	agents := filepath.Join(fakeHome, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := resolveScribeBinary()
	for _, job := range scribeJobs(binary) {
		// Unstamped content — renderPlist output verbatim, never run
		// through stampPlist — is what pre-#54 real installs look like.
		if err := os.WriteFile(plistPath(job.Name), []byte(renderPlist(job)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	c := &CronInstallCmd{}
	if err := c.Run(); err != nil {
		t.Fatalf("install over all-hand-edited plists (no --force): want nil error, got %v", err)
	}

	// Files must be byte-identical to what we wrote — nothing rewritten.
	for _, job := range scribeJobs(binary) {
		got, err := os.ReadFile(plistPath(job.Name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != renderPlist(job) {
			t.Errorf("job %s was rewritten despite being unstamped/hand-edited and no --force", job.Name)
		}
	}
}

// TestBootstrapAgentRetriesOnce pins the EIO-race handling: launchctl
// bootstrap can fail with exit 5 when it races the asynchronous teardown of
// a KeepAlive instance the preceding bootout is still draining (observed
// live on the #54 adoption run). bootstrapAgent must retry bootstrap exactly
// once — and only bootstrap, never a second bootout, which would restart
// the very teardown it is waiting out.
func TestBootstrapAgentRetriesOnce(t *testing.T) {
	origRun, origDelay := runLaunchctl, bootstrapRetryDelay
	defer func() { runLaunchctl, bootstrapRetryDelay = origRun, origDelay }()
	bootstrapRetryDelay = 0

	var calls [][]string
	bootstraps := 0
	runLaunchctl = func(args ...string) (string, error) {
		calls = append(calls, args)
		if args[0] != "bootstrap" {
			return "", nil
		}
		bootstraps++
		if bootstraps == 1 {
			return "Bootstrap failed: 5: Input/output error", errors.New("exit status 5")
		}
		return "", nil
	}

	bootstrapAgent("gui/501", "/fake/com.scribe.test.plist", "com.scribe.test")

	want := [][]string{
		{"bootout", "gui/501/com.scribe.test"},
		{"bootstrap", "gui/501", "/fake/com.scribe.test.plist"},
		{"bootstrap", "gui/501", "/fake/com.scribe.test.plist"},
	}
	if len(calls) != len(want) {
		t.Fatalf("launchctl calls = %v, want %v", calls, want)
	}
	for i := range want {
		if strings.Join(calls[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("call %d = %v, want %v", i, calls[i], want[i])
		}
	}
}

// TestCronInstallLoadsCurrentButUnloadedPlist drives Run() end to end over a
// LaunchAgents dir where every job's plist is stamped and current but NOT
// loaded into launchd. Plain install (no --force) must bootstrap every one of
// them without rewriting a single file — this is exactly the state a failed
// bootstrap leaves behind, and it is what makes doctor's "plist on disk but
// not loaded → fix: scribe cron install" line actually true.
func TestCronInstallLoadsCurrentButUnloadedPlist(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("LaunchAgent install is darwin-only")
	}
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	kb := t.TempDir() // throwaway on purpose — proves no write is attempted
	t.Setenv("SCRIBE_KB", kb)
	if err := os.WriteFile(filepath.Join(kb, "scribe.yaml"), []byte("kb_name: t\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	agents := filepath.Join(fakeHome, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := resolveScribeBinary()
	stamped := map[string]string{} // path -> exact bytes written
	for _, job := range scribeJobs(binary) {
		content := stampPlist(renderPlist(job))
		if err := os.WriteFile(plistPath(job.Name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		stamped[plistPath(job.Name)] = content
	}

	origRun := runLaunchctl
	defer func() { runLaunchctl = origRun }()
	bootstrapped := map[string]bool{} // plist path -> bootstrap seen
	runLaunchctl = func(args ...string) (string, error) {
		switch args[0] {
		case "print":
			return "Could not find service", errors.New("exit status 113")
		case "bootstrap":
			bootstrapped[args[2]] = true
		}
		return "", nil
	}

	c := &CronInstallCmd{}
	if err := c.Run(); err != nil {
		t.Fatalf("install over current-but-unloaded plists: want nil error, got %v", err)
	}

	for _, job := range scribeJobs(binary) {
		path := plistPath(job.Name)
		if !bootstrapped[path] {
			t.Errorf("job %s: current-but-unloaded plist was not bootstrapped", job.Name)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != stamped[path] {
			t.Errorf("job %s was rewritten despite being current (only a load was needed)", job.Name)
		}
	}
}
