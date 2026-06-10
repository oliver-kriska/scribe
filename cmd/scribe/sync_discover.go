// sync_discover.go — sync Phase 1/1.5: project discovery (Claude + Codex
// session dirs, worktree folding, source filters) and drop/research file
// collection from tracked projects.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// discover scans ~/.claude/projects/ for new projects and adds them to the manifest.
func (s *SyncCmd) discover(root string, manifest *Manifest, cfg *ScribeConfig) (int, error) {
	claudeDir := cfg.ClaudeProjectsDir
	if !dirExists(claudeDir) {
		return 0, nil
	}

	entries, err := os.ReadDir(claudeDir)
	if err != nil {
		return 0, fmt.Errorf("read claude projects dir: %w", err)
	}

	discovered := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		decoded := decodeClaudePath(entry.Name())
		if decoded == "" || !dirExists(decoded) {
			continue
		}

		if manifest.isIgnored(decoded) {
			continue
		}

		// Source filters (sources.include / sources.exclude in
		// scribe.yaml) gate discovery before the manifest ever sees the
		// path. Quiet skip — the filter is explicit user config.
		if !sourceAllowed(cfg, decoded) {
			continue
		}

		// Linked worktrees fold into the main repo's project instead of
		// enrolling as their own — see worktreeMainRoot. The worktree
		// path is recorded for drop/research collection.
		if main := worktreeMainRoot(decoded); main != "" {
			if n, changed := s.foldWorktree(root, manifest, cfg, decoded, main, "claude"); changed {
				discovered += n
			}
			continue
		}

		if !hasSignificantContent(decoded) {
			continue
		}

		pname := projectName(decoded)
		if existing, exists := manifest.Projects[pname]; exists {
			// Project already known. If it was previously surfaced via
			// Codex only, record that Claude has now seen it too (so
			// `discovered_from` promotes to "both") and persist.
			if existing.DiscoveredSource() != "claude" && existing.DiscoveredSource() != "both" {
				if !s.DryRun {
					existing.MergeDiscoveredFrom("claude")
					if err := manifest.save(); err != nil {
						logMsg("sync", "manifest save failed: %v", err)
					}
				}
			}
			continue
		}

		domain := manifest.resolveDomain(decoded)
		status := discoveryStatus(cfg)
		logMsg("sync", " DISCOVERED%s: %s -> %s (domain: %s)", pendingTag(status), pname, decoded, domain)
		discovered++

		if s.DryRun {
			continue
		}

		manifest.Projects[pname] = &ProjectEntry{
			Path:           decoded,
			Domain:         domain,
			DiscoveredFrom: "claude",
			Status:         status,
		}
		if err := manifest.save(); err != nil {
			logMsg("sync", "manifest save failed: %v", err)
		}

		// Create .repo.yaml in the project's wiki directory — approved
		// projects only; a pending project gets its KB dir when the user
		// approves it, not before.
		if status != statusPending {
			ensureRepoYAML(root, decoded, pname, domain)
		}
	}

	codexCount, err := s.discoverCodex(root, manifest, cfg)
	if err != nil {
		logMsg("sync", "codex discovery failed (continuing): %v", err)
	}
	discovered += codexCount

	return discovered, nil
}

// foldWorktree handles a discovered path that turned out to be a linked
// worktree of `main`: the worktree is recorded on the main project's
// entry (creating that entry first when the main repo was never itself
// discovered). `source` is the scanner that found it ("claude" or
// "codex"). Returns (newProjects, changed). The worktree inherits the
// main repo's filters — a worktree of an ignored/filtered repo is
// dropped entirely.
func (s *SyncCmd) foldWorktree(root string, manifest *Manifest, cfg *ScribeConfig, worktree, main, source string) (int, bool) {
	if manifest.isIgnored(main) || !sourceAllowed(cfg, main) {
		return 0, false
	}
	// The discovered path is a session cwd, which is often a
	// SUBDIRECTORY of the worktree. Drops and .claude/research live at
	// the worktree root — record that, or collection scans the wrong
	// dir forever.
	if top := runCmd(worktree, "git", "rev-parse", "--show-toplevel"); top != "" {
		worktree = top
	}
	// A pre-existing entry for the worktree itself (enrolled before
	// folding existed) keeps working until the user ignores it — doctor
	// flags those. Don't also record it on the main entry, or its drops
	// would be collected twice.
	if wEntry, ok := manifest.Projects[projectName(worktree)]; ok && wEntry != nil && samePath(wEntry.Path, worktree) {
		return 0, false
	}
	mname := projectName(main)
	if existing, ok := manifest.Projects[mname]; ok {
		// Basename collision: ~/src/api and ~/work/api both name "api".
		// Folding this repo's worktree onto the OTHER repo's entry would
		// route its drops and research under the wrong project — and
		// falling through to the create branch would overwrite that
		// entry. Leave it alone, loudly.
		if !samePath(existing.Path, main) {
			logMsg("sync", " worktree %s: main %s collides with existing project %q at %s — not folding", worktree, main, mname, existing.Path)
			return 0, false
		}
		if s.DryRun || !existing.recordWorktree(worktree) {
			return 0, false
		}
		logMsg("sync", " [%s] %s — recorded for drop/research collection", mname, describeWorktreeFold(worktree, main))
		if err := manifest.save(); err != nil {
			logMsg("sync", "manifest save failed: %v", err)
		}
		return 0, true
	}

	if !hasSignificantContent(main) {
		return 0, false
	}
	domain := manifest.resolveDomain(main)
	status := discoveryStatus(cfg)
	logMsg("sync", " DISCOVERED (via %s worktree)%s: %s -> %s (domain: %s)", source, pendingTag(status), mname, main, domain)
	if s.DryRun {
		return 1, true
	}
	manifest.Projects[mname] = &ProjectEntry{
		Path:           main,
		Domain:         domain,
		DiscoveredFrom: source,
		Status:         status,
		Worktrees:      []string{worktree},
	}
	if err := manifest.save(); err != nil {
		logMsg("sync", "manifest save failed: %v", err)
	}
	if status != statusPending {
		ensureRepoYAML(root, main, mname, domain)
	}
	return 1, true
}

// discoveryStatus returns the manifest status for a newly discovered
// project: pending by default, empty (= approved, byte-identical to
// pre-approval manifests) when sync.auto_approve is set.
func discoveryStatus(cfg *ScribeConfig) string {
	if cfg != nil && cfg.Sync.AutoApprove {
		return ""
	}
	return statusPending
}

// pendingTag renders the log suffix for a discovery line.
func pendingTag(status string) string {
	if status == statusPending {
		return " (pending approval)"
	}
	return ""
}

// ensureRepoYAML creates a .repo.yaml in the KB project directory if it doesn't exist.
func ensureRepoYAML(root, projectPath, pname, domain string) {
	// Determine the wiki directory for this project.
	wikiDir := filepath.Join(root, "projects", strings.ToLower(filepath.Base(projectPath)))
	if domain != "general" {
		candidate := filepath.Join(root, "projects", strings.ToLower(domain))
		if dirExists(candidate) {
			wikiDir = candidate
		}
	}

	if err := os.MkdirAll(wikiDir, 0o755); err != nil {
		logMsg("sync", "   mkdir %s: %v", wikiDir, err)
		return
	}

	repoYAML := filepath.Join(wikiDir, ".repo.yaml")
	if fileExists(repoYAML) {
		return
	}

	remote := gitRemoteURL(projectPath)
	branch := gitBranch(projectPath)
	if branch == "" {
		branch = "main"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "name: %s\n", pname)
	fmt.Fprintf(&sb, "path: %s\n", projectPath)
	fmt.Fprintf(&sb, "domain: %s\n", domain)
	if remote != "" {
		fmt.Fprintf(&sb, "remote: %s\n", remote)
	}
	fmt.Fprintf(&sb, "branch: %s\n", branch)

	if err := os.WriteFile(repoYAML, []byte(sb.String()), 0o644); err != nil {
		logMsg("sync", "   write %s: %v", repoYAML, err)
		return
	}
	logMsg("sync", "   created %s", repoYAML)
}

// collectDropFiles gathers unprocessed drop files from each project's
// .claude/<kb-name>/ dir (e.g. .claude/scribe/, or a renamed KB's own
// folder). Returns total count of collected drops.
func (s *SyncCmd) collectDropFiles(root string, manifest *Manifest) int {
	totalDrops := 0
	kb := kbName(root)

	for pname, entry := range manifest.Projects {
		if !entry.IsApproved() {
			continue
		}
		// Scan the main checkout plus recorded worktrees — drop files
		// written in a worktree can be branch-specific and never appear
		// in the main checkout.
		var unprocessed []string
		for _, base := range entry.collectionPaths() {
			dropDir := filepath.Join(base, ".claude", kb)
			if !dirExists(dropDir) {
				continue
			}
			drops, err := filepath.Glob(filepath.Join(dropDir, "*.md"))
			if err != nil || len(drops) == 0 {
				continue
			}
			// Filter to unprocessed drops (newer than last_drop_processed).
			unprocessed = append(unprocessed, filterNewerThan(drops, entry.LastDropProcessed)...)
		}

		if len(unprocessed) == 0 {
			continue
		}

		staging := filepath.Join(root, "output", "drops-"+pname)
		if err := os.MkdirAll(staging, 0o755); err != nil {
			logMsg("sync", " [%s] mkdir %s: %v", pname, staging, err)
			continue
		}

		for _, d := range unprocessed {
			data, err := os.ReadFile(d)
			if err != nil {
				continue
			}
			dest := filepath.Join(staging, filepath.Base(d))
			if err := os.WriteFile(dest, data, 0o644); err != nil {
				logMsg("sync", " [%s] write %s: %v", pname, dest, err)
			}
		}

		logMsg("sync", " [%s] %d drop file(s) collected", pname, len(unprocessed))
		totalDrops += len(unprocessed)
	}

	return totalDrops
}

// filterNewerThan returns the files modified after the RFC3339 cutoff.
// Empty cutoff means everything is new; an unparseable cutoff means
// nothing is (preserving the pre-worktree collector behavior).
func filterNewerThan(files []string, cutoff string) []string {
	if cutoff == "" {
		return files
	}
	t, err := time.Parse(time.RFC3339, cutoff)
	if err != nil {
		return nil
	}
	var out []string
	for _, f := range files {
		info, err := os.Stat(f)
		if err == nil && info.ModTime().After(t) {
			out = append(out, f)
		}
	}
	return out
}

// researchFile is one collected .claude/research/ markdown file: its
// absolute path plus its path relative to the research dir it came from
// (used for the flattened destination name).
type researchFile struct {
	path string
	rel  string
}

// collectResearchFiles gathers unprocessed .claude/research/**/*.md files from tracked projects
// and copies them into raw/articles/ with proper frontmatter for the absorb pipeline.
func (s *SyncCmd) collectResearchFiles(root string, manifest *Manifest) int {
	total := 0

	for pname, entry := range manifest.Projects {
		if !entry.IsApproved() {
			continue
		}
		// Scan the main checkout plus recorded worktrees — research
		// written in a worktree can be branch-specific and worth
		// keeping even though extraction only runs on the main path.
		var unscanned []researchFile
		for _, base := range entry.collectionPaths() {
			researchDir := filepath.Join(base, ".claude", "research")
			if !dirExists(researchDir) {
				continue
			}

			// Walk the entire research directory tree for .md files.
			var files []string
			_ = filepath.WalkDir(researchDir, func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil //nolint:nilerr // skip unreadable or directory, continue walk
				}
				if strings.HasSuffix(path, ".md") {
					files = append(files, path)
				}
				return nil
			})

			// Filter to files newer than last scan.
			for _, f := range filterNewerThan(files, entry.LastResearchScanned) {
				rel, _ := filepath.Rel(researchDir, f)
				unscanned = append(unscanned, researchFile{path: f, rel: rel})
			}
		}

		if len(unscanned) == 0 {
			continue
		}

		destDir := filepath.Join(root, "raw", "articles")
		domain := entry.Domain
		if domain == "" {
			domain = "general"
		}

		collected := 0
		for _, f := range unscanned {
			data, err := os.ReadFile(f.path)
			if err != nil {
				continue
			}

			// Build a flat dest filename from the relative path within research/.
			rel := f.rel
			flatName := strings.ReplaceAll(rel, string(filepath.Separator), "-")
			content := string(data)

			// Add frontmatter if missing.
			if !strings.HasPrefix(strings.TrimSpace(content), "---") {
				title := strings.TrimSuffix(flatName, ".md")
				// Strip date prefix if present (e.g. "2026-04-09-topic" → "topic").
				parts := strings.SplitN(title, "-", 4)
				if len(parts) >= 4 && len(parts[0]) == 4 && len(parts[1]) == 2 && len(parts[2]) == 2 {
					title = parts[3]
				}
				title = strings.ReplaceAll(title, "-", " ")
				words := strings.Fields(title)
				for i, w := range words {
					if len(w) > 0 {
						words[i] = strings.ToUpper(w[:1]) + w[1:]
					}
				}
				title = strings.Join(words, " ")

				fm := fmt.Sprintf("---\ntitle: \"%s\"\nsource_path: \"%s\"\ningested_at: \"%s\"\nformat: markdown\ndomain: %s\nproject: %s\n---\n\n",
					title, f.path, time.Now().UTC().Format(time.RFC3339), domain, pname)
				content = fm + content
			}

			destName := fmt.Sprintf("research-%s-%s", pname, flatName)
			dest := filepath.Join(destDir, destName)

			if s.DryRun {
				logMsg("sync", " [%s] would collect research: %s", pname, rel)
				collected++
				continue
			}

			if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
				logMsg("sync", " [%s] failed to write %s: %v", pname, destName, err)
				continue
			}

			logMsg("sync", " [%s] collected research: %s → %s", pname, rel, destName)
			collected++
		}

		total += collected

		// Update timestamp in manifest for this project.
		if !s.DryRun && collected > 0 {
			manifestMu.Lock()
			entry.LastResearchScanned = time.Now().UTC().Format(time.RFC3339)
			if err := manifest.save(); err != nil {
				logMsg("sync", "warn: manifest save failed: %v", err)
			}
			manifestMu.Unlock()
		}
	}

	return total
}
