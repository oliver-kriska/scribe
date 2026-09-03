package main

import (
	"fmt"
	"path/filepath"
)

// The KB registry (issue #26) is the `kbs:` list in the user config
// (~/.config/scribe/config.yaml). It is the single source of truth for
// "which KBs does this machine manage" — consumed by the KB-agnostic cron
// scheduler (`scribe each`) and, in future, by cwd routing and the agent
// handshake. There is no privileged "main" KB; kb_dir degrades to an
// optional default for bare commands run outside any project.

// registeredKBs returns the deduped, currently-valid KB roots from the
// registry. kb_dir is ALWAYS a member (it leads the list), unioned with the
// explicit `kbs:` entries — this matches kbRegistered, which already counts
// kb_dir as registered. Crucially it means adding a second KB never silently
// drops the kb_dir default from the cron rotation (the failure mode where
// registering enaia would stop syncing scriptorium). Non-existent / non-KB
// entries are skipped so one stale path can never break a cron tick.
func registeredKBs() []string {
	kbs, _ := registeredKBsChecked()
	return kbs
}

// registeredKBsChecked is registeredKBs for callers that must not treat
// an unreadable user config as "no KBs" — `scribe each` refuses to run
// zero jobs silently when the file exists but does not parse.
func registeredKBsChecked() ([]string, error) {
	uc, err := loadUserConfigChecked()
	if err != nil {
		return nil, err
	}
	var cands []string
	if uc.KBDir != "" {
		cands = append(cands, uc.KBDir)
	}
	cands = append(cands, uc.KBs...)
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
	return out, nil
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
// rest of the user config (kb_dir, keys, stop words). Returns whether a
// new entry was written. A path that isn't a KB root is rejected so the
// registry never accumulates dead entries.
//
// The file is rewritten through marshalUserConfig — the same serialiser
// `scribe init` uses — instead of splicing a "  - <path>" line into the
// existing text. The splice wrote 2-space items; init's yaml.Marshal
// writes 4-space items; the mix parsed (without error) as one folded
// scalar and emptied the registry, which stopped every cron job.
func registerKB(root string) (bool, error) {
	abs, err := filepath.Abs(expandHome(root))
	if err != nil {
		return false, err
	}
	if !isKBRoot(abs) {
		return false, fmt.Errorf("%s is not a scribe KB (no scribe.yaml or scripts/projects.json)", abs)
	}
	uc, err := loadUserConfigChecked()
	if err != nil {
		return false, fmt.Errorf("user config unreadable — fix it before registering: %w", err)
	}
	if kbRegistered(uc, abs) {
		return false, nil
	}
	uc.KBs = append(uc.KBs, abs)
	if err := writeUserConfig(uc); err != nil {
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
	uc, err := loadUserConfigChecked()
	if err != nil {
		return false, fmt.Errorf("user config unreadable — fix it before unregistering: %w", err)
	}
	kept := uc.KBs[:0:0]
	removed := false
	for _, k := range uc.KBs {
		if samePath(k, abs) {
			removed = true
			continue
		}
		kept = append(kept, k)
	}
	if !removed {
		return false, nil
	}
	uc.KBs = kept
	return true, writeUserConfig(uc)
}
