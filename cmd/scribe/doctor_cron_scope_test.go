package main

import "testing"

// TestCronScopeCheck covers the KB-scope cron headline (issue #27 item 1):
// "loaded" agents are a single KB-agnostic set, so doctor must say whether
// they actually serve THIS KB — keyed on registry membership, not the
// label prefix. HOME is an empty temp dir so no legacy other-KB plist is
// found, isolating the registry-membership branch.
func TestCronScopeCheck(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // empty ~/Library/LaunchAgents → no legacy plist
	isolateUserConfig(t)
	root := makeKBRoot(t, "mykb")

	// Not in the registry → cron does NOT serve this KB → warn.
	if c := cronScopeCheck(root); c.Status != statusWarn {
		t.Errorf("unregistered: status = %q, want %q\n  detail: %s", c.Status, statusWarn, c.Detail)
	}

	// Registered → cron serves it → ok.
	writeUserCfg(t, "kbs:\n  - "+root+"\n")
	c := cronScopeCheck(root)
	if c.Status != statusOK {
		t.Errorf("registered: status = %q, want %q\n  detail: %s", c.Status, statusOK, c.Detail)
	}
	if c.Name != "kb-scope" {
		t.Errorf("name = %q, want kb-scope", c.Name)
	}
}
