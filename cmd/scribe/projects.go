package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
)

// ProjectsCmd manages which discovered projects participate in the
// pipeline. Discovery (sync phase 1) lands new projects as
// status=pending; nothing is extracted, collected, or mined from them
// until the user approves here — the mise/direnv trust model for new
// sources. `scribe projects` with no subcommand lists everything.
type ProjectsCmd struct {
	List    ProjectsListCmd    `cmd:"" default:"1" help:"List projects with status (default)."`
	Approve ProjectsApproveCmd `cmd:"" help:"Approve pending project(s) for the pipeline."`
	Ignore  ProjectsIgnoreCmd  `cmd:"" help:"Remove project(s) from the manifest and block re-discovery."`
	Review  ProjectsReviewCmd  `cmd:"" help:"Interactively approve/ignore each pending project."`
}

type ProjectsListCmd struct {
	Pending bool `help:"Show only pending projects."`
}

func (c *ProjectsListCmd) ReadOnly() bool { return true }

func (c *ProjectsListCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	manifest, err := loadManifest(root)
	if err != nil {
		return err
	}

	names := make([]string, 0, len(manifest.Projects))
	for name := range manifest.Projects {
		names = append(names, name)
	}
	sort.Strings(names)

	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	shown := 0
	for _, name := range names {
		e := manifest.Projects[name]
		status := statusApproved
		if !e.IsApproved() {
			status = e.Status
		}
		if c.Pending && status != statusPending {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", status, name, e.Domain, e.Path)
		shown++
	}
	tw.Flush()
	if shown == 0 {
		if c.Pending {
			fmt.Println("no pending projects")
		} else {
			fmt.Println("no projects in manifest — run `scribe sync --discover`")
		}
	}
	return nil
}

type ProjectsApproveCmd struct {
	Names []string `arg:"" optional:"" help:"Project name(s) to approve."`
	All   bool     `help:"Approve every pending project."`
}

func (c *ProjectsApproveCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	manifest, err := loadManifest(root)
	if err != nil {
		return err
	}

	names := c.Names
	if c.All {
		names = manifest.pendingProjects()
	}
	if len(names) == 0 {
		return fmt.Errorf("nothing to approve — pass project name(s) or --all (see `scribe projects list --pending`)")
	}

	for _, name := range names {
		if err := approveProject(root, manifest, name); err != nil {
			return err
		}
		fmt.Printf("approved %s\n", name)
	}
	return manifest.save()
}

// approveProject flips one project to approved and performs the
// enrollment side effects discovery deferred (the KB .repo.yaml).
// The caller saves the manifest — batched so a multi-name approve
// doesn't rewrite the file N times.
func approveProject(root string, manifest *Manifest, name string) error {
	e, ok := manifest.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not in manifest (see `scribe projects list`)", name)
	}
	if e.IsApproved() {
		return nil
	}
	e.Status = statusApproved
	ensureRepoYAML(root, e.Path, name, e.Domain)
	return nil
}

type ProjectsIgnoreCmd struct {
	Names []string `arg:"" help:"Project name(s) to ignore."`
}

func (c *ProjectsIgnoreCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	manifest, err := loadManifest(root)
	if err != nil {
		return err
	}

	for _, name := range c.Names {
		if _, ok := manifest.Projects[name]; !ok {
			return fmt.Errorf("project %q not in manifest (see `scribe projects list`)", name)
		}
		manifest.ignoreProject(name)
		fmt.Printf("ignored %s (path blocked from re-discovery)\n", name)
	}
	return manifest.save()
}

type ProjectsReviewCmd struct{}

func (c *ProjectsReviewCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	manifest, err := loadManifest(root)
	if err != nil {
		return err
	}

	pending := manifest.pendingProjects()
	if len(pending) == 0 {
		fmt.Println("no pending projects")
		return nil
	}

	reader := bufio.NewReader(os.Stdin)
	approved, ignored := 0, 0
loop:
	for i, name := range pending {
		e := manifest.Projects[name]
		fmt.Printf("\n[%d/%d] %s\n  path:   %s\n  domain: %s\n  via:    %s\n",
			i+1, len(pending), name, e.Path, e.Domain, e.DiscoveredSource())
		for {
			fmt.Print("  [a]pprove / [i]gnore / [s]kip / [q]uit: ")
			line, err := reader.ReadString('\n')
			if err != nil {
				break loop // EOF — keep decisions made so far
			}
			switch strings.ToLower(strings.TrimSpace(line)) {
			case "a", "approve":
				if err := approveProject(root, manifest, name); err != nil {
					return err
				}
				approved++
			case "i", "ignore":
				manifest.ignoreProject(name)
				ignored++
			case "s", "skip", "":
				// leave pending
			case "q", "quit":
				break loop
			default:
				continue // re-prompt
			}
			break
		}
	}

	if err := manifest.save(); err != nil {
		return err
	}
	fmt.Printf("\n%d approved, %d ignored, %d still pending\n",
		approved, ignored, len(manifest.pendingProjects()))
	return nil
}
