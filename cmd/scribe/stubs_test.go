package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLooksLikeSlugTitle(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"turingpost-com-p-fod151", true},
		{`"turingpost-com-p-fod151"`, true},
		{"", true},
		{"  ", true},
		{`"FOD#151: Recursive Self-Learning"`, false},
		{"FOD#151 Recursive", false},
		{"plain title here", false},
	}
	for _, c := range cases {
		if got := looksLikeSlugTitle(c.in); got != c.want {
			t.Errorf("looksLikeSlugTitle(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRewriteRawArticleBody_UpdatesSlugTitle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stub.md")
	original := "---\n" +
		"title: \"turingpost-com-p-fod151\"\n" +
		"source_url: \"https://turingpost.com/p/fod151\"\n" +
		"fetched_via: stub\n" +
		"type: article\n" +
		"---\n\n" +
		"old body\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	res := fetchResult{
		Title: `FOD#151: Recursive "Self-Learning"`,
		Body:  "real body",
		Via:   "trafilatura",
	}
	if err := rewriteRawArticleBody(path, res); err != nil {
		t.Fatalf("rewriteRawArticleBody: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	if !strings.Contains(out, `title: "FOD#151: Recursive \"Self-Learning\""`) {
		t.Errorf("expected title rewritten with escaped quotes, got:\n%s", out)
	}
	if !strings.Contains(out, "fetched_via: trafilatura") {
		t.Errorf("expected fetched_via rewritten, got:\n%s", out)
	}
	if !strings.Contains(out, "real body") {
		t.Errorf("expected body replaced, got:\n%s", out)
	}
}

func TestRewriteRawArticleBody_PreservesHumanTitle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stub.md")
	original := "---\n" +
		"title: \"My Custom Title\"\n" +
		"fetched_via: stub\n" +
		"---\n\n" +
		"old body\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	res := fetchResult{
		Title: "Different Page Title",
		Body:  "real body",
		Via:   "trafilatura",
	}
	if err := rewriteRawArticleBody(path, res); err != nil {
		t.Fatalf("rewriteRawArticleBody: %v", err)
	}
	got, _ := os.ReadFile(path)
	out := string(got)
	if !strings.Contains(out, `title: "My Custom Title"`) {
		t.Errorf("expected human title preserved, got:\n%s", out)
	}
	if !strings.Contains(out, "fetched_via: trafilatura") {
		t.Errorf("expected fetched_via rewritten, got:\n%s", out)
	}
}

func TestRewriteRawArticleBody_NoTitleResultIsNoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stub.md")
	original := "---\n" +
		"title: \"some-slug-here\"\n" +
		"fetched_via: stub\n" +
		"---\n\n" +
		"old body\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	res := fetchResult{Title: "", Body: "real body", Via: "trafilatura"}
	if err := rewriteRawArticleBody(path, res); err != nil {
		t.Fatalf("rewriteRawArticleBody: %v", err)
	}
	got, _ := os.ReadFile(path)
	out := string(got)
	if !strings.Contains(out, `title: "some-slug-here"`) {
		t.Errorf("expected slug title left alone when fetcher had no title, got:\n%s", out)
	}
}
