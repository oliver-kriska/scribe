package main

import (
	"fmt"
	"time"
)

// ConfigCmd reviews and approves the shared-KB config trust state. See
// config_trust.go for the model: in a team KB the repo's scribe.yaml is
// writable by every member, so its sensitive keys are locked to a
// per-machine snapshot; this command is how the user inspects drift and
// accepts a legitimate change.
type ConfigCmd struct {
	Diff   ConfigDiffCmd   `cmd:"" help:"Show sensitive scribe.yaml keys that changed since they were last trusted."`
	Trust  ConfigTrustCmd  `cmd:"" help:"Approve the current repo scribe.yaml sensitive keys as trusted."`
	Update ConfigUpdateCmd `cmd:"" help:"Append commented docs for options added since scribe.yaml was scaffolded."`
}

type ConfigDiffCmd struct{}

func (c *ConfigDiffCmd) ReadOnly() bool { return true }

func (c *ConfigDiffCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	current, ok := repoSensitiveView(root)
	if !ok {
		return fmt.Errorf("cannot read %s/scribe.yaml", root)
	}
	rec := loadTrustRecord(root)
	if rec == nil {
		if current.Team {
			fmt.Println("no trust record yet — run `scribe config trust` (or the next `scribe sync` records the current config)")
		} else {
			fmt.Println("not a team KB (team: false) — the config trust layer is inactive")
		}
		return nil
	}
	drift := sensitiveDiff(rec.Sensitive, current)
	if len(drift) == 0 {
		fmt.Printf("no drift — repo scribe.yaml matches the snapshot trusted at %s\n", rec.ApprovedAt)
		return nil
	}
	fmt.Printf("repo scribe.yaml drifted from the snapshot trusted at %s:\n\n", rec.ApprovedAt)
	for _, line := range drift {
		fmt.Println("  " + line)
	}
	fmt.Println("\nscribe is running on the TRUSTED values. Accept the change with `scribe config trust`,")
	fmt.Println("or revert the repo file (git log scribe.yaml shows who changed it).")
	return nil
}

type ConfigTrustCmd struct{}

func (c *ConfigTrustCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	current, ok := repoSensitiveView(root)
	if !ok {
		return fmt.Errorf("cannot read %s/scribe.yaml", root)
	}
	if rec := loadTrustRecord(root); rec != nil {
		drift := sensitiveDiff(rec.Sensitive, current)
		if len(drift) == 0 {
			fmt.Println("already trusted — no drift")
			return nil
		}
		fmt.Println("approving these changes:")
		for _, line := range drift {
			fmt.Println("  " + line)
		}
	} else if !current.Team {
		fmt.Println("note: team is false — recording a trust snapshot anyway; it activates if team: true is ever set")
	}
	if err := saveTrustRecord(root, trustRecord{
		Sensitive:  current,
		ApprovedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return err
	}
	fmt.Println("trusted current scribe.yaml sensitive keys")
	return nil
}
