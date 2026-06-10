package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// PromoteCmd copies one article from this KB into another scribe KB —
// the personal→team promotion flow. The working multi-user KB model is
// a shared body plus private layers, strictly separated; promotion is
// the one sanctioned crossing: an article you wrote for yourself turns
// out to be team-relevant, so you push a copy (with provenance) to the
// shared KB instead of re-authoring it there. The source article stays
// where it is.
type PromoteCmd struct {
	Article string `arg:"" help:"Article path relative to the current KB (e.g. patterns/retry-budget.md)."`
	To      string `help:"Target KB root (must be a scribe KB)." required:"" type:"path"`
	Domain  string `help:"Re-domain the copy (default: keep the source domain)."`
	Force   bool   `help:"Overwrite when the target article already exists."`
	NoGit   bool   `help:"Skip the auto-commit in the target KB." name:"no-git"`
}

func (c *PromoteCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	target, err := filepath.Abs(c.To)
	if err != nil {
		return fmt.Errorf("resolve --to: %w", err)
	}
	if !isScribeKB(target) {
		return fmt.Errorf("%s is not a scribe KB (no scribe.yaml) — promote needs an initialized target", target)
	}
	if filepath.Clean(target) == filepath.Clean(root) {
		return fmt.Errorf("target KB is the current KB — nothing to promote")
	}

	rel := filepath.Clean(c.Article)
	if filepath.IsAbs(rel) {
		if r, err := filepath.Rel(root, rel); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		} else {
			return fmt.Errorf("%s is outside the current KB", c.Article)
		}
	}
	topDir, _, _ := strings.Cut(rel, string(filepath.Separator))
	if !slices.Contains(wikiDirs, topDir) {
		return fmt.Errorf("%s is not under a wiki content dir (%s)", rel, strings.Join(wikiDirs, ", "))
	}

	srcAbs := filepath.Join(root, rel)
	raw, err := os.ReadFile(srcAbs)
	if err != nil {
		return fmt.Errorf("read %s: %w", rel, err)
	}
	fm, err := parseFrontmatter(raw)
	if err != nil || fm == nil {
		return fmt.Errorf("%s has no parseable frontmatter — fix it before promoting (scribe lint)", rel)
	}

	destAbs := filepath.Join(target, rel)
	if fileExists(destAbs) && !c.Force {
		return fmt.Errorf("%s already exists in the target KB — rerun with --force to overwrite", rel)
	}

	// Provenance + domain rewrite on the copy only.
	domain := fm.Domain
	if c.Domain != "" {
		domain = c.Domain
	}
	targetCfg := loadConfig(target)
	if domain != "" && !slices.Contains(targetCfg.AllDomains(), domain) {
		fmt.Printf("warning: domain %q is not in the target KB's domain list (%s) — lint there will flag it; consider --domain\n",
			domain, strings.Join(targetCfg.AllDomains(), ", "))
	}
	if domain == "personal" {
		fmt.Println("warning: promoting with domain `personal` into a shared KB — re-domain with --domain unless that's intended")
	}

	content := string(raw)
	content = setFrontmatterScalar(content, "promoted_from", yamlSingleQuote(kbName(root)))
	content = setFrontmatterScalar(content, "promoted_at", yamlSingleQuote(time.Now().UTC().Format(time.RFC3339)))
	if domain != "" && domain != fm.Domain {
		content = setFrontmatterScalar(content, "domain", domain)
	}
	if fm.Contributor == "" {
		if who := resolveContributor(root); who != "" {
			content = setFrontmatterScalar(content, "contributor", yamlSingleQuote(who))
		}
	}

	if err := os.MkdirAll(filepath.Dir(destAbs), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(destAbs, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", destAbs, err)
	}
	fmt.Printf("promoted %s → %s\n", rel, destAbs)

	// Wikilinks pointing at articles the target doesn't have become
	// missing-page warnings there — surface them now as hints, never
	// rewrite the prose.
	if missing := missingWikilinksIn(target, raw); len(missing) > 0 {
		fmt.Printf("note: %d wikilink(s) have no article in the target KB yet: %s\n",
			len(missing), strings.Join(missing, ", "))
		fmt.Println("      they render as missing pages until those articles are promoted or written there")
	}

	if !c.NoGit && hasGit(target) {
		if !gitAddWiki(target) {
			fmt.Println("warning: a detected secret in the target KB could not be held back — commit skipped; resolve there and commit manually")
			fmt.Printf("next: the target's own sync reindexes it (or run `SCRIBE_KB=%s scribe sync` now)\n", target)
			return nil
		}
		if gitHasStagedChanges(target) {
			msg := fmt.Sprintf("promote: %s from %s", fm.Title, kbName(root))
			if err := gitCommit(target, msg); err != nil {
				fmt.Printf("warning: target commit failed: %v — commit manually\n", err)
			} else {
				fmt.Printf("committed in target: %s\n", msg)
			}
		}
	}

	fmt.Printf("next: the target's own sync reindexes it (or run `SCRIBE_KB=%s scribe sync` now)\n", target)
	return nil
}

// missingWikilinksIn lists wikilink targets in content that no article
// (title or alias) in the target KB satisfies.
func missingWikilinksIn(targetRoot string, content []byte) []string {
	links := extractWikilinks(content)
	if len(links) == 0 {
		return nil
	}
	titles := map[string]bool{}
	_ = walkArticles(targetRoot, func(_ string, data []byte) error {
		if fm, err := parseFrontmatter(data); err == nil && fm != nil && fm.Title != "" {
			titles[fm.Title] = true
			for _, alias := range toStringSlice(fm.Aliases) {
				if alias != "" {
					titles[alias] = true
				}
			}
			return nil
		}
		if t := extractTitleFast(data); t != "" {
			titles[t] = true
		}
		return nil
	})

	var missing []string
	seen := map[string]bool{}
	for _, l := range links {
		if !titles[l] && !seen[l] {
			seen[l] = true
			missing = append(missing, "[["+l+"]]")
		}
	}
	return missing
}
