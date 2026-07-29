package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestGitChangedFiles_FirstExtractionHonorsGitignore(t *testing.T) {
	repo := initTestGitRepo(t, "Extract Tester")

	// Tracked source doc + a gitignored dependency tree whose text files
	// match the extract patterns (the .venv-blows-past-the-cap case).
	writeTestArticle(t, repo, "README.md", "# proj\n")
	writeTestArticle(t, repo, ".gitignore", ".venv/\n")
	writeTestArticle(t, repo, ".venv/lib/pkg.txt", "junk\n")
	writeTestArticle(t, repo, ".venv/notes.md", "junk\n")
	gitRun(t, repo, "add", "README.md", ".gitignore")
	gitRun(t, repo, "commit", "-q", "-m", "init")
	// Untracked but NOT ignored — must still be extracted.
	writeTestArticle(t, repo, "docs/guide.txt", "kept\n")

	got := gitChangedFiles(repo, "", []string{"*.md", "*.txt", "*.exs", "*.ex"})

	has := func(suffix string) bool {
		for _, f := range got {
			if strings.HasSuffix(f, suffix) {
				return true
			}
		}
		return false
	}
	if !has("/README.md") {
		t.Errorf("tracked README.md missing from first-extraction set: %v", got)
	}
	if !has("/docs/guide.txt") {
		t.Errorf("untracked-non-ignored docs/guide.txt missing: %v", got)
	}
	for _, f := range got {
		if strings.Contains(f, "/.venv/") {
			t.Errorf("gitignored .venv file leaked into extraction set: %s", f)
		}
		if !filepath.IsAbs(f) {
			t.Errorf("returned path is not absolute: %s", f)
		}
	}
}

func TestPullBeforeSyncEnabled_DefaultsTrue(t *testing.T) {
	if !pullBeforeSyncEnabled(nil) {
		t.Fatalf("nil cfg should default to enabled")
	}
	cfg := &ScribeConfig{}
	if !pullBeforeSyncEnabled(cfg) {
		t.Fatalf("unset pointer should default to enabled")
	}
}

func TestPullBeforeSyncEnabled_ExplicitFalse(t *testing.T) {
	f := false
	cfg := &ScribeConfig{Sync: SyncConfig{AlwaysPullBeforeSync: &f}}
	if pullBeforeSyncEnabled(cfg) {
		t.Fatalf("explicit false should disable")
	}
}

func TestPullRebase_NonRepoIsNoOp(t *testing.T) {
	ok, pulled, err := pullRebase(t.TempDir())
	if err != nil {
		t.Fatalf("non-repo should not error: %v", err)
	}
	if ok || pulled {
		t.Fatalf("non-repo should return ok=false, pulled=false")
	}
}

func TestCommitDebounced_DisabledWhenZero(t *testing.T) {
	cfg := &ScribeConfig{Sync: SyncConfig{CommitDebounceMinutes: 0}}
	debounced, _, _ := commitDebounced(t.TempDir(), cfg)
	if debounced {
		t.Fatalf("expected no debounce when CommitDebounceMinutes=0")
	}
}

func TestCommitDebounced_NoRepoTreatedAsOld(t *testing.T) {
	// A directory without a git HEAD returns a very large age so callers
	// proceed to commit on first run of a fresh KB.
	cfg := &ScribeConfig{Sync: SyncConfig{CommitDebounceMinutes: 30}}
	debounced, _, _ := commitDebounced(t.TempDir(), cfg)
	if debounced {
		t.Fatalf("expected non-repo path to fall through to commit, not debounce")
	}
}
