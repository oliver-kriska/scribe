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
