---
target: site/public/index.html
total_score: 33
p0_count: 0
p1_count: 1
timestamp: 2026-06-23T07-38-24Z
slug: site-public-index-html
---
# Critique — getscribe.dev (site/public/index.html)

## Design Health Score

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 3 | Copy "copied" not announced to screen readers (no aria-live) |
| 2 | Match System / Real World | 4 | Speaks fluent developer; jargon is correct for the audience |
| 3 | User Control and Freedom | 3 | No skip-to-content link; tabs/menu otherwise escapable |
| 4 | Consistency and Standards | 3 | Radius scale drift (4/5/7/8 vs documented 6/10/14/20); side-stripe vs full-border |
| 5 | Error Prevention | 3 | Few input surfaces; external-link shim is safe (n/a-leaning) |
| 6 | Recognition Rather Than Recall | 4 | Everything visible/labeled; no icon-only nav; inline code examples |
| 7 | Flexibility and Efficiency | 3 | Copy buttons + tabs + theme toggle; tabs lack arrow-key nav |
| 8 | Aesthetic and Minimalist Design | 3 | Per-section kicker ×12, 2 side-stripes, 59 em-dashes add noise |
| 9 | Error Recovery | 3 | Clipboard-copy failure path not surfaced (n/a-leaning) |
| 10 | Help and Documentation | 4 | The page IS documentation: FAQ, CLI ref, config, comparison table |
| **Total** | | **33/40** | **Good (top of band, near Excellent)** |

## Anti-Patterns Verdict

**LLM assessment:** Does NOT read as AI-generated at a glance. This is a hand-built page — a real animated knowledge-graph canvas, a typed terminal demo, a full OKLCH dual-theme token system, evidence-driven mono copy. The gut reaction is "a developer built this," not "which AI made this." But it carries a handful of *concrete* tells underneath the surface.

**Deterministic scan (detect.mjs, exit 2):**
- **2× side-stripe accent border (warning)** — `border-left: 3px solid var(--accent)` at line 1446 (`.cmp tr.cmp__scribe th` — the highlighted "scribe" row of the comparison table) and line 1554 (`.cmp__quote` — an italic pull-quote callout). The quote is the textbook offender. Both violate the absolute ban *and* the "Don't use a >1px colored side-stripe" line just written into DESIGN.md.
- **59 em-dashes in body copy (warning)** — AI-cadence tell.
- **3 aphoristic constructions (warning)** — "X. No Y." / "Not a feature. A platform." manufactured-contrast cadence (e.g. "…recoverable. No web UI; the source…").
- **numbered section markers 01–06 (advisory)** — the `.stage__num` pipeline (capture→…→index). Justified: it's a genuine ordered sequence, numbers carry real information. Not a tell here.
- **Radius outside scale ×9 (advisory)** — 4/5/7/8px used where DESIGN.md documents 6/10/14/20. Real minor drift.
- **Color outside palette ×5 (advisory)** — `#000` ×2 are mask-gradient stops (false positives); the 3 oklch values are the terminal traffic-light dots (intentional ANSI, legitimate).

**Visual overlays:** Not run. I did not inject the live browser overlay this pass, so there is no user-visible overlay in a browser tab — findings are from full source review + the deterministic scan + computed contrast, not from in-page injection.

## Overall Impression
A genuinely strong, hand-crafted developer landing page that earns trust through receipts. The single biggest opportunity is **protecting that trust from its own copy**: the heavy em-dash + "Not X. Just Y." cadence and a couple of side-stripe callouts are exactly the manufactured-marketing texture a skeptical senior engineer (your core reader) is primed to distrust — the opposite of the "honest, technical, understated" positioning.

## What's Working
1. **Evidence as design.** The terminal demo, the knowledge-graph backdrop, and tabular-mono numbers do the persuading. This is the rare dev-tool page that shows instead of claims.
2. **A real design system.** OKLCH tokens, a true dark mode (separate ramp, not an inversion), no-FOUC bootstrap, theme-color metas. Consistency is structural, not incidental.
3. **Documentation-grade help.** Comparison table, FAQ (with JSON-LD), inline config + install + search examples, CLI pointer. A visitor can fully evaluate without leaving.

## Priority Issues

**[P1] `--muted-2` body text fails WCAG AA** (carried from audit)
- Why it matters: code comments and comparison "no" cells render at 3.5:1 (light) / 4.1:1 (dark); low-vision readers can't read them.
- Fix: darken light to ~`oklch(56% 0.01 250)`, lighten dark to ~`oklch(64% 0.012 250)`; re-verify ≥4.5:1.
- Suggested command: `/impeccable colorize`

**[P2] Two side-stripe accent borders**
- Why it matters: `border-left: 3px solid var(--accent)` is the single most recognizable AI-UI tell and contradicts your own DESIGN.md. The `.cmp__quote` pull-quote is the canonical offender.
- Fix: comparison "scribe" row — keep the `accent-soft` background, drop the 3px stripe (or use a full 1px accent border + a leading ✓). Quote — full 1px border or an `accent-soft` tint with a leading mark; lose the left stripe.
- Suggested command: `/impeccable quieter`

**[P2] Copy-cadence tells (59 em-dashes, 3+ aphorisms)**
- Why it matters: to the exact skeptical engineer you're courting, this texture reads as AI-written marketing and quietly undercuts the "honest, technical" voice — even though it echoes the maintainer's real cadence.
- Fix: thin em-dashes to commas/colons/periods where they aren't doing structural work; keep one or two "Not X. Just Y." beats and vary the rest. Preserve voice; cut the tic.
- Suggested command: `/impeccable clarify`

**[P3] Per-section mono kicker on all 12 sections**
- Why it matters: a named brand system, but applied uniformly above every section it becomes section grammar rather than voice — the "eyebrow on every section" pattern.
- Fix: keep it as the system, but vary the cadence — drop it on 2–3 sections, or differentiate treatment so it punctuates rather than labels by rote.
- Suggested command: design judgment (see question below) → `/impeccable typeset` or `/impeccable quieter`

**[P3] Radius scale drift**
- Why it matters: buttons/nav/cards use 4/5/7/8px while DESIGN.md documents 6/10/14/20; small inconsistency between system and usage.
- Fix: either add the real values to the `rounded` scale in DESIGN.md, or snap components to the documented steps.
- Suggested command: `/impeccable polish` + DESIGN.md update

## Persona Red Flags

**Sam (Accessibility-Dependent):** `--muted-2` comments below 4.5:1 (can't read); tabs have no arrow-key navigation; no skip-to-content link for keyboard users; "copied" confirmation not announced; focus rings rely on the UA default (present, but not guaranteed-contrast on the Ink/blue button fills). Several real blockers.

**Priya (Skeptical Staff Engineer — project persona from PRODUCT.md: technically fluent, hype-allergic, has already dismissed "AI memory" tools):** The receipts win her over — verified-by-reading-the-code comparison, real numbers, honest caveats. But the 59 em-dashes, the "Not X. Just Y." aphorism rhythm, and the italic side-stripe pull-quote read as the manufactured-marketing texture she distrusts, working *against* the honest-engineer positioning that would otherwise close her.

**Casey (Distracted Mobile):** Primary "Install scribe" CTA is reachable (nav + hero); theme toggle lives in the mobile menu. Long, dense, scroll-heavy page demands sustained attention; copy buttons are small tap targets (~24px — passes AA, tight one-handed). State (active tab) not persisted across reload.

## Minor Observations
- The hero stat row (7,400 docs / $0 / ~70s) is evidence, not the gradient hero-metric template — keep it.
- Comparison table is a real strength; only the accent-stripe emphasis mechanism needs rework.
- No `<img>` anywhere (all inline SVG + canvas) — zero alt-text debt, lean payload.

## Questions to Consider
- Is the per-section mono kicker a deliberate brand system you want on all 12 sections, or should its cadence vary so it punctuates instead of labels?
- Does the em-dash + "Not X. Just Y." cadence read as *your* voice, or as the AI-marketing cadence your skeptical-engineer reader is primed to reject?
- The comparison table leans on an accent side-stripe to say "this is us" — is the `accent-soft` background alone enough to carry that?
