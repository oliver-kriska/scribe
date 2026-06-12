package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHumanTokens(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1.0K"},
		{1500, "1.5K"},
		{999_999, "1000.0K"},
		{1_000_000, "1.0M"},
		{2_500_000, "2.5M"},
	}
	for _, tt := range tests {
		if got := humanTokens(tt.n); got != tt.want {
			t.Errorf("humanTokens(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

// estimateTestKB scaffolds raw/articles + wiki for the queue scanners.
func estimateTestKB(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte("domains: [acme]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"raw/articles", "wiki"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestUnprocessedForContextualize(t *testing.T) {
	root := estimateTestKB(t)
	writeKBFile(t, root, "raw/articles/fresh.md", "# Fresh\n\nbody\n")
	writeKBFile(t, root, "raw/articles/logged.md", "# Logged\n\nbody\n")
	writeKBFile(t, root, "raw/articles/marked.md",
		"# Marked\n\n"+retrievalContextMarker+"\nContext paragraph.\n")
	writeKBFile(t, root, "raw/articles/notes.txt", "not markdown")
	if err := os.MkdirAll(filepath.Join(root, "raw", "articles", "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeKBFile(t, root, "wiki/_contextualized_log.json", `{"logged.md": "2026-06-01"}`)

	got := unprocessedForContextualize(root)
	if len(got) != 1 || filepath.Base(got[0]) != "fresh.md" {
		t.Errorf("queue = %v, want only fresh.md", got)
	}

	if got := unprocessedForContextualize(t.TempDir()); got != nil {
		t.Errorf("missing raw dir should yield nil, got %v", got)
	}
}

func TestUnprocessedForAbsorb(t *testing.T) {
	root := estimateTestKB(t)
	writeKBFile(t, root, "raw/articles/fresh.md", "# Fresh\n\nbody\n")
	writeKBFile(t, root, "raw/articles/done.md", "# Done\n\nbody\n")
	writeKBFile(t, root, "wiki/_absorb_log.json", `{"done.md": {"at": "2026-06-01T00:00:00Z"}}`)

	got := unprocessedForAbsorb(root)
	if len(got) != 1 || filepath.Base(got[0]) != "fresh.md" {
		t.Errorf("queue = %v, want only fresh.md", got)
	}

	// Legacy v1 log shape (filename → timestamp string) still counts as done.
	writeKBFile(t, root, "wiki/_absorb_log.json", `{"done.md": "2026-06-01T00:00:00Z", "fresh.md": "2026-06-02T00:00:00Z"}`)
	if got := unprocessedForAbsorb(root); len(got) != 0 {
		t.Errorf("v1 log entries ignored: %v", got)
	}
}

func TestEstimateSync(t *testing.T) {
	root := estimateTestKB(t)
	// One standard source and one dense source in the absorb queue; both
	// also uncontextualized.
	writeKBFile(t, root, "raw/articles/standard.md",
		"---\ntitle: \"Std\"\ndensity: standard\n---\n\n"+strings.Repeat("word ", 300)+"\n")
	writeKBFile(t, root, "raw/articles/dense.md",
		"---\ntitle: \"Dense\"\ndensity: dense\n---\n\n"+strings.Repeat("word ", 500)+"\n")

	cfg := loadConfig(root) // defaults: contextualize enabled
	estimates := estimateSync(root, cfg)

	notes := make([]string, 0, len(estimates))
	for _, e := range estimates {
		notes = append(notes, e.Note)
		if e.InputTokens <= 0 || e.OutputTokens <= 0 {
			t.Errorf("estimate %q has non-positive tokens: %+v", e.Note, e)
		}
	}
	joined := strings.Join(notes, "; ")
	if !strings.Contains(joined, "2 source(s) × contextualize") {
		t.Errorf("contextualize phase missing or wrong: %v", notes)
	}
	if !strings.Contains(joined, "1 brief/standard source(s)") {
		t.Errorf("single-pass phase missing: %v", notes)
	}
	if !strings.Contains(joined, "1 dense source(s)") {
		t.Errorf("dense phase missing: %v", notes)
	}
}

func TestEstimateSync_OllamaContextualizeIsLabeledFree(t *testing.T) {
	root := estimateTestKB(t)
	yaml := `domains: [acme]
absorb:
  contextualize:
    provider: ollama
    model: gemma3
`
	if err := os.WriteFile(filepath.Join(root, "scribe.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	writeKBFile(t, root, "raw/articles/one.md", "# One\n\nbody text\n")

	cfg := loadConfig(root)
	estimates := estimateSync(root, cfg)
	found := false
	for _, e := range estimates {
		if strings.Contains(e.Note, "contextualize") {
			found = true
			if !strings.Contains(e.Model, "ollama:gemma3") || !strings.Contains(e.Model, "free") {
				t.Errorf("ollama contextualize not labeled free: %q", e.Model)
			}
		}
	}
	if !found {
		t.Errorf("no contextualize estimate: %+v", estimates)
	}
}

func TestEstimateSync_EmptyQueues(t *testing.T) {
	root := estimateTestKB(t)
	cfg := loadConfig(root)
	if got := estimateSync(root, cfg); len(got) != 0 {
		t.Errorf("empty KB should produce no estimates, got %+v", got)
	}
}

func TestPrintEstimate(t *testing.T) {
	t.Run("empty is a no-op message", func(t *testing.T) {
		out := captureLintStdout(t, func() { printEstimate(nil) })
		if !strings.Contains(out, "nothing queued") {
			t.Errorf("output:\n%s", out)
		}
	})

	t.Run("totals are summed", func(t *testing.T) {
		out := captureLintStdout(t, func() {
			printEstimate([]tokenEstimate{
				{InputTokens: 1000, OutputTokens: 100, Model: "haiku", Note: "a"},
				{InputTokens: 2000, OutputTokens: 400, Model: "sonnet", Note: "b"},
			})
		})
		if !strings.Contains(out, "total") {
			t.Errorf("missing total row:\n%s", out)
		}
		if !strings.Contains(out, "3.0K") || !strings.Contains(out, "500") {
			t.Errorf("totals not summed (want 3.0K in / 500 out):\n%s", out)
		}
	})
}
