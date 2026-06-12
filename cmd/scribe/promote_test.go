package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupPromoteKBs builds a source KB (git, so contributor resolves) and
// a target KB, both with scribe.yaml markers, and points SCRIBE_KB at
// the source.
func setupPromoteKBs(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	src := initTestGitRepo(t, "Promoter")
	writeKBFile(t, src, "scribe.yaml", "kb_name: srckb\ndomains:\n  - backend\n")
	writeKBFile(t, src, "patterns/cool-trick.md",
		"---\ntitle: \"Cool Trick\"\ntype: pattern\ndomain: backend\n---\n\nUses [[Missing Thing]] heavily.\n")
	t.Setenv("SCRIBE_KB", src)

	target := initTestGitRepo(t, "Team Bot")
	writeKBFile(t, target, "scribe.yaml", "kb_name: teamkb\ndomains:\n  - backend\n  - infra\n")
	return src, target
}

func TestPromoteCopiesWithProvenance(t *testing.T) {
	_, target := setupPromoteKBs(t)

	c := &PromoteCmd{Article: "patterns/cool-trick.md", To: target}
	if err := c.Run(); err != nil {
		t.Fatalf("promote: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(target, "patterns", "cool-trick.md"))
	if err != nil {
		t.Fatalf("promoted article missing: %v", err)
	}
	out := string(data)
	for _, want := range []string{
		"promoted_from: 'srckb'",
		"promoted_at: '",
		"contributor: 'Promoter'",
		"title: \"Cool Trick\"",
		"Uses [[Missing Thing]] heavily.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("promoted copy missing %q:\n%s", want, out)
		}
	}

	// Auto-committed in the target.
	log := runCmd(target, "git", "log", "--oneline")
	if !strings.Contains(log, "promote: Cool Trick from srckb") {
		t.Errorf("target commit missing; log:\n%s", log)
	}

	// Source untouched.
	src, _ := os.ReadFile(filepath.Join(os.Getenv("SCRIBE_KB"), "patterns", "cool-trick.md"))
	if strings.Contains(string(src), "promoted_from") {
		t.Error("source article was modified")
	}
}

func TestPromoteRefusesOverwriteWithoutForce(t *testing.T) {
	_, target := setupPromoteKBs(t)
	writeKBFile(t, target, "patterns/cool-trick.md", "---\ntitle: Existing\n---\n\nalready here\n")

	c := &PromoteCmd{Article: "patterns/cool-trick.md", To: target, NoGit: true}
	if err := c.Run(); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("expected overwrite refusal, got %v", err)
	}

	c.Force = true
	if err := c.Run(); err != nil {
		t.Fatalf("promote --force: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(target, "patterns", "cool-trick.md"))
	if !strings.Contains(string(data), "Cool Trick") {
		t.Error("force overwrite did not replace content")
	}
}

func TestPromoteRedomainsCopy(t *testing.T) {
	_, target := setupPromoteKBs(t)

	c := &PromoteCmd{Article: "patterns/cool-trick.md", To: target, Domain: "infra", NoGit: true}
	if err := c.Run(); err != nil {
		t.Fatalf("promote --domain: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(target, "patterns", "cool-trick.md"))
	if !strings.Contains(string(data), "domain: infra") {
		t.Errorf("copy not re-domained:\n%s", data)
	}
}

func TestPromoteValidation(t *testing.T) {
	src, target := setupPromoteKBs(t)

	// Target not a KB.
	c := &PromoteCmd{Article: "patterns/cool-trick.md", To: t.TempDir()}
	if err := c.Run(); err == nil || !strings.Contains(err.Error(), "not a scribe KB") {
		t.Errorf("non-KB target accepted: %v", err)
	}

	// Target == source.
	c = &PromoteCmd{Article: "patterns/cool-trick.md", To: src}
	if err := c.Run(); err == nil || !strings.Contains(err.Error(), "current KB") {
		t.Errorf("self-promotion accepted: %v", err)
	}

	// Path outside wiki content dirs.
	writeKBFile(t, src, "output/scratch.md", "---\ntitle: Scratch\n---\n\nx\n")
	c = &PromoteCmd{Article: "output/scratch.md", To: target}
	if err := c.Run(); err == nil || !strings.Contains(err.Error(), "wiki content dir") {
		t.Errorf("non-wiki path accepted: %v", err)
	}

	// Missing article.
	c = &PromoteCmd{Article: "patterns/nope.md", To: target}
	if err := c.Run(); err == nil {
		t.Error("missing article accepted")
	}
}

// TestPromoteRefusesDerivedFiles covers the guard against promoting
// scribe-managed/derived files: the topDir check alone accepts
// wiki/_index.md, and --force would then overwrite the target KB's own
// derived artifacts with a foreign stale copy.
func TestPromoteRefusesDerivedFiles(t *testing.T) {
	src, target := setupPromoteKBs(t)

	// Registry-known derived artifacts (special_files.go).
	for _, rel := range []string{"wiki/_index.md", "wiki/_backlinks.json", "wiki/_digest.md"} {
		writeKBFile(t, src, rel, "stale derived content\n")
		c := &PromoteCmd{Article: rel, To: target, Force: true, NoGit: true}
		if err := c.Run(); err == nil || !strings.Contains(err.Error(), "scribe-managed") {
			t.Errorf("%s: expected scribe-managed refusal, got %v", rel, err)
		}
	}

	// Underscore convention beyond the registry.
	for _, rel := range []string{"wiki/_hot.md", "wiki/_sessions_log.json", "patterns/_scratch.md"} {
		writeKBFile(t, src, rel, "derived\n")
		c := &PromoteCmd{Article: rel, To: target, Force: true, NoGit: true}
		if err := c.Run(); err == nil || !strings.Contains(err.Error(), "must not be promoted") {
			t.Errorf("%s: expected underscore refusal, got %v", rel, err)
		}
	}

	// Nothing landed in the target.
	for _, rel := range []string{"wiki/_index.md", "wiki/_hot.md"} {
		if fileExists(filepath.Join(target, rel)) {
			t.Errorf("%s was written to the target despite the refusal", rel)
		}
	}

	// Underscore-prefixed DIRECTORIES are fine — only the filename counts.
	writeKBFile(t, src, "patterns/sub/real-article.md",
		"---\ntitle: \"Real Article\"\ntype: pattern\ndomain: backend\n---\n\nbody\n")
	c := &PromoteCmd{Article: "patterns/sub/real-article.md", To: target, NoGit: true}
	if err := c.Run(); err != nil {
		t.Errorf("regular nested article refused: %v", err)
	}
}

func TestMissingWikilinksIn(t *testing.T) {
	target := t.TempDir()
	writeKBFile(t, target, "wiki/known.md", "---\ntitle: \"Known Thing\"\naliases:\n  - \"KT\"\n---\n\nbody\n")

	content := []byte("Links: [[Known Thing]], [[KT]], [[Unknown One]], [[Unknown One]]\n")
	missing := missingWikilinksIn(target, content)
	if len(missing) != 1 || missing[0] != "[[Unknown One]]" {
		t.Errorf("missing = %v, want [[[Unknown One]]]", missing)
	}
}
