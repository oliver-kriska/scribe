package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleProposals = `# Identity proposals — scanned 2026-06-01T00:00:00Z

### Jane Doe

- Existing page: people/jane-doe.md
- Surface forms:
  - jane@corp.example
  - @janedoe
- Confidence: high
- Suggested action: add-aliases

---

### Maybe Person

- Existing page: people/maybe.md
- Surface forms:
  - maybe@corp.example
- Confidence: low
- Suggested action: add-aliases

---

### New Face

- Surface forms:
  - new@corp.example
- Confidence: high
- Suggested action: create-new
`

func TestParseIdentityBlocks(t *testing.T) {
	blocks := parseIdentityBlocks(sampleProposals)
	// "New Face" has no Existing page line → dropped at flush.
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks, got %d: %+v", len(blocks), blocks)
	}

	jane := blocks[0]
	if jane.Page != "people/jane-doe.md" {
		t.Errorf("page = %q", jane.Page)
	}
	if jane.Action != "add-aliases" || !strings.EqualFold(jane.Confidence, "high") {
		t.Errorf("action/confidence = %q/%q", jane.Action, jane.Confidence)
	}
	if len(jane.SurfaceForms) != 2 || jane.SurfaceForms[0] != "jane@corp.example" || jane.SurfaceForms[1] != "@janedoe" {
		t.Errorf("surface forms = %v", jane.SurfaceForms)
	}

	if blocks[1].Page != "people/maybe.md" || !strings.EqualFold(blocks[1].Confidence, "low") {
		t.Errorf("second block parsed wrong: %+v", blocks[1])
	}
}

func TestParseIdentityBlocks_Resilience(t *testing.T) {
	t.Run("empty input", func(t *testing.T) {
		if got := parseIdentityBlocks(""); len(got) != 0 {
			t.Errorf("want no blocks, got %v", got)
		}
	})

	t.Run("page path with leading ./ normalized", func(t *testing.T) {
		body := "### X Y\n- Existing page: ./people/x.md\n- Suggested action: add-aliases\n"
		blocks := parseIdentityBlocks(body)
		if len(blocks) != 1 || blocks[0].Page != "people/x.md" {
			t.Errorf("got %+v", blocks)
		}
	})

	t.Run("non-indented line ends surface forms list", func(t *testing.T) {
		body := "### X Y\n- Existing page: people/x.md\n- Surface forms:\n  - one\nplain text line\n  - not-captured\n"
		blocks := parseIdentityBlocks(body)
		if len(blocks) != 1 {
			t.Fatalf("got %+v", blocks)
		}
		if len(blocks[0].SurfaceForms) != 1 || blocks[0].SurfaceForms[0] != "one" {
			t.Errorf("surface forms = %v, want just [one]", blocks[0].SurfaceForms)
		}
	})
}

func TestExistingAliases(t *testing.T) {
	tests := []struct {
		name string
		fm   string
		want []string
	}{
		{
			name: "inline array",
			fm:   "\ntitle: \"X\"\naliases: [a, \"B C\", 'd']\n",
			want: []string{"a", "b c", "d"},
		},
		{
			name: "block list",
			fm:   "\ntitle: \"X\"\naliases:\n  - a\n  - \"B C\"\nother: 1\n",
			want: []string{"a", "b c"},
		},
		{
			name: "no aliases key",
			fm:   "\ntitle: \"X\"\n",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := existingAliases(tt.fm)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for _, w := range tt.want {
				if !got[w] {
					t.Errorf("missing %q in %v", w, got)
				}
			}
		})
	}
}

func TestInsertOrExtendAliases(t *testing.T) {
	t.Run("creates key when absent", func(t *testing.T) {
		got := insertOrExtendAliases("\ntitle: \"X\"", []string{"new one"})
		if !strings.Contains(got, "aliases:\n  - \"new one\"") &&
			!strings.Contains(got, "aliases:\n  - new one") {
			t.Errorf("aliases block not appended:\n%s", got)
		}
	})

	t.Run("extends existing block list", func(t *testing.T) {
		fm := "\ntitle: \"X\"\naliases:\n  - old\nstatus: active"
		got := insertOrExtendAliases(fm, []string{"fresh"})
		oldIdx := strings.Index(got, "- old")
		freshIdx := strings.Index(got, "- fresh")
		statusIdx := strings.Index(got, "status: active")
		if oldIdx < 0 || freshIdx < 0 {
			t.Fatalf("entries missing:\n%s", got)
		}
		if oldIdx >= freshIdx || freshIdx >= statusIdx {
			t.Errorf("new alias not inserted inside the list:\n%s", got)
		}
	})

	t.Run("converts inline form to block form", func(t *testing.T) {
		fm := "\ntitle: \"X\"\naliases: [a, b]"
		got := insertOrExtendAliases(fm, []string{"c"})
		for _, want := range []string{"aliases:", "  - a", "  - b", "  - c"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in:\n%s", want, got)
			}
		}
		if strings.Contains(got, "[a, b]") {
			t.Errorf("inline form should be gone:\n%s", got)
		}
	})
}

func TestAppendAliasesToPeopleFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jane.md")
	content := "---\ntitle: \"Jane Doe\"\naliases: [jd]\n---\n\nBody.\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	added, err := appendAliasesToPeopleFile(path, []string{"jane@corp.example", "JD", ""}, false)
	if err != nil {
		t.Fatal(err)
	}
	// "JD" is a case-insensitive dup of jd; "" is dropped.
	if added != 1 {
		t.Errorf("added = %d, want 1", added)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "jane@corp.example") {
		t.Errorf("new alias not written:\n%s", data)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(data)), "Body.") {
		t.Errorf("body lost:\n%s", data)
	}

	// Idempotent: second pass adds nothing.
	added, err = appendAliasesToPeopleFile(path, []string{"jane@corp.example"}, false)
	if err != nil || added != 0 {
		t.Errorf("second pass added=%d err=%v, want 0/nil", added, err)
	}
}

func TestAppendAliasesToPeopleFile_DryRunAndErrors(t *testing.T) {
	dir := t.TempDir()

	t.Run("dry run reports but does not write", func(t *testing.T) {
		path := filepath.Join(dir, "dry.md")
		content := "---\ntitle: \"X\"\n---\n\nBody.\n"
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		added, err := appendAliasesToPeopleFile(path, []string{"new"}, true)
		if err != nil || added != 1 {
			t.Fatalf("added=%d err=%v", added, err)
		}
		data, _ := os.ReadFile(path)
		if string(data) != content {
			t.Errorf("dry run modified the file:\n%s", data)
		}
	})

	t.Run("no frontmatter", func(t *testing.T) {
		path := filepath.Join(dir, "plain.md")
		if err := os.WriteFile(path, []byte("just text"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := appendAliasesToPeopleFile(path, []string{"x"}, false); err == nil {
			t.Error("want error for missing frontmatter")
		}
	})

	t.Run("unclosed frontmatter", func(t *testing.T) {
		path := filepath.Join(dir, "unclosed.md")
		if err := os.WriteFile(path, []byte("---\ntitle: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := appendAliasesToPeopleFile(path, []string{"x"}, false); err == nil {
			t.Error("want error for unclosed frontmatter")
		}
	})
}

func TestRunApplyIdentities(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("domains: [acme]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCRIBE_KB", root)
	writeKBFile(t, root, "people/jane-doe.md",
		"---\ntitle: \"Jane Doe\"\n---\n\nBody.\n")
	// people/maybe.md exists too, but its block is low-confidence.
	writeKBFile(t, root, "people/maybe.md",
		"---\ntitle: \"Maybe Person\"\n---\n\nBody.\n")
	writeKBFile(t, root, "wiki/_identity-proposals.md", sampleProposals)

	var err error
	out := captureLintStdout(t, func() { err = runApplyIdentities("", false, false) })
	if err != nil {
		t.Fatalf("runApplyIdentities: %v", err)
	}
	if !strings.Contains(out, "touched 1 file(s)") {
		t.Errorf("expected 1 touched file:\n%s", out)
	}

	jane, _ := os.ReadFile(filepath.Join(root, "people", "jane-doe.md"))
	for _, want := range []string{"jane@corp.example", "@janedoe"} {
		if !strings.Contains(string(jane), want) {
			t.Errorf("jane missing alias %q:\n%s", want, jane)
		}
	}
	maybe, _ := os.ReadFile(filepath.Join(root, "people", "maybe.md"))
	if strings.Contains(string(maybe), "maybe@corp.example") {
		t.Errorf("low-confidence block applied without --apply-low:\n%s", maybe)
	}

	// --apply-low picks up the low-confidence block on a second pass.
	out = captureLintStdout(t, func() { err = runApplyIdentities("", true, false) })
	if err != nil {
		t.Fatalf("apply-low pass: %v", err)
	}
	maybe, _ = os.ReadFile(filepath.Join(root, "people", "maybe.md"))
	if !strings.Contains(string(maybe), "maybe@corp.example") {
		t.Errorf("--apply-low did not apply:\n%s", maybe)
	}
	// Jane's block is now a no-op — only maybe.md counts as touched.
	if !strings.Contains(out, "touched 1 file(s)") {
		t.Errorf("re-run should be idempotent for already-applied blocks:\n%s", out)
	}
}

func TestRunApplyIdentities_MissingInputs(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("domains: [acme]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCRIBE_KB", root)

	t.Run("no proposals file", func(t *testing.T) {
		err := runApplyIdentities("", false, false)
		if err == nil || !strings.Contains(err.Error(), "no identity proposals") {
			t.Errorf("want guidance error, got %v", err)
		}
	})

	t.Run("page not found is skipped, not fatal", func(t *testing.T) {
		writeKBFile(t, root, "wiki/_identity-proposals.md",
			"### Ghost Person\n- Existing page: people/ghost.md\n- Surface forms:\n  - g@x.io\n- Confidence: high\n- Suggested action: add-aliases\n")
		var err error
		out := captureLintStdout(t, func() { err = runApplyIdentities("", false, false) })
		if err != nil {
			t.Fatalf("missing page should skip, not fail: %v", err)
		}
		if !strings.Contains(out, "touched 0 file(s), skipped 1") {
			t.Errorf("expected skip accounting:\n%s", out)
		}
	})
}

func TestDryRunSuffix(t *testing.T) {
	if got := dryRunSuffix(false); got != "" {
		t.Errorf("got %q", got)
	}
	if got := dryRunSuffix(true); !strings.Contains(got, "dry-run") {
		t.Errorf("got %q", got)
	}
}
