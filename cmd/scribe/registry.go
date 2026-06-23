package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The KB registry (issue #26) is the `kbs:` list in the user config
// (~/.config/scribe/config.yaml). It is the single source of truth for
// "which KBs does this machine manage" — consumed by the KB-agnostic cron
// scheduler (`scribe each`) and, in future, by cwd routing and the agent
// handshake. There is no privileged "main" KB; kb_dir degrades to an
// optional default for bare commands run outside any project.

// registeredKBs returns the deduped, currently-valid KB roots from the
// registry. An empty `kbs:` falls back to [kb_dir] so single-KB installs
// migrate with zero changes. Non-existent / non-KB entries are skipped so
// one stale path can never break a cron tick.
func registeredKBs() []string {
	uc := loadUserConfig()
	cands := uc.KBs
	if len(cands) == 0 && uc.KBDir != "" {
		cands = []string{uc.KBDir}
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range cands {
		abs := p
		if a, err := filepath.Abs(p); err == nil {
			abs = a
		}
		if abs == "" || seen[abs] || !isKBRoot(abs) {
			continue
		}
		seen[abs] = true
		out = append(out, abs)
	}
	return out
}

// kbRegistered reports whether abs is already covered by the registry —
// either an explicit kbs: entry or the kb_dir default.
func kbRegistered(uc userConfig, abs string) bool {
	if uc.KBDir != "" && samePath(uc.KBDir, abs) {
		return true
	}
	for _, k := range uc.KBs {
		if samePath(k, abs) {
			return true
		}
	}
	return false
}

// registerKB idempotently adds root to the `kbs:` registry, preserving the
// rest of the user config (comments, kb_dir). Returns whether a new entry
// was written. A path that isn't a KB root is rejected so the registry
// never accumulates dead entries.
func registerKB(root string) (bool, error) {
	abs, err := filepath.Abs(expandHome(root))
	if err != nil {
		return false, err
	}
	if !isKBRoot(abs) {
		return false, fmt.Errorf("%s is not a scribe KB (no scribe.yaml or scripts/projects.json)", abs)
	}
	if kbRegistered(loadUserConfig(), abs) {
		return false, nil
	}
	path := userConfigPath()
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, []byte(appendKBEntry(string(raw), abs)), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// unregisterKB removes root from the `kbs:` registry. Returns whether an
// entry was removed. kb_dir is left untouched (it's the default, not a
// registry membership).
func unregisterKB(root string) (bool, error) {
	abs, err := filepath.Abs(expandHome(root))
	if err != nil {
		return false, err
	}
	path := userConfigPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	updated, removed := removeKBEntry(string(raw), abs)
	if !removed {
		return false, nil
	}
	return true, os.WriteFile(path, []byte(updated), 0o644)
}

// appendKBEntry returns user-config text with abs added under a `kbs:`
// block — inserted after an existing `kbs:` line, or as a new block
// appended to the file. Only block-list form is handled (the form scribe
// writes); inline `kbs: [...]` is not modified.
func appendKBEntry(raw, abs string) string {
	item := "  - " + abs
	lines := strings.Split(raw, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "kbs:" {
			out := append([]string{}, lines[:i+1]...)
			out = append(out, item)
			out = append(out, lines[i+1:]...)
			return strings.Join(out, "\n")
		}
	}
	block := "kbs:\n" + item + "\n"
	if raw == "" {
		return block
	}
	sep := "\n"
	if strings.HasSuffix(raw, "\n") {
		sep = ""
	}
	return raw + sep + block
}

// removeKBEntry drops the `- <abs>` list item from the user-config text,
// matching on resolved path. Returns the new text and whether anything was
// removed.
func removeKBEntry(raw, abs string) (string, bool) {
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	removed := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if rest, ok := strings.CutPrefix(t, "- "); ok {
			val := strings.Trim(strings.TrimSpace(rest), `"'`)
			if a, err := filepath.Abs(expandHome(val)); err == nil && samePath(a, abs) {
				removed = true
				continue
			}
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n"), removed
}
