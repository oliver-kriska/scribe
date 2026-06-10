package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// dreamLease coordinates the weekly dream cycle across team members
// through the repo itself — a committed claim file instead of the old
// manual "run dream on one machine only" rule. The holder writes the
// lease, commits, and pushes; everyone else's dream run sees an active
// lease after their pull and skips. Expired leases are stealable, so a
// decommissioned laptop can't hold the lease forever.
type dreamLease struct {
	Host       string `json:"host"`
	By         string `json:"by,omitempty"`
	AcquiredAt string `json:"acquired_at"`
	ExpiresAt  string `json:"expires_at"`
}

// dreamLeaseHours bounds one dream cycle. Generous — cycles run 15-45
// minutes; the slack covers reruns and clock skew between machines.
const dreamLeaseHours = 6

func dreamLeasePath(root string) string {
	return filepath.Join(root, "scripts", "dream-lease.json")
}

// readDreamLease returns nil when the file is missing or corrupt — an
// unreadable lease is treated as no lease (it's a coordination hint,
// not a lock; the worst case is one duplicated dream cycle).
func readDreamLease(root string) *dreamLease {
	data, err := os.ReadFile(dreamLeasePath(root))
	if err != nil {
		return nil
	}
	var l dreamLease
	if err := json.Unmarshal(data, &l); err != nil || l.Host == "" {
		return nil
	}
	return &l
}

func writeDreamLease(root string, l *dreamLease) error {
	if err := os.MkdirAll(filepath.Dir(dreamLeasePath(root)), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := dreamLeasePath(root) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dreamLeasePath(root))
}

// activeAt reports whether the lease is still claimed at `now`.
func (l *dreamLease) activeAt(now time.Time) bool {
	if l == nil {
		return false
	}
	exp, err := time.Parse(time.RFC3339, l.ExpiresAt)
	return err == nil && now.Before(exp)
}

func hostnameShort() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown-host"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}

// acquireDreamLease claims the dream cycle for this machine. Flow:
// pull (freshest lease state), check, claim, commit+push the claim.
// A failed push means someone raced us — pull again and re-check; the
// loser backs off. Returns (acquired, holder) where holder names the
// machine that beat us. Best-effort by design: with the remote
// unreachable both machines may dream once, which only costs a
// duplicate consolidation pass.
func acquireDreamLease(root string, now time.Time) (bool, string) {
	if _, _, err := pullRebase(root); err != nil {
		logMsg("dream", "lease pull skipped: %s (continuing)", err)
	}

	host := hostnameShort()
	if lease := readDreamLease(root); lease.activeAt(now) && lease.Host != host {
		return false, lease.Host
	}

	claim := &dreamLease{
		Host:       host,
		By:         resolveContributor(root),
		AcquiredAt: now.UTC().Format(time.RFC3339),
		ExpiresAt:  now.Add(dreamLeaseHours * time.Hour).UTC().Format(time.RFC3339),
	}
	if err := writeDreamLease(root, claim); err != nil {
		logMsg("dream", "lease write failed: %v — proceeding without coordination", err)
		return true, ""
	}
	commitDreamLease(root, "dream: lease claim by "+host)

	if gitRemoteURL(root) != "" {
		if err := gitPush(root); err != nil {
			// Push race: a teammate claimed first. Their claim arrives in
			// the pull; if it's active and not ours, back off.
			if _, _, pErr := pullRebase(root); pErr != nil {
				logMsg("dream", "lease re-check pull failed: %s — proceeding", pErr)
				return true, ""
			}
			if cur := readDreamLease(root); cur != nil && cur.Host != host && cur.activeAt(now) {
				return false, cur.Host
			}
		}
	}
	return true, ""
}

// releaseDreamLease expires this machine's lease once the cycle ends,
// so the slot frees immediately instead of after the full window. The
// commit rides out with dream's own commit/push flow.
func releaseDreamLease(root string) {
	lease := readDreamLease(root)
	if lease == nil || lease.Host != hostnameShort() {
		return
	}
	lease.ExpiresAt = time.Now().UTC().Format(time.RFC3339)
	if err := writeDreamLease(root, lease); err != nil {
		logMsg("dream", "lease release failed: %v", err)
		return
	}
	commitDreamLease(root, "dream: lease release by "+lease.Host)
}

// commitDreamLease commits ONLY the lease file (pathspec commit), so a
// claim never sweeps up unrelated staged work. Failures log and move
// on — the lease is coordination, not correctness.
func commitDreamLease(root string, msg string) {
	if !hasGit(root) {
		return
	}
	if _, err := runCmdErr(root, "git", "add", "--", "scripts/dream-lease.json"); err != nil {
		logMsg("dream", "lease stage failed: %v", err)
		return
	}
	if _, err := runCmdErr(root, "git", "commit", "--no-gpg-sign", "-m", msg, "--", "scripts/dream-lease.json"); err != nil {
		logMsg("dream", "lease commit failed: %v", err)
	}
}
