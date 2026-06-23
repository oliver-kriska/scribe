package main

import (
	"fmt"
	"path/filepath"
)

// KbCmd manages the machine's KB registry (issue #26) — the `kbs:` list in
// ~/.config/scribe/config.yaml that the KB-agnostic scheduler iterates.
type KbCmd struct {
	Add    KbAddCmd    `cmd:"" help:"Register a KB so the scheduler (scribe each) iterates it."`
	List   KbListCmd   `cmd:"" help:"List registered KBs (the registry + kb_dir default)."`
	Remove KbRemoveCmd `cmd:"" help:"Unregister a KB from the scheduler."`
}

type KbAddCmd struct {
	Path string `arg:"" optional:"" help:"KB root to register (default: current KB)."`
}

func (c *KbAddCmd) Run() error {
	root := c.Path
	if root == "" {
		r, err := kbDir()
		if err != nil {
			return err
		}
		root = r
	}
	added, err := registerKB(root)
	if err != nil {
		return err
	}
	abs, _ := filepath.Abs(expandHome(root))
	if added {
		fmt.Printf("registered %s\n", abs)
	} else {
		fmt.Printf("already registered: %s\n", abs)
	}
	fmt.Println("\nregistry now:")
	return (&KbListCmd{}).Run()
}

type KbListCmd struct{}

func (c *KbListCmd) Run() error {
	kbs := registeredKBs()
	if len(kbs) == 0 {
		fmt.Printf("no registered KBs (set kbs: or kb_dir in %s)\n", userConfigPath())
		return nil
	}
	uc := loadUserConfig()
	for _, kb := range kbs {
		marker := ""
		if uc.KBDir != "" && samePath(uc.KBDir, kb) {
			marker = "  (default)"
		}
		fmt.Printf("  %s%s\n", kb, marker)
	}
	return nil
}

type KbRemoveCmd struct {
	Path string `arg:"" help:"KB root to unregister."`
}

func (c *KbRemoveCmd) Run() error {
	removed, err := unregisterKB(c.Path)
	if err != nil {
		return err
	}
	abs, _ := filepath.Abs(expandHome(c.Path))
	if removed {
		fmt.Printf("unregistered %s\n", abs)
	} else {
		fmt.Printf("not in registry: %s\n", abs)
	}
	return nil
}
