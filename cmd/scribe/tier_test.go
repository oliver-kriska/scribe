package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComputeIndexTier_WordBuckets(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty", "", TierStub},
		{"50 words", strings.Repeat("foo ", 50), TierStub},
		{"81 words", strings.Repeat("foo ", 81), TierBrief},
		{"199 words", strings.Repeat("foo ", 199), TierBrief},
		{"200 words", strings.Repeat("foo ", 200), TierStandard},
		{"1000 words", strings.Repeat("foo ", 1000), TierStandard},
		{"2000 words", strings.Repeat("foo ", 2000), TierDeep},
		{"5000 words", strings.Repeat("foo ", 5000), TierDeep},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := computeIndexTier(nil, []byte(c.body), 0)
			if got != c.want {
				t.Errorf("words=%d: got %q, want %q", countBodyWords([]byte(c.body)), got, c.want)
			}
		})
	}
}

func TestComputeIndexTier_SectionsPromoteToDeep(t *testing.T) {
	body := []byte(strings.Repeat("foo ", 500)) // standard band
	if got := computeIndexTier(nil, body, 0); got != TierStandard {
		t.Fatalf("baseline standard: got %s", got)
	}
	if got := computeIndexTier(nil, body, 5); got != TierDeep {
		t.Errorf("with 5 sections: got %s, want deep", got)
	}
	if got := computeIndexTier(nil, body, 4); got != TierStandard {
		t.Errorf("with 4 sections: got %s, want standard (below threshold)", got)
	}
}

func TestComputeIndexTier_OverrideWins(t *testing.T) {
	fm := &Frontmatter{IndexTierOverride: TierReference}
	body := []byte(strings.Repeat("foo ", 5)) // would be stub
	if got := computeIndexTier(fm, body, 0); got != TierReference {
		t.Errorf("override should win: got %s", got)
	}
}

func TestComputeIndexTier_InvalidOverrideFallsThrough(t *testing.T) {
	fm := &Frontmatter{IndexTierOverride: "garbage"}
	body := []byte(strings.Repeat("foo ", 500))
	if got := computeIndexTier(fm, body, 0); got != TierStandard {
		t.Errorf("invalid override should fall through to computed: got %s", got)
	}
}

func TestComputeIndexTierForRaw_StubAndFxtwitterClamps(t *testing.T) {
	long := []byte(strings.Repeat("foo ", 500)) // would be standard
	if got := computeIndexTierForRaw("stub", nil, long, 0); got != TierStub {
		t.Errorf("fetched_via=stub: got %s, want stub", got)
	}
	if got := computeIndexTierForRaw("fxtwitter", nil, long, 0); got != TierBrief {
		t.Errorf("fetched_via=fxtwitter: got %s, want brief", got)
	}
	if got := computeIndexTierForRaw("trafilatura", nil, long, 0); got != TierStandard {
		t.Errorf("fetched_via=trafilatura: should compute normally, got %s", got)
	}
}

func TestUpdateFrontmatterField_AddsNewKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	original := "---\ntitle: \"Foo\"\ntype: research\nupdated: 2026-01-01\n---\n\nbody\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := updateFrontmatterField(path, "index_tier", "standard"); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "index_tier: standard") {
		t.Errorf("expected index_tier line; got:\n%s", got)
	}
}

func TestUpdateFrontmatterField_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	original := "---\ntitle: \"Foo\"\nindex_tier: brief\nupdated: 2026-01-01\n---\n\nbody\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := updateFrontmatterField(path, "index_tier", "deep"); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "index_tier: deep") {
		t.Errorf("expected index_tier: deep; got:\n%s", got)
	}
	if strings.Contains(string(got), "index_tier: brief") {
		t.Errorf("old value should be replaced; got:\n%s", got)
	}
}

func TestUpdateFrontmatterField_EmptyClearsKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	original := "---\ntitle: \"Foo\"\nindex_tier_override: deep\n---\n\nbody\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := updateFrontmatterField(path, "index_tier_override", ""); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := os.ReadFile(path)
	if strings.Contains(string(got), "index_tier_override") {
		t.Errorf("override should be removed; got:\n%s", got)
	}
}

func TestUpdateFrontmatterField_BumpsUpdatedOnTierWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	original := "---\ntitle: \"Foo\"\nupdated: 2026-01-01\n---\n\nbody\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := updateFrontmatterField(path, "index_tier", "standard"); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := os.ReadFile(path)
	if strings.Contains(string(got), "updated: 2026-01-01") {
		t.Errorf("updated: should be bumped on tier write; got:\n%s", got)
	}
}

func TestUpdateFrontmatterField_OverrideWriteDoesNotBumpUpdated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	original := "---\ntitle: \"Foo\"\nupdated: 2026-01-01\n---\n\nbody\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := updateFrontmatterField(path, "index_tier_override", "reference"); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "updated: 2026-01-01") {
		t.Errorf("override write must NOT bump updated:; got:\n%s", got)
	}
}

func TestIsRawArticle(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"raw/articles/foo.md", true},
		{"/kb/raw/articles/foo.md", true},
		{"wiki/research/foo.md", false},
		{"research/foo.md", false},
	}
	for _, c := range cases {
		if got := isRawArticle(c.path); got != c.want {
			t.Errorf("isRawArticle(%q)=%v want %v", c.path, got, c.want)
		}
	}
}

// TestTierWrite_SkipsMalformedFence is the convergence guard: a file whose
// opening fence is joined (`--- title:` with space-indented keys) makes the
// YAML parser and the line-writer disagree about index_tier, so --missing-only
// used to rewrite it every run. The guard must skip it byte-for-byte while a
// clean sibling still gets its tier written.
func TestTierWrite_SkipsMalformedFence(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SCRIBE_KB", root)
	pat := filepath.Join(root, "patterns")
	if err := os.MkdirAll(pat, 0o755); err != nil {
		t.Fatal(err)
	}
	malformed := "--- title: \"Bad\"\n type: pattern\n domain: general\nindex_tier: standard\n---\n\n" + strings.Repeat("foo ", 50) + "\n"
	malformedPath := filepath.Join(pat, "bad.md")
	if err := os.WriteFile(malformedPath, []byte(malformed), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanPath := filepath.Join(pat, "good.md")
	clean := "---\ntitle: \"Good\"\ntype: pattern\ncreated: 2026-01-01\nupdated: 2026-01-01\ndomain: general\nconfidence: medium\ntags: []\nrelated: []\nsources: []\n---\n\n" + strings.Repeat("foo ", 50) + "\n"
	if err := os.WriteFile(cleanPath, []byte(clean), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := (&TierWriteCmd{MissingOnly: true}).Run(); err != nil {
		t.Fatalf("tier write: %v", err)
	}

	// Malformed file is untouched.
	got, _ := os.ReadFile(malformedPath)
	if string(got) != malformed {
		t.Errorf("malformed-fence file was rewritten, want byte-identical skip:\n%s", got)
	}
	// Clean file got its tier.
	gotClean, _ := os.ReadFile(cleanPath)
	if !strings.Contains(string(gotClean), "index_tier:") {
		t.Errorf("clean sibling should have received index_tier:\n%s", gotClean)
	}
}
