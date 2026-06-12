// scan_run_test.go — driver tests for ScanCmd.Run. Scan is the one
// gocognit-exempt driver with no LLM call of its own (it renders the
// context packet other LLM passes consume), so the tests pin the
// report's section contract against fixture projects: stack detection,
// knowledge-tree filtering, root-doc inlining, KB linkage, and drop-file
// listing. Stdout is the function's only output channel, so the tests
// capture it.
package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout runs fn while capturing everything written to
// os.Stdout, returning the captured text and fn's error.
func captureStdoutErr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	runErr := fn()
	_ = w.Close()
	os.Stdout = old
	out := <-done
	_ = r.Close()
	return out, runErr
}

// scanProject builds a fixture Go project under a stable directory name.
func scanProject(t *testing.T, name string) string {
	t.Helper()
	proj := filepath.Join(t.TempDir(), name)
	files := map[string]string{
		"go.mod":           "module example.test/scanfix\n\ngo 1.24\n",
		"Makefile":         "build:\n\ttrue\n",
		"README.md":        "# Scan Fixture\n\nWe chose pgvector over qdrant because the stack already runs postgres.\n",
		"CLAUDE.md":        "# Agent guide\n\nShort guide body.\n",
		"NOTES.md":         "# Ad-hoc notes\n\nA loose root doc.\n",
		"docs/notes.md":    "# Docs\n\nThe importer is 3x faster after batching.\n",
		"test/excluded.md": "# Should not appear\n\nfixture noise\n",
	}
	for rel, content := range files {
		full := filepath.Join(proj, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return proj
}

// TestScanRun_ReportSectionsForGoProject: the full report renders every
// section for a plain (non-git, KB-unlinked) Go project, includes the
// knowledge files, and filters the /test/ noise.
func TestScanRun_ReportSectionsForGoProject(t *testing.T) {
	stubHarnessKB(t, "kb_name: stubkb\n")
	proj := scanProject(t, "scanfix")

	out, err := captureStdoutErr(t, (&ScanCmd{Path: proj}).Run)
	if err != nil {
		t.Fatalf("ScanCmd.Run: %v", err)
	}

	for _, want := range []string{
		"# Project Scan: scanfix",
		"**Path**: `" + proj + "`",
		"## Stack",
		"- Go (`go.mod`)",
		"- Makefile",
		"## Knowledge File Tree",
		"./docs/notes.md",
		"## Directory Summary",
		"## Key Entities",
		"chose pgvector over qdrant", // decisions regex hit from README
		"3x faster",                  // quantitative claim regex hit
		"### README.md (",            // root doc inlined with line count
		"### NOTES.md",               // ad-hoc root doc
		"(not a git repo)",           // git section fallback
		"### Makefile",               // config files section
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q", want)
		}
	}

	if strings.Contains(out, "excluded.md") {
		t.Errorf("knowledge tree leaked a /test/ file")
	}
	if strings.Contains(out, "## scribe Link") {
		t.Errorf("unlinked project must not render the scribe Link section")
	}
	if strings.Contains(out, "## scribe Drop Files") {
		t.Errorf("project without drops must not render the drop-files section")
	}
}

// TestScanRun_KBLinkAndDropFiles: a project registered in the KB
// (projects/<name>/.repo.yaml) gets the scribe Link section with the
// repo yaml + KB article list, and pending drop files under
// .claude/<kb_name>/ are surfaced.
func TestScanRun_KBLinkAndDropFiles(t *testing.T) {
	root := stubHarnessKB(t, "kb_name: stubkb\n")
	proj := scanProject(t, "AcmeProj")

	kbProjDir := filepath.Join(root, "projects", "acmeproj")
	if err := os.MkdirAll(kbProjDir, 0o755); err != nil {
		t.Fatalf("mkdir kb project dir: %v", err)
	}
	repoYAML := "path: " + proj + "\ndomain: general\n"
	if err := os.WriteFile(filepath.Join(kbProjDir, ".repo.yaml"), []byte(repoYAML), 0o644); err != nil {
		t.Fatalf("write .repo.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kbProjDir, "learnings.md"), []byte("# Learnings\n"), 0o644); err != nil {
		t.Fatalf("write learnings: %v", err)
	}

	dropDir := filepath.Join(proj, ".claude", "stubkb")
	if err := os.MkdirAll(dropDir, 0o755); err != nil {
		t.Fatalf("mkdir drop dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dropDir, "2026-06-01-insight.md"), []byte("---\nscriptorium: true\n---\n"), 0o644); err != nil {
		t.Fatalf("write drop file: %v", err)
	}

	out, err := captureStdoutErr(t, (&ScanCmd{Path: proj}).Run)
	if err != nil {
		t.Fatalf("ScanCmd.Run: %v", err)
	}

	for _, want := range []string{
		"## scribe Link",
		"domain: general",
		"KB articles in acmeproj/:",
		"- learnings.md",
		"## scribe Drop Files (1 pending)",
		"2026-06-01-insight.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q", want)
		}
	}
}

// TestScanRun_MixExsDepsSection: an Elixir project gets its deps block
// excerpted from mix.exs.
func TestScanRun_MixExsDepsSection(t *testing.T) {
	stubHarnessKB(t, "kb_name: stubkb\n")
	proj := filepath.Join(t.TempDir(), "phoenixapp")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mixExs := `defmodule Phoenixapp.MixProject do
  use Mix.Project

  defp deps do
    [
      {:phoenix, "~> 1.8"},
      {:ecto_sql, "~> 3.12"}
    ]
  end
end
`
	if err := os.WriteFile(filepath.Join(proj, "mix.exs"), []byte(mixExs), 0o644); err != nil {
		t.Fatalf("write mix.exs: %v", err)
	}

	out, err := captureStdoutErr(t, (&ScanCmd{Path: proj}).Run)
	if err != nil {
		t.Fatalf("ScanCmd.Run: %v", err)
	}
	for _, want := range []string{
		"- Elixir/Phoenix (`mix.exs`)",
		"### mix.exs deps",
		`{:phoenix, "~> 1.8"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q", want)
		}
	}
}

// TestScanRun_MissingPathErrors: scanning a nonexistent path fails fast.
func TestScanRun_MissingPathErrors(t *testing.T) {
	stubHarnessKB(t, "kb_name: stubkb\n")
	missing := filepath.Join(t.TempDir(), "definitely-not-here")

	_, err := captureStdoutErr(t, (&ScanCmd{Path: missing}).Run)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v, want does-not-exist error", err)
	}
}
