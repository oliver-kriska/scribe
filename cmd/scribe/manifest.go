package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// manifestPathKeyedVersion marks the manifest schema where Manifest.Projects
// is keyed by canonicalizePath(entry.Path) instead of the lossy derived
// projectName(path). Manifests below this version are basename-keyed and get
// migrated in-memory on load (see migrateToPathKeys).
const manifestPathKeyedVersion = 2

// Manifest represents scripts/projects.json.
type Manifest struct {
	Projects        map[string]*ProjectEntry `json:"projects"` // key: canonicalizePath(entry.Path)
	DomainAliases   map[string]string        `json:"domain_aliases"`
	IgnoredPaths    []string                 `json:"ignored_paths"`
	ManifestVersion int                      `json:"manifest_version,omitempty"`
	path            string
	migratedCount   int // set by migrateToPathKeys; save() logs+resets once (not persisted)
}

// ProjectEntry represents a project in the manifest.
type ProjectEntry struct {
	Path string `json:"path"` // kept explicit and always == the map key (see canonicalizePath)
	// Name is the human display label (the old projectName()-derived
	// value). Used by CLI args, .repo.yaml, wiki dir naming, log lines.
	// No longer required globally unique for correctness (identity lives
	// in the map key), but uniqueName still keeps it unique for CLI
	// ergonomics — see (*Manifest).resolve / uniqueName.
	Name                string `json:"name"`
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

// pendingProjects returns the manifest keys (canonical paths) of projects
// awaiting approval, sorted by their human-friendly Name for CLI display.
func (m *Manifest) pendingProjects() []string {
	var keys []string
	for key, e := range m.Projects {
		if e != nil && e.Status == statusPending {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return m.Projects[keys[i]].Name < m.Projects[keys[j]].Name })
	return keys
}

// ignoreProject removes a project from the manifest and blocks its path
// from re-discovery via IgnoredPaths. key is a manifest map key (canonical
// path). Idempotent.
func (m *Manifest) ignoreProject(key string) {
	e, ok := m.Projects[key]
	if !ok {
		return
	}
	if e != nil && e.Path != "" && !slices.Contains(m.IgnoredPaths, e.Path) {
		m.IgnoredPaths = append(m.IgnoredPaths, e.Path)
		sort.Strings(m.IgnoredPaths)
	}
	delete(m.Projects, key)
}

// unignorePath removes a path from IgnoredPaths so it can be enrolled
// again. The inverse of the IgnoredPaths side of ignoreProject: an explicit
// `scribe projects add` overrides a prior ignore. Idempotent.
func (m *Manifest) unignorePath(path string) {
	for i, p := range m.IgnoredPaths {
		if p == path {
			m.IgnoredPaths = append(m.IgnoredPaths[:i], m.IgnoredPaths[i+1:]...)
			return
		}
	}
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
				Projects:        make(map[string]*ProjectEntry),
				DomainAliases:   make(map[string]string),
				ManifestVersion: manifestPathKeyedVersion, // nothing to migrate
				path:            path,
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
	m.migrateToPathKeys()
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
	if err := os.Rename(tmp, m.path); err != nil {
		return err
	}
	// migratedCount is set once, by migrateToPathKeys on load, when the
	// on-disk file was still basename-keyed. Log the one-line notice on
	// the first save that actually persists the migrated form, then
	// clear it so a second save() in the same process doesn't re-log.
	if m.migratedCount > 0 {
		logMsg("manifest", "migrated %d project(s) to path-keyed identity (scripts/projects.json)", m.migratedCount)
		m.migratedCount = 0
	}
	return nil
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
// recorded worktrees — matches path (symlink-tolerant). Manifest.Projects
// is keyed by canonicalizePath(entry.Path), so the common case is now an
// O(1) map hit; the worktree fallback stays a scan over each entry's
// (typically tiny) Worktrees list, since a worktree's own canonical path
// is never a Projects key.
func (m *Manifest) entryForPath(path string) *ProjectEntry {
	if m == nil || path == "" {
		return nil
	}
	canon := canonicalizePath(path)
	if e, ok := m.Projects[canon]; ok {
		return e
	}
	for _, e := range m.Projects {
		if e == nil {
			continue
		}
		for _, w := range e.Worktrees {
			if canonicalizePath(w) == canon {
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

// canonicalizePath is the sole identity normalization used for
// Manifest.Projects map keys: absolute, cleaned, and symlink-resolved when
// possible (macOS /var vs /private/var is the canonical case this exists
// for — see evalSymlinksCached's doc comment in worktree.go).
//
// When EvalSymlinks fails (the directory was since deleted or moved, or a
// component is a dangling symlink), this falls back to the cleaned
// absolute path rather than "" — deliberately, so a project whose
// directory no longer exists on disk stays addressable by its
// last-known key instead of silently becoming unfindable. scribe doctor's
// existing dirExists gates already flag missing project directories
// separately; identity resolution must not also break on them.
func canonicalizePath(path string) string {
	abs, err := filepath.Abs(expandHome(path))
	if err != nil {
		abs = path
	}
	abs = filepath.Clean(abs)
	if resolved := evalSymlinksCached(abs); resolved != "" {
		return resolved
	}
	return abs
}

// newerExtracted mirrors ledgerEntryNewer (gitmerge.go): parse RFC3339,
// falling back to a raw string compare if either side doesn't parse
// (covers "" for never-extracted entries).
func newerExtracted(a, b string) bool {
	ta, errA := time.Parse(time.RFC3339, a)
	tb, errB := time.Parse(time.RFC3339, b)
	if errA != nil || errB != nil {
		return a > b
	}
	return ta.After(tb)
}

// migrateToPathKeys re-keys m.Projects from the legacy basename-derived
// key (projectName(path)) to canonicalizePath(entry.Path), the fix for the
// basename-collision bug (two different repos landing on the same
// derived name — see manifest_test.go's migration tests). In-memory only;
// nothing is written to disk here (see save()'s migratedCount handling) so
// read-only commands (ProjectsListCmd, DoctorCmd, StatusCmd) never mutate
// KB state as a side effect of loading the manifest.
//
// Idempotent: a manifest already at manifestPathKeyedVersion (including a
// freshly created empty one — see loadManifest) is a no-op.
//
// Old map keys were themselves already globally unique (Go/JSON maps
// can't have duplicate keys), so every surviving entry inherits its old
// key as Name 1:1 — except when N legacy entries canonicalize to the SAME
// real directory (e.g. one spelled through a symlink), which collapses to
// one surviving entry per canonical path. Two DIFFERENT canonical paths
// can therefore never end up sharing an inherited Name; no uniqueName call
// is needed here — uniqueName exists only for genuinely new discoveries
// after migration.
func (m *Manifest) migrateToPathKeys() {
	if m == nil || m.ManifestVersion >= manifestPathKeyedVersion {
		return
	}
	byCanon := map[string][]*ProjectEntry{}
	for oldName, e := range m.Projects {
		if e == nil {
			continue
		}
		if e.Name == "" {
			e.Name = oldName
		}
		e.Path = canonicalizePath(e.Path)
		byCanon[e.Path] = append(byCanon[e.Path], e)
	}
	migrated := make(map[string]*ProjectEntry, len(byCanon))
	for canon, entries := range byCanon {
		winner := entries[0]
		if len(entries) > 1 {
			sort.Slice(entries, func(i, j int) bool {
				if entries[i].LastExtracted != entries[j].LastExtracted {
					return newerExtracted(entries[i].LastExtracted, entries[j].LastExtracted)
				}
				return entries[i].Name < entries[j].Name
			})
			winner = entries[0]
			for _, loser := range entries[1:] {
				for _, w := range loser.Worktrees {
					winner.recordWorktree(w)
				}
			}
			logMsg("manifest", "migration: %d entries pointed at %s — kept %q's history, merged worktrees",
				len(entries), canon, winner.Name)
		}
		migrated[canon] = winner
	}
	m.migratedCount = len(m.Projects)
	m.Projects = migrated
	m.ManifestVersion = manifestPathKeyedVersion
}

// looksLikePath reports whether a CLI-typed project reference looks like a
// filesystem path (vs. a short display Name) — resolve uses this to decide
// which lookup to try first.
func looksLikePath(arg string) bool {
	return strings.ContainsRune(arg, filepath.Separator) || strings.HasPrefix(arg, "~") || arg == "." || arg == ".."
}

// resolve looks up a project by a CLI-typed reference: an absolute or
// relative filesystem path (canonicalized and matched against the map key
// directly), or a short display Name. Every CLI call site that used to do
// a raw manifest.Projects[arg] index routes through this instead, so
// typing the project's Name keeps working exactly as before path-keying
// while a full path also resolves.
func (m *Manifest) resolve(arg string) (*ProjectEntry, error) {
	if m == nil || arg == "" {
		return nil, errors.New("empty project reference")
	}
	if looksLikePath(arg) {
		if e, ok := m.Projects[canonicalizePath(arg)]; ok {
			return e, nil
		}
	}
	var matches []*ProjectEntry
	for _, e := range m.Projects {
		if e != nil && e.Name == arg {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("project %q not in manifest (see `scribe projects list`)", arg)
	case 1:
		return matches[0], nil
	default:
		sort.Slice(matches, func(i, j int) bool { return matches[i].Path < matches[j].Path })
		paths := make([]string, len(matches))
		for i, e := range matches {
			paths[i] = e.Path
		}
		return nil, fmt.Errorf("project name %q is ambiguous — matches %d projects: %s (pass the full path instead)",
			arg, len(matches), strings.Join(paths, ", "))
	}
}

// uniqueName returns a display Name for path guaranteed not to collide
// with any OTHER project's Name already in the manifest (a different
// canonical path). Called at discovery/enroll time so a newly-found
// project that happens to share a basename with an existing one still
// gets a name a human can type, instead of being refused entirely (the
// basename-collision bug this plan fixes).
func (m *Manifest) uniqueName(base, path string) string {
	canon := canonicalizePath(path)
	if !m.nameCollides(base, canon) {
		return base
	}
	if qualified := filepath.Base(filepath.Dir(path)) + "-" + base; !m.nameCollides(qualified, canon) {
		return qualified
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !m.nameCollides(candidate, canon) {
			return candidate
		}
	}
}

// nameCollides reports whether name is already used by a DIFFERENT
// canonical path than canon.
func (m *Manifest) nameCollides(name, canon string) bool {
	for key, e := range m.Projects {
		if e != nil && e.Name == name && key != canon {
			return true
		}
	}
	return false
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

			switch {
			case dirExists(withSlash):
				path = withSlash
			case dirExists(withDash):
				path = withDash
			default:
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
