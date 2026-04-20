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

// isIgnored checks if a path is in the ignored list or too shallow.
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
	return slices.Contains(m.IgnoredPaths, path)
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
