package main

import "testing"

func testResolverEntries() []resolverEntry {
	return []resolverEntry{
		{Title: "Go Modules", Aliases: []string{"gomod"}, Path: "tools/go-modules.md"},
		{Title: "Self-Grant COMPANY_ADMIN Vulnerability", Path: "solutions/self-grant.md"},
		{Title: "Guest/OneTime JWT", Path: "solutions/guest-jwt.md"},
		{Title: "Old Thing", Path: "tools/old-thing.md", Superseded: true},
		{Title: "Kafka", Path: "tools/kafka.md"},
	}
}

func TestResolveLink(t *testing.T) {
	resolver := newLinkResolver(testResolverEntries())
	cases := []struct {
		input         string
		wantCanonical string
		wantStatus    linkStatus
	}{
		{"Go Modules", "Go Modules", linkExact},
		{"gomod", "Go Modules", linkExact},
		{"Go Modules#Usage", "Go Modules", linkExact},
		{"Kafka.md", "Kafka", linkExact},
		{"Old Thing", "Old Thing", linkExact},
		{"go modules", "Go Modules", linkFuzzy},
		{"go-modules", "Go Modules", linkFuzzy},
		{"Self-Grant Company Admin Vulnerability", "Self-Grant COMPANY_ADMIN Vulnerability", linkFuzzy},
		{"self-grant", "Self-Grant COMPANY_ADMIN Vulnerability", linkFuzzy},
		{"tools/kafka", "Kafka", linkFuzzy},
		{"old thing", "Old Thing", linkFuzzy},
		{"Nope", "Nope", linkMissing},
		{"---", "---", linkMissing},
	}
	for _, tc := range cases {
		gotCanonical, gotStatus := resolver.resolve(tc.input)
		if gotCanonical != tc.wantCanonical || gotStatus != tc.wantStatus {
			t.Errorf("resolve(%q) = (%q, %v), want (%q, %v)", tc.input, gotCanonical, gotStatus, tc.wantCanonical, tc.wantStatus)
		}
	}
}

func assertResolve(t *testing.T, resolver linkResolver, input, wantCanonical string, wantStatus linkStatus) {
	t.Helper()
	gotCanonical, gotStatus := resolver.resolve(input)
	if gotCanonical != wantCanonical || gotStatus != wantStatus {
		t.Errorf("resolve(%q) = (%q, %v), want (%q, %v)", input, gotCanonical, gotStatus, wantCanonical, wantStatus)
	}
}

func TestResolve_CaseCollisionIsAmbiguous(t *testing.T) {
	resolver := newLinkResolver([]resolverEntry{
		{Title: "Go Modules", Path: "a/go-modules.md"},
		{Title: "go modules", Path: "b/go-modules-2.md"},
	})
	_, gotStatus := resolver.resolve("GO MODULES")
	if gotStatus != linkAmbiguous {
		t.Errorf("status = %v, want linkAmbiguous", gotStatus)
	}
}

func TestResolve_TitleBeatsOtherAlias(t *testing.T) {
	resolver := newLinkResolver([]resolverEntry{
		{Title: "A", Aliases: []string{"B"}, Path: "a.md"},
		{Title: "B", Path: "b.md"},
	})
	assertResolve(t, resolver, "B", "B", linkExact)
}

func TestResolve_SharedAliasIsAmbiguous(t *testing.T) {
	resolver := newLinkResolver([]resolverEntry{
		{Title: "A", Aliases: []string{"shared"}, Path: "a.md"},
		{Title: "B", Aliases: []string{"shared"}, Path: "b.md"},
	})
	_, gotStatus := resolver.resolve("shared")
	if gotStatus != linkAmbiguous {
		t.Errorf("status = %v, want linkAmbiguous", gotStatus)
	}
}

func TestResolve_DuplicateTitleIsNotAmbiguous(t *testing.T) {
	resolver := newLinkResolver([]resolverEntry{
		{Title: "X", Path: "a/x.md"},
		{Title: "X", Path: "b/x.md"},
	})
	assertResolve(t, resolver, "x", "X", linkFuzzy)
}

func TestResolve_LiveBeatsSuperseded(t *testing.T) {
	resolver := newLinkResolver([]resolverEntry{
		{Title: "Kafka", Path: "tools/kafka.md"},
		{Title: "kafka", Path: "old/kafka.md", Superseded: true},
	})
	assertResolve(t, resolver, "KAFKA", "Kafka", linkFuzzy)
}

func TestResolve_LiveAliasBeatsSupersededTitle(t *testing.T) {
	resolver := newLinkResolver([]resolverEntry{
		{Title: "Old Name", Path: "old/old-name.md", Superseded: true},
		{Title: "New Name", Aliases: []string{"Old Name"}, Path: "tools/new-name.md"},
	})
	assertResolve(t, resolver, "Old Name", "New Name", linkExact)
}

func TestResolve_SlashTitle(t *testing.T) {
	resolver := newLinkResolver(testResolverEntries())
	assertResolve(t, resolver, "Guest/OneTime JWT", "Guest/OneTime JWT", linkExact)
	assertResolve(t, resolver, "guest/onetime jwt", "Guest/OneTime JWT", linkFuzzy)
	assertResolve(t, resolver, "guest/kafka", "guest/kafka", linkMissing)
}

func TestNormalizeLinkTarget(t *testing.T) {
	cases := []struct{ raw, want string }{{" Foo # x ", "Foo"}, {"#x", ""}, {"a.md", "a.md"}}
	for _, tc := range cases {
		raw, want := tc.raw, tc.want
		if got := normalizeLinkTarget(raw); got != want {
			t.Errorf("normalizeLinkTarget(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestResolve_TitleEndingInMD(t *testing.T) {
	resolver := newLinkResolver([]resolverEntry{
		{Title: "Notes in CLAUDE.md", Path: "notes/notes.md"},
		{Title: "Kafka", Path: "tools/kafka.md"},
	})
	assertResolve(t, resolver, "Notes in CLAUDE.md", "Notes in CLAUDE.md", linkExact)
	assertResolve(t, resolver, "kafka.md", "Kafka", linkFuzzy)
	assertResolve(t, resolver, "Nope.md", "Nope.md", linkMissing)
}

func TestResolve_TitleWithCodeSpans(t *testing.T) {
	resolver := newLinkResolver([]resolverEntry{
		{Title: "Alembic wraps `upgrade head` in ONE transaction", Path: "solutions/alembic.md"},
		{Title: "Title with `code` inside", Path: "solutions/code-title.md"},
	})
	assertResolve(t, resolver, "Alembic wraps  in ONE transaction",
		"Alembic wraps `upgrade head` in ONE transaction", linkFuzzy)

	links := extractWikilinks([]byte("See [[Title with `code` inside]] now."))
	if len(links) != 1 {
		t.Fatalf("extractWikilinks = %v, want one link", links)
	}
	assertResolve(t, resolver, links[0], "Title with `code` inside", linkFuzzy)
}
