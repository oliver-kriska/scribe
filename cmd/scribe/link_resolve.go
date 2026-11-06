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

// cleanLinkTarget trims a raw wikilink target and drops a trailing
// backslash: inside a table, [[A\|label]] is the escaped form of [[A|label]].
func cleanLinkTarget(raw string) string {
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), `\`))
}

// normalizeLinkTarget cuts a #anchor off a raw wikilink target. It keeps a
// trailing .md, because some titles end in it; resolve handles the suffix.
func normalizeLinkTarget(raw string) string {
	target, _, _ := strings.Cut(cleanLinkTarget(raw), "#")
	return strings.TrimSpace(target)
}

// slugSafeRunes are the non-alphanumeric ASCII characters slugify may drop
// without changing what a name means: separators, prose punctuation and
// code-span backticks.
const slugSafeRunes = " \t-_./:,;'\"()!?`"

// slugSafe reports whether slugify loses nothing but separators and prose
// punctuation from text. Symbols and non-ASCII letters carry meaning that
// slugify drops: "C++" and "C#" slug to "c", "λ calculus" to "calculus" and
// "Čaj" to "aj". A slug match on such a name would credit, and tell the
// dream to rewrite a link to, a different article.
func slugSafe(text string) bool {
	for _, r := range text {
		isAlnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !isAlnum && !strings.ContainsRune(slugSafeRunes, r) {
			return false
		}
	}
	return true
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
			if slugSafe(name) {
				keys = append(keys, slugify(name))
			}
		}
		return keys
	default:
		return []string{strings.ToLower(strings.TrimSuffix(entry.Path, ".md"))}
	}
}

func targetKey(tier int, target string) string {
	switch tier {
	case tierFold:
		return strings.ToLower(target)
	case tierSlug:
		if !slugSafe(target) {
			return ""
		}
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
func (r linkResolver) resolve(rawTarget string) (string, linkStatus) {
	_, canonical, status := r.resolveTarget(rawTarget)
	return canonical, status
}

// resolveTarget also returns the normalized target the match was made on.
// A title may contain '#' ("C#", "Issue #139"), so before cutting the
// #anchor it tries the whole target, then each shorter '#'-prefix, against
// the exact and case-fold tiers. Only then is the anchor cut at the first
// '#' and every tier tried. A target ending in .md is tried as given first,
// then without the suffix.
func (r linkResolver) resolveTarget(rawTarget string) (string, string, linkStatus) {
	if full := cleanLinkTarget(rawTarget); strings.Contains(full, "#") {
		for end := len(full); end > 0; end = strings.LastIndex(full[:end], "#") {
			candidate := strings.TrimSpace(full[:end])
			if candidate == "" {
				continue
			}
			if canonical, status := r.resolveTiers(candidate, tierSlug); status != linkMissing {
				return candidate, canonical, status
			}
		}
	}
	target := normalizeLinkTarget(rawTarget)
	canonical, status := r.resolveTiers(target, tierCount)
	if status == linkMissing && strings.HasSuffix(target, ".md") {
		if strippedCanonical, strippedStatus := r.resolveTiers(strings.TrimSuffix(target, ".md"), tierCount); strippedStatus != linkMissing {
			return target, strippedCanonical, strippedStatus
		}
	}
	return target, canonical, status
}

// resolveTiers tries the tiers below tierLimit against live articles first,
// and only when none matches tries them against superseded ones.
func (r linkResolver) resolveTiers(target string, tierLimit int) (string, linkStatus) {
	if canonical, status := resolveInIndexes(r.live, target, tierLimit); status != linkMissing {
		return canonical, status
	}
	return resolveInIndexes(r.superseded, target, tierLimit)
}

func resolveInIndexes(indexes [tierCount]tierIndex, target string, tierLimit int) (string, linkStatus) {
	for tier := range tierLimit {
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
