package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// extractionLedger is the COMMITTED record of which repo revisions have
// already been extracted, keyed by normalized git remote URL. Unlike
// scripts/projects.json (per-machine on team KBs — local paths differ),
// the remote URL is the identity every member shares: when a teammate
// extracts org/repo at SHA X and pushes, everyone else's next sync sees
// the ledger entry and skips re-extracting the same revision.
type extractionLedger struct {
	Repos map[string]ledgerEntry `json:"repos"`

	path string
}

// ledgerEntry records the most recent extraction of one repo.
type ledgerEntry struct {
	SHA         string `json:"sha"`
	ExtractedAt string `json:"extracted_at"`
	By          string `json:"by,omitempty"`
}

func ledgerPath(root string) string {
	return filepath.Join(root, "scripts", "extraction-ledger.json")
}

// loadLedger reads the ledger, returning an empty one when the file
// doesn't exist yet. Corrupt files also start fresh — the ledger is an
// optimization (skip duplicate work), never a source of truth, so
// losing it only costs one redundant extraction per repo.
func loadLedger(root string) *extractionLedger {
	l := &extractionLedger{Repos: map[string]ledgerEntry{}, path: ledgerPath(root)}
	data, err := os.ReadFile(l.path)
	if err != nil {
		return l
	}
	if err := json.Unmarshal(data, l); err != nil || l.Repos == nil {
		l.Repos = map[string]ledgerEntry{}
	}
	return l
}

// save writes the ledger back atomically.
func (l *extractionLedger) save() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// record stores the extraction of key at sha by contributor `by`.
func (l *extractionLedger) record(key, sha, by string) {
	if key == "" || sha == "" {
		return
	}
	l.Repos[key] = ledgerEntry{SHA: sha, ExtractedAt: time.Now().UTC().Format(time.RFC3339), By: by}
}

// lookup returns the ledger entry for a normalized remote key.
func (l *extractionLedger) lookup(key string) (ledgerEntry, bool) {
	if key == "" {
		return ledgerEntry{}, false
	}
	e, ok := l.Repos[key]
	return e, ok
}

// repoLedgerKey returns the normalized origin remote URL for a checkout,
// or "" when the repo has no origin remote (a remote-less repo has no
// shared identity, so it never participates in the ledger).
func repoLedgerKey(path string) string {
	return normalizeRemoteURL(runCmd(path, "git", "remote", "get-url", "origin"))
}

// normalizeRemoteURL canonicalizes the many spellings of the same git
// remote — git@github.com:org/repo.git, https://github.com/org/repo,
// ssh://git@github.com/org/repo.git — to "github.com/org/repo" so two
// members cloning over different protocols still share a ledger key.
// Local-path remotes pass through unchanged (machine-specific anyway).
func normalizeRemoteURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return trimGitSuffix(s)
		}
		return strings.ToLower(u.Host) + "/" + trimGitSuffix(strings.TrimPrefix(u.Path, "/"))
	}

	// scp-like syntax: [user@]host:path — the colon separates host from
	// path only when no slash precedes it (a slash first means it's a
	// plain filesystem path like /srv/git:archive). A single letter
	// before the colon is a Windows drive prefix, not a host.
	if i := strings.Index(s, ":"); i > 1 && !strings.ContainsAny(s[:i], "/\\") {
		host := s[:i]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		return strings.ToLower(host) + "/" + trimGitSuffix(strings.TrimPrefix(s[i+1:], "/"))
	}

	// Local filesystem remote.
	return trimGitSuffix(s)
}

func trimGitSuffix(p string) string {
	p = strings.TrimSuffix(strings.TrimSpace(p), "/")
	return strings.TrimSuffix(p, ".git")
}
