package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Manifest represents scripts/projects.json.
type Manifest struct {
	Projects      map[string]*ProjectEntry `json:"projects"`
	DomainAliases map[string]string        `json:"domain_aliases"`
	IgnoredPaths  []string                 `json:"ignored_paths"`
	path          string
}

// ProjectEntry represents a project in the manifest.
type ProjectEntry struct {
	Path                string `json:"path"`
	Domain              string `json:"domain"`
	LastSHA             string `json:"last_sha"`
	LastExtracted       string `json:"last_extracted"`
	LastMDScan          string `json:"last_md_scan"`
	LastDropProcessed   string `json:"last_drop_processed,omitempty"`
	LastResearchScanned string `json:"last_research_scanned,omitempty"`
	ExtractedDirs       string `json:"extracted_dirs,omitempty"`
	// DiscoveredFrom records which agent surface first surfaced this
	// project to the manifest. "claude" | "codex" | "both". Empty
	// reads as "claude" for back-compat — every entry written before
	// this field existed came in via the Claude scanner.
	DiscoveredFrom string `json:"discovered_from,omitempty"`
	// Worktrees lists linked git-worktree checkouts of this project that
	// discovery folded into this entry instead of enrolling separately.
	// Extraction runs on Path only (worktrees share the repo's history),
	// but drop-file and research collection scan these too — a worktree
	// can carry branch-specific drops/.claude/research that the main
	// checkout doesn't have. Machine-local paths, like Path.
	Worktrees []string `json:"worktrees,omitempty"`
	// Status gates whether the project participates in the pipeline.
	// "pending" means discovered but not yet approved by the user —
	// extraction, drop/research collection, and session mining all skip
	// it until `scribe projects approve` (or review) flips it. Empty or
	// "approved" means active: every entry written before this field
	// existed was auto-enrolled, so empty MUST read as approved.
	Status string `json:"status,omitempty"`
}

// Project status values. statusApproved is written explicitly only by
// the approve command — auto-approved discoveries leave Status empty so
// pre-existing manifests round-trip byte-identical.
const (
	statusApproved = "approved"
	statusPending  = "pending"
)

// IsApproved reports whether the project participates in extraction and
// collection. Empty status is approved for back-compat (see Status doc).
func (e *ProjectEntry) IsApproved() bool {
	return e != nil && (e.Status == "" || e.Status == statusApproved)
}

// pendingProjects returns the sorted names of projects awaiting approval.
func (m *Manifest) pendingProjects() []string {
	var names []string
	for name, e := range m.Projects {
		if e != nil && e.Status == statusPending {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// ignoreProject removes a project from the manifest and blocks its path
// from re-discovery via IgnoredPaths. Idempotent.
func (m *Manifest) ignoreProject(name string) {
	e, ok := m.Projects[name]
	if !ok {
		return
	}
	if e != nil && e.Path != "" && !slices.Contains(m.IgnoredPaths, e.Path) {
		m.IgnoredPaths = append(m.IgnoredPaths, e.Path)
		sort.Strings(m.IgnoredPaths)
	}
	delete(m.Projects, name)
}

// DiscoveredSource normalises the back-compat default. Pre-existing
// manifests have ProjectEntry.DiscoveredFrom == "" because the field
// didn't exist when they were written; treat those as Claude entries.
func (e *ProjectEntry) DiscoveredSource() string {
	if e == nil || e.DiscoveredFrom == "" {
		return "claude"
	}
	return e.DiscoveredFrom
}

// MergeDiscoveredFrom records that this project was just seen from
// `source` ("claude" or "codex"). If the project was already attributed
// to the other agent, the field promotes to "both". Idempotent.
func (e *ProjectEntry) MergeDiscoveredFrom(source string) {
	if e == nil || source == "" {
		return
	}
	current := e.DiscoveredSource()
	if current == source || current == "both" {
		if e.DiscoveredFrom == "" {
			e.DiscoveredFrom = current
		}
		return
	}
	e.DiscoveredFrom = "both"
}

// loadManifest reads the projects.json manifest.
func loadManifest(root string) (*Manifest, error) {
	path := filepath.Join(root, "scripts", "projects.json")
	data, err := os.ReadFile(path)
	if err != nil {
		// Shared-KB clones gitignore scripts/projects.json so each
		// machine keeps its own manifest (paths and SHAs are machine-
		// local). On such a clone the file legitimately doesn't exist
		// yet — start empty and let the first save create it. Only when
		// the scribe.yaml marker proves this is a KB root; a bad -C /
		// SCRIBE_KB still fails loudly.
		if os.IsNotExist(err) && isScribeKB(root) {
			return &Manifest{
				Projects:      make(map[string]*ProjectEntry),
				DomainAliases: make(map[string]string),
				path:          path,
			}, nil
		}
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Projects == nil {
		m.Projects = make(map[string]*ProjectEntry)
	}
	if m.DomainAliases == nil {
		m.DomainAliases = make(map[string]string)
	}
	m.path = path
	return &m, nil
}

// save writes the manifest back to disk atomically.
func (m *Manifest) save() error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Fresh shared-KB clones may not have scripts/ yet (git doesn't
	// track empty dirs and projects.json is gitignored there).
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

// isIgnored checks if a path is in the ignored list, too shallow, or under a
// macOS TCC-protected location whose first access would prompt the user.
func (m *Manifest) isIgnored(path string) bool {
	parts := strings.Split(path, "/")
	nonEmpty := 0
	for _, p := range parts {
		if p != "" {
			nonEmpty++
		}
	}
	if nonEmpty < 4 {
		return true
	}
	if isTCCProtected(path) {
		return true
	}
	// A scribe KB must never be conscripted into a pipeline — its own or
	// another KB's. Extracting a KB feeds wiki articles back through the
	// extractor, which the LLM re-materializes as near-duplicate pages
	// (the reported readme.md → readme.md.md → readme_md.md fan-out).
	// Worse, every sync commits to the KB and bumps its git SHA, so the
	// self-extract retriggers on every run and the duplicates compound.
	// The walk-up check covers the active KB, any other KB on disk, and
	// project paths nested anywhere INSIDE a KB (a session run in
	// ~/team-kb/wiki/ must not enroll that subdir as a project here).
	if withinScribeKB(path) {
		return true
	}
	return slices.Contains(m.IgnoredPaths, path)
}

// isScribeKB reports whether path is the root of a scribe knowledge base,
// detected by the scribe.yaml marker that `scribe init` always writes.
// Discovery and extraction both consult this so a KB is never processed as
// one of its own source projects.
func isScribeKB(path string) bool {
	return fileExists(filepath.Join(path, "scribe.yaml"))
}

// withinScribeKB reports whether path is a KB root OR nested anywhere
// inside one, by walking up to / looking for the scribe.yaml marker.
// This is the check ingestion paths must use: with multiple KBs on one
// machine (personal + team), a Claude session run in a SUBDIRECTORY of
// KB B (~/team-kb/wiki/) must not be discovered or mined into KB A —
// the exact-root isScribeKB misses that case. KBs never harvest each
// other, at any depth.
func withinScribeKB(path string) bool {
	if path == "" {
		return false
	}
	for dir := filepath.Clean(path); ; dir = filepath.Dir(dir) {
		if isScribeKB(dir) {
			return true
		}
		if dir == filepath.Dir(dir) { // reached filesystem root
			return false
		}
	}
}

// isWithinKB reports whether path is the KB root at `root` or nested inside
// it. Path-only (no stat), so it works for session cwds that may no longer
// exist on disk. Used to keep work done *inside* the KB out of the mining
// pipeline.
func isWithinKB(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	r := filepath.Clean(root)
	p := filepath.Clean(path)
	return p == r || strings.HasPrefix(p, r+string(filepath.Separator))
}

// sessionInKB reports whether a session whose working directory was
// projectPath should be excluded from mining because it was spent inside a
// scribe KB — the active KB at `root` (or a subdir of it), or any other KB
// on disk (scribe.yaml marker). Mining such a session re-emits the wiki's
// own content as "new" articles, the same self-ingestion loop that produces
// duplicate pages on the extraction side.
func sessionInKB(root, projectPath string) bool {
	return isWithinKB(root, projectPath) || withinScribeKB(projectPath)
}

// tccProtectedSubdirs are top-level $HOME subdirectories gated by macOS TCC.
// Walking under any of these triggers a per-service consent prompt
// (Downloads, Desktop, Documents, Pictures = Photos, Library = AppData/iCloud,
// Music, Movies). Auto-discovered Claude Code projects rooted in any of these
// are almost always accidental — a one-off `claude` invocation in ~/Downloads
// shouldn't conscript the entire folder into the KB pipeline.
var tccProtectedSubdirs = []string{
	"Downloads",
	"Desktop",
	"Documents",
	"Pictures",
	"Movies",
	"Music",
	"Library",
}

// isTCCProtected reports whether path is at or under a TCC-protected
// $HOME subdirectory.
func isTCCProtected(path string) bool {
	home := os.Getenv("HOME")
	if home == "" {
		return false
	}
	clean := filepath.Clean(path)
	for _, sub := range tccProtectedSubdirs {
		root := filepath.Join(home, sub)
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// resolveDomain determines the domain for a project path.
func (m *Manifest) resolveDomain(path string) string {
	name := filepath.Base(path)
	if alias, ok := m.DomainAliases[name]; ok {
		return alias
	}
	parent := filepath.Base(filepath.Dir(path))
	if alias, ok := m.DomainAliases[parent+"/"+name]; ok {
		return alias
	}
	return "general"
}

// projectRoots are the parent directory basenames whose direct children are
// treated as top-level projects. If a path's parent matches one of these, the
// leaf is used as the project name; otherwise "parent-leaf" is used to keep
// nested checkouts (e.g. ~/code/org/repo) disambiguated.
//
// Default matches the common "~/Projects/<repo>" layout plus "src" and "code".
// Override via scribe.yaml → project_roots:[...], or via the
// SCRIBE_PROJECT_ROOTS env var (colon-separated). Absolute paths are also
// accepted (e.g. "/Users/foo") — any parent segment equal to a listed value
// triggers the leaf-only naming.
var projectRoots = defaultProjectRoots()

func defaultProjectRoots() map[string]bool {
	roots := map[string]bool{
		"Projects": true,
		"projects": true,
		"src":      true,
		"code":     true,
		"repos":    true,
		"work":     true,
	}
	if env := os.Getenv("SCRIBE_PROJECT_ROOTS"); env != "" {
		for r := range strings.SplitSeq(env, ":") {
			if r = strings.TrimSpace(r); r != "" {
				roots[r] = true
			}
		}
	}
	return roots
}

// entryForPath finds the project entry whose Path — or one of whose
// recorded worktrees — matches path (symlink-tolerant). The keyed
// projectName lookup alone is wrong in two ways: a worktree's basename
// differs from the project key (its sessions would bypass per-project
// gates), and two repos sharing a basename collide on the key (one
// project's gate would govern the other's sessions).
func (m *Manifest) entryForPath(path string) *ProjectEntry {
	if m == nil || path == "" {
		return nil
	}
	if e, ok := m.Projects[projectName(path)]; ok && e != nil && samePath(e.Path, path) {
		return e
	}
	for _, e := range m.Projects {
		if e == nil {
			continue
		}
		if samePath(e.Path, path) {
			return e
		}
		for _, w := range e.Worktrees {
			if samePath(w, path) {
				return e
			}
		}
	}
	return nil
}

// projectName derives a canonical project name from a path.
func projectName(path string) string {
	parent := filepath.Base(filepath.Dir(path))
	name := filepath.Base(path)
	if projectRoots[parent] {
		return name
	}
	return parent + "-" + name
}

// decodeClaudePath converts a Claude project dir name back to a real filesystem path.
// Claude encodes /Users/foo/my-project → -Users-foo-my-project.
// Dashes are ambiguous (separator OR literal hyphen). Strategy: greedy rebuild.
func decodeClaudePath(dirname string) string {
	parts := strings.Split(dirname, "-")
	path := ""
	for i := range parts {
		seg := parts[i]
		if seg == "" {
			continue
		}
		if path == "" {
			path = "/" + seg
		} else {
			withSlash := path + "/" + seg
			withDash := path + "-" + seg

			if dirExists(withSlash) {
				path = withSlash
			} else if dirExists(withDash) {
				path = withDash
			} else {
				// Lookahead: check if dash leads somewhere
				foundWithDash := false
				lookahead := withDash
				for j := i + 1; j < len(parts); j++ {
					la1 := lookahead + "/" + parts[j]
					if dirExists(la1) {
						foundWithDash = true
						break
					}
					la2 := lookahead + "-" + parts[j]
					if dirExists(la2) {
						foundWithDash = true
						break
					}
					lookahead = la2
				}
				if foundWithDash {
					path = withDash
				} else {
					path = withSlash
				}
			}
		}
	}
	if dirExists(path) {
		return path
	}
	return ""
}

const (
	// claudeMaxLineBytes caps the per-line scanner buffer when reading a
	// Claude session JSONL for its cwd. Large events (tool results,
	// pasted files) can exceed bufio.Scanner's 64 KB default, so we lift
	// the ceiling to 1 MB — matching codexMaxFirstLineBytes. A cwd that
	// only appears past a line larger than this is treated as absent, and
	// discovery falls back to decodeClaudePath.
	claudeMaxLineBytes = 1 << 20

	// claudeCwdScanLines bounds how many leading lines we scan for a cwd.
	// Resumed sessions can lead with a cwd-less summary event, so we look
	// past the first line; every normal event carries cwd, so a handful
	// of lines is plenty.
	claudeCwdScanLines = 50
)

// cwdKey is the JSON-key prefilter: skip lines that can't contain a cwd
// before paying for a full json.Unmarshal.
var cwdKey = []byte(`"cwd"`)

// resolveClaudeProjectPath returns the real filesystem path for a Claude
// Code project session directory. It prefers the verbatim `cwd` recorded
// inside the session JSONL — decode-free, the same ground truth Codex's
// session_meta provides — and falls back to the lossy directory-name
// decode (decodeClaudePath) only when no session file yields a cwd.
//
// This is what lets discovery enroll projects whose paths contain '_' or
// '.', which Claude's directory-name encoding collapses to '-'
// indistinguishably from a real hyphen, so decodeClaudePath can never
// rebuild them. Returns "" when neither route resolves to an existing dir.
//
// sessionDir is the full path to the encoded session directory;
// encodedName is its basename, used only for the decode fallback.
func resolveClaudeProjectPath(sessionDir, encodedName string) string {
	if cwd := claudeSessionCwd(sessionDir); cwd != "" && dirExists(cwd) {
		return cwd
	}
	return decodeClaudePath(encodedName)
}

// claudeSessionCwd returns the first verbatim `cwd` found in any session
// JSONL inside sessionDir, or "" if none is readable. Claude records the
// absolute cwd on nearly every event, so the first session file carrying
// a cwd is authoritative — all sessions in a dir share one cwd (the dir
// is keyed by it).
func claudeSessionCwd(sessionDir string) string {
	matches, err := filepath.Glob(filepath.Join(sessionDir, "*.jsonl"))
	if err != nil {
		return ""
	}
	sort.Strings(matches)
	for _, path := range matches {
		if cwd := readClaudeCwd(path); cwd != "" {
			return cwd
		}
	}
	return ""
}

// readClaudeCwd scans up to claudeCwdScanLines leading lines of a Claude
// session JSONL for a `cwd` field, returning the first non-empty value.
// Returns "" on any open/read/parse failure — discovery degrades to the
// name-decode rather than erroring on a malformed session file.
func readClaudeCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), claudeMaxLineBytes)

	for i := 0; i < claudeCwdScanLines && scanner.Scan(); i++ {
		line := scanner.Bytes()
		if !bytes.Contains(line, cwdKey) { // cheap prefilter before json.Unmarshal
			continue
		}
		var rec struct {
			Cwd string `json:"cwd"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Cwd != "" {
			return rec.Cwd
		}
	}
	return ""
}
