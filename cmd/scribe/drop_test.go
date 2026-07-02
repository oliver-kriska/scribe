// drop_test.go — coverage for `scribe drop`: schema validation, path
// resolution (cwd vs KB root), collision handling, and frontmatter
// correctness. Mirrors write_test.go's structure and reuses its
// testKBRoot fixture — drop tests additionally t.Chdir to a *separate*
// "project" tempdir, since scribe drop writes relative to cwd, never to
// SCRIBE_KB (see docs/issue-21-drop-authoring-cli-plan.md §2.2/§4).
package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dropTestEnv builds a KB (testKBRoot) plus a distinct "project" tempdir
// and chdirs into the project dir — the shape every real `scribe drop`
// invocation runs in: SCRIBE_KB resolves the KB for name/domain lookup,
// but cwd is some unrelated project the file actually lands in.
func dropTestEnv(t *testing.T) (kbRoot, projectDir string) {
	t.Helper()
	kbRoot = testKBRoot(t)
	projectDir = t.TempDir()
	t.Chdir(projectDir)
	return kbRoot, projectDir
}

// dropUnresolvedKBEnv sandboxes kbDir() so it cannot resolve any KB: no
// --root, no SCRIBE_KB, no user-config default, and cwd sits in a bare
// tempdir with no scribe.yaml in its parent chain. Mirrors
// TestKBDirResolution's isolation (config_test.go:18-45) — the "nowhere to
// resolve" branch, replicated here since that helper is a closure local to
// TestKBDirResolution and not reusable across files.
func dropUnresolvedKBEnv(t *testing.T) string {
	t.Helper()
	saved := globalRoot
	globalRoot = ""
	t.Cleanup(func() { globalRoot = saved })
	t.Setenv("SCRIBE_KB", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

// captureDropStderr runs fn with os.Stderr redirected into a pipe and
// returns everything written to it. Mirrors captureStdoutErr
// (scan_run_test.go) but for stderr, which DropCmd.Run uses for its
// domain/inside-KB warnings.
func captureDropStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stderr = orig
	out := <-done
	_ = r.Close()
	return out
}

// dropTarget computes the expected drop-file path — the same construction
// DropCmd.Run uses internally — so tests read as assertions about
// behavior, not path arithmetic.
func dropTarget(projectDir, kbNameStr, date, slug string) string {
	return filepath.Join(projectDir, ".claude", kbNameStr, date+"-"+slug+".md")
}

func TestDropRun_CreateBasic(t *testing.T) {
	kbRoot, projectDir := dropTestEnv(t)

	d := &DropCmd{
		Title:  "My Reusable Insight",
		Type:   "pattern",
		Domain: "acme",
		Action: "create",
		Tags:   []string{"a", "b"},
		Body:   "This is the body of the drop file.",
	}
	if err := d.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	kbNameExpected := kbName(kbRoot)
	today := time.Now().UTC().Format("2006-01-02")
	target := dropTarget(projectDir, kbNameExpected, today, "my-reusable-insight")

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("drop file not at expected path %s: %v", target, err)
	}

	raw, err := parseFrontmatterRaw(data)
	if err != nil {
		t.Fatalf("parse frontmatter: %v\n%s", err, data)
	}
	if v, _ := raw[kbNameExpected].(bool); !v {
		t.Errorf("expected %q: true in frontmatter, got %v", kbNameExpected, raw[kbNameExpected])
	}
	if raw["action"] != "create" {
		t.Errorf("action = %v, want create", raw["action"])
	}
	if raw["title"] != "My Reusable Insight" {
		t.Errorf("title = %v", raw["title"])
	}
	if raw["type"] != "pattern" {
		t.Errorf("type = %v", raw["type"])
	}
	if raw["domain"] != "acme" {
		t.Errorf("domain = %v", raw["domain"])
	}
	tags, ok := raw["tags"].([]any)
	if !ok || len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
		t.Errorf("tags = %v", raw["tags"])
	}
	if !strings.Contains(string(data), "This is the body of the drop file.") {
		t.Errorf("body missing from %s", data)
	}
}

func TestDropRun_MissingTitle(t *testing.T) {
	dropTestEnv(t)
	d := &DropCmd{Type: "pattern", Domain: "acme", Action: "create", Body: "body"}
	err := d.Run()
	if err == nil || !strings.Contains(err.Error(), "--title is required") {
		t.Errorf("expected --title required error, got: %v", err)
	}
}

func TestDropRun_InvalidType(t *testing.T) {
	dropTestEnv(t)
	d := &DropCmd{Title: "T", Type: "bogus", Domain: "acme", Action: "create", Body: "body"}
	err := d.Run()
	if err == nil {
		t.Fatal("expected invalid type error")
	}
	if !strings.Contains(err.Error(), "invalid --type") || !strings.Contains(err.Error(), "idea") {
		t.Errorf("error should list valid types incl. idea, got: %v", err)
	}
}

func TestDropRun_InvalidDomainKBResolved(t *testing.T) {
	_, projectDir := dropTestEnv(t)
	d := &DropCmd{Title: "T", Type: "pattern", Domain: "nonexistent", Action: "create", Body: "body"}
	err := d.Run()
	if err == nil || !strings.Contains(err.Error(), "invalid --domain") {
		t.Errorf("expected invalid domain error, got: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(projectDir, ".claude")); len(entries) != 0 {
		t.Errorf("no file should have been written on domain validation failure")
	}
}

func TestDropRun_FreeformDomainKBUnresolved(t *testing.T) {
	projectDir := dropUnresolvedKBEnv(t)
	d := &DropCmd{Title: "Freeform Domain Note", Type: "pattern", Domain: "whatever-domain", Action: "create", KBName: "myproj", Body: "body text"}
	stderr := captureDropStderr(t, func() {
		if err := d.Run(); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})
	if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, "whatever-domain") {
		t.Errorf("expected domain warning in stderr, got: %s", stderr)
	}
	today := time.Now().UTC().Format("2006-01-02")
	target := dropTarget(projectDir, "myproj", today, "freeform-domain-note")
	if !fileExists(target) {
		t.Errorf("expected drop file at %s", target)
	}
}

func TestDropRun_KBUnresolvedNoKBName(t *testing.T) {
	dropUnresolvedKBEnv(t)
	d := &DropCmd{Title: "T", Type: "pattern", Domain: "general", Action: "create", Body: "body"}
	err := d.Run()
	if err == nil || !strings.Contains(err.Error(), "--kb-name") {
		t.Errorf("expected --kb-name required error, got: %v", err)
	}
}

func TestDropRun_RollingTargetRequiresAppend(t *testing.T) {
	dropTestEnv(t)
	d := &DropCmd{Title: "T", Type: "project", Domain: "acme", Action: "create", RollingTarget: "learnings", Body: "body"}
	err := d.Run()
	if err == nil || !strings.Contains(err.Error(), "requires --action append") {
		t.Errorf("expected rolling-target/action mismatch error, got: %v", err)
	}
}

func TestDropRun_InvalidRollingTarget(t *testing.T) {
	dropTestEnv(t)
	d := &DropCmd{Title: "T", Type: "project", Domain: "acme", Action: "append", RollingTarget: "bogus", Body: "body"}
	err := d.Run()
	if err == nil || !strings.Contains(err.Error(), "learnings, decisions-log") {
		t.Errorf("expected valid rolling-target list in error, got: %v", err)
	}
}

func TestDropRun_InvalidAction(t *testing.T) {
	dropTestEnv(t)
	d := &DropCmd{Title: "T", Type: "pattern", Domain: "acme", Action: "delete", Body: "body"}
	err := d.Run()
	if err == nil || !strings.Contains(err.Error(), "create, update, append") {
		t.Errorf("expected valid action list in error, got: %v", err)
	}
}

func TestDropRun_InvalidDateFormat(t *testing.T) {
	dropTestEnv(t)
	d := &DropCmd{Title: "T", Type: "pattern", Domain: "acme", Action: "create", Date: "2026-5-7", Body: "body"}
	err := d.Run()
	if err == nil || !strings.Contains(err.Error(), "YYYY-MM-DD") {
		t.Errorf("expected date format error, got: %v", err)
	}
}

func TestDropRun_SlugOverride(t *testing.T) {
	kbRoot, projectDir := dropTestEnv(t)
	d := &DropCmd{Title: "Some Title That Would Slugify Differently", Type: "pattern", Domain: "acme", Action: "create", Slug: "custom-slug", Body: "body"}
	if err := d.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	kbNameExpected := kbName(kbRoot)
	today := time.Now().UTC().Format("2006-01-02")
	target := dropTarget(projectDir, kbNameExpected, today, "custom-slug")
	if !fileExists(target) {
		t.Errorf("expected file at %s", target)
	}
}

func TestDropRun_DateOverride(t *testing.T) {
	kbRoot, projectDir := dropTestEnv(t)
	d := &DropCmd{Title: "Date Override Note", Type: "pattern", Domain: "acme", Action: "create", Date: "2020-01-01", Body: "body"}
	if err := d.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	kbNameExpected := kbName(kbRoot)
	target := dropTarget(projectDir, kbNameExpected, "2020-01-01", "date-override-note")
	if !fileExists(target) {
		t.Errorf("expected file at %s", target)
	}
}

func TestDropRun_CollisionRefused(t *testing.T) {
	kbRoot, projectDir := dropTestEnv(t)

	d1 := &DropCmd{Title: "Same Title", Type: "pattern", Domain: "acme", Action: "create", Date: "2026-01-01", Body: "first body"}
	if err := d1.Run(); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	kbNameExpected := kbName(kbRoot)
	target := dropTarget(projectDir, kbNameExpected, "2026-01-01", "same-title")
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read after first run: %v", err)
	}

	d2 := &DropCmd{Title: "Same Title", Type: "pattern", Domain: "acme", Action: "create", Date: "2026-01-01", Body: "second body"}
	err = d2.Run()
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected collision error, got: %v", err)
	}

	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read after second run: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("first file was modified by the refused second run:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestDropRun_CollisionForced(t *testing.T) {
	kbRoot, projectDir := dropTestEnv(t)

	d1 := &DropCmd{Title: "Same Title", Type: "pattern", Domain: "acme", Action: "create", Date: "2026-01-01", Body: "first body"}
	if err := d1.Run(); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	d2 := &DropCmd{Title: "Same Title", Type: "pattern", Domain: "acme", Action: "create", Date: "2026-01-01", Body: "second body", Force: true}
	if err := d2.Run(); err != nil {
		t.Fatalf("forced second Run: %v", err)
	}

	kbNameExpected := kbName(kbRoot)
	target := dropTarget(projectDir, kbNameExpected, "2026-01-01", "same-title")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read after forced run: %v", err)
	}
	if !strings.Contains(string(data), "second body") {
		t.Errorf("expected second run's body, got:\n%s", data)
	}
	if strings.Contains(string(data), "first body") {
		t.Errorf("first run's body should have been overwritten:\n%s", data)
	}
}

func TestDropRun_EmptyBody(t *testing.T) {
	dropTestEnv(t)
	d := &DropCmd{Title: "T", Type: "pattern", Domain: "acme", Action: "create", Body: ""}
	err := d.Run()
	if err == nil || !strings.Contains(err.Error(), "body is empty") {
		t.Errorf("expected empty-body error, got: %v", err)
	}
}

func TestDropRun_BodyFromFile(t *testing.T) {
	kbRoot, projectDir := dropTestEnv(t)
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(bodyFile, []byte("Body loaded from a file.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &DropCmd{Title: "Body From File", Type: "pattern", Domain: "acme", Action: "create", Body: "file:" + bodyFile}
	if err := d.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	kbNameExpected := kbName(kbRoot)
	today := time.Now().UTC().Format("2006-01-02")
	target := dropTarget(projectDir, kbNameExpected, today, "body-from-file")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read drop file: %v", err)
	}
	if !strings.Contains(string(data), "Body loaded from a file.") {
		t.Errorf("file body missing from drop file:\n%s", data)
	}
}

func TestDropRun_BodyFromStdin(t *testing.T) {
	kbRoot, projectDir := dropTestEnv(t)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })

	go func() {
		_, _ = w.WriteString("Body piped in from stdin.")
		_ = w.Close()
	}()

	d := &DropCmd{Title: "Body From Stdin", Type: "pattern", Domain: "acme", Action: "create", Body: "-"}
	if err := d.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	kbNameExpected := kbName(kbRoot)
	today := time.Now().UTC().Format("2006-01-02")
	target := dropTarget(projectDir, kbNameExpected, today, "body-from-stdin")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read drop file: %v", err)
	}
	if !strings.Contains(string(data), "Body piped in from stdin.") {
		t.Errorf("stdin body missing from drop file:\n%s", data)
	}
}

func TestDropRun_TagsCommaAndRepeat(t *testing.T) {
	kbRoot, projectDir := dropTestEnv(t)
	d := &DropCmd{Title: "Tags Test", Type: "pattern", Domain: "acme", Action: "create", Tags: []string{"a,b", "c"}, Body: "body"}
	if err := d.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	kbNameExpected := kbName(kbRoot)
	today := time.Now().UTC().Format("2006-01-02")
	target := dropTarget(projectDir, kbNameExpected, today, "tags-test")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := parseFrontmatterRaw(data)
	if err != nil {
		t.Fatalf("parse frontmatter: %v", err)
	}
	tags, ok := raw["tags"].([]any)
	if !ok || len(tags) != 3 {
		t.Fatalf("tags = %v, want 3 entries", raw["tags"])
	}
	want := []string{"a", "b", "c"}
	for i, wantTag := range want {
		if tags[i] != wantTag {
			t.Errorf("tags[%d] = %v, want %q", i, tags[i], wantTag)
		}
	}
}

func TestDropRun_KBNameKeyQuotedWhenUnsafe(t *testing.T) {
	projectDir := dropUnresolvedKBEnv(t)
	d := &DropCmd{Title: "Weird KB Name Note", Type: "pattern", Domain: "general", Action: "create", KBName: "weird name", Body: "body"}
	if err := d.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	today := time.Now().UTC().Format("2006-01-02")
	target := dropTarget(projectDir, "weird name", today, "weird-kb-name-note")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read drop file: %v", err)
	}
	if !strings.Contains(string(data), `"weird name": true`) {
		t.Errorf("expected quoted kb-name key in frontmatter:\n%s", data)
	}
	raw, err := parseFrontmatterRaw(data)
	if err != nil {
		t.Fatalf("parse frontmatter: %v", err)
	}
	if v, _ := raw["weird name"].(bool); !v {
		t.Errorf(`expected "weird name": true in parsed frontmatter, got %v`, raw["weird name"])
	}
}

func TestDropRun_WarnsInsideKB(t *testing.T) {
	kbRoot := testKBRoot(t)
	t.Chdir(kbRoot)

	d := &DropCmd{Title: "Inside KB Note", Type: "pattern", Domain: "acme", Action: "create", Body: "body"}
	stderr := captureDropStderr(t, func() {
		if err := d.Run(); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})
	if !strings.Contains(stderr, "inside the KB") {
		t.Errorf("expected inside-KB warning, got: %s", stderr)
	}

	kbNameExpected := kbName(kbRoot)
	today := time.Now().UTC().Format("2006-01-02")
	target := dropTarget(kbRoot, kbNameExpected, today, "inside-kb-note")
	if !fileExists(target) {
		t.Errorf("expected drop file still written at %s (warning is non-fatal)", target)
	}
}

func TestDropRun_DryRun(t *testing.T) {
	_, projectDir := dropTestEnv(t)
	d := &DropCmd{Title: "Dry Run Note", Type: "pattern", Domain: "acme", Action: "create", Body: "dry body", DryRun: true}
	stdout, err := captureStdoutErr(t, func() error { return d.Run() })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(stdout, "action: create") || !strings.Contains(stdout, "dry body") {
		t.Errorf("dry-run stdout missing expected content: %s", stdout)
	}
	if dirExists(filepath.Join(projectDir, ".claude")) {
		t.Errorf("dry-run must not create %s", filepath.Join(projectDir, ".claude"))
	}
}

func TestReadBodySource_StdinFileInline(t *testing.T) {
	t.Run("inline literal", func(t *testing.T) {
		got, err := readBodySource("just some text")
		if err != nil {
			t.Fatal(err)
		}
		if got != "just some text" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("file prefix", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "body.txt")
		if err := os.WriteFile(f, []byte("from a file"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := readBodySource("file:" + f)
		if err != nil {
			t.Fatal(err)
		}
		if got != "from a file" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("stdin", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		orig := os.Stdin
		os.Stdin = r
		t.Cleanup(func() { os.Stdin = orig })
		go func() {
			_, _ = w.WriteString("from stdin")
			_ = w.Close()
		}()
		got, err := readBodySource("-")
		if err != nil {
			t.Fatal(err)
		}
		if got != "from stdin" {
			t.Errorf("got %q", got)
		}
	})
}
