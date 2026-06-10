package main

import (
	"testing"
)

func TestSubscriptionMatches(t *testing.T) {
	subs := SubscriptionsConfig{Domains: []string{"backend"}, Tags: []string{"auth", "perf"}}

	tests := []struct {
		name string
		fm   Frontmatter
		want bool
	}{
		{"domain hit", Frontmatter{Domain: "backend"}, true},
		{"domain case-insensitive", Frontmatter{Domain: "Backend"}, true},
		{"domain miss", Frontmatter{Domain: "infra"}, false},
		{"tag hit", Frontmatter{Domain: "infra", Tags: []any{"auth"}}, true},
		{"tag miss", Frontmatter{Domain: "infra", Tags: []any{"caching"}}, false},
		{"no metadata", Frontmatter{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := subscriptionMatches(subs, &tt.fm); got != tt.want {
				t.Errorf("subscriptionMatches = %v, want %v", got, tt.want)
			}
		})
	}

	if (SubscriptionsConfig{}).empty() != true {
		t.Error("zero config should be empty")
	}
	if subs.empty() {
		t.Error("populated config should not be empty")
	}
}

func TestCollectSubscribedArrivals(t *testing.T) {
	root := initTestGitRepo(t, "Alice")
	writeKBFile(t, root, "wiki/seed.md", "---\ntitle: Seed\ndomain: general\n---\n\nbody\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-q", "-m", "seed")
	oldSHA := gitSHA(root)

	// "Pulled" commits: one matching domain, one matching tag, one
	// miss, one underscore file.
	writeKBFile(t, root, "wiki/backend-thing.md", "---\ntitle: Backend Thing\ndomain: backend\ncontributor: \"Bob\"\n---\n\nbody\n")
	writeKBFile(t, root, "patterns/tagged.md", "---\ntitle: Tagged\ndomain: infra\ntags: [perf]\n---\n\nbody\n")
	writeKBFile(t, root, "wiki/boring.md", "---\ntitle: Boring\ndomain: general\n---\n\nbody\n")
	writeKBFile(t, root, "wiki/_index.md", "- [[Backend Thing]]\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-q", "-m", "incoming")

	subs := SubscriptionsConfig{Domains: []string{"backend"}, Tags: []string{"perf"}}
	got := collectSubscribedArrivals(root, subs, oldSHA)

	if len(got) != 2 {
		t.Fatalf("got %d arrivals %+v, want 2", len(got), got)
	}
	byTitle := map[string]subscribedArrival{}
	for _, a := range got {
		byTitle[a.Title] = a
	}
	bt, ok := byTitle["Backend Thing"]
	if !ok {
		t.Fatalf("domain match missing: %+v", got)
	}
	if bt.Contributor != "Bob" || bt.Domain != "backend" || bt.Path != "wiki/backend-thing.md" {
		t.Errorf("arrival = %+v", bt)
	}
	if _, ok := byTitle["Tagged"]; !ok {
		t.Errorf("tag match missing: %+v", got)
	}

	// No subscriptions → nothing surfaces even with changes.
	if got := collectSubscribedArrivals(root, SubscriptionsConfig{}, oldSHA); len(got) != 0 {
		t.Errorf("empty subs matched %v", got)
	}
}
