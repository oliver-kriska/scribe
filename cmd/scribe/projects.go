package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
	Add     ProjectsAddCmd     `cmd:"" help:"Enroll a project by path (widens sources.include, approves it)."`
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

// withSyncLock runs fn while holding the sync advisory lock. The
// projects commands do load → mutate → save on the manifest; without
// the lock a cron sync's own saves interleave and whichever writes
// last silently reverts the other (an approval flips back to pending,
// or a sync's collected research vanishes). Sync probe-acquires the
// same lock and skips its run when busy, so holding it here is safe.
func withSyncLock(root string, fn func() error) error {
	err := withLock(loadConfig(root).LockDir, "sync", root, fn)
	if errors.Is(err, errLockBusy) {
		return errors.New("a sync is running (lock busy) — retry in a moment")
	}
	return err
}

func (c *ProjectsApproveCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	return withSyncLock(root, func() error { return c.run(root) })
}

func (c *ProjectsApproveCmd) run(root string) error {
	manifest, err := loadManifest(root)
	if err != nil {
		return err
	}

	names := c.Names
	if c.All {
		names = manifest.pendingProjects()
	}
	if len(names) == 0 {
		return errors.New("nothing to approve — pass project name(s) or --all (see `scribe projects list --pending`)")
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
	return withSyncLock(root, func() error { return c.run(root) })
}

func (c *ProjectsIgnoreCmd) run(root string) error {
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
		printOrphanedArticlesHint(root, name)
	}
	return manifest.save()
}

// printOrphanedArticlesHint tells the user when an ignored project
// leaves extracted articles behind in the KB. Ignoring only stops
// future discovery/extraction — already-written pages stay searchable
// until someone deals with them, which is easy to forget.
func printOrphanedArticlesHint(root, name string) {
	if n := projectArticleCount(root, name); n > 0 {
		fmt.Printf("  note: %d article(s) under projects/%s/ remain in the KB — review and remove or merge them if no longer wanted\n", n, name)
	}
}

// projectArticleCount counts the .md articles under projects/<name>/.
func projectArticleCount(root, name string) int {
	count := 0
	_ = filepath.WalkDir(filepath.Join(root, "projects", name), func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // skip unreadable, continue walk
		}
		base := d.Name()
		if strings.HasSuffix(base, ".md") && !strings.HasPrefix(base, "_") && !strings.HasPrefix(base, ".") {
			count++
		}
		return nil
	})
	return count
}

type ProjectsReviewCmd struct{}

func (c *ProjectsReviewCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	// The lock is held for the whole interactive session: a cron sync
	// firing mid-review skips that run (it probes the same lock), which
	// beats it silently reverting decisions made on a stale snapshot.
	return withSyncLock(root, func() error { return c.run(root) })
}

func (c *ProjectsReviewCmd) run(root string) error {
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
				printOrphanedArticlesHint(root, name)
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
