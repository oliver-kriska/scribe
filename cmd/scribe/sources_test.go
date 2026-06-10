package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSourcePatternMatches(t *testing.T) {
	home := os.Getenv("HOME")
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		// Plain paths: self + everything beneath.
		{"/Users/x/work", "/Users/x/work", true},
		{"/Users/x/work", "/Users/x/work/api", true},
		{"/Users/x/work", "/Users/x/work/api/deep/nested", true},
		{"/Users/x/work", "/Users/x/workspace", false},
		{"/Users/x/work", "/Users/x/other", false},
		// Trailing /** is the same as the plain form.
		{"/Users/x/work/**", "/Users/x/work/api", true},
		{"/Users/x/work/**", "/Users/x/other", false},
		// Globs match the path or any ancestor.
		{"/Users/x/work-*", "/Users/x/work-foo", true},
		{"/Users/x/work-*", "/Users/x/work-foo/inner", true},
		{"/Users/x/work-*", "/Users/x/personal", false},
		{"/Users/x/*/api", "/Users/x/proj/api", true},
		{"/Users/x/*/api", "/Users/x/proj/web", false},
		// Home expansion.
		{"~/somework", home + "/somework/api", true},
		// Malformed glob never matches.
		{"/Users/x/[", "/Users/x/anything", false},
		// Empty pattern never matches.
		{"", "/Users/x/work", false},
	}
	for _, tt := range tests {
		if got := sourcePatternMatches(tt.pattern, tt.path); got != tt.want {
			t.Errorf("sourcePatternMatches(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

func TestSourceAllowed(t *testing.T) {
	tests := []struct {
		name    string
		include []string
		exclude []string
		path    string
		want    bool
	}{
		{"no filters allows all", nil, nil, "/Users/x/anything", true},
		{"include match", []string{"/Users/x/work"}, nil, "/Users/x/work/api", true},
		{"include miss", []string{"/Users/x/work"}, nil, "/Users/x/personal/blog", false},
		{"exclude wins over include", []string{"/Users/x/work"}, []string{"/Users/x/work/secret"}, "/Users/x/work/secret/repo", false},
		{"exclude only", nil, []string{"/Users/x/personal"}, "/Users/x/personal/blog", false},
		{"exclude only, other path", nil, []string{"/Users/x/personal"}, "/Users/x/work/api", true},
		{"multiple includes", []string{"/Users/x/a", "/Users/x/b"}, nil, "/Users/x/b/proj", true},
	}
	for _, tt := range tests {
		cfg := &ScribeConfig{Sources: SourcesConfig{Include: tt.include, Exclude: tt.exclude}}
		if got := sourceAllowed(cfg, tt.path); got != tt.want {
			t.Errorf("%s: sourceAllowed = %v, want %v", tt.name, got, tt.want)
		}
	}
	if !sourceAllowed(nil, "/anything") {
		t.Error("nil config must allow all")
	}
}

func TestInitTemplateRendersSourcesBlock(t *testing.T) {
	vars := templateVars{
		OwnerName:      "Test",
		Domains:        []string{"general"},
		LLMProvider:    "anthropic",
		SourcesInclude: []string{"~/work", "~/Projects/client-*"},
		SourcesExclude: []string{"~/personal"},
	}
	out, err := renderTemplate("templates/scribe.yaml", vars)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sources:", "include:", `- "~/work"`, `- "~/Projects/client-*"`, "exclude:", `- "~/personal"`} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered scribe.yaml missing %q\n%s", want, out)
		}
	}

	// The rendered file must be valid YAML that round-trips into the
	// config struct with the filters intact.
	var cfg ScribeConfig
	if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("rendered scribe.yaml is not valid YAML: %v\n%s", err, out)
	}
	if len(cfg.Sources.Include) != 2 || cfg.Sources.Include[0] != "~/work" {
		t.Errorf("Sources.Include = %v, want [~/work ~/Projects/client-*]", cfg.Sources.Include)
	}
	if len(cfg.Sources.Exclude) != 1 || cfg.Sources.Exclude[0] != "~/personal" {
		t.Errorf("Sources.Exclude = %v, want [~/personal]", cfg.Sources.Exclude)
	}

	// Without --allow/--disallow only the commented example renders.
	vars.SourcesInclude, vars.SourcesExclude = nil, nil
	out, err = renderTemplate("templates/scribe.yaml", vars)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\nsources:") {
		t.Errorf("rendered scribe.yaml has active sources block without flags:\n%s", out)
	}
	if !strings.Contains(out, "# sources:") {
		t.Error("rendered scribe.yaml lost the commented sources example")
	}
}

func TestInitTemplateGitignoreTeamMode(t *testing.T) {
	vars := templateVars{TeamMode: true}
	out, err := renderTemplate("templates/gitignore", vars)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "scripts/projects.json") {
		t.Errorf("--team gitignore must exclude the manifest:\n%s", out)
	}

	vars.TeamMode = false
	out, err = renderTemplate("templates/gitignore", vars)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "scripts/projects.json") {
		t.Errorf("single-user gitignore must NOT exclude the manifest:\n%s", out)
	}
	if !strings.Contains(out, "output/") {
		t.Errorf("gitignore lost its base entries:\n%s", out)
	}
}
