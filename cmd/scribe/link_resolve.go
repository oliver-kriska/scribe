package main

import (
	"path"
	"strings"
)

type linkStatus int

const (
	linkExact linkStatus = iota
	linkFuzzy
	linkAmbiguous
	linkMissing
)

// resolverEntry is one article as the resolver sees it.
type resolverEntry struct {
	Title      string
	Aliases    []string
	Path       string // KB-relative, forward slashes
	Superseded bool
}

const (
	tierTitle = iota
	tierAlias
	tierFold
	tierSlug
	tierPath
	tierCount
)

// tierIndex maps a tier's lookup key to the distinct titles that own it.
type tierIndex map[string][]string

// linkResolver holds one lookup map per tier, for live and superseded
// articles. It is never mutated after newLinkResolver.
type linkResolver struct {
	live       [tierCount]tierIndex
	superseded [tierCount]tierIndex
}

// normalizeLinkTarget cuts a #anchor off a raw wikilink target. It keeps a
// trailing .md, because some titles end in it; resolve handles the suffix.
func normalizeLinkTarget(raw string) string {
	target, _, _ := strings.Cut(strings.TrimSpace(raw), "#")
	return strings.TrimSpace(target)
}

func entryKeys(tier int, entry resolverEntry) []string {
	names := append([]string{entry.Title}, entry.Aliases...)
	switch tier {
	case tierTitle:
		return []string{entry.Title}
	case tierAlias:
		return entry.Aliases
	case tierFold:
		keys := make([]string, 0, len(names))
		for _, name := range names {
			keys = append(keys, strings.ToLower(name))
		}
		return keys
	case tierSlug:
		stem := strings.TrimSuffix(path.Base(entry.Path), ".md")
		keys := []string{stem}
		for _, name := range names {
			plainSlug := slugify(name)
			keys = append(keys, plainSlug)
			// extractWikilinks drops inline code spans from link text, so a
			// link to a title containing code arrives without them.
			if codeFreeSlug := slugify(stripCodeSpans(name)); codeFreeSlug != plainSlug {
				keys = append(keys, codeFreeSlug)
			}
		}
		return keys
	default:
		return []string{strings.ToLower(strings.TrimSuffix(entry.Path, ".md"))}
	}
}

func stripCodeSpans(text string) string {
	return codeSpanRE.ReplaceAllString(codeSpanDoubleRE.ReplaceAllString(text, ""), "")
}

func targetKey(tier int, target string) string {
	switch tier {
	case tierFold:
		return strings.ToLower(target)
	case tierSlug:
		return slugify(target)
	case tierPath:
		if strings.Contains(target, "/") {
			return strings.ToLower(target)
		}
		return ""
	default:
		return target
	}
}

func newLinkResolver(entries []resolverEntry) linkResolver {
	var resolver linkResolver
	for tier := range resolver.live {
		resolver.live[tier] = tierIndex{}
		resolver.superseded[tier] = tierIndex{}
	}
	for _, entry := range entries {
		indexes := &resolver.live
		if entry.Superseded {
			indexes = &resolver.superseded
		}
		for tier := range indexes {
			for _, key := range entryKeys(tier, entry) {
				if key == "" || containsString(indexes[tier][key], entry.Title) {
					continue
				}
				indexes[tier][key] = append(indexes[tier][key], entry.Title)
			}
		}
	}
	return resolver
}

// resolve returns the canonical title a wikilink target names. A missing
// target keeps its normalized text as the canonical value.
// A target ending in .md is tried as given first, then without the suffix.
func (r linkResolver) resolve(rawTarget string) (string, linkStatus) {
	target := normalizeLinkTarget(rawTarget)
	canonical, status := r.resolveTiers(target)
	if status == linkMissing && strings.HasSuffix(target, ".md") {
		if strippedCanonical, strippedStatus := r.resolveTiers(strings.TrimSuffix(target, ".md")); strippedStatus != linkMissing {
			return strippedCanonical, strippedStatus
		}
	}
	return canonical, status
}

// resolveTiers tries every tier against live articles first, and only when
// none matches tries every tier against superseded ones.
func (r linkResolver) resolveTiers(target string) (string, linkStatus) {
	if canonical, status := resolveInIndexes(r.live, target); status != linkMissing {
		return canonical, status
	}
	return resolveInIndexes(r.superseded, target)
}

func resolveInIndexes(indexes [tierCount]tierIndex, target string) (string, linkStatus) {
	for tier := range tierCount {
		key := targetKey(tier, target)
		if key == "" {
			continue
		}
		titles := indexes[tier][key]
		status := linkFuzzy
		if tier <= tierAlias {
			status = linkExact
		}
		switch len(titles) {
		case 0:
			continue
		case 1:
			return titles[0], status
		default:
			return target, linkAmbiguous
		}
	}
	return target, linkMissing
}
