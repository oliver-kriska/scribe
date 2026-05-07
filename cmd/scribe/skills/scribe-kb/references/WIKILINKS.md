# Wikilink syntax

A scribe-managed KB uses Obsidian-flavored wikilinks for every internal reference. External URLs use standard markdown links.

## Basic forms

```markdown
[[Article Title]]                    # Link by exact frontmatter title
[[Article Title|display text]]       # Custom display text (alias rendering)
[[Article Title#Section Heading]]    # Link to a heading inside the article
[[Article Title#^section-id]]        # Link to a section anchor (Phase 5A)
[[#Section in same article]]         # Same-article heading link
```

**Use `[[wikilinks]]` for everything inside the KB**, even if the target article doesn't exist yet — `scribe lint` will surface the missing page so a follow-up creates it. Use `[text](url)` only for external URLs.

## Section anchors (Phase 5A onward)

Every wiki article has a sidecar at `wiki/_sections/<dir>/<slug>.json` listing each H1/H2/H3 with an Obsidian-compatible anchor ID. Anchor format: lowercase, hyphenated, alphanumerics only.

```markdown
"## Methods and Results"   →  ^methods-and-results
"# Why It Matters"         →  ^why-it-matters
"### Setup"                →  ^setup
```

Reference a section like:

```markdown
See [[Postgres TS Vectors#^performance-tuning]] for the index strategy.
```

This works in Obsidian (which renders the anchor as a heading link) and Logseq (which uses the same `^` anchor convention as block IDs). qmd treats it the same as a regular wikilink at retrieval time but downstream tooling can use the anchor for section-level fetch.

## Aliases

`aliases:` in frontmatter lets one article respond to multiple wikilink names:

```yaml
title: "Andrej Karpathy"
aliases:
  - "@karpathy"
  - "karpathy"
```

A `[[@karpathy]]` link from another article resolves to the Karpathy page without triggering a missing-page warning.

## Backlinks

The KB rebuilds `wiki/_backlinks.json` on every `scribe sync`. To find what links to an article:

```bash
jq '.["Article Title"]' wiki/_backlinks.json
```

Or use the qmd-aware path: `qmd query "articles linking to <title>"`. Don't grep manually — backlinks resolve aliases too.

## Validation rules

`scribe lint` reports two link-related warnings:

- **Orphan articles** — articles with zero inbound `[[...]]` references. Either add cross-links from neighbors, or delete the article if it's truly disconnected.
- **Missing pages** — wikilinks pointing at a title that doesn't exist. Either create the page or fix the wikilink.

A wikilink in the body of a `_<meta>.md` file (like `_index.md`, `_hot.md`) counts as a real reference for missing-page detection but does NOT count as inbound for orphan detection — meta files don't rescue orphans, since they list everything.
