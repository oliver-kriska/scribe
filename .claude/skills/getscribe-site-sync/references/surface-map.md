# Surface map — what to edit and in what order

All paths under `site/public/`. `index.html` is canonical; everything else
mirrors it. Keep them in lockstep — a search engine or an LLM may read any one.

| Surface | What it is | How it relates |
|---|---|---|
| `index.html` | The rendered page. Single self-contained HTML/CSS + two inline `<script>` (Plausible tag, hero canvas animation). Holds the JSON-LD blocks. | **Canonical.** Edit here first. |
| `index.md` | Prose mirror of the page for the `/index.md` route. | Mirror every prose change from the HTML. |
| `llms.txt` | Short llms.txt-spec index (intro blockquote + resource links). | Mirror only the high-level claims (intro line, the one-paragraph summary). Not every sentence. |
| `llms-full.txt` | Full content for large-context LLMs. | **Exact copy of `index.md`.** Regenerate with `cp index.md llms-full.txt` — never hand-edit. |
| `og.png` | 1200×630 social card, text baked into pixels. | Regenerate from `assets/og.svg.tmpl` via `scripts/regen_og.sh` only if the eyebrow tagline or the one-line lede changed. Never a version. |

## Editing order

1. `index.html` — find every place a claim appears: hero lede, eyebrow,
   stat grid, the numbered "How it works" / "autonomous loop" stages, the
   "Strong points" feature cards, the "Run it locally" section, the visible
   FAQ `<dl>`, **and** the JSON-LD (`FAQPage` answers + the `Article`/
   `SoftwareApplication` descriptions). The FAQ exists twice — visible HTML
   and structured data — keep both in sync.
2. `index.md` — mirror the same prose edits.
3. `llms.txt` — update the intro blockquote + summary paragraph if the
   high-level pitch changed.
4. `llms-full.txt` — `cp index.md llms-full.txt`.
5. `og.png` — `scripts/regen_og.sh` if the card's text changed.

## Freshness fields (dates, not versions — keep current, never remove)

- `sitemap.xml` → `<lastmod>YYYY-MM-DD</lastmod>`
- `index.html` JSON-LD → `"dateModified": "YYYY-MM-DD"`
- `index.md` → `**Last updated:** YYYY-MM-DD`

Set all three to `date +%F` whenever you change content.

## JSON-LD specifics

- There must be **no** `softwareVersion` key. If a release tempts you to add
  one back, don't — the audit treats it as a pin.
- `SoftwareApplication.description`, `Article.description`, and every
  `FAQPage` answer are evergreen prose — same rules as visible copy.
- The `HowTo` install steps reference `brew`/`scribe init`/`scribe cron
  install` — only touch them if a release actually changes the install flow.
