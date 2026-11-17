package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLatestVersionsByAuthor(t *testing.T) {
	cases := []struct {
		name   string
		gitLog string
		want   []committerVersion
		wantOK bool
	}{
		{name: "empty", gitLog: "", want: nil, wantOK: true},
		{
			name:   "newest wins per author",
			gitLog: "a@x\x00v0.6.3\n\x1e\na@x\x00v0.6.2\n\x1e\nb@x\x00v0.6.2\n\x1e",
			want:   []committerVersion{{"a@x", "v0.6.3"}, {"b@x", "v0.6.2"}},
			wantOK: true,
		},
		{
			name:   "manual commit without trailer does not hide version",
			gitLog: "a@x\x00\x1e\na@x\x00v0.6.3\n\x1e",
			want:   []committerVersion{{"a@x", "v0.6.3"}},
			wantOK: true,
		},
		{
			name:   "no trailer at all is pre-trailer",
			gitLog: "b@x\x00\x1e" + "\nb@x\x00\x1e",
			want:   []committerVersion{{"b@x", ""}},
			wantOK: true,
		},
		{
			name:   "two trailer lines keep the first",
			gitLog: "a@x\x000.6.3\n0.6.2\n\x1e",
			want:   []committerVersion{{"a@x", "0.6.3"}},
			wantOK: true,
		},
		{
			name:   "old git prints the placeholder literally",
			gitLog: "a@x\x00%(trailers:key=Scribe-Version,valueonly)\x1e",
			want:   nil,
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := latestVersionsByAuthor(tc.gitLog)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestTeamVersionCheck(t *testing.T) {
	cases := []struct {
		name       string
		versions   []committerVersion
		binary     string
		wantStatus checkStatus
		wantDetail []string
	}{
		{
			name:       "all match, v prefix ignored",
			versions:   []committerVersion{{"me@x", "v0.6.3"}, {"b@x", "0.6.3"}},
			binary:     "0.6.3",
			wantStatus: statusOK,
		},
		{
			name:       "behind, pre-trailer, dev and self are named",
			versions:   []committerVersion{{"me@x", "0.6.2"}, {"b@x", ""}, {"c@x", "dev"}, {"d@x", "0.6.3"}},
			binary:     "0.6.3",
			wantStatus: statusWarn,
			wantDetail: []string{"me@x (you): 0.6.2", "b@x: pre-trailer", "c@x: dev build"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := teamVersionCheck(tc.versions, tc.binary, "me@x")
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s (%s)", got.Status, tc.wantStatus, got.Detail)
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(got.Detail, want) {
					t.Fatalf("detail %q missing %q", got.Detail, want)
				}
			}
			if strings.Contains(got.Detail, "d@x") {
				t.Fatalf("matching member listed: %q", got.Detail)
			}
		})
	}
}

func TestGitCommitAddsVersionTrailer(t *testing.T) {
	root := t.TempDir()
	gitInitKB(t, root)
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "a.md")
	if err := gitCommit(root, "sync: test"); err != nil {
		t.Fatal(err)
	}
	got := runCmd(root, "git", "log", "-1", "--format=%(trailers:key="+scribeVersionTrailerKey+",valueonly)")
	if got != version {
		t.Fatalf("trailer = %q, want %q", got, version)
	}
	if subject := runCmd(root, "git", "log", "-1", "--format=%s"); subject != "sync: test" {
		t.Fatalf("subject = %q", subject)
	}

	checks := checkTeamVersions(root)
	if len(checks) != 1 || checks[0].Status != statusOK {
		t.Fatalf("doctor row = %+v, want one ok row", checks)
	}
}
