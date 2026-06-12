package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestTemplateConfigSegments(t *testing.T) {
	segs := templateConfigSegments()
	if len(segs) == 0 {
		t.Fatal("no segments parsed from embedded template")
	}
	byKey := map[string]templateSegment{}
	for _, s := range segs {
		if s.key == "" {
			t.Errorf("segment with empty key: %v", s.lines)
		}
		if strings.Contains(strings.Join(s.lines, "\n"), "{{") {
			t.Errorf("segment %q leaked template directives", s.key)
		}
		byKey[s.key] = s
	}
	// The blocks this command exists for must survive parsing. `sources`
	// comes from the fallback list: its template block lives inside
	// {{if}} directives, which the parser drops — without the fallback,
	// allowed_remotes (the team discovery gate) could never be surfaced
	// to pre-existing KBs.
	for _, want := range []string{"owners", "secret_scan", "subscriptions", "sources"} {
		seg, ok := byKey[want]
		if !ok {
			t.Errorf("missing segment %q", want)
			continue
		}
		// Doc comments above the key must travel with it.
		if len(seg.lines) < 2 {
			t.Errorf("segment %q has no docs: %v", want, seg.lines)
		}
	}
	if !strings.Contains(strings.Join(byKey["sources"].lines, "\n"), "allowed_remotes") {
		t.Error("sources fallback segment lost allowed_remotes documentation")
	}
}

func TestConfigMentionsKeyGenerousComments(t *testing.T) {
	for _, content := range []string{
		"sources:\n", "# sources:\n", "#  sources:\n", "## sources:\n", "#sources:\n",
	} {
		if !configMentionsKey(content, "sources") {
			t.Errorf("mention not detected in %q", content)
		}
	}
	// Indented sub-keys are NOT top-level mentions.
	if configMentionsKey("capture:\n  sources:\n", "sources") {
		t.Error("indented sub-key counted as a top-level mention")
	}
}

func TestMissingTemplateBlocks(t *testing.T) {
	missing := missingTemplateBlocks("owner_name: x\n")
	keys := make([]string, 0, len(missing))
	for _, seg := range missing {
		keys = append(keys, seg.key)
	}
	joined := strings.Join(keys, ",")
	for _, want := range []string{"owners", "secret_scan", "subscriptions"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s not reported missing on minimal config (got %s)", want, joined)
		}
	}

	// Mentioned keys — active or commented — are never re-appended.
	content := "owners:\n  backend: Alice\n# secret_scan:\n#   disable: false\n"
	for _, seg := range missingTemplateBlocks(content) {
		if seg.key == "owners" || seg.key == "secret_scan" {
			t.Errorf("%s reported missing despite being mentioned", seg.key)
		}
	}
}

func TestAppendTemplateBlocksIsCommentedAndValid(t *testing.T) {
	original := "owner_name: x\ndomains:\n  - backend\n"
	out := appendTemplateBlocks(original, missingTemplateBlocks(original))

	if !strings.HasPrefix(out, original) {
		t.Fatal("existing content was rewritten, not appended to")
	}
	for _, line := range strings.Split(strings.TrimPrefix(out, original), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			t.Errorf("appended line not commented: %q", line)
		}
	}
	// Appending must never change what the file parses to.
	var before, after ScribeConfig
	if err := yaml.Unmarshal([]byte(original), &before); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(out), &after); err != nil {
		t.Fatalf("appended file no longer parses: %v", err)
	}
	if before.OwnerName != after.OwnerName || len(before.Domains) != len(after.Domains) {
		t.Error("append changed parsed config values")
	}
}

// captureStdout swaps os.Stdout for a pipe while fn runs and returns
// everything written. A concurrent reader keeps fn from blocking on the
// pipe buffer (runBootstrap prints a multi-KB plan).
func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(&buf, r)
	}()
	runErr := fn()
	os.Stdout = orig
	_ = w.Close()
	<-done
	if runErr != nil {
		t.Fatalf("command failed: %v\noutput:\n%s", runErr, buf.String())
	}
	return buf.String()
}

// TestConfigUpdateFreshScaffoldNothingMissing automates half of PR #1's
// manual verification #2: against a KB scaffolded today (the real init
// path, embedded templates), `scribe config update --check` must report
// nothing missing — every reported block must be GENUINELY missing, so
// a fresh file yields zero false positives.
func TestConfigUpdateFreshScaffoldNothingMissing(t *testing.T) {
	if testing.Short() {
		t.Skip("scaffolds a full KB")
	}
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_CONFIG_HOME", "")
	kb := filepath.Join(t.TempDir(), "fresh-kb")
	c := &InitCmd{Path: kb, Yes: true, NoCron: true, NoGit: true, Domains: []string{"general"}}
	_ = captureStdout(t, c.runBootstrap) // scaffold output not under test

	t.Setenv("SCRIBE_KB", kb)
	path := filepath.Join(kb, "scribe.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("scaffold did not write scribe.yaml: %v", err)
	}

	out := captureStdout(t, (&ConfigUpdateCmd{Check: true}).Run)
	if !strings.Contains(out, "already documents every current option") {
		t.Errorf("fresh scaffold reported missing blocks (false positives):\n%s", out)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Error("--check modified a fresh scaffold")
	}
}

// TestConfigUpdateCheckListsOnlyGenuinelyMissing pins the other half of
// manual verification #2 at the Run() level: keys the user file already
// mentions (active OR commented) must not be listed or re-appended; the
// appended tail must be commented documentation only, after the
// untouched original content.
func TestConfigUpdateCheckListsOnlyGenuinelyMissing(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SCRIBE_KB", root)
	path := filepath.Join(root, "scribe.yaml")
	original := "owner_name: x\nowners:\n  backend: Alice\n# secret_scan:\n#   disable: false\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, (&ConfigUpdateCmd{Check: true}).Run)
	if !strings.Contains(out, "would append") {
		t.Fatalf("--check did not report pending blocks:\n%s", out)
	}
	for _, mentioned := range []string{"owners", "secret_scan"} {
		if strings.Contains(out, mentioned) {
			t.Errorf("--check listed %q although the file already mentions it:\n%s", mentioned, out)
		}
	}
	for _, missing := range []string{"subscriptions", "sources"} {
		if !strings.Contains(out, missing) {
			t.Errorf("--check did not list genuinely missing block %q:\n%s", missing, out)
		}
	}

	// Real run: append-only commented tail covering exactly the missing set.
	captureStdout(t, (&ConfigUpdateCmd{}).Run)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), original) {
		t.Fatal("update rewrote existing content instead of appending")
	}
	tail := strings.TrimPrefix(string(data), original)
	if !strings.Contains(tail, "appended by `scribe config update`") {
		t.Error("appended tail missing the provenance header")
	}
	for _, line := range strings.Split(tail, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			t.Errorf("appended tail has an active (uncommented) line: %q", line)
		}
	}
	if configMentionsKey(tail, "owners") || configMentionsKey(tail, "secret_scan") {
		t.Error("update re-appended a block the file already mentioned")
	}
	if !configMentionsKey(tail, "subscriptions") || !configMentionsKey(tail, "sources") {
		t.Errorf("update did not append the missing blocks; tail:\n%s", tail)
	}
}

func TestConfigUpdateCmdEndToEnd(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SCRIBE_KB", root)
	path := filepath.Join(root, "scribe.yaml")
	original := "owner_name: x\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	// --check writes nothing.
	if err := (&ConfigUpdateCmd{Check: true}).Run(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != original {
		t.Fatal("--check modified the file")
	}

	// Real run appends.
	if err := (&ConfigUpdateCmd{}).Run(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "# secret_scan:") {
		t.Error("secret_scan block not appended")
	}

	// Idempotent: a second run finds nothing missing.
	if err := (&ConfigUpdateCmd{}).Run(); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path)
	if string(again) != string(data) {
		t.Error("second run changed the file — not idempotent")
	}
}
