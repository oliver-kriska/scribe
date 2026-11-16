package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newUnpushedFixture builds a bare remote and a clone tracking origin/main.
func newUnpushedFixture(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	mustGit(t, tmp, "init", "--bare", "-b", "main", remote)
	clone := filepath.Join(tmp, "clone")
	mustGit(t, tmp, "init", "-b", "main", clone)
	setIdentity(t, clone, "Alice")
	mustGit(t, clone, "config", "commit.gpgsign", "false")
	mustGit(t, clone, "commit", "-q", "--allow-empty", "-m", "base")
	mustGit(t, clone, "remote", "add", "origin", remote)
	mustGit(t, clone, "push", "-q", "-u", "origin", "main")
	return clone
}

func commitAt(t *testing.T, repo string, when time.Time) {
	t.Helper()
	mustGit(t, repo, "commit", "-q", "--allow-empty", "-m", "local", "--date="+when.Format(time.RFC3339))
}

func TestUnpushedWork(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name         string
		setup        func(t *testing.T, repo string)
		wantUpstream bool
		wantCount    int
		wantStuck    bool
	}{
		{"no upstream", func(t *testing.T, repo string) {
			mustGit(t, repo, "branch", "--unset-upstream")
			commitAt(t, repo, now.Add(-48*time.Hour))
		}, false, 0, false},
		{"zero ahead", func(*testing.T, string) {}, true, 0, false},
		{"ahead and young", func(t *testing.T, repo string) {
			commitAt(t, repo, now.Add(-time.Hour))
		}, true, 1, false},
		{"ahead and old", func(t *testing.T, repo string) {
			commitAt(t, repo, now.Add(-7*time.Hour)) // oldest decides
			commitAt(t, repo, now.Add(-time.Minute))
		}, true, 2, true},
		{"ahead and old, rebase left in progress", func(t *testing.T, repo string) {
			commitAt(t, repo, now.Add(-7*time.Hour))
			mustGit(t, repo, "checkout", "-q", "--detach") // HEAD detaches mid-rebase
		}, true, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newUnpushedFixture(t)
			tc.setup(t, repo)
			got, ok := unpushedWork(repo, now)
			if ok != tc.wantUpstream || got.Count != tc.wantCount || got.stuck() != tc.wantStuck {
				t.Fatalf("got %+v ok=%v stuck=%v; want ok=%v count=%d stuck=%v",
					got, ok, got.stuck(), tc.wantUpstream, tc.wantCount, tc.wantStuck)
			}
		})
	}
}

func TestCheckUnpushedWorkRow(t *testing.T) {
	now := time.Now()
	repo := newUnpushedFixture(t)
	if rows := checkUnpushedWork(repo, now); len(rows) != 1 || rows[0].Status != statusOK || rows[0].Name != "unpushed-work" {
		t.Fatalf("clean clone: got %+v, want one ok unpushed-work row", rows)
	}
	commitAt(t, repo, now.Add(-7*time.Hour))
	rows := checkUnpushedWork(repo, now)
	if len(rows) != 1 || rows[0].Status != statusFail || !strings.Contains(rows[0].Detail, "1 commit") {
		t.Fatalf("stuck clone: got %+v, want one FAIL row naming 1 commit", rows)
	}
	mustGit(t, repo, "branch", "--unset-upstream")
	if rows := checkUnpushedWork(repo, now); len(rows) != 0 {
		t.Fatalf("no upstream: got %+v, want no row", rows)
	}
}

func TestNotifyUnpushedThrottle(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var sent []string
	orig := notifyDesktop
	t.Cleanup(func() { notifyDesktop = orig })
	notifyDesktop = func(msg string) error { sent = append(sent, msg); return nil }

	now := time.Now()
	stuck := unpushed{Count: 3, Oldest: now.Add(-7 * time.Hour), Now: now}
	notifyUnpushedDaily("kb", stuck, now)
	notifyUnpushedDaily("kb", stuck, now.Add(time.Hour))
	if len(sent) != 1 {
		t.Fatalf("within a day: sent %d notifications, want 1", len(sent))
	}
	notifyUnpushedDaily("kb", stuck, now.Add(25*time.Hour))
	if len(sent) != 2 {
		t.Fatalf("after a day: sent %d notifications, want 2", len(sent))
	}

	// A failed notification does not stamp, so the next run retries.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	notifyDesktop = func(string) error { return errors.New("no permission") }
	notifyUnpushedDaily("kb", stuck, now)
	if _, err := os.Stat(unpushedNotifyStampPath()); !os.IsNotExist(err) {
		t.Fatalf("failed notify wrote a stamp: %v", err)
	}
}
