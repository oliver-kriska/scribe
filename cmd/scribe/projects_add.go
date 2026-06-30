package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// projects_add.go — `scribe projects add <path>` (#41).
//
// Enrolling a repo a KB owns used to be a hand edit of scribe.yaml's
// sources.include plus a manual manifest entry, and in team mode a two-file
// edit with a silent footgun (see applyLocalOverrides). This command does
// both halves: it widens sources.include (the committed scribe.yaml by
// default, or scribe.local.yaml with --local, always merge-never-replace)
// and enrolls the project in the manifest as approved (an explicit add IS
// the approval), discovered_from: manual — the overlap with #28.

type ProjectsAddCmd struct {
	Path   string `arg:"" help:"Project path to enroll (the repo, not the KB)."`
	Local  bool   `help:"Widen sources in scribe.local.yaml (this machine only) instead of the committed scribe.yaml."`
	Domain string `help:"Domain to file the project under (default: resolved from domain_aliases, else general)."`
	Name   string `help:"Manifest project name (default: derived from the path)."`
}

func (c *ProjectsAddCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	return withSyncLock(root, func() error { return c.run(root) })
}

func (c *ProjectsAddCmd) run(root string) error {
	abs, err := filepath.Abs(expandHome(c.Path))
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	if !dirExists(abs) {
		return fmt.Errorf("path does not exist: %s", abs)
	}

	cfg := loadConfig(root)

	// Fold a linked worktree into its main checkout, mirroring discovery:
	// enroll the main repo and record the worktree for drop/research scans.
	enrollPath := abs
	var worktreeOf string
	if main := worktreeMainRoot(abs); main != "" {
		// Session cwds (and `add` targets) can be subdirs of the worktree;
		// record its git toplevel so collection scans the right dir.
		worktreeOf = abs
		if top := runCmd(abs, "git", "rev-parse", "--show-toplevel"); top != "" {
			worktreeOf = filepath.Clean(top)
		}
		enrollPath = filepath.Clean(main)
		fmt.Printf("note: %s is a worktree of %s — enrolling the main checkout, recording the worktree\n", abs, enrollPath)
	}

	// A KB must never be conscripted as a source project (self-extraction
	// loop). The walk-up check covers the active KB, any other KB, and any
	// path nested inside one.
	if withinScribeKB(enrollPath) {
		return fmt.Errorf("%s is inside a scribe KB — KBs are never enrolled as source projects", enrollPath)
	}

	// exclude and allowed_remotes still apply to an explicit add. include is
	// the gate we're about to satisfy, so check only the other two against
	// the would-be config; refuse rather than silently enroll a path the
	// next sync's discovery filters would reject anyway.
	if sourceExcluded(cfg, enrollPath) {
		return fmt.Errorf("%s matches sources.exclude — remove the exclude entry first", enrollPath)
	}
	if !remoteAllowed(cfg.Sources.AllowedRemotes, enrollPath) {
		return fmt.Errorf("%s has no origin remote matching sources.allowed_remotes — add the remote to the allowlist (or push an origin) first", enrollPath)
	}

	if err := c.widenSources(root, cfg, enrollPath); err != nil {
		return err
	}

	return c.enroll(root, enrollPath, worktreeOf)
}

// widenSources adds enrollPath to sources.include when needed. The empty-
// include case is "allow all" — adding a single entry there would NARROW
// scope to just this path, so it is deliberately left untouched. The guard
// uses the committed include for a committed write and the effective
// (merged) include for a --local write, so neither path can shrink the
// other's allow-all.
func (c *ProjectsAddCmd) widenSources(root string, cfg *ScribeConfig, enrollPath string) error {
	includeForGuard := cfg.Sources.Include // merged view, for --local
	if !c.Local {
		if committed, ok := repoSensitiveView(root); ok {
			includeForGuard = committed.Sources.Include
		}
	}

	if len(includeForGuard) == 0 {
		fmt.Println("note: sources.include is empty (allow-all) — leaving it alone; the path is already in scope")
		return nil
	}
	if includeCovers(includeForGuard, enrollPath) {
		return nil // already in scope, nothing to widen
	}

	target := "scribe.yaml"
	if c.Local {
		target = localConfigName
	}
	added, err := appendIncludePath(filepath.Join(root, target), enrollPath)
	if err != nil {
		return fmt.Errorf("update %s: %w", target, err)
	}
	if added {
		fmt.Printf("added %s to sources.include in %s\n", enrollPath, target)
	}
	if !c.Local {
		retrustAfterConfigEdit(root)
		fmt.Println("note: teammates pulling this scribe.yaml will see it as drift — they accept it with `scribe config trust`")
	}
	return nil
}

// enroll writes (or updates) the manifest entry for enrollPath as an
// approved, manually-added project. A pre-existing entry for the same path
// is approved-if-pending and gets the folded worktree recorded; a name that
// already maps to a DIFFERENT path is a hard error (pass --name).
func (c *ProjectsAddCmd) enroll(root, enrollPath, worktreeOf string) error {
	manifest, err := loadManifest(root)
	if err != nil {
		return err
	}

	pname := c.Name
	if pname == "" {
		pname = projectName(enrollPath)
	}
	domain := c.Domain
	if domain == "" {
		domain = manifest.resolveDomain(enrollPath)
	}

	// An explicit add overrides a prior `projects ignore`.
	manifest.unignorePath(enrollPath)

	if existing, ok := manifest.Projects[pname]; ok && existing != nil {
		if !samePath(existing.Path, enrollPath) {
			return fmt.Errorf("project name %q already maps to %s — pass --name to enroll %s under a different name", pname, existing.Path, enrollPath)
		}
		changed := false
		if !existing.IsApproved() {
			existing.Status = statusApproved
			ensureRepoYAML(root, existing.Path, pname, existing.Domain)
			fmt.Printf("approved existing project %s\n", pname)
			changed = true
		}
		if worktreeOf != "" && existing.recordWorktree(worktreeOf) {
			fmt.Printf("recorded worktree %s\n", worktreeOf)
			changed = true
		}
		if !changed {
			fmt.Printf("%s already enrolled (%s)\n", pname, existing.Path)
			return nil
		}
		return manifest.save()
	}

	entry := &ProjectEntry{
		Path:           enrollPath,
		Domain:         domain,
		DiscoveredFrom: "manual",
		Status:         statusApproved,
	}
	if worktreeOf != "" {
		entry.Worktrees = []string{worktreeOf}
	}
	manifest.Projects[pname] = entry
	if err := manifest.save(); err != nil {
		return err
	}
	ensureRepoYAML(root, enrollPath, pname, domain)
	if !hasGit(enrollPath) {
		fmt.Printf("note: %s is not a git repo — enrolled, but extraction records it as no-git\n", enrollPath)
	}
	fmt.Printf("enrolled %s -> %s (domain: %s, approved, via manual)\n", pname, enrollPath, domain)
	return nil
}

// retrustAfterConfigEdit re-records this machine's trust snapshot after a
// deliberate committed-scribe.yaml edit, so the actor's own next sync sees
// no drift and doesn't revert the change it just made. No-op when no trust
// record exists (a solo KB, or a team KB this machine hasn't synced yet —
// the next sync's TOFU records the already-edited file).
func retrustAfterConfigEdit(root string) {
	if loadTrustRecord(root) == nil {
		return
	}
	sv, ok := repoSensitiveView(root)
	if !ok {
		return
	}
	if err := saveTrustRecord(root, trustRecord{Sensitive: sv, ApprovedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		logMsg("config", "could not update trust record after add: %v", err)
	}
}

// appendIncludePath inserts path into the sources.include sequence of the
// YAML file at filePath, preserving the rest of the file (comments, key
// order, surrounding blocks) via the yaml.v3 Node API. It creates the file,
// the top-level `sources` mapping, and the `include` sequence as needed.
// Idempotent: an exact-string entry already present returns added=false and
// leaves the file untouched.
func appendIncludePath(filePath, path string) (bool, error) {
	var doc yaml.Node
	data, err := os.ReadFile(filePath)
	switch {
	case err == nil:
		if uerr := yaml.Unmarshal(data, &doc); uerr != nil {
			return false, fmt.Errorf("parse: %w", uerr)
		}
	case os.IsNotExist(err):
		// fresh file — built below
	default:
		return false, err
	}

	if doc.Kind == 0 {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return false, fmt.Errorf("unexpected YAML structure in %s", filePath)
	}
	top := doc.Content[0]

	sources := mappingValue(top, "sources")
	if sources == nil {
		sources = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		top.Content = append(top.Content, scalarNode("sources"), sources)
	} else if sources.Kind != yaml.MappingNode {
		*sources = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}

	include := mappingValue(sources, "include")
	if include == nil {
		include = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		sources.Content = append(sources.Content, scalarNode("include"), include)
	} else if include.Kind != yaml.SequenceNode {
		*include = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	}

	for _, item := range include.Content {
		if item.Value == path {
			return false, nil // already present
		}
	}
	include.Content = append(include.Content, scalarNode(path))

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return false, err
	}
	if err := enc.Close(); err != nil {
		return false, err
	}

	tmp := filePath + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, filePath)
}

// mappingValue returns the value node for key in a mapping node, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// scalarNode builds a plain string scalar node.
func scalarNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}
