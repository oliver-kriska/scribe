package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// SubscriptionsConfig is the per-user "what do I want to hear about"
// filter for articles arriving via pull-before-sync. The serverless
// version of wiki watch-notifications: pull already knows exactly what
// arrived, so matching it against a subscription list costs nothing.
// Belongs in scribe.local.yaml (gitignored) — subscriptions are
// personal, not team config.
type SubscriptionsConfig struct {
	// Domains to match against the article's `domain:` frontmatter.
	Domains []string `yaml:"domains"`
	// Tags to match against the article's `tags:` list (any overlap).
	Tags []string `yaml:"tags"`
	// Notify fires a macOS notification per sync run with matches
	// (best effort, darwin only). The sync log always gets the lines.
	Notify bool `yaml:"notify"`
}

func (s SubscriptionsConfig) empty() bool {
	return len(s.Domains) == 0 && len(s.Tags) == 0
}

// subscribedArrival is one pulled article that matched a subscription.
type subscribedArrival struct {
	Path        string
	Title       string
	Domain      string
	Contributor string
}

// surfaceSubscribedArrivals logs (and optionally notifies about) newly
// pulled articles matching the user's subscriptions. oldSHA is HEAD
// before the pull. Quiet no-op without subscriptions or matches.
func surfaceSubscribedArrivals(root string, cfg *ScribeConfig, oldSHA string) {
	if cfg == nil || cfg.Subscriptions.empty() || oldSHA == "" {
		return
	}
	matches := collectSubscribedArrivals(root, cfg.Subscriptions, oldSHA)
	if len(matches) == 0 {
		return
	}
	for _, m := range matches {
		who := m.Contributor
		if who == "" {
			who = "unknown"
		}
		logMsg("sync", "subscribed: %s (%s) by %s — %s", m.Title, m.Domain, who, m.Path)
	}
	if cfg.Subscriptions.Notify && runtime.GOOS == "darwin" {
		notifySubscriptions(matches)
	}
}

// collectSubscribedArrivals diffs oldSHA..HEAD over the wiki dirs and
// returns the changed articles whose frontmatter matches the
// subscription filter.
func collectSubscribedArrivals(root string, subs SubscriptionsConfig, oldSHA string) []subscribedArrival {
	args := make([]string, 0, 4+len(wikiDirs))
	args = append(args, "diff", "--name-only", oldSHA+"..HEAD", "--")
	args = append(args, wikiDirs...)
	out := runCmd(root, "git", args...)
	if out == "" {
		return nil
	}

	var matches []subscribedArrival
	for line := range strings.SplitSeq(out, "\n") {
		rel := strings.TrimSpace(line)
		if rel == "" || !strings.HasSuffix(rel, ".md") || strings.HasPrefix(filepath.Base(rel), "_") {
			continue
		}
		fm := readArticleFrontmatter(root, rel)
		if fm == nil || !subscriptionMatches(subs, fm) {
			continue
		}
		title := fm.Title
		if title == "" {
			title = filepath.Base(rel)
		}
		matches = append(matches, subscribedArrival{
			Path:        rel,
			Title:       title,
			Domain:      fm.Domain,
			Contributor: fm.Contributor,
		})
	}
	return matches
}

// subscriptionMatches reports whether an article's frontmatter hits the
// user's domain or tag subscriptions.
func subscriptionMatches(subs SubscriptionsConfig, fm *Frontmatter) bool {
	for _, d := range subs.Domains {
		if d != "" && strings.EqualFold(d, fm.Domain) {
			return true
		}
	}
	if len(subs.Tags) == 0 {
		return false
	}
	for _, tag := range toStringSlice(fm.Tags) {
		for _, want := range subs.Tags {
			if want != "" && strings.EqualFold(want, tag) {
				return true
			}
		}
	}
	return false
}

// readArticleFrontmatter parses the frontmatter of a KB-relative file,
// nil when unreadable.
func readArticleFrontmatter(root, rel string) *Frontmatter {
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return nil
	}
	fm, err := parseFrontmatter(data)
	if err != nil {
		return nil
	}
	return fm
}

// notifySubscriptions fires one aggregate macOS notification. Best
// effort — a missing osascript or sandboxed environment just no-ops.
func notifySubscriptions(matches []subscribedArrival) {
	if len(matches) == 0 {
		return
	}
	body := matches[0].Title
	if len(matches) > 1 {
		body = fmt.Sprintf("%s + %d more", matches[0].Title, len(matches)-1)
	}
	script := fmt.Sprintf("display notification %q with title %q", body, "scribe — subscribed articles arrived")
	_ = exec.Command("osascript", "-e", script).Run() //nolint:noctx // fire-and-forget local notification
}
