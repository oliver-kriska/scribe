---
name: scribe
description: Your knowledge base, written by your tools.
colors:
  bg: "oklch(99% 0.002 240)"
  bg-sub: "oklch(97.5% 0.003 250)"
  surface: "oklch(100% 0 0)"
  fg: "oklch(18% 0.012 250)"
  fg-soft: "oklch(32% 0.012 250)"
  muted: "oklch(54% 0.012 250)"
  muted-2: "oklch(53% 0.009 250)"
  border: "oklch(92% 0.005 250)"
  border-2: "oklch(95% 0.004 250)"
  border-hover: "oklch(85% 0.007 250)"
  accent: "oklch(55% 0.18 255)"
  accent-soft: "oklch(96% 0.025 255)"
  accent-fg: "oklch(99% 0 0)"
  accent-hover: "oklch(49% 0.18 255)"
  btn-primary-hover: "oklch(28% 0.014 250)"
  success: "oklch(50% 0.15 152)"
  warn: "oklch(54% 0.13 75)"
  term-bg: "oklch(20% 0.014 250)"
  term-bg-2: "oklch(24% 0.014 250)"
  term-fg: "oklch(94% 0.005 240)"
  term-green: "oklch(78% 0.15 150)"
  term-cyan: "oklch(78% 0.10 220)"
  term-yellow: "oklch(82% 0.14 90)"
  term-blue: "oklch(74% 0.13 255)"
  term-pink: "oklch(74% 0.16 350)"
  term-dot-close: "oklch(62% 0.16 30)"
  term-dot-min: "oklch(78% 0.14 90)"
  term-dot-max: "oklch(70% 0.14 150)"
typography:
  display:
    fontFamily: "-apple-system, BlinkMacSystemFont, 'SF Pro Display', 'Inter', system-ui, sans-serif"
    fontSize: "clamp(34px, 5.4vw, 68px)"
    fontWeight: 600
    lineHeight: 1.05
    letterSpacing: "-0.032em"
  headline:
    fontFamily: "-apple-system, BlinkMacSystemFont, 'SF Pro Display', 'Inter', system-ui, sans-serif"
    fontSize: "clamp(28px, 3.2vw + 6px, 42px)"
    fontWeight: 600
    lineHeight: 1.08
    letterSpacing: "-0.026em"
  title:
    fontFamily: "-apple-system, BlinkMacSystemFont, 'SF Pro Display', 'Inter', system-ui, sans-serif"
    fontSize: "19px"
    fontWeight: 600
    lineHeight: 1.2
    letterSpacing: "-0.014em"
  body:
    fontFamily: "-apple-system, BlinkMacSystemFont, 'SF Pro Text', 'Inter', system-ui, sans-serif"
    fontSize: "clamp(15px, 0.92vw + 12px, 16.5px)"
    fontWeight: 400
    lineHeight: 1.55
    letterSpacing: "normal"
  label:
    fontFamily: "ui-monospace, 'JetBrains Mono', 'SF Mono', Menlo, Consolas, monospace"
    fontSize: "11.5px"
    fontWeight: 500
    lineHeight: 1.4
    letterSpacing: "0.09em"
rounded:
  xs: "4px"
  sm: "6px"
  md: "10px"
  lg: "14px"
  xl: "20px"
  pill: "999px"
spacing:
  xs: "6px"
  sm: "10px"
  md: "14px"
  lg: "22px"
  xl: "32px"
components:
  button-primary:
    backgroundColor: "{colors.fg}"
    textColor: "{colors.bg}"
    rounded: "{rounded.sm}"
    padding: "9px 15px"
  button-primary-hover:
    backgroundColor: "{colors.btn-primary-hover}"
    textColor: "{colors.bg}"
  button-accent:
    backgroundColor: "{colors.accent}"
    textColor: "{colors.accent-fg}"
    rounded: "{rounded.sm}"
    padding: "9px 15px"
  button-accent-hover:
    backgroundColor: "{colors.accent-hover}"
    textColor: "{colors.accent-fg}"
  button-ghost:
    backgroundColor: "transparent"
    textColor: "{colors.fg}"
    rounded: "{rounded.sm}"
    padding: "9px 15px"
  button-sm:
    rounded: "{rounded.sm}"
    padding: "6px 11px"
  nav-link:
    textColor: "{colors.muted}"
    rounded: "{rounded.sm}"
    padding: "6px 11px"
  feature-card:
    backgroundColor: "{colors.surface}"
    rounded: "{rounded.lg}"
    padding: "22px 22px 24px"
  tag:
    backgroundColor: "{colors.bg-sub}"
    textColor: "{colors.muted}"
    typography: "{typography.label}"
    rounded: "{rounded.pill}"
    padding: "3px 7px"
  hero-eyebrow:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.muted}"
    rounded: "{rounded.pill}"
    padding: "4px 11px 4px 6px"
  terminal:
    backgroundColor: "{colors.term-bg}"
    textColor: "{colors.term-fg}"
    rounded: "{rounded.lg}"
---

# Design System: scribe

## 1. Overview

**Creative North Star: "The Lab Notebook"**

scribe's surface behaves like a careful researcher's bound notebook: dated, measured, and unwilling to assert anything it can't back with a figure. The page is a bright, near-white workspace where the only things that raise their voice are the evidence — a dark terminal showing a real run, monospace metadata carrying exact numbers, and a single blue accent that marks what matters. Tabular numerals, hairline rules, and mono labels are the notebook's ruled lines; the prose sits between them at a comfortable reading measure. Nothing is decorated for its own sake.

This is a **product**-register surface, not a brand campaign. It is treated like a well-tuned tool: design serves the message, restraint is the floor, and the interface disappears into the content the way scribe itself disappears into your cron schedule. The system explicitly rejects the four anti-references in PRODUCT.md — the **generic SaaS template** (gradient hero, three identical feature cards, a hero-metric block), **AI-startup slop** (purple gradients, glassmorphism, gradient text, "supercharge your workflow" copy), **corporate/enterprise** (stock photography, navy-and-gold, buzzword soup), and **over-designed editorial** (display-serif drop caps on a tool that isn't a magazine). Where most dev-tool landing pages shout, scribe states.

Two themes are first-class: a paper-bright light mode and a near-black `oklch(14%)` dark mode, switched by `data-theme` with a system fallback and a view-transition crossfade. The accent shifts brighter in dark mode (`oklch(70%)`) so the one bright thing stays the one bright thing.

**Key Characteristics:**
- Evidence over adjectives: numbers in tabular mono, a live terminal, a knowledge-graph backdrop — not slogans.
- One accent ("Prompt Blue"), spent sparingly on state and emphasis, never as decoration.
- Flat, bordered surfaces; shadow is reserved for the hero terminal and interaction.
- System-native type (no webfonts) + a monospace voice for all metadata and labels.
- OKLCH throughout, dual light/dark themes, reduced-motion honored.

## 2. Colors

A restrained, near-neutral palette tinted faintly toward blue (`hue 250`), carrying exactly one saturated accent and a self-contained terminal sub-palette for the demo.

### Primary
- **Prompt Blue** (`oklch(55% 0.18 255)` light / `oklch(70% 0.16 255)` dark): the single accent. Used for the terminal prompt, links, current/active state, the hero `h1` highlight word, section kickers, feature-card icons, the scroll-progress bar, and the knowledge-graph nodes. It is the only chromatic voice on the page; everything else is neutral. `accent-hover` (`oklch(49%)`) deepens it on press; `accent-soft` (`oklch(96% 0.025 255)`) is the faint wash for tinted backgrounds; `accent-fg` (near-white) is text on accent fills.

### Neutral
- **Paper** (`bg` `oklch(99% 0.002 240)`): the body background. A true near-white at near-zero chroma — not a warm cream. **Subtle Paper** (`bg-sub` `oklch(97.5%)`) backs nav-hover, tags, and step circles.
- **Card White** (`surface` `oklch(100% 0 0)`): pure white for cards, the nav pill, and inputs, lifted one tonal step above Paper.
- **Ink** (`fg` `oklch(18% 0.012 250)`): primary text and the primary-button fill. **Soft Ink** (`fg-soft` `oklch(32%)`): secondary prose and sub-headings. **Muted** (`oklch(54%)`) and **Muted-2** (`oklch(53%)`, darkened to clear AA where it carries comment/cell text): labels, captions, and tertiary metadata.
- **Hairline** (`border` `oklch(92%)`, `border-2` `oklch(95%)`, `border-hover` `oklch(85%)`): 1px dividers, card outlines, and the grid lines between how-it-works steps.

### Tertiary — Terminal sub-palette
A self-contained dark surface palette used only inside the terminal demo, so the live run reads like a real ANSI shell: `term-bg` `oklch(20%)`, `term-fg` `oklch(94%)`, with syntax roles **green** (`oklch(78% 0.15 150)`, prompt user / ok), **cyan** (host / keys), **yellow** (path / warn), **blue** (links), **pink** (sigil). `success` (`oklch(50% 0.15 152)`) and `warn` (`oklch(54% 0.13 75)`) carry status outside the terminal, tuned dark enough to read as AA body text in the comparison table (dark theme keeps the brighter values).

### Named Rules
**The One Voice Rule.** Prompt Blue is the only saturated color outside the terminal. It appears on a small fraction of any screen; its rarity is what makes it read as "look here." Never introduce a second accent hue to add interest — add it with weight, size, or a number instead.

**The True-White-Not-Cream Rule.** The body background is `oklch(99% 0.002 240)` — near-zero chroma, faintly cool. It is never warmed toward beige/sand/paper-cream. Warmth is not this brand's voice.

## 3. Typography

**Display / Body Font:** the OS system stack — `-apple-system, BlinkMacSystemFont, 'SF Pro Display'/'SF Pro Text', 'Inter', system-ui, sans-serif`. No webfonts are loaded; the page renders in the reader's native UI font.
**Label / Mono Font:** `ui-monospace, 'JetBrains Mono', 'SF Mono', Menlo, Consolas, monospace`.

**Character:** one neutral sans does all structural work (headings, body, buttons, data) with weight and size carrying the hierarchy; the monospace is a distinct second voice reserved for anything that is or describes machine output — terminal text, section kickers, tags, stat units, step indices. The contrast between "human prose in the system sans" and "machine facts in mono" is the core typographic idea. Display weight is a restrained 600 (never a hairline display weight); negative tracking tightens large headings (`-0.022em` to `-0.032em`).

### Hierarchy
- **Display** (600, `clamp(34px, 5.4vw, 68px)`, lh 1.05, tracking −0.032em): the hero `h1` only. `text-wrap: pretty`, hyphenation on, to survive long compound words at narrow widths.
- **Headline** (600, `clamp(28px, 3.2vw + 6px, 42px)`, lh 1.08, tracking −0.026em): section titles (`.s__title`).
- **Title** (600, 19px / 16px, tracking −0.014em): step titles and feature-card `h3`s.
- **Body** (400, `clamp(15px, 0.92vw + 12px, 16.5px)`, lh 1.55): all prose. Constrained to **58–60ch** (`.hero__sub`, `.s__sub`) so the measure stays readable.
- **Label** (mono, 500, 11.5px, uppercase, tracking +0.09em, Prompt Blue): the section kicker (`.s__eyebrow`). Also drives tags (11px) and stat labels.
- **Stat number** (600, 26px display, tracking −0.02em, `tabular-nums`): the hero metrics, with a smaller muted unit suffix.

### Named Rules
**The Two-Voice Rule.** The system sans is for humans; the monospace is for machines and metadata. A label that names a number, a command, a tag, or a file is mono. Don't blur the line by setting prose in mono "to look technical," or metadata in the sans.

**The One-Kicker Rule.** The mono Prompt-Blue kicker labels a section **once**, as a deliberate wayfinding system — never stacked with a second eyebrow, never used decoratively mid-section. It earns its place because it doubles as the page's machine-voice motif, not because "landing pages put text above headings."

**The Earned-Kicker Rule.** A kicker only appears when it adds a frame the heading does not already carry (e.g. "How it works", "How it compares", "Who it's for"). When the heading already names the topic ("38 subcommands. One binary.", "Search from any terminal…", "Common questions."), the kicker is dropped — its presence must mean something, not scaffold every section. As of this writing the kicker rides 7 of 10 sections, deliberately skipping the three reference sections whose headings are self-labeling. An eyebrow on *every* section is the AI-grammar tell this rule exists to prevent.

## 4. Elevation

Flat by default; shadow earns its place. Surfaces are distinguished by 1px hairline borders and one-step tonal tints (`Paper` → `Subtle Paper` → `Card White`), not by ambient shadow. Two things are allowed to lift off the page, and both are meaningful: the **hero terminal**, whose deep layered shadow marks it as the page's primary evidence object, and **interactive cards**, which raise a soft shadow and translate −1px on hover to confirm they respond. Depth is a signal here, never a default texture.

### Shadow Vocabulary
- **Terminal** (`--shadow-terminal`: `0 1px 1px / 0 10px 30px / 0 28px 60px` stacked, ~0.04–0.10 alpha light, heavier in dark): the single hero object. Reserved for the terminal demo.
- **Card hover** (`--shadow-card-hover`: `0 4px 14px oklch(20% 0.012 250 / 0.06)`): appears only on `:hover` of feature cards, paired with `translateY(-1px)`.
- **Modal** (`--shadow-modal`): for the mobile menu / overlays.

### Named Rules
**The Flat-At-Rest Rule.** Cards, panels, tags, and inputs are flat with a hairline border until the user interacts. A resting surface with a drop shadow is a bug. The only standing exception is the terminal demo, because it is the literal subject of the page.

## 5. Components

Precise and restrained: tight radii (`r-xs` 4px on inline/tiny chrome → `r-xl` 20px, with interactive controls unified on `r-sm` 6px), hairline 1px borders, monospace metadata, quiet at rest and reactive on hover/focus. Same shapes everywhere; consistency is the point.

### Buttons
- **Shape:** `r-sm` (6px) radius — shared with small buttons, nav links, and icon chrome so every interactive control reads as one family — `display: inline-flex`, 7px icon gap, weight 500, 14px, slight negative tracking.
- **Primary:** Ink fill (`fg`) with Paper text — the inverted, highest-emphasis action; hover lightens to `btn-primary-hover` (`oklch(28%)`).
- **Accent:** Prompt-Blue fill with `accent-fg` text; hover → `accent-hover`. Used for the single most important conversion ("Get started").
- **Ghost:** transparent with a 1px `border`, Ink text; hover fills `bg-sub` and shifts the border to `border-hover`.
- **Press:** all buttons translate +0.5px on `:active` (a 60ms tactile dip). Icons are inline 14px SVG.

### Tags / Chips
- **Style:** mono 11px, Muted text on `bg-sub`, 1px `border`, pill radius (999px), 3px 7px padding. Used for capability tags under how-it-works steps. Static, non-interactive.

### Cards / Containers
- **Feature card:** Card White on a 1px `border`, 14px radius (`r-lg`), ~22px padding. Hover → `border-hover` + Card-hover shadow + `translateY(-1px)`. A 32px tinted icon tile (Prompt-Blue glyph on `bg-sub`) leads each.
- **How-it-works steps:** three equal cells in a single bordered container, separated by 1px grid lines (`gap: 1px` over a `border` background) rather than gaps — a ledger-grid look. Each step leads with a numbered mono index in a 22px circle.

### Navigation
- **Style:** sticky, translucent `nav-bg` with `backdrop-filter: saturate(140%) blur(10px)`; a bottom hairline appears only once scrolled (`.is-scrolled`). Links are Muted, 14px, `r-sm` (6px) radius, filling `bg-sub` and going to Ink on hover. The brand mark is a 22px Ink rounded square with a mono `s` glyph. Collapses to a menu button below the `md` breakpoint; the theme toggle sits inline (icon) on desktop and full-width labeled in the mobile menu.

### Terminal (signature component)
- The hero's right column: a dark `term-bg` card, 14px radius, deep layered shadow, traffic-light dots, a centered mono title, and a typed body using the ANSI sub-palette with a blinking caret. This is the brand's primary proof device — a real `scribe` run, not a screenshot mockup. Min-height 340px so layout doesn't jump as lines type in.

### Knowledge-graph backdrop (signature component)
- A fixed full-viewport `<canvas>` at `z-index: 0`, opacity ~0.62, masked with a vertical fade + side feather so it breathes near nav and footer and keeps focus on the content column. Nodes/edges use the `--graph-*` Prompt-Blue rgba tokens. Reduced-motion lowers its opacity and (per JS) should not animate.

## 6. Do's and Don'ts

### Do:
- **Do** keep Prompt Blue as the only saturated color outside the terminal — spend it on state, links, and one emphasized word, never as fill-for-interest. (The One Voice Rule.)
- **Do** set every number, command, tag, file path, and label in the monospace voice with `tabular-nums`. Receipts are the brand. (The Two-Voice Rule.)
- **Do** keep surfaces flat with 1px hairline borders at rest; let only the terminal and hover states carry shadow. (The Flat-At-Rest Rule.)
- **Do** hold body prose to 58–75ch and lead large headings with `text-wrap: pretty`/`balance` and negative tracking (−0.022em to −0.032em).
- **Do** keep the body background a near-zero-chroma cool white (`oklch(99% 0.002 240)`); carry both themes through every contrast check.
- **Do** add a visible, non-color-only `:focus-visible` ring to every interactive element (this is the one place the shipped CSS is currently silent — see audit).

### Don't:
- **Don't** ship the **generic SaaS template**: no gradient hero, no three identical icon+heading+text cards as the whole story, no pricing-tier grid, no big-number hero-metric block as decoration, no "Trusted by" logo wall.
- **Don't** drift into **AI-startup slop**: no purple/violet gradients, no decorative glassmorphism, no emoji bullets, and **never** `background-clip: text` gradient text. Emphasis comes from weight, size, or the accent — one solid color.
- **Don't** go **corporate/enterprise**: no stock-photo people, no navy-and-gold, no buzzword soup, no formal distance from a technical reader.
- **Don't** go **over-designed editorial**: no display-serif + italic drop caps + broadsheet grid. scribe is a tool, not a magazine.
- **Don't** warm the neutrals toward cream/sand/beige, or introduce a second accent hue. Tint stays faintly cool toward `hue 250`.
- **Don't** put a drop shadow on a resting card, stack a second eyebrow above the mono kicker, or set prose in mono "to look technical."
- **Don't** use `border-left`/`border-right` greater than 1px as a colored accent stripe on cards, callouts, or list items.
