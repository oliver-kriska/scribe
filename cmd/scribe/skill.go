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
// `scribe skill install [--target <dir>]` writes the embedded skill
// tree (cmd/scribe/skills/) to the user's chosen location. The
// default target is `.claude/skills/scribe-kb/` in the KB root, so a
// fresh `scribe init` followed by `scribe skill install` gives any
// Claude Code session opening the KB a self-describing skill bundle.
//
// Bundle format follows the [agentskills.io specification](https://agentskills.io/specification):
// a top-level `SKILL.md` with frontmatter (name, description) plus
// optional reference files. Compatible with Claude Code, Codex CLI,
// and OpenCode without any per-vendor adaptation.
//
// Source of truth: cmd/scribe/skills/. Update content there; the
// embed picks it up on next `make build`.

//go:embed skills/scribe-kb/SKILL.md skills/scribe-kb/references/*.md
var skillsFS embed.FS

const skillRootInFS = "skills/scribe-kb"

// SkillCmd is the kong CLI surface.
//
//	scribe skill install [--target <dir>] [--check] [--force]
//	scribe skill list
type SkillCmd struct {
	Install SkillInstallCmd `cmd:"" help:"Write the embedded scribe-kb skill bundle to disk."`
	List    SkillListCmd    `cmd:"" help:"List the files in the embedded skill bundle."`
}

// SkillInstallCmd writes the embedded tree under `<target>/scribe-kb/`.
//
// Default target resolution:
//
//  1. --target <dir>             explicit override
//  2. KB root + ".claude/skills"  when invoked inside a scribe KB
//  3. ./.claude/skills            generic fallback
//
// `--check` reports drift between the embedded version and what's on
// disk without writing. Useful in pre-commit hooks or CI to surface
// "your installed skill is older than your scribe binary."
type SkillInstallCmd struct {
	Target string `help:"Parent directory; the bundle lands at <target>/scribe-kb/. Default: KB-root/.claude/skills."`
	Check  bool   `help:"Compare embedded vs installed without writing. Exits non-zero on drift."`
	Force  bool   `help:"Overwrite even when on-disk content is newer or hand-edited."`
}

func (s *SkillInstallCmd) Run() error {
	target := s.Target
	if target == "" {
		root, err := kbDir()
		if err == nil {
			target = filepath.Join(root, ".claude", "skills")
		} else {
			target = filepath.Join(".claude", "skills")
		}
	}
	dest := filepath.Join(target, "scribe-kb")

	embedded, err := readEmbeddedSkillFiles()
	if err != nil {
		return fmt.Errorf("read embedded skill bundle: %w", err)
	}

	if s.Check {
		return s.runCheck(dest, embedded)
	}

	wrote, skipped := 0, 0
	for relPath, content := range embedded {
		out := filepath.Join(dest, relPath)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(out), err)
		}

		// Skip when on-disk matches embedded (idempotent re-runs).
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
			return fmt.Errorf("write %s: %w", out, err)
		}
		wrote++
	}

	logMsg("skill", "install done: target=%s wrote=%d skipped=%d files=%d",
		dest, wrote, skipped, len(embedded))
	logMsg("skill", "use this skill in Claude Code by ensuring %s is on the agent's skill path", dest)
	return nil
}

// runCheck compares embedded vs installed without writing, returning
// a non-zero exit when any file differs. Useful in CI / pre-commit.
func (s *SkillInstallCmd) runCheck(dest string, embedded map[string][]byte) error {
	missing, drifted := 0, 0
	for relPath, content := range embedded {
		out := filepath.Join(dest, relPath)
		existing, err := os.ReadFile(out)
		if err != nil {
			missing++
			fmt.Printf("MISSING  %s\n", relPath)
			continue
		}
		if hashBytes(existing) != hashBytes(content) {
			drifted++
			fmt.Printf("DRIFTED  %s (run `scribe skill install` to update)\n", relPath)
		}
	}
	if missing == 0 && drifted == 0 {
		fmt.Println("OK: installed skill matches embedded version")
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
