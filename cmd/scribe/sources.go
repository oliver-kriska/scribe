package main

import (
	"path/filepath"
	"strings"
)

// SourcesConfig scopes which project paths discovery may enroll. The
// shared-KB use case: a dev runs two KBs (private + team) and uses
// include/exclude to keep personal projects out of the team repo and
// vice versa. Filters apply at discovery time only — projects already
// in the manifest are managed via `scribe projects`.
//
// Pattern semantics (see sourcePatternMatches):
//   - plain path → matches itself and everything beneath it
//     ("~/work" covers ~/work/api and ~/work/api/sub)
//   - glob (*, ?, [) → filepath.Match against the path and each of its
//     ancestors ("~/work-*" covers ~/work-foo and ~/work-foo/inner)
//   - a trailing "/**" is accepted and means the same as the plain form
type SourcesConfig struct {
	// Include: when non-empty, a project path must match at least one
	// entry to be discovered. Empty = everything allowed.
	Include []string `yaml:"include"`
	// Exclude: a path matching any entry is never discovered, even if
	// it also matches an include. Exclude always wins.
	Exclude []string `yaml:"exclude"`
	// AllowedRemotes: when non-empty, a project must have a git origin
	// remote whose normalized URL (see normalizeRemoteURL) matches one
	// of these entries on a path-segment boundary — "github.com/myorg"
	// covers github.com/myorg/api but not github.com/myorg-fork/api.
	// Repos WITHOUT an origin remote are rejected outright: in a team
	// KB this is the guard that only org repos get discovered, no
	// matter where members keep their checkouts, so an unidentifiable
	// repo can't slip through. Entries accept any remote spelling
	// (https://, git@host:, bare host/org) — both sides normalize.
	AllowedRemotes []string `yaml:"allowed_remotes"`
}

// unionPaths returns base followed by the entries of extra that aren't
// already present, de-duplicated on exact string. Used to merge a local
// scribe.local.yaml sources.include/exclude into the committed list rather
// than replacing it (#41) — yaml.v3 unmarshal REPLACES slices, so without
// this a one-entry local include would silently narrow a team member's
// whole source scope to that single path. Order is stable (committed first)
// so the result reads predictably in `scribe config` output.
func unionPaths(base, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base)+len(extra))
	out := make([]string, 0, len(base)+len(extra))
	for _, p := range base {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range extra {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// sourceAllowed reports whether discovery may enroll the project at
// path under the configured source filters. Nil config = allow all.
func sourceAllowed(cfg *ScribeConfig, path string) bool {
	if cfg == nil {
		return true
	}
	for _, pattern := range cfg.Sources.Exclude {
		if sourcePatternMatches(pattern, path) {
			return false
		}
	}
	if !remoteAllowed(cfg.Sources.AllowedRemotes, path) {
		return false
	}
	if len(cfg.Sources.Include) == 0 {
		return true
	}
	for _, pattern := range cfg.Sources.Include {
		if sourcePatternMatches(pattern, path) {
			return true
		}
	}
	return false
}

// sourceExcluded reports whether path matches any sources.exclude pattern.
// Split out from sourceAllowed so `scribe projects add` can test the exclude
// gate independently of the include gate it is about to satisfy.
func sourceExcluded(cfg *ScribeConfig, path string) bool {
	if cfg == nil {
		return false
	}
	for _, pattern := range cfg.Sources.Exclude {
		if sourcePatternMatches(pattern, path) {
			return true
		}
	}
	return false
}

// includeCovers reports whether path is already matched by one of the
// include patterns — i.e. adding it again would be redundant.
func includeCovers(include []string, path string) bool {
	for _, pattern := range include {
		if sourcePatternMatches(pattern, path) {
			return true
		}
	}
	return false
}

// remoteAllowed implements sources.allowed_remotes: with a non-empty
// allowlist, the repo's normalized origin remote must prefix-match an
// entry on a segment boundary. No origin remote → rejected (the
// allowlist is exactly the "only repos we can identify" stance).
func remoteAllowed(allowed []string, path string) bool {
	if len(allowed) == 0 {
		return true
	}
	key := repoLedgerKey(path)
	if key == "" {
		return false
	}
	for _, a := range allowed {
		entry := normalizeRemoteURL(a)
		if entry == "" {
			continue
		}
		if key == entry || strings.HasPrefix(key, entry+"/") {
			return true
		}
	}
	return false
}

// sourcePatternMatches implements the pattern semantics documented on
// SourcesConfig. Both pattern and path get ~ expanded and cleaned, so
// scribe.yaml entries can be written either way.
func sourcePatternMatches(pattern, path string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	pattern = filepath.Clean(strings.TrimSuffix(expandHome(pattern), "/**"))
	path = filepath.Clean(expandHome(path))

	if !strings.ContainsAny(pattern, "*?[") {
		return path == pattern || strings.HasPrefix(path, pattern+string(filepath.Separator))
	}
	// Glob: match the path itself, then each ancestor, so a pattern on
	// a parent dir ("~/work-*") covers projects nested anywhere below
	// the matching directory. filepath.Match errors (malformed pattern)
	// read as no-match.
	for p := path; ; p = filepath.Dir(p) {
		if ok, err := filepath.Match(pattern, p); err != nil {
			return false
		} else if ok {
			return true
		}
		if p == filepath.Dir(p) { // reached root
			return false
		}
	}
}
