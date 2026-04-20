package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// WriteCmd creates new KB articles or appends entries to rolling memory files.
// Exists as an alternative to a live MCP write-back surface: a skill can shell
// out to `scribe write` and get frontmatter + path conventions enforced by the
// same binary that runs sync, lint, and dream. Respects Rule 12 (append-only):
// no destructive rewrite mode — use direct editing if you genuinely need to
// replace an article.
type WriteCmd struct {
	Type    string   `help:"Article type: decision | research | solution | tool | pattern | person | project." short:"t"`
	Title   string   `help:"Article title (required)." short:"T"`
	Domain  string   `help:"Domain tag (e.g. personal, general, work, or any domain you add to scribe.yaml)." short:"d"`
	Tags    []string `help:"Tags (repeatable or comma-separated)." short:"g"`
	Related []string `help:"Related article titles (wikilink targets, repeatable)."`
	Sources []string `help:"Source paths or URLs (repeatable)."`
	Status  string   `help:"Optional status field (e.g. 'decided', 'active')."`
	// Append-to-rolling mode
	Rolling string `help:"Append to rolling memory file: 'decisions' or 'learnings'."`
	Project string `help:"Project name for rolling mode (must match an existing projects/<name>/ directory)."`
	// Output control
	Path   string `help:"Override the output path (relative to KB root). Advanced."`
	Body   string `help:"Body source: '-' for stdin, 'file:path' for file, or inline string." default:"-"`
	DryRun bool   `help:"Print the article without writing." name:"dry-run"`
}

// typeDirs maps the 7 entity types to their canonical directory.
// Kept separate from validate.go's validTypes (a bool-set for validation).
var typeDirs = map[string]string{
	"decision": "decisions",
	"research": "research",
	"solution": "solutions",
	"tool":     "tools",
	"pattern":  "patterns",
	"person":   "people",
	"project":  "projects",
}

func (w *WriteCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}

	if w.Title == "" {
		return fmt.Errorf("--title is required")
	}

	body, err := w.readBody()
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return fmt.Errorf("body is empty (read from %s)", w.Body)
	}

	if w.Rolling != "" {
		return w.runRolling(root, body)
	}
	return w.runCreate(root, body)
}

// runCreate writes a brand-new article with full frontmatter.
func (w *WriteCmd) runCreate(root, body string) error {
	if w.Type == "" {
		return fmt.Errorf("--type is required (or use --rolling for rolling memory)")
	}
	dir, ok := typeDirs[w.Type]
	if !ok {
		return fmt.Errorf("invalid --type %q (must be one of: decision, research, solution, tool, pattern, person, project)", w.Type)
	}
	if w.Domain == "" {
		return fmt.Errorf("--domain is required")
	}
	if !validDomainsForRoot(root)[w.Domain] {
		return fmt.Errorf("invalid --domain %q", w.Domain)
	}

	// Build target path. Projects live at projects/{slug}/overview.md,
	// everything else at {dir}/{slug}.md.
	var rel string
	if w.Path != "" {
		rel = w.Path
	} else if w.Type == "project" {
		rel = filepath.Join(dir, slugify(w.Title), "overview.md")
	} else {
		rel = filepath.Join(dir, slugify(w.Title)+".md")
	}
	target := filepath.Join(root, rel)

	// Defense against `--path ../../..` or absolute paths: the final target must
	// sit under the KB root. filepath.Join collapses `..`, so a cleaned target
	// that doesn't have the root as a prefix has escaped.
	cleanRoot := filepath.Clean(root) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(target)+string(os.PathSeparator), cleanRoot) {
		return fmt.Errorf("--path %q escapes KB root", rel)
	}

	if fileExists(target) {
		return fmt.Errorf("%s already exists — Rule 12 is append-only, edit directly or use --rolling", rel)
	}

	content := w.buildFrontmatter() + "\n" + body + "\n"

	if w.DryRun {
		fmt.Fprintf(os.Stderr, "# would write: %s\n", rel)
		fmt.Print(content)
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	logMsg("write", "wrote %s (%d bytes)", rel, len(content))

	w.reindex(root)
	return nil
}

// runRolling appends a dated entry to a project's rolling memory file.
// The file must already exist — we never create new rolling files through
// this path because their frontmatter has load-bearing fields (rolling:true,
// tags list) that should be authored deliberately.
func (w *WriteCmd) runRolling(root, body string) error {
	if w.Project == "" {
		return fmt.Errorf("--project is required for --rolling mode")
	}
	var filename string
	switch w.Rolling {
	case "decisions":
		filename = "decisions-log.md"
	case "learnings":
		filename = "learnings.md"
	default:
		return fmt.Errorf("invalid --rolling %q", w.Rolling)
	}
	rel := filepath.Join("projects", w.Project, filename)
	target := filepath.Join(root, rel)

	if !fileExists(target) {
		return fmt.Errorf("%s does not exist — create it manually first with rolling:true frontmatter", rel)
	}

	existing, err := os.ReadFile(target)
	if err != nil {
		return fmt.Errorf("read %s: %w", rel, err)
	}

	// Rolling format (from CLAUDE.md Rule 10): newest entries at top,
	// each entry is `## YYYY-MM-DD | Title\n\nBody\n\n---`. Newest goes
	// after the closing frontmatter delimiter, before any existing entries.
	date := time.Now().Format("2006-01-02")
	newEntry := fmt.Sprintf("## %s | %s\n\n%s\n\n---\n\n", date, w.Title, strings.TrimSpace(body))

	updated, err := insertAfterFrontmatter(string(existing), newEntry)
	if err != nil {
		return fmt.Errorf("insert: %w", err)
	}

	if w.DryRun {
		fmt.Fprintf(os.Stderr, "# would append to: %s\n", rel)
		fmt.Print(newEntry)
		return nil
	}

	if err := os.WriteFile(target, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	logMsg("write", "appended to %s (%d bytes added)", rel, len(newEntry))

	w.reindex(root)
	return nil
}

// insertAfterFrontmatter places entry immediately after the closing `---`
// frontmatter delimiter of content, preserving the rolling-file newest-first
// convention.
func insertAfterFrontmatter(content, entry string) (string, error) {
	if !strings.HasPrefix(content, "---\n") {
		return "", fmt.Errorf("no frontmatter found")
	}
	end := strings.Index(content[4:], "\n---\n")
	if end < 0 {
		return "", fmt.Errorf("no closing frontmatter delimiter")
	}
	splitAt := 4 + end + len("\n---\n")
	head := content[:splitAt]
	tail := strings.TrimLeft(content[splitAt:], "\n")
	return head + "\n" + entry + tail, nil
}

// buildFrontmatter returns a YAML block including the closing delimiter.
func (w *WriteCmd) buildFrontmatter() string {
	today := time.Now().Format("2006-01-02")
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %q\n", w.Title)
	fmt.Fprintf(&b, "type: %s\n", w.Type)
	fmt.Fprintf(&b, "created: %s\n", today)
	fmt.Fprintf(&b, "updated: %s\n", today)
	fmt.Fprintf(&b, "domain: %s\n", w.Domain)
	if w.Status != "" {
		fmt.Fprintf(&b, "status: %s\n", w.Status)
	}
	b.WriteString("confidence: medium\n")
	b.WriteString("tags: [")
	b.WriteString(joinTags(w.Tags))
	b.WriteString("]\n")
	b.WriteString("related: [")
	b.WriteString(joinWikilinks(w.Related))
	b.WriteString("]\n")
	b.WriteString("sources: [")
	b.WriteString(joinQuoted(w.Sources))
	b.WriteString("]\n")
	b.WriteString("---\n")
	return b.String()
}

// joinTags flattens a possibly comma-containing slice into `a, b, c`.
func joinTags(tags []string) string {
	var out []string
	for _, t := range tags {
		for piece := range strings.SplitSeq(t, ",") {
			piece = strings.TrimSpace(piece)
			if piece != "" {
				out = append(out, piece)
			}
		}
	}
	return strings.Join(out, ", ")
}

func joinWikilinks(items []string) string {
	var out []string
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		out = append(out, fmt.Sprintf(`"[[%s]]"`, it))
	}
	return strings.Join(out, ", ")
}

func joinQuoted(items []string) string {
	var out []string
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		out = append(out, fmt.Sprintf("%q", it))
	}
	return strings.Join(out, ", ")
}

// readBody resolves the --body flag: "-" = stdin, "file:path" = read file,
// anything else = treat as inline literal.
func (w *WriteCmd) readBody() (string, error) {
	if w.Body == "-" {
		data, err := io.ReadAll(bufio.NewReader(os.Stdin))
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	if after, ok := strings.CutPrefix(w.Body, "file:"); ok {
		path := after
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	return w.Body, nil
}

// reindex runs scribe index + scribe backlinks after a successful write.
// Silent on failure — the article is already on disk, reindex is a best effort.
//
// Skipped when SCRIBE_SKIP_REINDEX=1, which tests set to avoid
// re-executing the compiled test binary (os.Executable() returns the
// test binary in tests, and running that binary with "index" as argv
// re-enters the test suite instead of the production CLI).
func (w *WriteCmd) reindex(root string) {
	if w.DryRun {
		return
	}
	if os.Getenv("SCRIBE_SKIP_REINDEX") == "1" {
		return
	}
	scribeExe, _ := os.Executable()
	if scribeExe == "" {
		scribeExe = "scribe"
	}
	for _, sub := range [][]string{{"index"}, {"backlinks"}} {
		cmd := exec.Command(scribeExe, sub...) //nolint:noctx // local scribe self-invocation
		cmd.Dir = root
		_ = cmd.Run()
	}
}
