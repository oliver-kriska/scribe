---
target: site/public/index.html
total_score: 34
p0_count: 0
p1_count: 1
timestamp: 2026-06-30T15-51-07Z
slug: site-public-index-html
---
## Design Health Score

Fresh, independent design-review (isolated agent, full markup + ~1700 lines of
inline CSS + JS read, real WCAG ratios computed for both themes). This re-grades
the page **after** Tasks 1–5 of this session (duplicate-id anchor fix, skip link,
grid differentiation, eyebrow trims, em-dash/cadence softening) but the review
ran **before** the Teams security rewrite and the accent-on-soft contrast fix
that followed it — see "Resolved since this critique" below.

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 3 | Solid feedback (copy ✓, scroll bar, tab/theme state, live terminal); the labeled "Install" CTA gives no hint it leads to a re-CTA, not the command. |
| 2 | Match System / Real World | 4 | brew/cron/git-remote/launchd/BM25/Ollama — terminal-dev vocabulary, authentic. |
| 3 | User Control and Freedom | 3 | Theme + copy + collapsible FAQ; mobile menu has no Esc/backdrop close, no focus trap. |
| 4 | Consistency and Standards | 3 | Excellent token/component consistency, undercut by desktop-vs-mobile nav drift (different items AND labels) and two different snapshot dates. |
| 5 | Error Prevention | 3 | copy try/catch, theme bootstrap guards private mode. |
| 6 | Recognition Rather Than Recall | 4 | Comparison tables, labeled tabs, eyebrows aid recognition. |
| 7 | Flexibility and Efficiency | 4 | Copy buttons, arrow-key tablist, skip link, two install paths, search shown two ways. |
| 8 | Aesthetic and Minimalist Design | 3 | Per-component minimal but informationally maximal: 14 sections, two compare tables, 12-Q FAQ, 130-col cost table, site-wide animated graph. |
| 9 | Error Recovery | 3 | Largely n/a; copy → ⌘C hint, theme → system fallback. |
| 10 | Help and Documentation | 4 | 12-Q FAQ + JSON-LD FAQPage, CLI examples, doctor, footer links. |
| **Total** | | **34/40** | **Good (upper end), brushing excellent** |

**Trend:** 33 (2026-06-23 baseline) → 35 (2026-06-30 session-start, my own scoring,
pre-fix) → 34 (this fresh independent agent, post-Tasks-1–5). The ~1-point wobble
is grader variance (two different evaluators), not a regression — the verified
P1 from the session-start critique (duplicate `id="install"`) is **gone**, and
this agent confirms the skip link, h3 heading, and differentiated grids are in
place. What caps the score now is structural (density + nav consistency) plus one
new, genuine contrast finding — a sharper, deeper backlog than before.

## Anti-Patterns Verdict

**AI-slop risk: LOW.** "A category-fluent dev would believe a careful human built
this." Real checkable receipts (issue #3 $115/20min, the anonymized `scribe cost`
table, $103 vs $0.34 vs $0), a coherent OKLCH dual-theme system tuned to pass
contrast, a correct WAI-ARIA tablist, and a reduced-motion branch for every
animation. Named tells absent: no purple gradient, no gradient text, no
glassmorphism beyond standard nav blur, no emoji bullets, no fake logo wall, no
pricing tiers. The genuine failure mode is **over-building and length**, not slop.

Two spots a skeptic might pause: the Features 6-icon-card grid (the most
template-shaped element), and the hero stat block (redeemed by real, quiet
numbers).

**Detector (detect.mjs), run separately this session:** em-dashes 33 → 24 after
the cadence pass; `aphoristic-cadence` cleared; `numbered-section-markers`
(01–05) defensible (real pipeline sequence, the legitimate exception).

## Priority Issues

### [P1] The primary CTA defers the primary action
- **What:** "Install scribe" (the most prominent button, blue, ×3) and nav
  "Install" route to `#install`, a re-CTA section whose only buttons are "Get
  scribe on GitHub" and "Read the CLI →". The actual copyable `brew install` /
  `curl … | bash` lines live in `#cli` under the install tab — two hops from the
  hero, and the button literally labeled "Install" delivers a GitHub link, not
  the command.
- **Why it matters:** Conversion is the page's whole job; a skeptical terminal
  dev wants paste-and-go. Label/destination mismatch taxes the exact funnel.
- **Fix:** Put the real `brew install …` line + copy button in the `#install` CTA
  section (or repoint hero/nav "Install" → `#cli`); ideally a one-line install
  with copy in the hero itself. **(Judgment call — depends on whether GitHub-first
  was intentional; raised with the user.)**

### [P2] Desktop and mobile nav are two different site maps
- **What:** Desktop nav (10 items) vs mobile menu (12) differ in membership AND
  labels ("Loop"↔"Autonomous loop", "Cost"↔"Inference & cost", "Compare"↔"How it
  compares"); desktop omits Commands and In-practice; neither links `#who` or
  `#search`. `menuBtn` keeps `aria-label="Open menu"` while open, no `aria-controls`.
- **Fix:** Single source of truth for nav items+labels; flip aria-label/aria-expanded
  together; add `aria-controls`. **(Judgment call — which nav is canonical; raised.)**

### [P3] Two snapshot dates in the same Compare section
- **What:** cmp-quick caption "Snapshot 2026-06-08"; detailed table caption
  "Snapshot 2026-05-18"; footer/OG/JSON-LD all 2026-06-08.
- **Why it matters:** On a "receipts over adjectives" page, a receipt-obsessed
  reader notices the month-stale table and discounts it.
- **Fix:** Re-verify and unify, or state why they differ. **(Content-accuracy
  decision — I won't fabricate a re-verification date; raised with the user.)**

### [P3] Dense data blocks don't degrade on mobile
- **What:** The `scribe cost` terminal (~130-col, 11.5px, `white-space:pre`) and
  cmp-quick (`min-width:680px`) force horizontal scroll on phones with the
  scrollbar hidden (`scrollbar-width:none`). The detailed comparison gracefully
  stacks at ≤720px; these two don't.
- **Fix:** Mobile-condensed/cards view or a visible "scroll →" hint.

### [P3, judgment] Site-wide animated graph vs the understated north star
- The force-graph canvas is `position:fixed; inset:0` behind the entire page. On
  a page whose thesis is "the only loud things are evidence," a perpetually-moving
  particle field behind all body text is the loudest non-evidence element.
- **Fix:** Confine the live graph to the hero, or drop mid-page layer opacity.

## Resolved since this critique ran
- **Light-theme contrast (the agent's one genuine sub-AA finding):**
  accent-on-`--accent-soft` was 4.38:1. Fixed via a theme-specific
  `--accent-on-soft` token (light 5.4:1, dark 5.8:1) on .local__pill / .stage__num
  / .stage__pill / .anecdote__chip, plus the borderline 4.49:1 scribe-row metadata
  → `--fg-soft` (11.3:1). Verified by computation validated against this agent's
  own measured numbers (green 5.49 vs its 5.48). Shipped.
- **Twin 6-icon-card grids:** Teams was rewritten to a 6-card, icon-free,
  security-led spec list promoting the market-unique features (trust layer,
  secret-scan gate, extraction ledger, approval gate, promote, leader lease) and
  the dead-DOM hidden icons were removed. The Features grid remains the one
  template-shaped block.

## Persona Red Flags
- **Jordan (first-timer):** h1 is evocative but doesn't say what scribe *does*;
  meaning arrives only in the dense ~80-word sub. No dead-simple one-liner.
- **Riley (receipts):** mostly satisfied; flags the two snapshot dates and the
  unbacked "7,472 docs · zero typed by hand" (private KB, unverifiable).
- **Casey (one-handed mobile):** cost terminal + cmp-quick horizontal-scroll with
  hidden scrollbar; mobile menu dismiss only via the hardest-reach corner.
- **Skeptical senior dev:** the page is built for them and largely wins (Compare
  answers their objections, cost receipts earn trust); frictions are the deferred
  install command and more ambient motion than a minimalist trusts.

## Questions to Consider
1. Why is the literal `brew install` line two scrolls from a hero button *labeled*
   Install that leads to GitHub? What does conversion do with a one-line install +
   copy in the hero?
2. Does a graph animating behind every paragraph serve "only evidence is loud," or
   quietly contradict it? Would the skeptic trust the tool *more* if the page sat
   still below the hero?
3. Fourteen sections, two compare matrices, a 12-Q FAQ — selling to the 2-minute
   decider or the already-half-sold receipt-reader? If the former, which five
   sections get cut?
