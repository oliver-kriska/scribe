# Vault tool compatibility

A scribe-managed KB is markdown-first. The frontmatter, wikilinks, and section anchors all use shapes that read correctly in Obsidian and Logseq without scribe-specific dialect. This file documents the compatibility surface so an agent picking up the KB through one of those tools doesn't trip on dialect differences.

## Obsidian

The KB **is** an Obsidian vault. Open the KB root in Obsidian and:

- All `[[wikilinks]]` resolve.
- `[[Title#Heading]]` jumps to the heading.
- `[[Title#^section-id]]` jumps to the Phase 5A section anchor (Obsidian renders these as block-style anchors).
- Frontmatter shows up in the Properties pane.
- The graph view is populated from wikilinks.

Limitations:

- Obsidian doesn't run `scribe lint` — the user's pre-commit hook does that.
- `wiki/_index.md`, `wiki/_hot.md`, and other generated meta files live in the vault and will appear in Obsidian's file tree. Don't edit them — they're rebuilt by `scribe sync`.

## Logseq

A scribe KB renders in Logseq with caveats:

- Articles are paragraph-shaped, not block-outliner-shaped. Logseq displays paragraphs as top-level blocks; readable, but the block-graph features (block-refs, queries) won't add value because the content isn't bullets-first.
- `[[wikilinks]]` resolve.
- Section anchors `[[Article#^id]]` use the same `^` syntax Logseq uses for block IDs, so the form works — but a Logseq block ID points at a *block* (one bullet) while the scribe anchor points at a *section* (a heading and its body). Treat the form as cross-tool but the semantics as scribe-flavored.
- `tags::` and `id::` Logseq-style block properties are NOT used in scribe — properties live in YAML frontmatter at the article level, not inline per-block.

**Don't move the KB to Logseq's primary block-outliner format.** Article-shaped content is intentional; converting decisions, solutions, and patterns to nested bullets degrades them.

## Drift to avoid

When writing inside the KB, stick to the syntaxes documented in the other reference files. Avoid:

- Obsidian's `==highlight==` syntax (not standard; renders as literal in Logseq and many editors).
- Obsidian's `%%comment%%` syntax (same — kept literal in non-Obsidian renderers).
- Logseq's `tags::` / `id::` block properties (frontmatter is the property surface).
- Org-mode dialect (the KB is markdown-pure; `scribe init` doesn't scaffold for Org).

Standard markdown + frontmatter + wikilinks + section anchors is the durable subset. Anything else risks rendering inconsistently as the user moves between tools.

## Defuddle on the capture side

If the KB has a `raw/articles/` inbox, content there may have been fetched via `defuddle` (a CLI from kepano's tools). The body is clean markdown — same shape as trafilatura/jina output. No special handling required when reading or absorbing those files.
