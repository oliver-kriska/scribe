package main

import "testing"

func stubUserCrontab(t *testing.T, raw string, err error) {
	t.Helper()
	prev := readUserCrontab
	readUserCrontab = func() (string, error) { return raw, err }
	t.Cleanup(func() { readUserCrontab = prev })
}

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

func TestCheckLinuxCron(t *testing.T) {
	isolateUserConfig(t)
	root := makeKBRoot(t, "linux-kb")
	writeUserCfg(t, "kbs:\n  - "+root+"\n")
	stubUserCrontab(t, "# ---- scribe ----\n0 * * * * /usr/bin/scribe each -- commit\n# ---- end scribe ----\n", nil)

	checks := checkLinuxCron(root)
	if len(checks) != 2 {
		t.Fatalf("checks = %d, want 2: %+v", len(checks), checks)
	}
	for _, c := range checks {
		if c.Status != statusOK {
			t.Errorf("%s status = %q, want ok: %+v", c.Name, c.Status, c)
		}
	}
}

func TestCheckLinuxCronReportsMissingBlock(t *testing.T) {
	isolateUserConfig(t)
	root := makeKBRoot(t, "linux-kb")
	stubUserCrontab(t, "0 * * * * echo unrelated\n", nil)

	checks := checkLinuxCron(root)
	if len(checks) != 2 || checks[0].Status != statusFail || checks[1].Status != statusFail {
		t.Fatalf("unregistered KB + missing block should fail both checks: %+v", checks)
	}
}
