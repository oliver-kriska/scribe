package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnerHelpers(t *testing.T) {
	cfg := &ScribeConfig{
		Domains: []string{"backend", "infra", "general"},
		Owners:  map[string]string{"backend": "Alice"},
	}
	if got := ownerFor(cfg, "backend"); got != "Alice" {
		t.Errorf("ownerFor(backend) = %q", got)
	}
	if got := ownerFor(cfg, "infra"); got != "" {
		t.Errorf("ownerFor(infra) = %q, want empty", got)
	}
	if got := ownerFor(nil, "backend"); got != "" {
		t.Errorf("ownerFor(nil cfg) = %q, want empty", got)
	}
	if got := ownerSuffix(cfg, "backend"); got != " — owner: Alice" {
		t.Errorf("ownerSuffix = %q", got)
	}
	unowned := unownedDomains(cfg)
	if len(unowned) != 2 || unowned[0] != "general" || unowned[1] != "infra" {
		t.Errorf("unownedDomains = %v, want [general infra]", unowned)
	}
}

func TestGitDigestActivity(t *testing.T) {
	root := initTestGitRepo(t, "Alice")
	writeKBFile(t, root, "wiki/first.md", "---\ntitle: First\ndomain: backend\n---\n\nbody\n")
	writeKBFile(t, root, "wiki/_index.md", "- [[First]]\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-q", "-m", "alice adds")

	gitRun(t, root, "config", "user.name", "Bob")
	writeKBFile(t, root, "wiki/first.md", "---\ntitle: First\ndomain: backend\n---\n\nbody v2\n")
	writeKBFile(t, root, "patterns/trick.md", "---\ntitle: Trick\ndomain: infra\n---\n\nbody\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-q", "-m", "bob edits")

	activity := gitDigestActivity(root, 7)
	if len(activity) != 2 {
		t.Fatalf("got %d authors, want 2: %+v", len(activity), activity)
	}
	// Sorted: Alice before Bob.
	alice, bob := activity[0], activity[1]
	if alice.author != "Alice" || bob.author != "Bob" {
		t.Fatalf("authors = %q, %q", alice.author, bob.author)
	}
	// Underscore files excluded.
	if len(alice.added) != 1 || alice.added[0] != "wiki/first.md" {
		t.Errorf("alice added = %v, want [wiki/first.md]", alice.added)
	}
	if len(bob.added) != 1 || bob.added[0] != "patterns/trick.md" {
		t.Errorf("bob added = %v", bob.added)
	}
	if len(bob.updated) != 1 || bob.updated[0] != "wiki/first.md" {
		t.Errorf("bob updated = %v", bob.updated)
	}
}

func TestBuildDigestContent(t *testing.T) {
	root := initTestGitRepo(t, "Alice")
	writeKBFile(t, root, "wiki/good.md", "---\ntitle: Good\ndomain: backend\n---\n\nbody\n")
	writeKBFile(t, root, "wiki/broken.md", "---\ntitle: Broken\ndomain: backend\n---\n<<<<<<< HEAD\nours\n>>>>>>> theirs\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-q", "-m", "seed")

	cfg := &ScribeConfig{
		Domains: []string{"backend", "infra"},
		Owners:  map[string]string{"backend": "Alice"},
	}
	out := buildDigest(root, cfg, 7)

	for _, want := range []string{
		"# Team digest",
		"## Activity",
		"**Alice** — 2 new",
		"## Quality findings",
		"conflict markers",
		"wiki/broken.md:5 — owner: Alice",
		"## Owners",
		"backend → Alice",
		"Unowned domains: infra",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("digest missing %q:\n%s", want, out)
		}
	}
	// Deterministic: same inputs, same output.
	if again := buildDigest(root, cfg, 7); again != out {
		t.Error("digest not deterministic across runs")
	}
}

func TestBuildDigestEmptyKB(t *testing.T) {
	root := t.TempDir()
	out := buildDigest(root, &ScribeConfig{}, 7)
	if !strings.Contains(out, "No article changes") {
		t.Errorf("empty digest missing no-activity line:\n%s", out)
	}
	if !strings.Contains(out, "Nothing outstanding.") {
		t.Errorf("empty digest missing clean findings line:\n%s", out)
	}
	if strings.Contains(out, "## Owners") {
		t.Errorf("ownerless digest should omit Owners section:\n%s", out)
	}
}

func TestWriteDigestFile(t *testing.T) {
	root := initTestGitRepo(t, "Alice")
	if err := os.MkdirAll(filepath.Join(root, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDigestFile(root, &ScribeConfig{})
	data, err := os.ReadFile(filepath.Join(root, "wiki", "_digest.md"))
	if err != nil {
		t.Fatalf("digest not written: %v", err)
	}
	if !strings.Contains(string(data), "# Team digest") {
		t.Errorf("digest content wrong:\n%s", data)
	}
}
