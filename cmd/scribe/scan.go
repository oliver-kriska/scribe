package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type ScanCmd struct {
	Path string `arg:"" help:"Project path to scan."`
}

var (
	excludeDirs      = regexp.MustCompile(`node_modules|deps|_build|\.git|\.elixir_ls|\.ruff_cache|\.pytest_cache|\.husky`)
	knowledgeExclude = regexp.MustCompile(`/test/|/priv/|/qa/|_cache/|/cache/|/fixtures/|/samples/|/prompts/`)

	stackDetectors = []struct {
		file  string
		label string
	}{
		{"mix.exs", "Elixir/Phoenix (`mix.exs`)"},
		{"package.json", "Node.js (`package.json`)"},
		{"Cargo.toml", "Rust (`Cargo.toml`)"},
		{"go.mod", "Go (`go.mod`)"},
		{"pyproject.toml", "Python (`pyproject.toml`)"},
		{"Gemfile", "Ruby (`Gemfile`)"},
		{".claude-plugin/plugin.json", "Claude Code Plugin"},
		{"fly.toml", "Fly.io deployment"},
		{"Dockerfile", "Docker"},
		{"Makefile", "Makefile"},
	}

	decisionRE = regexp.MustCompile(`(?i)(chose|decided|picked|selected).*(because|over|instead)`)
	quantRE    = regexp.MustCompile(`[0-9]+[0-9.]*(%|ms|[0-9]s| MB| KB| seconds|x faster|x slower| hours| minutes)`)
	toolsRE    = regexp.MustCompile(`(?i)(uses|integrates with|depends on|powered by|built with|replaced) [A-Z][a-zA-Z]*`)
)

func (s *ScanCmd) Run() error {
	projectPath, err := filepath.Abs(s.Path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(projectPath); os.IsNotExist(err) {
		return fmt.Errorf("project path does not exist: %s", projectPath)
	}

	projectName := filepath.Base(projectPath)

	root, _ := kbDir()

	// ─── Metadata ───
	fmt.Printf("# Project Scan: %s\n\n", projectName)
	fmt.Printf("**Path**: `%s`\n", projectPath)
	fmt.Printf("**Scanned**: %s\n", time.Now().UTC().Format(time.RFC3339))

	if isGitRepo(projectPath) {
		sha := gitOutput(projectPath, "rev-parse", "--short", "HEAD")
		branch := gitOutput(projectPath, "branch", "--show-current")
		fmt.Printf("**Git SHA**: %s\n", sha)
		fmt.Printf("**Branch**: %s\n", branch)
	}

	// ─── scribe context ───
	if root != "" {
		projLower := strings.ToLower(projectName)
		repoYaml := filepath.Join(root, "projects", projLower, ".repo.yaml")
		if _, err := os.Stat(repoYaml); err == nil {
			fmt.Printf("\n## scribe Link\n```yaml\n")
			data, _ := os.ReadFile(repoYaml)
			fmt.Print(string(data))
			fmt.Println("```")

			kbDir := filepath.Dir(repoYaml)
			entries, _ := filepath.Glob(filepath.Join(kbDir, "*.md"))
			if len(entries) > 0 {
				fmt.Printf("\nKB articles in %s/:\n", filepath.Base(kbDir))
				for _, e := range entries {
					fmt.Printf("- %s\n", filepath.Base(e))
				}
			}
		}
	}

	// ─── Drop files ───
	dropDir := filepath.Join(projectPath, ".claude", kbName(root))
	if drops, _ := filepath.Glob(filepath.Join(dropDir, "*.md")); len(drops) > 0 {
		fmt.Printf("\n## scribe Drop Files (%d pending)\n", len(drops))
		for _, d := range drops {
			fmt.Println(filepath.Base(d))
		}
	}

	// ─── Stack ───
	fmt.Println("\n## Stack")
	detected := 0
	for _, s := range stackDetectors {
		if _, err := os.Stat(filepath.Join(projectPath, s.file)); err == nil {
			fmt.Printf("- %s\n", s.label)
			detected++
		}
	}
	if detected == 0 {
		fmt.Println("- (none detected)")
	}

	// ─── Knowledge file tree ───
	knowledgeFiles := collectKnowledgeFiles(projectPath)

	fmt.Println("\n## Knowledge File Tree")
	fmt.Println("```")
	maxTree := 80
	if len(knowledgeFiles) == 0 {
		fmt.Println("(no knowledge files found)")
	} else {
		for i, f := range knowledgeFiles {
			if i >= maxTree {
				break
			}
			fmt.Println(f)
		}
		if len(knowledgeFiles) > maxTree {
			fmt.Printf("\n... (%d total, showing first %d)\n", len(knowledgeFiles), maxTree)
		}
	}
	fmt.Println("```")

	// ─── Directory summary ───
	fmt.Println("\n## Directory Summary")
	fmt.Println("\nTop directories by content volume (knowledge files only):")
	fmt.Println()
	fmt.Println("| Directory | Files | Lines | Avg |")
	fmt.Println("|-----------|-------|-------|-----|")

	dirStats := make(map[string]struct{ files, lines int })
	for _, f := range knowledgeFiles {
		absPath := filepath.Join(projectPath, f)
		dir := filepath.Dir(f)
		content, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}
		lines := countLines(content)
		stats := dirStats[dir]
		stats.files++
		stats.lines += lines
		dirStats[dir] = stats
	}

	type dirEntry struct {
		dir   string
		files int
		lines int
	}
	var entries []dirEntry
	for dir, stats := range dirStats {
		entries = append(entries, dirEntry{dir, stats.files, stats.lines})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].files > entries[j].files })
	maxDirs := 25
	for i, e := range entries {
		if i >= maxDirs {
			break
		}
		avg := 0
		if e.files > 0 {
			avg = e.lines / e.files
		}
		fmt.Printf("| %-50s | %5d | %5d | %3d |\n", e.dir, e.files, e.lines, avg)
	}

	// ─── Key entities ───
	fmt.Println("\n## Key Entities")
	fmt.Println("\nDecisions, quantitative claims, and named tools/patterns found in knowledge files:")

	maxScanFiles := 40
	scanFiles := knowledgeFiles
	if len(scanFiles) > maxScanFiles {
		scanFiles = scanFiles[:maxScanFiles]
	}

	printEntitySection("Decisions", projectPath, scanFiles, decisionRE, 15)
	printEntitySection("Quantitative Claims", projectPath, scanFiles, quantRE, 15)
	printEntitySection("Tools & Libraries", projectPath, scanFiles, toolsRE, 10)

	// ─── Root docs ───
	fmt.Println("\n## Root Docs")
	maxInline := 200

	for _, name := range []string{"CLAUDE.md", "README.md", "CHANGELOG.md", "AGENTS.md"} {
		docPath := filepath.Join(projectPath, name)
		content, err := os.ReadFile(docPath)
		if err != nil {
			continue
		}
		lines := countLines(content)
		fmt.Printf("\n### %s (%d lines)\n\n", name, lines)
		if lines <= maxInline {
			fmt.Print(string(content))
		} else {
			printFirstNLines(content, maxInline)
			fmt.Printf("\n*... truncated at %d of %d lines*\n", maxInline, lines)
		}
	}

	// Ad-hoc root docs
	rootDocs, _ := filepath.Glob(filepath.Join(projectPath, "*.md"))
	for _, f := range rootDocs {
		name := filepath.Base(f)
		switch name {
		case "CLAUDE.md", "README.md", "CHANGELOG.md", "AGENTS.md":
			continue
		}
		content, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		lines := countLines(content)
		fmt.Printf("\n### %s (%d lines)\n\n", name, lines)
		if lines <= 60 {
			fmt.Print(string(content))
		} else {
			printFirstNLines(content, 40)
			fmt.Printf("\n*... truncated at 40 of %d lines*\n", lines)
		}
	}

	// ─── Git history ───
	fmt.Println("\n## Git Log (last 30)")
	fmt.Println("```")
	if isGitRepo(projectPath) {
		fmt.Print(gitOutput(projectPath, "log", "--oneline", "-30", "--no-color"))
	} else {
		fmt.Println("(not a git repo)")
	}
	fmt.Println("```")

	// ─── Config files ───
	fmt.Println("\n## Config Files")
	for _, name := range []string{".claude-plugin/plugin.json", "fly.toml", "Dockerfile", "docker-compose.yml", "Makefile"} {
		cfgPath := filepath.Join(projectPath, name)
		content, err := os.ReadFile(cfgPath)
		if err != nil {
			continue
		}
		lines := countLines(content)
		fmt.Printf("\n### %s (%d lines)\n```\n", name, lines)
		if lines <= 30 {
			fmt.Print(string(content))
		} else {
			printFirstNLines(content, 25)
			fmt.Printf("... (%d total)\n", lines)
		}
		fmt.Println("```")
	}

	// CI workflows
	workflows, _ := filepath.Glob(filepath.Join(projectPath, ".github", "workflows", "*.yml"))
	workflows2, _ := filepath.Glob(filepath.Join(projectPath, ".github", "workflows", "*.yaml"))
	workflows = append(workflows, workflows2...)
	for _, f := range workflows {
		name := ".github/workflows/" + filepath.Base(f)
		content, _ := os.ReadFile(f)
		lines := countLines(content)
		fmt.Printf("\n### %s (%d lines)\n```\n", name, lines)
		printFirstNLines(content, 20)
		fmt.Printf("... (%d total)\n", lines)
		fmt.Println("```")
	}

	// mix.exs deps
	mixPath := filepath.Join(projectPath, "mix.exs")
	if content, err := os.ReadFile(mixPath); err == nil {
		fmt.Println("\n### mix.exs deps")
		fmt.Println("```elixir")
		inDeps := false
		lineCount := 0
		for line := range strings.SplitSeq(string(content), "\n") {
			if strings.Contains(line, "defp deps") {
				inDeps = true
			}
			if inDeps {
				fmt.Println(line)
				lineCount++
				if lineCount > 40 {
					break
				}
				if strings.TrimSpace(line) == "end" && lineCount > 1 {
					break
				}
			}
		}
		fmt.Println("```")
	}

	return nil
}

func collectKnowledgeFiles(projectPath string) []string {
	var files []string
	_ = filepath.Walk(projectPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable, continue walk
		}
		if info.IsDir() {
			rel, _ := filepath.Rel(projectPath, path)
			if excludeDirs.MatchString(rel + "/") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".md" && ext != ".txt" && ext != ".pdf" {
			return nil
		}
		rel, _ := filepath.Rel(projectPath, path)
		if knowledgeExclude.MatchString("/" + rel) {
			return nil
		}
		files = append(files, "./"+rel)
		return nil
	})
	sort.Strings(files)
	return files
}

func printEntitySection(title, projectPath string, files []string, pattern *regexp.Regexp, maxLines int) {
	var matches []string
	for _, f := range files {
		absPath := filepath.Join(projectPath, strings.TrimPrefix(f, "./"))
		content, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(content), "\n") {
			if pattern.MatchString(line) {
				matches = append(matches, fmt.Sprintf("%s:%d:%s", f, i+1, strings.TrimSpace(line)))
				if len(matches) >= maxLines {
					break
				}
			}
		}
		if len(matches) >= maxLines {
			break
		}
	}
	if len(matches) > 0 {
		fmt.Printf("\n### %s\n```\n", title)
		for _, m := range matches {
			fmt.Println(m)
		}
		fmt.Println("```")
	}
}

func printFirstNLines(content []byte, n int) {
	lines := strings.Split(string(content), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	fmt.Println(strings.Join(lines, "\n"))
}

func isGitRepo(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

func gitOutput(dir string, args ...string) string {
	cmd := exec.Command("git", args...) //nolint:noctx // git subprocess
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "(unknown)"
	}
	return strings.TrimSpace(string(out))
}
