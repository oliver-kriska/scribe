package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// codexSessionMeta is the decoded payload of the first event of every
// Codex CLI rollout file. The full event is wrapped in
//
//	{"timestamp":"...","type":"session_meta","payload":{...}}
//
// — we keep only the fields discovery needs. Codex records `cwd` as a
// verbatim absolute path, which is the entire point of this format
// vs. Claude's lossy `decodeClaudePath` rebuild.
type codexSessionMeta struct {
	Cwd        string `json:"cwd"`
	ID         string `json:"id"`
	Originator string `json:"originator"`
	Source     string `json:"source"`
	Git        struct {
		RepositoryURL string `json:"repository_url"`
		Branch        string `json:"branch"`
		CommitHash    string `json:"commit_hash"`
	} `json:"git"`
}

// codexRolloutEnvelope is the first-line shape of every rollout.jsonl.
// We unmarshal once into this and pull out payload only when
// type == "session_meta".
type codexRolloutEnvelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// codexMaxFirstLineBytes caps bufio.Scanner's buffer for the first line
// of a rollout. `base_instructions.text` inlines Codex's full system
// prompt (~8 KB today), so the default 64 KB is plenty — but we lift
// the ceiling to 1 MB so future prompt growth doesn't silently break
// discovery.
const codexMaxFirstLineBytes = 1 << 20

// readCodexSessionMeta opens path, reads the first JSONL line, and
// returns the session_meta payload if that's what it is. Returns
// (nil, nil) when:
//   - the file is empty
//   - the first event is not "session_meta"
//
// Discovery treats both of those as "skip, not a Codex rollout we
// recognize" rather than erroring — a malformed rollout in the corner
// of the tree should not stop the scan.
func readCodexSessionMeta(path string) (*codexSessionMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), codexMaxFirstLineBytes)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		// Empty file.
		return nil, nil
	}

	var env codexRolloutEnvelope
	if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
		return nil, fmt.Errorf("parse session_meta: %w", err)
	}
	if env.Type != "session_meta" {
		return nil, nil
	}
	var meta codexSessionMeta
	if err := json.Unmarshal(env.Payload, &meta); err != nil {
		return nil, fmt.Errorf("parse session_meta payload: %w", err)
	}
	return &meta, nil
}

// walkCodexSessions walks root for rollout-*.jsonl files and invokes fn
// once per unique cwd (most-recent rollout wins on the tie). Order of
// traversal is descending by year/month/day partition so the
// most-recent rollout for any cwd is what fn sees.
//
// Skips:
//   - the sibling `archived_sessions/` tree at the root (`<root>/../archived_sessions`)
//   - files that are not `rollout-*.jsonl`
//   - any first event that is not `session_meta` (silent skip — see readCodexSessionMeta)
//
// Symlinked rollout dirs are followed once via the initial Stat; the
// manual os.ReadDir walk below treats a symlinked subdir as a non-dir
// entry and skips it, which is exactly what we want — Codex never
// writes symlinks here.
func walkCodexSessions(root string, fn func(meta *codexSessionMeta, sessionPath string)) error {
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("codex sessions root is not a directory: %s", root)
	}

	years, err := readSortedDirNamesDesc(root)
	if err != nil {
		return err
	}

	seen := make(map[string]struct{})

	for _, year := range years {
		yearDir := filepath.Join(root, year)
		months, err := readSortedDirNamesDesc(yearDir)
		if err != nil {
			continue
		}
		for _, month := range months {
			monthDir := filepath.Join(yearDir, month)
			days, err := readSortedDirNamesDesc(monthDir)
			if err != nil {
				continue
			}
			for _, day := range days {
				dayDir := filepath.Join(monthDir, day)
				files, err := os.ReadDir(dayDir)
				if err != nil {
					continue
				}
				// Walk newest rollout first within a day so the
				// most-recent metadata wins the cwd race.
				sort.Slice(files, func(i, j int) bool {
					return files[i].Name() > files[j].Name()
				})
				for _, fi := range files {
					if fi.IsDir() || !strings.HasPrefix(fi.Name(), "rollout-") || !strings.HasSuffix(fi.Name(), ".jsonl") {
						continue
					}
					p := filepath.Join(dayDir, fi.Name())
					meta, err := readCodexSessionMeta(p)
					if err != nil {
						logMsg("codex", "skip %s: %v", p, err)
						continue
					}
					if meta == nil || meta.Cwd == "" {
						continue
					}
					if _, dup := seen[meta.Cwd]; dup {
						continue
					}
					seen[meta.Cwd] = struct{}{}
					fn(meta, p)
				}
			}
		}
	}
	return nil
}

// walkCodexRollouts walks root for rollout-*.jsonl files modified
// within the lookback window and invokes fn once per rollout, newest
// first. Unlike walkCodexSessions (which dedupes by cwd for project
// discovery), session mining wants every distinct session, so there
// is no cwd dedup here — two sessions in the same project are two
// minable transcripts.
//
// sinceHours <= 0 disables the time filter (every rollout is yielded).
// Otherwise a rollout is skipped when its file mtime is older than
// now-sinceHours; this is the cheap pre-gate that keeps a cron mining
// pass from re-reading the entire ~/.codex/ history every run (the
// processed-set log in _codex_sessions_log.json is the durable dedup;
// the window just bounds the scan).
func walkCodexRollouts(root string, sinceHours int, fn func(rolloutPath string, meta *codexSessionMeta, mtime time.Time)) error {
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("codex sessions root is not a directory: %s", root)
	}

	var cutoff time.Time
	if sinceHours > 0 {
		cutoff = time.Now().Add(-time.Duration(sinceHours) * time.Hour)
	}

	years, err := readSortedDirNamesDesc(root)
	if err != nil {
		return err
	}
	for _, year := range years {
		yearDir := filepath.Join(root, year)
		months, err := readSortedDirNamesDesc(yearDir)
		if err != nil {
			continue
		}
		for _, month := range months {
			monthDir := filepath.Join(yearDir, month)
			days, err := readSortedDirNamesDesc(monthDir)
			if err != nil {
				continue
			}
			for _, day := range days {
				dayDir := filepath.Join(monthDir, day)
				files, err := os.ReadDir(dayDir)
				if err != nil {
					continue
				}
				sort.Slice(files, func(i, j int) bool {
					return files[i].Name() > files[j].Name()
				})
				for _, fi := range files {
					if fi.IsDir() || !strings.HasPrefix(fi.Name(), "rollout-") || !strings.HasSuffix(fi.Name(), ".jsonl") {
						continue
					}
					fInfo, ierr := fi.Info()
					if ierr != nil {
						continue
					}
					if !cutoff.IsZero() && fInfo.ModTime().Before(cutoff) {
						continue
					}
					p := filepath.Join(dayDir, fi.Name())
					meta, merr := readCodexSessionMeta(p)
					if merr != nil {
						logMsg("codex", "skip %s: %v", p, merr)
						continue
					}
					if meta == nil {
						continue
					}
					fn(p, meta, fInfo.ModTime())
				}
			}
		}
	}
	return nil
}

// readSortedDirNamesDesc returns the immediate child directory names of
// dir, sorted lexicographically in descending order. Date partitions
// (YYYY/MM/DD) sort correctly under this ordering. Non-directory
// children are filtered out so a stray file in the sessions root
// doesn't break the walk.
func readSortedDirNamesDesc(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}

// discoverCodex enumerates ~/.codex/sessions, finds projects whose cwd
// is not already in the manifest, and inserts them. Mirrors the Claude
// branch in `SyncCmd.discover` (manifest.isIgnored, hasSignificantContent,
// manifest.resolveDomain, ensureRepoYAML) so a project that has both
// Claude and Codex sessions stays one manifest entry — the second
// scanner just records "both" in DiscoveredFrom.
func (s *SyncCmd) discoverCodex(root string, manifest *Manifest, cfg *ScribeConfig) (int, error) {
	codexDir := cfg.CodexSessionsDir
	if codexDir == "" || !dirExists(codexDir) {
		return 0, nil
	}

	discovered := 0
	walkErr := walkCodexSessions(codexDir, func(meta *codexSessionMeta, _ string) {
		cwd := meta.Cwd
		if cwd == "" || !dirExists(cwd) {
			return
		}
		if manifest.isIgnored(cwd) {
			return
		}
		if !sourceAllowed(cfg, cwd) {
			return
		}
		// Linked worktrees fold into the main repo's project — see
		// worktreeMainRoot and SyncCmd.foldWorktree.
		if main := worktreeMainRoot(cwd); main != "" {
			if n, changed := s.foldWorktree(root, manifest, cfg, cwd, main, "codex"); changed {
				discovered += n
			}
			return
		}
		if !hasSignificantContent(cwd) {
			return
		}

		canon := canonicalizePath(cwd)
		if existing, exists := manifest.Projects[canon]; exists {
			// Already in manifest — promote DiscoveredFrom if Claude
			// surfaced it first, otherwise leave alone.
			if existing.DiscoveredSource() != "codex" && existing.DiscoveredSource() != "both" {
				if !s.DryRun {
					existing.MergeDiscoveredFrom("codex")
					if err := manifest.save(); err != nil {
						logMsg("sync", "manifest save failed: %v", err)
					}
				}
			}
			return
		}

		domain := manifest.resolveDomain(cwd)
		status := discoveryStatus(cfg)
		pname := manifest.uniqueName(projectName(cwd), cwd)
		logMsg("sync", " DISCOVERED (codex)%s: %s -> %s (domain: %s)", pendingTag(status), pname, cwd, domain)
		discovered++

		if s.DryRun {
			return
		}

		manifest.Projects[canon] = &ProjectEntry{
			Path:           canon,
			Name:           pname,
			Domain:         domain,
			DiscoveredFrom: "codex",
			Status:         status,
		}
		if err := manifest.save(); err != nil {
			logMsg("sync", "manifest save failed: %v", err)
		}

		if status != statusPending {
			ensureRepoYAML(root, cwd, pname, domain)
		}
	})

	return discovered, walkErr
}

// codexRolloutCount returns the number of rollout-*.jsonl files under
// root, capped at limit (use 0 for unlimited). Used by doctor to decide
// OK vs WARN without paying the cost of a full walk on huge histories.
func codexRolloutCount(root string, limit int) int {
	if root == "" || !dirExists(root) {
		return 0
	}
	count := 0
	// Two-level shallow walk: year/month/day/rollout-*.jsonl. We don't
	// use filepath.Walk because we want to short-circuit at `limit`.
	years, _ := os.ReadDir(root)
	for _, y := range years {
		if !y.IsDir() {
			continue
		}
		months, _ := os.ReadDir(filepath.Join(root, y.Name()))
		for _, m := range months {
			if !m.IsDir() {
				continue
			}
			days, _ := os.ReadDir(filepath.Join(root, y.Name(), m.Name()))
			for _, d := range days {
				if !d.IsDir() {
					continue
				}
				files, _ := os.ReadDir(filepath.Join(root, y.Name(), m.Name(), d.Name()))
				for _, f := range files {
					if f.IsDir() {
						continue
					}
					name := f.Name()
					if strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl") {
						count++
						if limit > 0 && count >= limit {
							return count
						}
					}
				}
			}
		}
	}
	return count
}

// codexProbeRollout returns the most recent rollout file path under
// root, or "" if none exists. Doctor uses it to schema-probe a single
// payload so a future Codex rename of `cwd` shows up as a WARN row
// instead of silently breaking discovery. Shallow-walks the YYYY/MM/DD
// partition tree in descending order and stops on the first hit.
func codexProbeRollout(root string) string {
	if root == "" || !dirExists(root) {
		return ""
	}
	years, _ := readSortedDirNamesDesc(root)
	for _, year := range years {
		yearDir := filepath.Join(root, year)
		months, _ := readSortedDirNamesDesc(yearDir)
		for _, month := range months {
			monthDir := filepath.Join(yearDir, month)
			days, _ := readSortedDirNamesDesc(monthDir)
			for _, day := range days {
				dayDir := filepath.Join(monthDir, day)
				files, _ := os.ReadDir(dayDir)
				sort.Slice(files, func(i, j int) bool {
					return files[i].Name() > files[j].Name()
				})
				for _, f := range files {
					if f.IsDir() {
						continue
					}
					name := f.Name()
					if strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl") {
						return filepath.Join(dayDir, name)
					}
				}
			}
		}
	}
	return ""
}
