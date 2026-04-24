package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/template"
	"time"
)

// templateFS carries every *template file* embedded in the binary. Kept here
// (rather than in templates/ next to each consumer) so the same embed.FS
// serves `scribe init` for a fresh KB bootstrap AND the CLAUDE.md block
// sync for existing KBs.
//
//go:embed templates/*.md templates/*.yaml templates/gitignore
var templateFS embed.FS

const (
	claudeMDMarkerBegin = "<!-- scribe:begin — managed by `scribe init`, do not edit by hand -->"
	claudeMDMarkerEnd   = "<!-- scribe:end -->"
)

// depSpec describes an external binary scribe depends on. Shared between
// `scribe init` and `scribe doctor` so the two can't drift.
type depSpec struct {
	Name     string
	Binary   string
	Required bool
	Note     string
	Fix      string
}

var scribeDeps = []depSpec{
	{Name: "claude", Binary: "claude", Required: true, Note: "Claude Code CLI — required for session extraction", Fix: "curl -fsSL https://claude.ai/install.sh | bash"},
	{Name: "ccrider", Binary: "ccrider", Required: true, Note: "session database — required for `scribe triage`", Fix: "brew install neilberkman/tap/ccrider"},
	{Name: "qmd", Binary: "qmd", Required: true, Note: "semantic search index over the KB", Fix: "npm install -g @tobilu/qmd"},
	{Name: "sqlite3", Binary: "sqlite3", Required: true, Note: "required by ccrider + capture", Fix: "brew install sqlite3 / apt install sqlite3"},
	{Name: "git", Binary: "git", Required: true, Note: "KB auto-commit + cron sync", Fix: "install git"},
	{Name: "trafilatura", Binary: "trafilatura", Required: false, Note: "URL → markdown (fallback: Jina Reader)", Fix: "pipx install trafilatura"},
	{Name: "jq", Binary: "jq", Required: false, Note: "JSON helpers in fzf preview + manual triage", Fix: "brew install jq / apt install jq"},
	{Name: "fzf", Binary: "fzf", Required: false, Note: "`scribe triage --interactive` requires this", Fix: "brew install fzf / apt install fzf"},
}

// InitCmd creates a new scribe knowledge base, or checks/upgrades an existing
// one. Running in a directory that already has `scripts/projects.json` acts
// as status/check; running elsewhere creates a fresh KB from embedded
// templates.
type InitCmd struct {
	Path         string   `help:"Create a fresh KB at this path. Defaults to current directory when bootstrapping." short:"p"`
	OwnerName    string   `help:"Owner name (e.g. your display name)."`
	OwnerContext string   `help:"One-paragraph owner context (role, projects, preferences)."`
	Handle       string   `help:"iMessage self-chat handle for capture (phone or email; optional)."`
	Domains      []string `help:"Domains this KB uses (comma-separated). 'personal' and 'general' are always added."`
	KBName       string   `help:"Display name for the KB (defaults to the directory basename)."`
	Check        bool     `help:"Only check status; never prompt or write files." short:"c"`
	Yes          bool     `help:"Assume yes to prompts (non-interactive)." short:"y"`
	NoGit        bool     `help:"Skip git init in the new KB." name:"no-git"`
	NoCron       bool     `help:"Skip cron setup instructions." name:"no-cron"`
	Force        bool     `help:"Overwrite existing KB files during bootstrap."`
}

// templateVars is what every embedded template receives. One struct is easier
// to reason about than threading a map through every template call site, and
// Go's text/template rejects unknown fields at parse time — so a typo fails
// fast rather than silently emitting "<no value>".
type templateVars struct {
	OwnerName            string
	OwnerContext         string
	OwnerContextIndented string
	KBName               string
	KBDir                string
	Domains              []string
	DomainsCSV           string
	DomainsPipe          string
	SelfChatHandle       string
	CodePatternKeywords  string
	Today                string
}

func (c *InitCmd) Run() error {
	// Decide between status-mode (existing KB) and bootstrap-mode (new KB).
	root, err := kbDir()
	bootstrap := err != nil
	if c.Path != "" {
		bootstrap = true
	}
	if bootstrap {
		return c.runBootstrap()
	}
	return c.runStatus(root)
}

// --- bootstrap mode ---

func (c *InitCmd) runBootstrap() error {
	target := c.Path
	if target == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve cwd: %w", err)
		}
		target = cwd
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", abs, err)
	}

	// Refuse to overwrite an existing KB unless --force was given.
	manifestPath := filepath.Join(abs, "scripts", "projects.json")
	if fileExists(manifestPath) && !c.Force {
		return fmt.Errorf("%s already looks like a scribe KB (scripts/projects.json exists)\nRe-run with --force to overwrite, or drop --path to run status checks", abs)
	}

	vars, err := c.collectVars(abs)
	if err != nil {
		return err
	}

	fmt.Printf("Bootstrapping KB at %s\n\n", abs)
	if err := writeEmbeddedTemplates(abs, vars); err != nil {
		return err
	}
	if err := createKBSkeleton(abs); err != nil {
		return err
	}
	if !c.NoGit {
		if err := initGit(abs); err != nil {
			fmt.Printf("  git init skipped: %v\n", err)
		}
	}
	// Bootstrap never silently re-points an existing user config or
	// CLAUDE.md block away from another KB — that would be destructive to
	// someone with multiple scribes. Require --force or --yes to proceed.
	allowUserWrites := c.Force || c.Yes
	uc := loadUserConfig()
	if uc.KBDir != "" && uc.KBDir != abs && !allowUserWrites {
		fmt.Printf("\n~/.config/scribe/config.yaml already points at %s.\n", uc.KBDir)
		fmt.Printf("Not re-pointing to %s — pass --force to switch, or manually edit the file.\n", abs)
	} else if err := installUserConfig(abs, c.Check, true); err != nil {
		fmt.Printf("  warning: %s\n", err)
	}
	if allowUserWrites {
		if err := installClaudeMD(abs, vars, c.Check, true); err != nil {
			fmt.Printf("  warning: %s\n", err)
		}
	} else {
		fmt.Println("  (skipping ~/.claude/CLAUDE.md block — run `scribe init --yes` from inside the new KB to install)")
	}

	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Review the generated scribe.yaml and tweak domains/triage keywords.")
	fmt.Println("  2. Install cron: scribe cron install")
	fmt.Println("     (On Linux, `scribe cron install` prints crontab entries you paste manually.)")
	fmt.Println("  3. Run `scribe doctor` to verify dependencies + freshness checks.")

	// Offer to walk the user through Full Disk Access right now. Only on
	// macOS with capture enabled (handle was set); Linux capture uses a
	// different path that doesn't need TCC, and a KB with no handle has no
	// capture pipeline to worry about.
	if runtime.GOOS == "darwin" && vars.SelfChatHandle != "" {
		fmt.Println()
		if c.Yes || promptYesNo("Grant Full Disk Access now? (required for scribe capture on macOS)", true) {
			cmd := &FDACmd{}
			if err := cmd.Run(); err != nil {
				fmt.Printf("  FDA setup reported: %v\n", err)
				fmt.Println("  Re-run later: scribe fda")
			}
		} else {
			fmt.Println("  Skipped. Run later: scribe fda")
		}
	}
	return nil
}

// collectVars gathers template values from flags + prompts. In --yes or
// --check mode any missing value falls back to a safe default so init can
// run unattended (CI, shell scripts).
func (c *InitCmd) collectVars(abs string) (templateVars, error) {
	kbName := c.KBName
	if kbName == "" {
		kbName = filepath.Base(abs)
	}
	owner := c.OwnerName
	if owner == "" && !c.Yes {
		owner = prompt("Owner name", os.Getenv("USER"))
	}
	if owner == "" {
		owner = os.Getenv("USER")
	}
	ownerCtx := c.OwnerContext
	if ownerCtx == "" && !c.Yes {
		ownerCtx = prompt("Owner context (one sentence about your role/projects)", "Knowledge base owner.")
	}
	if ownerCtx == "" {
		ownerCtx = "Knowledge base owner."
	}
	domains := c.Domains
	if len(domains) == 0 && !c.Yes {
		raw := prompt("Domains (comma-separated, e.g. work, oss, personal)", "")
		for d := range strings.SplitSeq(raw, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				domains = append(domains, d)
			}
		}
	}
	handle := c.Handle
	if handle == "" && !c.Yes {
		handle = prompt("iMessage self-chat handle (leave empty to disable capture)", "")
	}

	// Universal domains are always appended.
	seen := map[string]bool{}
	dedup := []string{}
	for _, d := range domains {
		if !seen[d] {
			seen[d] = true
			dedup = append(dedup, d)
		}
	}
	for _, d := range universalDomains {
		if !seen[d] {
			seen[d] = true
			dedup = append(dedup, d)
		}
	}
	sort.Strings(dedup)

	return templateVars{
		OwnerName:            owner,
		OwnerContext:         ownerCtx,
		OwnerContextIndented: indent(ownerCtx, "  "),
		KBName:               kbName,
		KBDir:                abs,
		Domains:              dedup,
		DomainsCSV:           strings.Join(dedup, ", "),
		DomainsPipe:          strings.Join(dedup, " | "),
		SelfChatHandle:       handle,
		CodePatternKeywords:  defaultTriageKeywords["code_pattern"],
		Today:                time.Now().UTC().Format("2006-01-02"),
	}, nil
}

// writeEmbeddedTemplates renders each embedded template against `vars` and
// writes it to its target path under `root`. File mapping is explicit so
// renames are discoverable by reading this one table.
func writeEmbeddedTemplates(root string, vars templateVars) error {
	mapping := []struct {
		Src, Dst string
	}{
		{"templates/kb-CLAUDE.md", "CLAUDE.md"},
		{"templates/scribe.yaml", "scribe.yaml"},
		{"templates/gitignore", ".gitignore"},
		{"templates/wiki-index.md", "wiki/_index.md"},
		{"templates/wiki-hot.md", "wiki/_hot.md"},
		{"templates/log.md", "log.md"},
	}
	for _, m := range mapping {
		out, err := renderTemplate(m.Src, vars)
		if err != nil {
			return err
		}
		dst := filepath.Join(root, m.Dst)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, []byte(out), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
		fmt.Printf("  wrote %s\n", m.Dst)
	}
	return nil
}

func renderTemplate(name string, vars templateVars) (string, error) {
	data, err := templateFS.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("read template %s: %w", name, err)
	}
	tmpl, err := template.New(name).Parse(string(data))
	if err != nil {
		return "", fmt.Errorf("parse template %s: %w", name, err)
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", fmt.Errorf("render template %s: %w", name, err)
	}
	return buf.String(), nil
}

// createKBSkeleton ensures all wiki directories exist and seeds the two
// JSON state files with empty objects so downstream scribe commands don't
// crash on missing files.
func createKBSkeleton(root string) error {
	dirs := append([]string{}, wikiDirs...)
	dirs = append(dirs, "raw/articles", "raw/imports", "raw/assets", "scripts", "output/runs")
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	// Seed empty state files so scribe sync / lint / triage work out of the box.
	seeds := []struct {
		rel, body string
	}{
		{"scripts/projects.json", seedProjectsJSON()},
		{"wiki/_backlinks.json", "{}\n"},
		{"wiki/_sessions_log.json", `{"processed":{}}` + "\n"},
	}
	for _, s := range seeds {
		path := filepath.Join(root, s.rel)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := os.WriteFile(path, []byte(s.body), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", s.rel, err)
		}
	}
	return nil
}

// seedProjectsJSON produces an empty, well-formed manifest so the scan/sync
// pipeline can run immediately without a special "file missing" branch.
func seedProjectsJSON() string {
	empty := map[string]any{
		"projects":       map[string]any{},
		"ignored_paths":  []string{},
		"domain_aliases": map[string]string{},
	}
	b, _ := json.MarshalIndent(empty, "", "  ")
	return string(b) + "\n"
}

func initGit(root string) error {
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		fmt.Println("  .git already exists — skipping git init")
		return nil
	}
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git not in PATH")
	}
	out, err := exec.Command("git", "-C", root, "init", "-b", "main").CombinedOutput() //nolint:noctx // one-shot init
	if err != nil {
		return fmt.Errorf("git init: %w\n%s", err, out)
	}
	fmt.Println("  git init done")
	return nil
}

// --- status mode ---

func (c *InitCmd) runStatus(root string) error {
	fmt.Printf("KB root: %s\n\n", root)

	fmt.Println("Dependencies:")
	missingRequired := 0
	for _, d := range scribeDeps {
		path, err := exec.LookPath(d.Binary)
		status := path
		if err != nil {
			status = "MISSING"
			if d.Required {
				missingRequired++
			} else {
				status = "missing (optional)"
			}
		}
		fmt.Printf("  %-13s %s\n", d.Name, status)
		if d.Note != "" && (err != nil || d.Required) {
			fmt.Printf("               %s\n", d.Note)
		}
	}

	cfgPath := filepath.Join(root, "scribe.yaml")
	fmt.Println("\nConfig (scribe.yaml):")
	if fileExists(cfgPath) {
		fmt.Printf("  %s (present)\n", cfgPath)
		// Top up missing sections with their commented defaults so long-
		// running KBs don't drift behind new config knobs.
		if !c.Check {
			if added, err := ensureAbsorbSection(cfgPath, c.Yes); err != nil {
				fmt.Printf("  warning: could not merge absorb defaults: %v\n", err)
			} else if added {
				fmt.Println("  added missing `absorb:` section with commented defaults")
			}
		}
	} else {
		fmt.Printf("  %s MISSING\n", cfgPath)
		if !c.Check && (c.Yes || promptYesNo("  create default scribe.yaml?", true)) {
			vars, err := c.collectVars(root)
			if err != nil {
				return err
			}
			out, err := renderTemplate("templates/scribe.yaml", vars)
			if err != nil {
				return err
			}
			if err := os.WriteFile(cfgPath, []byte(out), 0o644); err != nil {
				return fmt.Errorf("write %s: %w", cfgPath, err)
			}
			fmt.Printf("  wrote %s\n", cfgPath)
		}
	}

	cfg := loadConfig(root)
	fmt.Printf("  owner_name:          %s\n", cfg.OwnerName)
	fmt.Printf("  domains:             %s\n", strings.Join(cfg.AllDomains(), ", "))
	fmt.Printf("  claude_projects_dir: %s\n", cfg.ClaudeProjectsDir)
	fmt.Printf("  ccrider_db:          %s\n", cfg.CcriderDB)
	fmt.Printf("  default_model:       %s\n", cfg.DefaultModel)
	fmt.Printf("  capture.self_chat:   %s\n", cfg.Capture.SelfChatHandle)
	fmt.Printf("  contextualize:       %s/%s\n", cfg.Absorb.Contextualize.Provider, cfg.Absorb.Contextualize.Model)
	if strings.EqualFold(cfg.Absorb.Contextualize.Provider, "anthropic") {
		fmt.Println("                       tip: set absorb.contextualize.provider: ollama in scribe.yaml for free local mode")
		fmt.Println("                            (one-time: `brew install ollama`; scribe auto-pulls the model on first run)")
	}

	fmt.Println("\nUser config (~/.config/scribe/config.yaml):")
	if err := installUserConfig(root, c.Check, c.Yes); err != nil {
		fmt.Printf("  warning: %s\n", err)
	}

	fmt.Println("\nUser CLAUDE.md (~/.claude/CLAUDE.md):")
	vars, _ := c.collectVars(root)
	// In status mode, prefer the stored config over fresh prompts.
	if cfg.OwnerName != "" {
		vars.OwnerName = cfg.OwnerName
	}
	if cfg.OwnerContext != "" {
		vars.OwnerContext = cfg.OwnerContext
	}
	vars.KBDir = root
	vars.Domains = cfg.AllDomains()
	vars.DomainsCSV = strings.Join(vars.Domains, ", ")
	vars.DomainsPipe = strings.Join(vars.Domains, " | ")
	if err := installClaudeMD(root, vars, c.Check, c.Yes); err != nil {
		fmt.Printf("  warning: %s\n", err)
	}

	if !c.NoCron {
		fmt.Println("\nCron:")
		fmt.Println("  Install with:  scribe cron install")
		fmt.Println("  Check status:  scribe cron status")
	}

	fmt.Println()
	if missingRequired > 0 {
		return fmt.Errorf("%d required dependency(ies) missing — install them before running scribe sync", missingRequired)
	}
	fmt.Println("KB is ready.")
	return nil
}

// ensureAbsorbSection appends the commented absorb-defaults block to
// scribe.yaml if it has no top-level `absorb:` key. Never mutates an
// existing absorb section — user values win. When yes is true or stdin
// is non-interactive, skips the confirmation prompt.
//
// Returns (added, err). `added == true` means the block was written. A
// best-effort atomic write: original bytes + block, swapped via temp file.
func ensureAbsorbSection(cfgPath string, yes bool) (bool, error) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return false, err
	}
	if hasTopLevelKey(string(data), "absorb") {
		return false, nil
	}
	if !yes && !promptYesNo("  append commented `absorb:` defaults to scribe.yaml?", true) {
		return false, nil
	}
	block := absorbDefaultYAMLBlock()
	// Ensure file ends with a newline before appending.
	merged := string(data)
	if !strings.HasSuffix(merged, "\n") {
		merged += "\n"
	}
	merged += block

	tmp := cfgPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(merged), 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, cfgPath); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// hasTopLevelKey returns true if any line of the YAML document starts with
// `<key>:` at column zero (ignoring leading BOM and blank-line padding).
// Comments (#-prefixed) are skipped so a commented-out key doesn't count as
// present. Good enough for our single-level config structure.
func hasTopLevelKey(yaml, key string) bool {
	prefix := key + ":"
	for line := range strings.SplitSeq(yaml, "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

// --- helpers shared with doctor ---

func claudeMDPath() string {
	return filepath.Join(os.Getenv("HOME"), ".claude", "CLAUDE.md")
}

// buildClaudeMDBlock renders the embedded block template against the given
// vars and wraps it in the idempotency markers.
func buildClaudeMDBlock(vars templateVars) (string, error) {
	body, err := renderTemplate("templates/claude-md-kb.md", vars)
	if err != nil {
		return "", err
	}
	body = strings.TrimRight(body, "\n")
	return claudeMDMarkerBegin + "\n" + body + "\n" + claudeMDMarkerEnd, nil
}

// installClaudeMD syncs the scribe block in ~/.claude/CLAUDE.md. Four
// cases: missing, present-without-markers, present-in-sync, present-drifted.
// User content outside the markers is never touched.
func installClaudeMD(_ string, vars templateVars, check, yes bool) error {
	path := claudeMDPath()
	block, err := buildClaudeMDBlock(vars)
	if err != nil {
		return err
	}

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	if os.IsNotExist(err) {
		fmt.Printf("  %s MISSING\n", path)
		if check {
			fmt.Println("  (check mode — not creating)")
			return nil
		}
		if !yes && !promptYesNo("  create with scribe block?", true) {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("mkdir: %w", err)
		}
		if err := os.WriteFile(path, []byte(block+"\n"), 0o644); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		fmt.Printf("  wrote %s\n", path)
		return nil
	}

	content := string(existing)
	beginIdx := strings.Index(content, claudeMDMarkerBegin)
	if beginIdx < 0 {
		fmt.Printf("  %s (no scribe block)\n", path)
		if check {
			fmt.Println("  (check mode — not appending)")
			return nil
		}
		if !yes && !promptYesNo("  append scribe block to the end?", true) {
			return nil
		}
		sep := "\n\n"
		switch {
		case strings.HasSuffix(content, "\n\n"):
			sep = ""
		case strings.HasSuffix(content, "\n"):
			sep = "\n"
		}
		if err := os.WriteFile(path, []byte(content+sep+block+"\n"), 0o644); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		fmt.Printf("  appended scribe block to %s\n", path)
		return nil
	}

	endIdx := strings.Index(content[beginIdx:], claudeMDMarkerEnd)
	if endIdx < 0 {
		return fmt.Errorf("found %q but no matching %q in %s — refusing to touch a malformed block",
			claudeMDMarkerBegin, claudeMDMarkerEnd, path)
	}
	endIdx += beginIdx + len(claudeMDMarkerEnd)
	currentBlock := content[beginIdx:endIdx]

	if currentBlock == block {
		fmt.Printf("  %s (scribe block up to date)\n", path)
		return nil
	}

	fmt.Printf("  %s (scribe block out of date)\n", path)
	if check {
		fmt.Println("  (check mode — not refreshing)")
		return nil
	}
	if !yes && !promptYesNo("  refresh scribe block?", true) {
		return nil
	}
	updated := content[:beginIdx] + block + content[endIdx:]
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	fmt.Printf("  refreshed scribe block in %s\n", path)
	return nil
}

// installUserConfig writes ~/.config/scribe/config.yaml with kb_dir set to
// the current KB root. Lets scribe work from any directory without env vars.
func installUserConfig(root string, check, yes bool) error {
	path := userConfigPath()
	uc := loadUserConfig()

	if uc.KBDir == root {
		fmt.Printf("  %s (kb_dir up to date)\n", path)
		return nil
	}

	if uc.KBDir != "" {
		fmt.Printf("  %s (kb_dir: %s → %s)\n", path, uc.KBDir, root)
	} else {
		fmt.Printf("  %s (kb_dir not set)\n", path)
	}

	if check {
		fmt.Println("  (check mode — not writing)")
		return nil
	}
	if !yes && !promptYesNo(fmt.Sprintf("  set kb_dir to %s?", root), true) {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	content := fmt.Sprintf("# scribe user config — written by `scribe init`\nkb_dir: %s\n", root)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	fmt.Printf("  wrote %s\n", path)
	return nil
}

// --- prompt helpers ---

func promptYesNo(question string, defaultYes bool) bool {
	suffix := " [Y/n] "
	if !defaultYes {
		suffix = " [y/N] "
	}
	fmt.Print(question + suffix)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return defaultYes
	}
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return defaultYes
	}
	return line == "y" || line == "yes"
}

func prompt(question, defaultValue string) string {
	if defaultValue != "" {
		fmt.Printf("%s [%s]: ", question, defaultValue)
	} else {
		fmt.Printf("%s: ", question)
	}
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return defaultValue
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultValue
	}
	return line
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = prefix + ln
	}
	return strings.Join(lines, "\n")
}
