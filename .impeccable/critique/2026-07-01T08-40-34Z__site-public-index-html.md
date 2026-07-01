---
target: site/public/index.html
total_score: 32
p0_count: 0
p1_count: 2
timestamp: 2026-07-01T08-40-34Z
slug: site-public-index-html
---
## Design Health Score

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 4 | Animated real `scribe sync` terminal, `copied ✓` + ⌘C fallback, scroll-progress bar, count-up, `aria-selected` tabs. Excellent. |
| 2 | Match System / Real World | 3 | Dev-native vocab fits the audience, but scribe-coined terms (absorb pass 1→2, Dream cycle, triage) appear before they're defined. |
| 3 | User Control and Freedom | 3 | Theme toggle, collapsible details, mobile menu with Esc + focus trap. No back-to-top on a very long page; no recollapse-all on the 13-item FAQ. |
| 4 | Consistency and Standards | 4 | Strong token/ARIA discipline; "44 subcommands" stated identically in scope/commands/surface. |
| 5 | Error Prevention | 3 | Clipboard fallback; little to err on a static page. |
| 6 | Recognition Rather Than Recall | 3 | Install command repeated at hero/CLI/CTA; undercut by sheer page length. |
| 7 | Flexibility and Efficiency | 3 | Multiple install paths, copy buttons, keyboard tabs — but the "evaluate in minutes" JTBD fights the length. |
| 8 | Aesthetic and Minimalist Design | 2 | Weakest axis. 16 sections; differentiator restated ~8×; two comparison tables; 13 FAQs with 6 near-dupes. Visually minimal, structurally maximal. |
| 9 | Error Recovery | 3 | Limited applicability; the one real failure path (clipboard) degrades gracefully. |
| 10 | Help and Documentation | 4 | FAQ, CLI tabs, `scribe doctor`, honest caveats, literal source-file citations. Outstanding. |
| **Total** | | **32/40** | **Good, bordering Excellent** |

## Anti-Patterns Verdict

**Does this look AI-generated? No.** It reads as hand-built and deliberately restrained, and it actively defeats the common slop tells.

**LLM assessment.** No gradient text (single solid `--accent`, One-Voice Rule holds). The icon-card grid tell is defused — `.feature` cards use a mono receipt-kicker, not a decorative icon tile. The eyebrow rides only 7/10 content sections (dropped on self-labeling headings), which is genuine Earned-Kicker discipline, not reflexive scaffolding. Three bespoke, theme-aware SVGs (pipeline rail, typed knowledge graph with *real* KB slugs, trust-boundary diagram) are hand-drawn information design — the clearest human-built signal on the page. No side-stripe borders. The one real slop *risk* is quantitative sameness: the "big mono number + small label" block appears 4× (hero stats, scope strip, cost figures, orphaned local metrics), which starts to read as a stat template.

**Deterministic scan (detector, 2 findings).**
- **Em-dash overuse — CONFIRMED (warning).** 37 em-dashes in body prose (56 total; the 1,465 `--` are CLI flags in code, correctly excluded). A real cadence tell for an "understated, honest" voice — the page leans on the em-dash as its default connective. This is the one issue the detector caught that the design review missed.
- **Numbered section markers 01–06 — FALSE POSITIVE in context (advisory).** These are `.stage__num` in the #autonomous section — a real ordered sequence (the 5 cron pipeline stages: discover→extract→capture→absorb→dream), used on one section only, where order carries meaning. That is the explicitly sanctioned exception, not AI scaffolding. Dismissed.

**Visual overlays.** No user-visible in-browser overlay was injected (no live-server/detect.js injection into a presented tab). Assessment used headless-Chrome full-page + component renders in both themes as the visual evidence instead. The full-page capture shows the hero apparently repeated at the very bottom — verified to be a headless stitching artifact, not duplicated content (the real DOM tail is the Install CTA + footer).

## Overall Impression

This is a strong, trustworthy page that wins its skeptical audience early and decisively — the animated real terminal run, the to-the-cent `scribe cost` table, and literal source-file citations are textbook trust engineering for a developer who distrusts marketing. The single biggest opportunity is **subtraction**: the depth work has grown the page to 16 sections where the core claim is restated ~8 times, and the weakest axis (aesthetic/minimalist, scored 2) is entirely about scale, not craft. The page is visually minimal but structurally maximal — and for a "no marketing theater" brand, repetition is the one thing that quietly spends the honesty everything else earns.

## What's Working

1. **Terminal-as-proof, three ways.** The animated hero `scribe sync` (resolves to `0 errors $0.00 68.4s`), the real cost-reconciliation table ($103.91 to the cent), and `scribe doctor` output turn "receipts over adjectives" into a live artifact. The medium *is* the argument for a terminal-dwelling dev.
2. **Two-Voice type discipline + Earned-Kicker restraint.** Machine facts are mono/tabular-nums, prose is system-sans, cleanly separated; the mono kicker deliberately appears on 7/10 sections, not all — the exact discipline that separates this from generated landing pages.
3. **Bespoke, theme-aware SVG evidence.** The knowledge graph renders a real KB slice with actual slugs and the real 10-kind edge schema; the trust-boundary diagram renders the security model as flow. Hand-drawn, not stock.
4. **Honest trust engineering.** Volunteered caveats ("a hosted provider sees your KB content on every call"), dated comparison snapshots, and code citations. Admitting limits is how this audience is won.

## Priority Issues

**[P1] Page length + differentiator repetition defeats the "evaluate in a few minutes" JTBD.**
16 content sections; the core claim (compiled markdown wiki vs RAG/vector-DB/manual notes) is restated in the h1 sub, features sub, compare sub, two comparison tables, #who, and ~6 FAQ answers. Search and command listings each appear twice (CLI tab + dedicated section).
- *Why it matters:* Success is defined as a skeptic who evaluates in minutes and copies the install line. This page can't be read in minutes; repetition reads as insecurity to an audience that distrusts overselling.
- *Fix:* Cut FAQ 13→~6 (collapse the RAG/AnythingLLM/Obsidian variants); delete either #commands or the `.surface` spec (duplicates); fold #search into the CLI `search` tab it already contains.
- *Command:* **/impeccable distill**

**[P1] Hero sub is a ~90-word wall that buries the lede.**
`.hero__sub` opens with the plain promise, then detours into "the compiled, LLM-written knowledge base, the 'LLM Wiki' pattern… no vector DB and no RAG… cross-project, cron-driven… 100% locally on Ollama." The clearest human explanation of scribe (the two #feels anecdotes) is 9 sections below the fold.
- *Why it matters:* A skeptic skims the first two lines; the hero leads with pattern-jargon instead of the one-sentence "what it is."
- *Fix:* Two short sentences (what it does + agents read it back); move the "LLM Wiki / no vector DB" framing into #features.
- *Command:* **/impeccable clarify** → **/impeccable distill**

**[P2] Receipts self-inconsistency — the one flaw that attacks the brand's core promise.**
For a page that says "every figure is checkable against `scribe --help`," its own figures disagree: (a) headline "Install in 60 seconds" vs JSON-LD HowTo `totalTime: PT5M` (5 min); (b) "last updated" is three-way split — footer `2026-06-30`, OG `article:modified_time 2026-06-30`, JSON-LD `dateModified 2026-07-01` (correct value is today, 2026-07-01); (c) OG/Twitter alt "Over 7,400 docs" vs precise "7,472" everywhere else.
- *Why it matters:* This is the cheapest fix with the highest brand leverage — the scrutinizing persona (Riley) *will* diff these, and internal number disagreement directly undercuts the receipts thesis.
- *Fix:* Single source of truth for doc count and modified date; pick 60s *or* 5min and use it in both the visible copy and the structured data.
- *Command:* **/impeccable clarify**

**[P2] Mobile removes the always-visible primary action.**
At ≤640px, `.nav__cta { display: none }` drops the sticky Install button; the sole conversion is then only reachable via the hamburger or by scrolling to an in-page CTA.
- *Why it matters:* The single goal is "copy the install line." Hiding the persistent CTA where attention is shortest is a conversion own-goal.
- *Fix:* Keep a compact Install button in the mobile nav bar.
- *Command:* **/impeccable adapt** (with **/impeccable layout**)

**[P2] The "big number + label" block is reused 4× — the page's one real sameness risk.**
Hero stats, scope strip, cost figures, and orphaned local metrics all render the same treatment; the scope strip and hero stats overlap in function.
- *Why it matters:* On-brand individually; four times it's the closest the page comes to AI-slop layout-sameness.
- *Fix:* Cut one (hero stats vs scope strip say overlapping things) or give each number-block a distinct visual role.
- *Command:* **/impeccable distill** / **/impeccable quieter**

**[P3] Em-dash cadence — 37 in body prose.**
The page leans on the em-dash as its default connective. For an "understated, honest" voice, that many is itself a faint tell.
- *Why it matters:* Small, but this audience notices cadence; commas/colons/periods/parentheses would read as more deliberately written.
- *Fix:* Sweep body copy and convert ~half the em-dashes to other punctuation.
- *Command:* **/impeccable clarify**

**[P3] Dead CSS from the folded #local section.**
`.local`, `.local__body`, `.local__metrics`, `.metric`, `.local__code*` are orphaned after the standalone local section was folded into `.cost__local` (a CSS comment says as much); only `.local__pill` survives.
- *Why it matters:* A single-file, receipts-first source shouldn't ship dead rules — the code-level version of an unbacked claim.
- *Fix:* Remove the orphaned rules.
- *Command:* **/impeccable distill**

## Persona Red Flags

**Jordan (confused first-timer)** — stalls in the hero. The h1 is evocative but doesn't say *what it is*; the 90-word `.hero__sub` is a wall; the eyebrow "runs on cron · self-hosted" assumes cron literacy. The plainest explanation (the #feels anecdotes) is 9 sections down. Saved only by the animated terminal visibly doing the thing.

**Riley (deliberate stress tester)** — the best-served audience, and the one who catches the flaws: the 60s-vs-5min, the two "last updated" dates, and 7,400-vs-7,472; and will read the 6 near-duplicate FAQ variants as try-hard for a "no marketing theater" brand. Will also probe "0 required API keys" against the Anthropic default path (`claude -p` on CLI auth) — technically keyless, but a stress-tester wants that asterisk.

**Casey (distracted mobile user)** — hit hardest. The buyer-intent comparison stays a horizontal scroller on phones; the `scribe cost` ASCII block overflows sideways (`white-space: pre`, 11.5px); the hero wall is worse on a small screen; and the persistent Install CTA is hidden ≤640px.

**Skeptical team lead (security is the gate)** — well served by the trust-boundary diagram + 6 mechanism cards + "a mechanism in the code, not a policy in a doc." But the trust-boundary SVG is **desktop-only** (`.tb { display:none }` <820px), so a lead skimming on a tablet loses the single clearest security artifact; and the nav has "Teams" but no "Security/Trust" signpost, so the security story is discoverable only by knowing to look under Teams.

## Minor Observations

- **Dark-mode "Under the hood" divider is nearly invisible** — the band is `bg-sub` (~17% L) on `bg` (~14% L); a 3% lightness gap barely reads in dark theme, so the intended "spine ends / reference begins" landmark loses its job. A hairline or wider tonal step would restore it.
- **Hero terminal is `aria-hidden="true"`** — screen-reader users get none of the core proof device; the stats and inline command carry the load, so acceptable, but a caption alternative would help.
- **DESIGN.md is stale** in two spots (focus rings are actually present; the canvas backdrop was replaced by the inline SVG graph) — worth reconciling so the doc matches the shipped CSS.
- **Reduced-motion handling is above average** — transitions killed, caret off, static terminal scene, count-up skipped.
- **Dark/light token set is duplicated** across `[data-theme="dark"]` and the `prefers-color-scheme` block (~50 lines) — necessary for the fallback but a two-places-to-edit smell in a hand-maintained file.

## Questions to Consider

1. Which 5 sections actually carry the evaluation decision — and do the other 11 exist for evaluation, or for SEO? What would you delete to make the page readable in three minutes?
2. The brand is "no marketing theater," yet the FAQ carries 13 entries with 6 near-duplicate competitor variants. Does that quietly spend the honesty the terminal and cost table earn?
3. The clearest explanation of scribe — the #feels anecdotes — sits 9 sections below the fold. What happens to comprehension if "what it actually feels like" leads and the pipeline diagram follows the story?
4. "0 required API keys" is a headline figure, but the default provider is Anthropic via `claude -p`. Does the bare zero need the same asterisk the hosted-provider caveat already models?
5. The only conversion is "copy the install line," yet on mobile the persistent Install button is removed. Why is the primary action least reachable exactly where attention is shortest?
