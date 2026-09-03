package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateUserConfig points userConfigPath() at a temp dir so registry tests
// never read or write the real ~/.config/scribe/config.yaml.
func isolateUserConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
}

// makeKBRoot creates a minimal directory that isKBRoot() accepts (has a
// scribe.yaml) and returns its absolute path.
func makeKBRoot(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scribe.yaml"), []byte("kb_name: "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeUserCfg(t *testing.T, body string) {
	t.Helper()
	p := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRegisteredKBs_FallbackToKBDir(t *testing.T) {
	isolateUserConfig(t)
	kb := makeKBRoot(t, "scriptorium")
	writeUserCfg(t, "kb_dir: "+kb+"\n")
	got := registeredKBs()
	if len(got) != 1 || got[0] != kb {
		t.Fatalf("empty registry should fall back to [kb_dir]; got %v", got)
	}
}

func TestRegisteredKBs_DedupAndFilter(t *testing.T) {
	isolateUserConfig(t)
	a := makeKBRoot(t, "a")
	b := makeKBRoot(t, "b")
	ghost := filepath.Join(t.TempDir(), "ghost") // not a KB root
	writeUserCfg(t, "kb_dir: "+a+"\nkbs:\n  - "+a+"\n  - "+b+"\n  - "+b+"\n  - "+ghost+"\n")
	got := registeredKBs()
	if len(got) != 2 {
		t.Fatalf("want a+b deduped, ghost filtered; got %v", got)
	}
	want := map[string]bool{a: true, b: true}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected registry entry %s", g)
		}
	}
}

// TestRegisteredKBs_KBDirUnionedWithKBs is the regression for the bug where
// adding a second KB to `kbs:` silently dropped kb_dir from the rotation:
// kb_dir (not listed in kbs:) plus one explicit entry must BOTH be returned,
// so registering enaia never stops cron from syncing the scriptorium default.
func TestRegisteredKBs_KBDirUnionedWithKBs(t *testing.T) {
	isolateUserConfig(t)
	primary := makeKBRoot(t, "scriptorium")
	extra := makeKBRoot(t, "enaia")
	writeUserCfg(t, "kb_dir: "+primary+"\nkbs:\n  - "+extra+"\n")
	got := registeredKBs()
	if len(got) != 2 {
		t.Fatalf("kb_dir must be unioned with kbs:, want 2 got %v", got)
	}
	if got[0] != primary {
		t.Errorf("kb_dir should lead the rotation; got %v", got)
	}
	saw := map[string]bool{}
	for _, g := range got {
		saw[g] = true
	}
	if !saw[primary] || !saw[extra] {
		t.Errorf("both kb_dir and the explicit entry must appear; got %v", got)
	}
}

func TestRegisterKB_IdempotentPreservesConfig(t *testing.T) {
	isolateUserConfig(t)
	a := makeKBRoot(t, "a")
	b := makeKBRoot(t, "b")
	writeUserCfg(t, "# scribe user config\nkb_dir: "+a+"\n")

	added, err := registerKB(b)
	if err != nil || !added {
		t.Fatalf("first registerKB(b): added=%v err=%v", added, err)
	}
	raw, _ := os.ReadFile(userConfigPath())
	if !strings.Contains(string(raw), "# scribe user config") || !strings.Contains(string(raw), "kb_dir: "+a) {
		t.Errorf("registerKB clobbered existing config:\n%s", raw)
	}
	if !strings.Contains(string(raw), b) {
		t.Errorf("b not added:\n%s", raw)
	}
	// idempotent
	if added2, _ := registerKB(b); added2 {
		t.Error("re-registering b should be a no-op")
	}
	// kb_dir counts as registered
	if added3, _ := registerKB(a); added3 {
		t.Error("kb_dir should already count as registered")
	}
}

func TestRegisterKB_RejectsNonKB(t *testing.T) {
	isolateUserConfig(t)
	if _, err := registerKB(t.TempDir()); err == nil {
		t.Error("expected error registering a non-KB path")
	}
}

func TestUnregisterKB(t *testing.T) {
	isolateUserConfig(t)
	a := makeKBRoot(t, "a")
	b := makeKBRoot(t, "b")
	writeUserCfg(t, "kbs:\n  - "+a+"\n  - "+b+"\n")
	removed, err := unregisterKB(b)
	if err != nil || !removed {
		t.Fatalf("unregisterKB(b): removed=%v err=%v", removed, err)
	}
	raw, _ := os.ReadFile(userConfigPath())
	if strings.Contains(string(raw), b) {
		t.Errorf("b still present after unregister:\n%s", raw)
	}
	if !strings.Contains(string(raw), a) {
		t.Errorf("a wrongly removed:\n%s", raw)
	}
	if removed2, _ := unregisterKB(b); removed2 {
		t.Error("removing an absent entry should report false")
	}
}

// TestRegisterKB_AfterInitRewriteStaysParseable is the regression for the
// registry corruption: `scribe init` rewrote the file with yaml.Marshal
// (4-space list items) and the old registry writer then spliced a 2-space
// item under the same `kbs:` key. yaml.v3 parsed that mix without error
// into ONE folded scalar ("/a - /b"), registeredKBs() dropped it, and every
// `scribe each` job stopped. Both writers now share marshalUserConfig.
func TestRegisterKB_AfterInitRewriteStaysParseable(t *testing.T) {
	isolateUserConfig(t)
	a := makeKBRoot(t, "a")
	b := makeKBRoot(t, "b")
	c := makeKBRoot(t, "c")

	// What installUserConfig leaves behind.
	if err := writeUserConfig(userConfig{KBDir: a, KBs: []string{b}, LLMAPIKey: "sk-test"}); err != nil {
		t.Fatal(err)
	}
	if added, err := registerKB(c); err != nil || !added {
		t.Fatalf("registerKB(c): added=%v err=%v", added, err)
	}

	uc, err := loadUserConfigChecked()
	if err != nil {
		t.Fatalf("config must stay parseable after init+register: %v", err)
	}
	if uc.KBDir != a || uc.LLMAPIKey != "sk-test" {
		t.Errorf("registerKB dropped other fields: %+v", uc)
	}
	if len(uc.KBs) != 2 || uc.KBs[0] != b || uc.KBs[1] != c {
		t.Errorf("want kbs [b c], got %v", uc.KBs)
	}
	got := registeredKBs()
	if len(got) != 3 {
		t.Errorf("want a,b,c in rotation, got %v", got)
	}
	raw, _ := os.ReadFile(userConfigPath())
	for _, ln := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "- ") && !strings.HasPrefix(ln, "    - ") {
			t.Errorf("list item not at yaml.v3's 4-space indent: %q", ln)
		}
	}
	if !strings.HasPrefix(string(raw), "# scribe user config") {
		t.Errorf("header comment missing:\n%s", raw)
	}
	fi, _ := os.Stat(userConfigPath())
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file carries an API key; want 0600, got %o", fi.Mode().Perm())
	}
}

// TestLoadUserConfig_UnfoldsCorruptedKBList: a file already corrupted by
// the old writer mix must still yield every KB (the in-memory heal), and
// doctor must be able to see that the on-disk shape is wrong.
func TestLoadUserConfig_UnfoldsCorruptedKBList(t *testing.T) {
	isolateUserConfig(t)
	a := makeKBRoot(t, "a")
	b := makeKBRoot(t, "b")
	c := makeKBRoot(t, "c")
	writeUserCfg(t, "kb_dir: "+a+"\nkbs:\n  - "+c+"\n    - "+a+"\n    - "+b+"\n")

	uc, err := loadUserConfigChecked()
	if err != nil {
		t.Fatalf("yaml.v3 accepts this shape; got %v", err)
	}
	if len(uc.KBs) != 3 || uc.KBs[0] != c || uc.KBs[1] != a || uc.KBs[2] != b {
		t.Fatalf("folded kbs must be split back apart; got %v", uc.KBs)
	}
	if got := registeredKBs(); len(got) != 3 {
		t.Errorf("registry must survive the corruption; got %v", got)
	}
	if !userConfigFolded() {
		t.Error("userConfigFolded must flag the on-disk corruption for doctor")
	}
	if ck := checkUserConfig(); ck.Status != statusWarn {
		t.Errorf("doctor must WARN on a folded kbs list; got %+v", ck)
	}

	// A rewrite through the registry heals the file.
	d := makeKBRoot(t, "d")
	if _, err := registerKB(d); err != nil {
		t.Fatal(err)
	}
	if userConfigFolded() {
		t.Error("registerKB must rewrite the file with consistent indentation")
	}
	if ck := checkUserConfig(); ck.Status != statusOK {
		t.Errorf("doctor must be OK after the rewrite; got %+v", ck)
	}
}

// TestUserConfig_ParseErrorIsSurfaced: a file that does not parse must
// not masquerade as a fresh install — each refuses to run, doctor FAILs,
// and the registry writers refuse to touch the file.
func TestUserConfig_ParseErrorIsSurfaced(t *testing.T) {
	isolateUserConfig(t)
	a := makeKBRoot(t, "a")
	writeUserCfg(t, "kb_dir: "+a+"\nkb_dir: "+a+"\n") // duplicate key

	if _, err := loadUserConfigChecked(); err == nil {
		t.Fatal("duplicate key must be reported")
	}
	if uc := loadUserConfig(); uc.KBDir != "" {
		t.Errorf("lenient loader must return the zero value; got %+v", uc)
	}
	if ck := checkUserConfig(); ck.Status != statusFail {
		t.Errorf("doctor must FAIL; got %+v", ck)
	}
	prev := eachRunner
	eachRunner = func(string, []string) error { t.Error("no job may run on an unreadable registry"); return nil }
	t.Cleanup(func() { eachRunner = prev })
	err := (&EachCmd{Args: []string{"sync"}}).Run()
	if err == nil || !strings.Contains(err.Error(), userConfigPath()) {
		t.Errorf("each must refuse and name the file; got %v", err)
	}
	if _, err := registerKB(a); err == nil {
		t.Error("registerKB must refuse to rewrite a file it cannot parse")
	}
}
