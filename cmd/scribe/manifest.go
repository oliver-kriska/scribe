package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	return slices.Contains(m.IgnoredPaths, path)
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
