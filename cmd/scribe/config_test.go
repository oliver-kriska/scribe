package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestKBDirResolution pins the root-resolution priority:
// --root flag → SCRIBE_KB env → CWD walk → user config.
//
// The CWD-beats-user-config ordering is the regression under test: with
// a personal KB pinned in ~/.config/scribe/config.yaml and a team KB
// checked out elsewhere, `cd team-kb && scribe <cmd>` must operate on
// the team KB. Before 0.2.30 the user config won and every command
// silently hit the personal KB (found live: an e2e sweep inside a
// scratch KB regenerated the production KB's derived artifacts).
func TestKBDirResolution(t *testing.T) {
	newKB := func(t *testing.T) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "projects", "kb")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "scribe.yaml"), []byte("owner: t\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	setUserConfigKB := func(t *testing.T, kb string) {
		t.Helper()
		cfgHome := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", cfgHome)
		if err := os.MkdirAll(filepath.Join(cfgHome, "scribe"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfgHome, "scribe", "config.yaml"),
			[]byte("kb_dir: "+kb+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reset := func(t *testing.T) {
		t.Helper()
		saved := globalRoot
		globalRoot = ""
		t.Cleanup(func() { globalRoot = saved })
		t.Setenv("SCRIBE_KB", "")
	}

	t.Run("cwd inside a KB beats the user-config default", func(t *testing.T) {
		reset(t)
		personal := newKB(t)
		team := newKB(t)
		setUserConfigKB(t, personal)
		t.Chdir(team)

		got, err := kbDir()
		if err != nil {
			t.Fatal(err)
		}
		if got != team {
			t.Errorf("kbDir() = %q, want cwd KB %q (user config must be the fallback, not the override)", got, team)
		}
	})

	t.Run("cwd nested below a KB root resolves to that root", func(t *testing.T) {
		reset(t)
		personal := newKB(t)
		team := newKB(t)
		setUserConfigKB(t, personal)
		nested := filepath.Join(team, "wiki", "deep")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(nested)

		got, err := kbDir()
		if err != nil {
			t.Fatal(err)
		}
		if got != team {
			t.Errorf("kbDir() = %q, want enclosing KB %q", got, team)
		}
	})

	t.Run("outside any KB the user config is the fallback", func(t *testing.T) {
		reset(t)
		personal := newKB(t)
		setUserConfigKB(t, personal)
		t.Chdir(t.TempDir())

		got, err := kbDir()
		if err != nil {
			t.Fatal(err)
		}
		if got != personal {
			t.Errorf("kbDir() = %q, want user-config KB %q", got, personal)
		}
	})

	t.Run("SCRIBE_KB env beats the cwd walk", func(t *testing.T) {
		reset(t)
		team := newKB(t)
		other := newKB(t)
		setUserConfigKB(t, "")
		t.Setenv("SCRIBE_KB", other)
		t.Chdir(team)

		got, err := kbDir()
		if err != nil {
			t.Fatal(err)
		}
		if got != other {
			t.Errorf("kbDir() = %q, want SCRIBE_KB %q", got, other)
		}
	})

	t.Run("nowhere to resolve errors out", func(t *testing.T) {
		reset(t)
		setUserConfigKB(t, "")
		t.Chdir(t.TempDir())

		if got, err := kbDir(); err == nil {
			t.Errorf("kbDir() = %q, want error when no KB is findable", got)
		}
	})
}

// TestLoadConfigParseFailureIsRecorded pins LoadErr + requireParseable:
// LLM-cost commands abort on an unparseable scribe.yaml instead of
// running on defaults nobody chose (provider=anthropic, team=false).
func TestLoadConfigParseFailureIsRecorded(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"),
		[]byte("team: true\nteam: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(root)
	if cfg.LoadErr == nil {
		t.Fatal("LoadErr not set for duplicate-key scribe.yaml")
	}
	if err := cfg.requireParseable(); err == nil {
		t.Error("requireParseable must reject a config that failed to parse")
	}
	if cfg.Team {
		t.Error("broken config must not accidentally keep team=true — the gate guards via LoadErr instead")
	}

	good := t.TempDir()
	if err := os.WriteFile(filepath.Join(good, "scribe.yaml"),
		[]byte("owner_name: t\nteam: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg := loadConfig(good); cfg.LoadErr != nil || cfg.requireParseable() != nil {
		t.Errorf("valid config flagged unparseable: %v", cfg.LoadErr)
	}
}
