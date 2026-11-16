package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

const ownerTestReply = "This article records the connection pool saturation incident, walking through the observed timeline, the database metrics inspected during triage, and the remediation the engineering team applied to restore stable throughput across services."

// isolateGitConfig keeps the developer's global/system git config (signing,
// identity) out of the test repos.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func writeRawArticle(t *testing.T, root, name, body string) string {
	t.Helper()
	rawDir := filepath.Join(root, "raw", "articles")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rawDir, name)
	if err := os.WriteFile(path, []byte("---\ntitle: "+name+"\n---\n\n"+body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func cloneAs(t *testing.T, remote, dir, email string) {
	t.Helper()
	gitRun(t, filepath.Dir(dir), "clone", "-q", remote, dir)
	gitRun(t, dir, "config", "user.email", email)
	gitRun(t, dir, "config", "user.name", email)
}

// TestContextualizeRawArticles_OnlyOwnArticles: the machine that added a raw
// article contextualizes it; a teammate's clone skips it after pulling, so the
// two machines never write competing auto blocks. A file not yet committed is
// always this machine's.
func TestContextualizeRawArticles_OnlyOwnArticles(t *testing.T) {
	isolateGitConfig(t)
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	gitRun(t, base, "init", "-q", "--bare", "-b", "main", remote)

	cloneA := filepath.Join(base, "a")
	cloneAs(t, remote, cloneA, "alice@example.com")
	pathA := writeRawArticle(t, cloneA, "from-a.md", "Alpha body.")
	gitRun(t, cloneA, "add", "-A")
	gitRun(t, cloneA, "commit", "-q", "-m", "add from-a")
	gitRun(t, cloneA, "push", "-q", "origin", "HEAD:main")

	cloneB := filepath.Join(base, "b")
	cloneAs(t, remote, cloneB, "Bob@Example.com")
	pathBShared := filepath.Join(cloneB, "raw", "articles", "from-a.md")
	pathBLocal := writeRawArticle(t, cloneB, "b-local.md", "Bravo body, not committed yet.")

	stub := &stubLLM{DefaultReply: ownerTestReply}
	installStubLLM(t, stub)

	// A (after its push) owns from-a.md.
	if err := contextualizeRawArticles(cloneA, 0, "haiku", false, false); err != nil {
		t.Fatal(err)
	}
	if n := len(stub.Calls()); n != 1 {
		t.Fatalf("clone A: LLM called %d times, want 1", n)
	}
	if !fileHasMarker(pathA, retrievalContextMarker) {
		t.Error("clone A did not contextualize the article it added")
	}

	// B skips A's article and contextualizes only its own uncommitted file.
	before, err := os.ReadFile(pathBShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := contextualizeRawArticles(cloneB, 0, "haiku", false, false); err != nil {
		t.Fatal(err)
	}
	if n := len(stub.Calls()); n != 2 {
		t.Fatalf("after clone B: LLM called %d times in total, want 2", n)
	}
	after, err := os.ReadFile(pathBShared)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("clone B rewrote an article that clone A added")
	}
	if !fileHasMarker(pathBLocal, retrievalContextMarker) {
		t.Error("clone B did not contextualize its own uncommitted article")
	}
}

// TestContextualizeRawArticles_NoEmailContextualizesAll: without a
// user.email there is no owner to compare, so today's behavior stays.
func TestContextualizeRawArticles_NoEmailContextualizesAll(t *testing.T) {
	isolateGitConfig(t)
	root := t.TempDir()
	gitRun(t, root, "init", "-q", "-b", "main")
	path := writeRawArticle(t, root, "other.md", "Body from someone else.")
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "-c", "user.email=other@example.com", "-c", "user.name=Other", "commit", "-q", "-m", "add")

	stub := &stubLLM{DefaultReply: ownerTestReply}
	installStubLLM(t, stub)
	if err := contextualizeRawArticles(root, 0, "haiku", false, false); err != nil {
		t.Fatal(err)
	}
	if n := len(stub.Calls()); n != 1 {
		t.Fatalf("LLM called %d times, want 1", n)
	}
	if !fileHasMarker(path, retrievalContextMarker) {
		t.Error("article was not contextualized with no user.email set")
	}
}

func TestParseRawArticleAdders_NewestAddWins(t *testing.T) {
	out := "\x00new@example.com\n\nre-added.md\n\x00old@example.com\n\nre-added.md\nonly-old.md\n"
	got := parseRawArticleAdders([]byte(out))
	if got["re-added.md"] != "new@example.com" || got["only-old.md"] != "old@example.com" || len(got) != 2 {
		t.Errorf("got %v", got)
	}
}

// TestContextualizeRawArticles_NoGitContextualizesAll: a KB that is not a git
// repo has no shared history to decide ownership, so git log fails and every
// article is contextualized, as before.
func TestContextualizeRawArticles_NoGitContextualizesAll(t *testing.T) {
	isolateGitConfig(t)
	if err := os.WriteFile(os.Getenv("GIT_CONFIG_GLOBAL"), []byte("[user]\n\temail = me@example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path := writeRawArticle(t, root, "solo.md", "Body in a KB with no git.")

	stub := &stubLLM{DefaultReply: ownerTestReply}
	installStubLLM(t, stub)
	if err := contextualizeRawArticles(root, 0, "haiku", false, false); err != nil {
		t.Fatal(err)
	}
	if n := len(stub.Calls()); n != 1 {
		t.Fatalf("LLM called %d times, want 1", n)
	}
	if !fileHasMarker(path, retrievalContextMarker) {
		t.Error("article was not contextualized in a KB without git")
	}
}
