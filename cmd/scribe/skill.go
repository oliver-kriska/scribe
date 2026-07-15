package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Phase 7A: agent-skill bundle.
//
// `scribe skill install [--agent <list>]` writes the embedded skill tree
// (cmd/scribe/skills/) into each selected agent's skill-discovery directory
// under the KB root. Each skill lands in its own `<dir>/<skill-name>/`
// subdirectory, so a fresh `scribe init` followed by `scribe skill install`
// gives any agent session opening the KB a set of self-describing skills.
//
// The bundle ships two skills:
//   - scribe-kb       — how to query/write the KB (frontmatter, wikilinks, drop files)
//   - scribe-kb-tidy  — how to work the `scribe lint` content-quality queue
//     (split bloated, expand/merge thin, archive rolling, merge self-named dirs)
//
// Bundle format follows the [agentskills.io specification](https://agentskills.io/specification):
// each skill is a directory with a top-level `SKILL.md` (frontmatter:
// name, description) plus optional reference files. The SKILL.md body and
// references/ are byte-identical across Claude Code, Codex CLI, OpenCode, and
// Pi — all built on the open Agent Skills standard — so the ONLY thing that
// differs per agent is the install path. `.claude/skills` serves Claude Code;
// `.agents/skills` is the cross-tool standard that Codex, Pi, and OpenCode all
// read. Two directories therefore cover every known agent with no per-vendor
// translation (the optional Codex `agents/openai.yaml` composer metadata is not
// part of the standard and is deliberately omitted — implicit description
// matching surfaces the skills without it). See agentSkillDir below.
//
// Source of truth: cmd/scribe/skills/. Update content there; the
// embed picks it up on next `make build`. Adding a new skill means
// adding its files to the //go:embed line below — the walk, install,
// and list logic are all N-skill generic.

//go:embed skills/scribe-kb/SKILL.md skills/scribe-kb/references/*.md skills/scribe-kb-tidy/SKILL.md skills/scribe-kb-tidy/references/*.md
var skillsFS embed.FS

// skillRootInFS is the embed-FS parent of every shipped skill. The walk
// keeps the `<skill-name>/…` path segment so install recreates one
// subdirectory per skill under the target.
const skillRootInFS = "skills"

// SkillCmd is the kong CLI surface.
//
//	scribe skill install [--agent <list>] [--target <dir>] [--check] [--force]
//	scribe skill list
type SkillCmd struct {
	Install SkillInstallCmd `cmd:"" help:"Write the embedded scribe skill bundle (scribe-kb, scribe-kb-tidy) to disk."`
	List    SkillListCmd    `cmd:"" help:"List the files in the embedded skill bundle."`
}

// agentSkillDir maps an `--agent` selector to the skills directory that
// agent scans, relative to the project root. Every listed agent loads the
// SAME agentskills.io-format bundle — SKILL.md + references/ are byte-identical
// across Claude Code, Codex, OpenCode, and Pi — so only the discovery path
// differs. `.agents/skills` is the cross-tool standard read by Codex, Pi, and
// OpenCode alike, which is why two directories cover every known agent.
var agentSkillDir = map[string][]string{
	"claude":   {".claude", "skills"},   // Claude Code
	"codex":    {".agents", "skills"},   // Codex CLI — alias of "agents"
	"agents":   {".agents", "skills"},   // agentskills.io standard: Codex + Pi + OpenCode
	"opencode": {".opencode", "skills"}, // OpenCode native (also reads .claude + .agents)
	"pi":       {".pi", "skills"},       // Pi native (also reads .agents)
}

// defaultAgents is what `scribe skill install` writes with no `--agent`:
// Claude Code (`.claude/skills`) plus the cross-tool standard (`.agents/skills`),
// which together make the bundle discoverable in Claude Code, Codex, OpenCode,
// and Pi without any per-vendor translation.
var defaultAgents = []string{"claude", "agents"}

// SkillInstallCmd writes the embedded tree, one subdirectory per skill
// (e.g. `<dir>/scribe-kb/`, `<dir>/scribe-kb-tidy/`), into every selected
// agent's skill-discovery directory.
//
// Target resolution:
//
//  1. --target <dir>   explicit, agent-agnostic override — writes there directly
//  2. --agent <list>   one or more of claude, codex, agents, opencode, pi, all;
//     each maps to <project-root>/<agent-dir> (default: claude, agents)
//  3. project root      = the scribe KB root, or the cwd as a generic fallback
//
// `--check` reports drift between the embedded version and what's on
// disk without writing. Useful in pre-commit hooks or CI to surface
// "your installed skill is older than your scribe binary."
type SkillInstallCmd struct {
	Agent  []string `help:"Which agents to install for: claude, codex, agents, opencode, pi, all. Repeatable. Default: claude,agents (covers Claude Code, Codex, OpenCode, Pi)." sep:","`
	Target string   `help:"Explicit parent directory; each skill lands at <target>/<skill-name>/. Agent-agnostic — overrides --agent. Default: resolve per --agent under the KB root."`
	Check  bool     `help:"Compare embedded vs installed without writing. Exits non-zero on drift."`
	Force  bool     `help:"Overwrite even when on-disk content is newer or hand-edited."`
}

// resolveTargets returns the destination directories to write the bundle
// into. An explicit --target wins and is agent-agnostic. Otherwise each
// --agent selector maps to a directory under the project root (KB root, or
// cwd as a fallback), de-duplicated by resolved path so `codex` and `agents`
// don't double-write the same `.agents/skills`.
func (s *SkillInstallCmd) resolveTargets() ([]string, error) {
	if s.Target != "" {
		return []string{s.Target}, nil
	}

	base := "."
	if root, err := kbDir(); err == nil {
		base = root
	}

	agents := s.Agent
	if len(agents) == 0 {
		agents = defaultAgents
	}

	seen := make(map[string]struct{})
	var dirs []string
	add := func(a string) error {
		parts, ok := agentSkillDir[a]
		if !ok {
			return fmt.Errorf("unknown --agent %q (valid: claude, codex, agents, opencode, pi, all)", a)
		}
		dir := filepath.Join(append([]string{base}, parts...)...)
		if _, dup := seen[dir]; dup {
			return nil
		}
		seen[dir] = struct{}{}
		dirs = append(dirs, dir)
		return nil
	}
	for _, raw := range agents {
		a := strings.ToLower(strings.TrimSpace(raw))
		if a == "all" {
			for _, x := range []string{"claude", "agents", "opencode", "pi"} {
				if err := add(x); err != nil {
					return nil, err
				}
			}
			continue
		}
		if err := add(a); err != nil {
			return nil, err
		}
	}
	return dirs, nil
}

func (s *SkillInstallCmd) Run() error {
	embedded, err := readEmbeddedSkillFiles()
	if err != nil {
		return fmt.Errorf("read embedded skill bundle: %w", err)
	}

	dests, err := s.resolveTargets()
	if err != nil {
		return err
	}

	if s.Check {
		return s.runCheck(dests, embedded)
	}

	skills := skillNames(embedded)
	for _, dest := range dests {
		wrote, skipped, err := s.installTo(dest, embedded)
		if err != nil {
			return err
		}
		logMsg("skill", "install done: target=%s wrote=%d skipped=%d files=%d skills=%s",
			dest, wrote, skipped, len(embedded), strings.Join(skills, ","))
	}
	logMsg("skill", "use these skills by keeping the target dir(s) on the agent's skill path: %s",
		strings.Join(dests, ", "))
	return nil
}

// installTo writes the embedded bundle under dest, one subdirectory per
// skill. Idempotent: files whose on-disk content already matches embedded
// are skipped, as are hand-edited files (unless --force).
func (s *SkillInstallCmd) installTo(dest string, embedded map[string][]byte) (wrote, skipped int, err error) {
	for relPath, content := range embedded {
		out := filepath.Join(dest, relPath)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return wrote, skipped, fmt.Errorf("mkdir %s: %w", filepath.Dir(out), err)
		}

		if existing, err := os.ReadFile(out); err == nil {
			if hashBytes(existing) == hashBytes(content) {
				skipped++
				continue
			}
			if !s.Force && hasUserEdits(existing) {
				logMsg("skill", "skip (hand-edited): %s — pass --force to overwrite", out)
				skipped++
				continue
			}
		}

		if err := os.WriteFile(out, content, 0o644); err != nil {
			return wrote, skipped, fmt.Errorf("write %s: %w", out, err)
		}
		wrote++
	}
	return wrote, skipped, nil
}

// skillNames returns the distinct top-level skill directory names from a
// bundle-relative file map (e.g. "scribe-kb", "scribe-kb-tidy"), sorted.
func skillNames(embedded map[string][]byte) []string {
	seen := make(map[string]struct{})
	for rel := range embedded {
		name := rel
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			name = rel[:i]
		}
		seen[name] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sortStrings(names)
	return names
}

// runCheck compares embedded vs installed across every target directory
// without writing, returning a non-zero exit when any file is missing or
// differs. Useful in CI / pre-commit. Each finding prints its full path so
// drift in one agent's copy is distinguishable from another's.
func (s *SkillInstallCmd) runCheck(dests []string, embedded map[string][]byte) error {
	missing, drifted := 0, 0
	for _, dest := range dests {
		for relPath, content := range embedded {
			out := filepath.Join(dest, relPath)
			existing, err := os.ReadFile(out)
			if err != nil {
				missing++
				fmt.Printf("MISSING  %s\n", out)
				continue
			}
			if hashBytes(existing) != hashBytes(content) {
				drifted++
				fmt.Printf("DRIFTED  %s (run `scribe skill install` to update)\n", out)
			}
		}
	}
	if missing == 0 && drifted == 0 {
		fmt.Println("OK: installed skills match embedded version")
		return nil
	}
	return fmt.Errorf("skill drift: missing=%d drifted=%d", missing, drifted)
}

// SkillListCmd prints the files in the embedded bundle, one per line.
// Useful for sanity-checking what would be written before running
// `install`, and for downstream tooling that wants to enumerate the
// skill tree.
type SkillListCmd struct{}

func (l *SkillListCmd) Run() error {
	embedded, err := readEmbeddedSkillFiles()
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(embedded))
	for p := range embedded {
		paths = append(paths, p)
	}
	// Stable order for piping into other commands.
	sortStrings(paths)
	for _, p := range paths {
		fmt.Printf("%s\t%d bytes\n", p, len(embedded[p]))
	}
	return nil
}

// readEmbeddedSkillFiles walks the embedded FS and returns a map from
// bundle-relative path → file content. The bundle-relative path drops
// the `skills/scribe-kb/` prefix so callers can write directly under
// any target directory.
func readEmbeddedSkillFiles() (map[string][]byte, error) {
	out := make(map[string][]byte)
	err := fs.WalkDir(skillsFS, skillRootInFS, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := skillsFS.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(skillRootInFS, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// hashBytes returns a hex-encoded SHA-256. Used to short-circuit the
// install when the on-disk file already matches embedded content,
// keeping `scribe skill install` idempotent and noise-free.
func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// hasUserEdits reports whether on-disk content carries the
// hand-edit marker scribe writes when a user opts to keep their own
// version. The marker is a single line in the file:
//
//	<!-- scribe-skill: hand-edited, do not overwrite -->
//
// When present, install skips the file unless --force is passed. This
// lets a user customize the skill (e.g., add KB-specific examples) and
// keep those edits across `scribe skill install` runs.
func hasUserEdits(content []byte) bool {
	return strings.Contains(string(content), "<!-- scribe-skill: hand-edited, do not overwrite -->")
}

// sortStrings — local stable sort, kept here to avoid a sort import
// elsewhere in this file. Tiny, readable, no perf concern.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
