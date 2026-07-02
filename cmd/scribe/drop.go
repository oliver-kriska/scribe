// drop.go — `scribe drop`: author a validated drop-file handoff for the
// current (non-KB) project's `.claude/<kb_name>/` directory. See
// cmd/scribe/skills/scribe-kb/references/DROP_FILES.md for the schema this
// enforces and cmd/scribe/skills/scribe-kb/SKILL.md for the agent-facing
// workflow this replaces hand-authored frontmatter for.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// dropValidActions and dropValidRollingTargets are drop-file-only closed
// sets — distinct from validTypes/validDomainsForRoot (validate.go), which
// govern wiki articles, not the handoff documents that precede them.
var (
	dropValidActions        = map[string]bool{"create": true, "update": true, "append": true}
	dropValidRollingTargets = map[string]bool{"learnings": true, "decisions-log": true}
	// bareYAMLKeyRE matches a key that never needs quoting.
	bareYAMLKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
)

// DropCmd writes a schema-validated drop file to the current project's
// .claude/<kb_name>/ directory, for a later `scribe sync` in the target KB
// to absorb. Unlike WriteCmd, this never requires standing inside a KB
// checkout — kbDir() resolution is used only to learn the KB's name and
// (when resolvable) its valid domain set; the file itself is always
// written relative to the CURRENT directory, never the KB root.
type DropCmd struct {
	Title         string   `help:"Article title (required)." short:"T"`
	Type          string   `help:"Article type: project | tool | person | decision | pattern | solution | research | idea." short:"t"`
	Domain        string   `help:"Domain tag. Validated against the resolved KB's scribe.yaml when one is found." short:"d"`
	Action        string   `help:"create | update | append." default:"create"`
	Tags          []string `help:"Tags (repeatable or comma-separated)." short:"g"`
	RollingTarget string   `help:"learnings | decisions-log. Requires --action append." name:"rolling-target"`
	Body          string   `help:"Body source: '-' for stdin, 'file:path' for file, or inline string." default:"-"`
	Slug          string   `help:"Override the filename slug (default: derived from --title)."`
	Date          string   `help:"Override the YYYY-MM-DD date prefix (default: today)."`
	KBName        string   `help:"Override the resolved KB name (required if no KB can be resolved)." name:"kb-name"`
	Force         bool     `help:"Overwrite an existing drop file at the target path."`
	DryRun        bool     `help:"Print the drop file without writing." name:"dry-run"`
}

func (d *DropCmd) Run() error {
	if err := d.validateStatic(); err != nil {
		return err
	}

	root, kbResolved := "", true
	r, err := kbDir()
	if err != nil {
		kbResolved = false
	} else {
		root = r
	}

	kbNameResolved, domains, err := d.resolveKBContext(root, kbResolved)
	if err != nil {
		return err
	}
	if !domains[d.Domain] {
		if kbResolved {
			return fmt.Errorf("invalid --domain %q (expected one of: %s)", d.Domain, strings.Join(sortedKeys(domains), ", "))
		}
		fmt.Fprintf(os.Stderr, "scribe drop: warning: no KB resolved — --domain %q not validated against any config\n", d.Domain)
	}

	body, err := readBodySource(d.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return fmt.Errorf("body is empty (read from %s)", d.Body)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	d.warnIfInsideKB(root, kbResolved, cwd)

	date := d.Date
	if date == "" {
		date = time.Now().UTC().Format("2006-01-02")
	}
	slug := d.Slug
	if slug == "" {
		slug = slugify(d.Title)
	}
	if slug == "" {
		return errors.New("--title produced an empty slug; pass --slug explicitly")
	}

	dropDir := filepath.Join(cwd, ".claude", kbNameResolved)
	target := filepath.Join(dropDir, fmt.Sprintf("%s-%s.md", date, slug))

	if fileExists(target) && !d.Force {
		return fmt.Errorf("%s already exists — pass --force to overwrite, or vary --slug/--date", target)
	}

	content := d.buildFrontmatter(kbNameResolved) + "\n" + body + "\n"

	if d.DryRun {
		fmt.Fprintf(os.Stderr, "# would write: %s\n", target)
		fmt.Print(content)
		return nil
	}

	if err := os.MkdirAll(dropDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dropDir, err)
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	logMsg("drop", "wrote %s (%d bytes)", target, len(content))
	fmt.Printf("dropped: %s\n", target)
	return nil
}

// validateStatic checks everything that doesn't require KB resolution:
// required fields and the two drop-file-only closed sets.
func (d *DropCmd) validateStatic() error {
	if d.Title == "" {
		return errors.New("--title is required")
	}
	if !validTypes[d.Type] {
		return fmt.Errorf("invalid --type %q (expected one of: %s)", d.Type, strings.Join(sortedKeys(validTypes), ", "))
	}
	if d.Domain == "" {
		return errors.New("--domain is required")
	}
	if !dropValidActions[d.Action] {
		return fmt.Errorf("invalid --action %q (expected: create, update, append)", d.Action)
	}
	if d.RollingTarget != "" {
		if !dropValidRollingTargets[d.RollingTarget] {
			return fmt.Errorf("invalid --rolling-target %q (expected: learnings, decisions-log)", d.RollingTarget)
		}
		if d.Action != "append" {
			return fmt.Errorf("--rolling-target requires --action append (got --action %s)", d.Action)
		}
	}
	if d.Date != "" && !dateRE.MatchString(d.Date) {
		return fmt.Errorf("--date %q not in YYYY-MM-DD format", d.Date)
	}
	return nil
}

// resolveKBContext returns the effective kb_name and the valid-domain set to
// check --domain against. When kbResolved is false, domains is an empty
// (non-nil) map so a plain `map[access]` lookup naturally reports "not
// found" without a nil-map special case in Run.
func (d *DropCmd) resolveKBContext(root string, kbResolved bool) (string, map[string]bool, error) {
	if kbResolved {
		name := d.KBName
		if name == "" {
			name = kbName(root)
		}
		return name, validDomainsForRoot(root), nil
	}
	if d.KBName == "" {
		return "", nil, errors.New("no scribe KB could be resolved (no -C, SCRIBE_KB, or default kb_dir) — pass --kb-name explicitly")
	}
	return d.KBName, map[string]bool{}, nil
}

// warnIfInsideKB nudges toward `scribe write` when cwd is inside the
// resolved KB itself — a drop file there would just wait for a sync that's
// already standing in the same checkout. Non-fatal by design; see plan
// §2.3.
func (d *DropCmd) warnIfInsideKB(root string, kbResolved bool, cwd string) {
	if !kbResolved || root == "" {
		return
	}
	cleanRoot := filepath.Clean(root) + string(os.PathSeparator)
	cleanCwd := filepath.Clean(cwd) + string(os.PathSeparator)
	if strings.HasPrefix(cleanCwd, cleanRoot) || filepath.Clean(cwd) == filepath.Clean(root) {
		fmt.Fprintf(os.Stderr, "scribe drop: warning: %s is inside the KB (%s) — consider `scribe write` instead\n", cwd, root)
	}
}

// buildFrontmatter renders the drop-file YAML block including the closing
// delimiter. kbNameKey is quoted only when it isn't a bare-safe YAML key.
func (d *DropCmd) buildFrontmatter(kbNameKey string) string {
	key := kbNameKey
	if !bareYAMLKeyRE.MatchString(key) {
		key = fmt.Sprintf("%q", key)
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "%s: true\n", key)
	fmt.Fprintf(&b, "action: %s\n", d.Action)
	fmt.Fprintf(&b, "title: %q\n", d.Title)
	fmt.Fprintf(&b, "type: %s\n", d.Type)
	fmt.Fprintf(&b, "domain: %s\n", d.Domain)
	b.WriteString("tags: [")
	b.WriteString(joinTags(d.Tags))
	b.WriteString("]\n")
	if d.RollingTarget != "" {
		fmt.Fprintf(&b, "rolling_target: %s\n", d.RollingTarget)
	}
	b.WriteString("---\n")
	return b.String()
}
