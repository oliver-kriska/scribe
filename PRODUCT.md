# Product

> Scope: this file describes the **getscribe.dev marketing site** (`site/public/index.html`) —
> the only frontend surface in this repo. The scribe CLI itself is documented in `README.md`
> and `CLAUDE.md`. impeccable commands read this file before touching the site.

## Register

product

The surface is a marketing landing page, but it is treated as **product UI**, not a brand
campaign: design serves the message, restraint is the floor, and the page should disappear
into the content the way a good tool disappears into the task. No marketing theater.

## Users

Developers who live in the terminal and run AI coding agents — Claude Code and/or Codex CLI —
across more than one repo. They are technically fluent, skeptical of hype, and have likely
already tried (or dismissed) RAG stacks, Obsidian/Notion notes, and other "AI memory" tools.

Their context on the page: they arrived from a link, a search ("RAG alternative", "Claude Code
knowledge base", "AnythingLLM alternative"), or word of mouth, and they are deciding in a few
minutes whether scribe is worth installing. The job to be done is **evaluation**: understand
what scribe does, whether the claims hold up, how it differs from the tools they already know,
and how to install it — then either run the install command, star the repo, or leave.

## Product Purpose

getscribe.dev exists to explain scribe — a single-binary CLI that writes and maintains a
personal, LLM-written knowledge base from your git repos, agent sessions, and self-sent links —
clearly and credibly enough that a technical visitor decides to install it.

Success is a developer who reads the page, *trusts* the claims (because each is backed by a
concrete number, command, or verifiable detail), understands the niche versus the alternatives,
and copies the install command or opens the GitHub repo. The page is the top of a developer-tool
funnel; its currency is earned trust, not impressions.

## Brand Personality

**Technical, honest, understated.** Builder-to-builder, written by someone who reads the source
before making a claim. Proof over adjectives: the voice names real numbers ("$115 in 20 min",
"884 files after 2 weeks"), real commands, and real limitations. It is confident but dry — no
exclamation marks, no "supercharge your workflow," no growth-hack urgency. It volunteers caveats
("Caveats — honest ones") because admitting limits is how a technical audience earns trust.

Emotional goal: a skeptical engineer should come away thinking *"this person knows exactly what
they built and isn't trying to sell me"* — quiet credibility, not excitement.

## Anti-references

The page must not read like any of these:

- **Generic SaaS template** — gradient hero, three identical icon+heading+text feature cards,
  pricing tiers, the big-number hero-metric block, a "Trusted by" logo wall.
- **AI-startup slop** — purple/violet gradients, glassmorphism everywhere, emoji bullets,
  gradient text, vague "supercharge / unlock / 10x your workflow" copy.
- **Corporate/enterprise** — stock-photo people, navy-and-gold, dense legalese, buzzword soup,
  formal distance from the reader.
- **Over-designed editorial** — display-serif + italic drop caps + broadsheet magazine grid
  affectation on a tool that is not a magazine.

## Design Principles

1. **Receipts over adjectives.** Every claim earns its place with a concrete number, command,
   schedule, or verifiable detail. If a sentence could appear on any tool's landing page, cut
   or specify it. (The copy already does this — protect it.)
2. **The product's values, made visible.** scribe is local-first, plain-markdown, no-lock-in,
   no-SaaS. The site should embody that: fast, static, light on tracking, honest about cost —
   the medium agrees with the message.
3. **Earned familiarity.** Treat it like product UI. A developer fluent in Linear, Stripe, and
   Raycast should trust it on sight; no invented affordances, no strangeness without purpose.
4. **Honest by default.** Name the caveats and the competitors fairly. Trust compounds; a
   visible limitation is more persuasive than a hidden one.
5. **Respect the reader's competence.** Dense, scannable, skimmable by an expert in a hurry.
   Assume a technical audience; cut hand-holding and marketing connective tissue.

## Accessibility & Inclusion

Target **WCAG 2.2 AA**.

- Body text ≥ 4.5:1 contrast against its background; large/bold text ≥ 3:1. Watch muted grays on
  the near-white and dark surfaces — the existing `--muted`/`--muted-2` tokens must clear the bar
  in both light and dark themes.
- Visible, non-color-only focus states on every interactive element; full keyboard navigation.
- Respect `prefers-reduced-motion` — the hero knowledge-graph canvas and any reveals need a
  static/crossfade alternative.
- Light and dark themes are first-class (already implemented via `data-theme` + system fallback);
  both must pass contrast independently.
- Don't rely on color alone to convey state (success/warn/accent) — pair with text or icon.
