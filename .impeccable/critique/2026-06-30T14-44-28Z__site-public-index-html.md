---
target: site/public/index.html
total_score: 35
p0_count: 0
p1_count: 1
timestamp: 2026-06-30T14-44-28Z
slug: site-public-index-html
---
## Design Health Score

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 4 | Solid — copy "copied ✓", scroll-progress bar, tab/theme state, sticky-nav border, live terminal. |
| 2 | Match System / Real World | 4 | Speaks the terminal-dev's language correctly (cron, BM25, launchd, FTS5, Ollama). |
| 3 | User Control and Freedom | 3 | Theme persists, FAQ collapses, no traps — but `#install` anchor lands on the wrong element (duplicate id). |
| 4 | Consistency and Standards | 3 | Strong token system; duplicate `id="install"` (invalid HTML), stat numbers in sans vs the mono rule, eyebrow over-use. |
| 5 | Error Prevention | 4 | Graceful fallbacks: clipboard→⌘C hint, localStorage try/catch, IntersectionObserver guards. |
| 6 | Recognition Rather Than Recall | 3 | Clear labels everywhere; no active-section indicator in a sticky nav on a 14-section page. |
| 7 | Flexibility and Efficiency | 4 | Copy buttons, keyboard-navigable tabs, two install routes (brew/curl), shell + MCP search both shown. |
| 8 | Aesthetic and Minimalist Design | 3 | Visually clean but informationally maximal — two comparison tables, 12 overlapping FAQs, two 6-card grids. |
| 9 | Error Recovery | 3 | Largely n/a (static page); the only recovery path (copy failure) is handled. Scored generously. |
| 10 | Help and Documentation | 4 | Rich: 12-item FAQ, CLI examples, `scribe doctor`, source-file citations, GitHub/issues/install links. |
| **Total** | | **35/40** | **Good (top of band, brushing excellent)** |

## Anti-Patterns Verdict

**Does this look AI-generated? Mostly no — and for a product-register page, that's the win.**

**LLM assessment:** AI-slop risk is LOW. The page avoids every loud tell: true cool-white bg (not cream), one blue accent, flat hairline cards, system fonts, mono reserved for machine output, no purple gradients / glassmorphism / emoji bullets / gradient text. Controls are native and restrained (real ARIA tab pattern with roving tabindex + arrow keys, native `<details>` FAQ). The product-register failure mode — strangeness-without-purpose, invented affordances, mismatched controls — does not occur. The residual smell is **content-side, not pixels**: heavy positioning repetition and copy cadence (below).

**Deterministic scan (`detect.mjs`):** 3 findings.
- `em-dash-overuse` (warning) — **33 em-dashes in body copy**. A real copy-cadence tell the design review did not call out; additive.
- `aphoristic-cadence` (warning) — 3 manufactured-contrast constructions: "Not RAG. Not Obsidian." (line 2362), "No reindex per tool. No format conversion." (line 2697), and the Teams "No two laptops race…" line. **This corroborates the review's "repetition reads as machine-tuned" finding** — the detector and the LLM agree from different angles.
- `numbered-section-markers` (advisory) — flags the `01`–`05` sequence. **Defensible / effectively a false positive in context:** these number the autonomous-loop pipeline stages (each pinned to a real `cmd/scribe/*.go` function) and the 3-step How-it-works flow — genuine ordered sequences where the number carries information, which is the legitimate exception to the ban, not default scaffolding.

**Visual overlays:** Not available — the Chrome extension is disconnected, so no live browser overlay was produced. This critique is a source review + CLI scan; the contrast figures below are computed estimates, not measured, and should be confirmed in-browser before shipping a fix.

## Overall Impression

This is a genuinely good page that already lives the brand: receipts over adjectives, quiet credibility, the Lab-Notebook restraint. The strongest moments — source-file citations on every pipeline stage, the brutally honest `$103.91` cost output, dual-theme + reduced-motion engineering done correctly — are exactly what converts a skeptical terminal dev. The biggest opportunity is **subtraction**: one verified anchor bug, a light-theme contrast gap, and a layer of copy repetition/cadence are the only things standing between "very good, but tuned" and "clearly hand-made by someone who knows."

## What's Working

1. **Source-file citations on the autonomous-loop stages** (`cmd/scribe/sync.go: discover()` → `~/.claude/projects/*` → `manifest.Projects`, on all five stages). The strongest trust signal on the page — each marketing claim pinned to an exact Go function and path, in the audience's own vocabulary, in mono.
2. **Theme + reduced-motion engineering.** No-FOUC bootstrap before first paint, dual `data-theme` + `prefers-color-scheme` fallback, view-transition crossfade gated on reduced-motion, canvas graph / terminal typer / stat count-up each branching to a static frame under reduced-motion, and a global `:focus-visible` accent ring with `outline-offset` covering every control. Thorough and rarely done this well.
3. **The Cost section's intellectual honesty.** Showing the real `scribe cost` output — the `$103.91` Anthropic bill, per-provider breakdown, and "Coverage: 3175/3331 calls had token data… ~$0.62 estimated" — trusts the reader with the unflattering number. More persuasive to this audience than any adjective.

## Priority Issues

### [P1] Duplicate `id="install"` sends the primary CTA to the wrong element (verified)
- **What:** The CLI tab button (`<button … id="install">`, line 2591) and the final CTA section (`<section class="s cta-end" id="install">`, line 2929) share an id. All three `href="#install"` anchors — nav CTA (1987), hero "Install scribe" (2016), mobile menu (2039) — resolve via `getElementById`/fragment to the **first** match: the tab strip mid-CLI-section. The dedicated "Install in 60 seconds" CTA is never the scroll target, and the document is invalid HTML (duplicate id).
- **Why it matters:** "Install" is the primary conversion action. Clicking it and landing on a tab strip instead of the install CTA is confusing, and this exact audience runs validators and files issues about broken anchors. High-impact, trivial-effort. (Not fully broken — the tab does show `brew install` — so it's P1, not P0.)
- **Fix:** Rename the tab to `id="tab-install"`, update its `aria-controls`/panel `aria-labelledby`, keep `id="install"` only on the CTA section. Verify all three anchors then land on the CTA.

### [P2] Light-theme status colors are borderline/under AA
- **What:** The chromatic status colors in the comparison matrix — `--success` green `oklch(50% 0.15 152)` ≈ **~4.3:1** and `--warn` amber `oklch(54% 0.13 75)` ≈ **~4.3–4.5:1** — render as **12.5px** "Yes"/"Partial" labels on white/surface, normal-size text that needs 4.5:1. The neutral gray tokens (`--muted`, `--muted-2`, `--term-dim`) estimate ≥4.5:1 and pass; the dark theme passes (bright on near-black). This is a light-theme-only failure, and "both themes must pass independently" is the stated bar.
- **Why it matters:** WCAG 2.2 AA is a declared target and the comparison matrix is decision-critical. State is already conveyed by text+shape (not color alone — good), but the text contrast itself must still pass.
- **Fix:** Darken light-theme `--success` to ~`oklch(45%)` and `--warn` to ~`oklch(48%)`, **or** set the status words in `--fg-soft` and keep the hue only on the dot. Verify in-browser with a contrast checker.

### [P2] Generic-SaaS feature-card cliché, twice
- **What:** Features (6 cards) and Teams (6 cards) are both 3-column `icon-box + h3 + paragraph` grids — the exact "identical icon+heading+text cards" anti-reference, and two of them amplify the template read.
- **Why it matters:** This is the most "AI landing page" pattern on an otherwise hand-crafted page; the skeptic's pattern-match fires on stacked icon-card grids.
- **Fix:** Differentiate at least one. Make Teams a tighter 2-column text/`<dl>` list without icon boxes, or replace the Features grid with an annotated `scribe doctor` / terminal output so the product's own output *is* the feature list. Break the mirrored rhythm.

### [P2] Copy cadence + positioning read as machine-tuned (detector ⨯ review agree)
- **What:** The anti-RAG/Obsidian/AnythingLLM positioning recurs in the hero sub, the features intro, the compare title+sub+caption+quote, and ~5 of 12 FAQs; two near-equivalent comparison tables sit in one section. The detector independently flags **33 em-dashes** and **3 aphoristic "Not X / No Y" constructions** on top of that.
- **Why it matters:** For an audience that prizes "quiet credibility," protesting-too-much undercuts it — confidence doesn't repeat itself five times. This is the clearest residual signal that copy was machine-optimized.
- **Fix:** State the positioning **once**, with receipts (one comparison table + one FAQ); collapse the four "how is it different from X" FAQs into one; cut either the quick or the detailed compare table; thin the em-dashes toward commas/colons/periods.

### [P3] Eyebrow kicker over-rides the Earned-Kicker rule
- **What:** Mono blue eyebrows ride ~9 sections, including self-labeling ones — Features ("What makes scribe different"), Who-it's-for ("Built for developers…"), arguably How-it-works. The page *correctly* omits the eyebrow on Commands, Search, and FAQ, so the discipline exists; it just isn't applied to the self-labeling sections.
- **Why it matters:** An eyebrow on nearly every section is a named AI-tell and dilutes the ones that genuinely add a frame (Cost, Compare, CLI, Teams).
- **Fix:** Drop the eyebrow on Features and Who-it's-for (and consider How-it-works).

*(Borderline P2/P3 — the 11px Cost table and the ~680px-min comparison/cost tables horizontal-scroll under a thumb; they're the worst small-screen moments. Raise cost font to ≥12px or ship a condensed mobile cost view.)*

## Persona Red Flags

**Jordan (confused first-timer):** Bounces on the **hero-sub jargon wall** — "compiled, LLM-written knowledge base," "LLM Wiki pattern," "no vector DB, no RAG," "entity-first," "ccrider," "qmd," "Codex rollouts" — all before any plain-language payoff. Then clicks "Install scribe" → the **duplicate-id bug** drops them on a tab strip mid-CLI-section, not the friendly "Install in 60 seconds" CTA.

**Riley (deliberate stress-tester):** Mostly satisfied — receipts everywhere. Will flag: the **duplicate id** (on inspect), a **project-count mismatch** (terminal demo "41 projects" vs local block "37 found"), the **OG vs Twitter image alt** disagreement ("over 7,000" vs "over 7,400" docs), and the over-repeated positioning. Trusts the substance, dings the polish.

**Casey (distracted, one-handed mobile):** Mobile sticky nav is **logo + hamburger only** (Install/theme hidden) — must scroll to the hero CTA or open the menu. The **11px Cost table and the min-680px comparison table both horizontal-scroll** under a thumb — the most painful moments. Tap targets are otherwise fine (menu ≈44px, controls ≥34px).

**Skeptical senior terminal dev (already dismissed RAG/Obsidian/AnythingLLM):** The page is built *for* them and lands the core argument (compiled wiki vs vector DB, source-file receipts, real cost bill, honest caveats). The risk is **not** a weak argument — it's that the argument is **repeated so often it reads as over-positioning**, contradicting the quiet-credibility voice they respect. Secondary judgments: the **site-wide animated graph** (decoration on an evidence-only page) and the **two stacked icon-card grids**. None are bounce-triggers; together they're the gap between "hand-made" and "tuned."

## Minor Observations

- **No "skip to content" link** — keyboard users tab through ~10 nav items before `main`. Cheap add.
- **Heading skip:** `.anecdote h4` sits directly under the section h2 with no intervening h3. Use h3.
- **Stat/metric numbers render in sans** (`--font-display`) — a minor deviation from the "numbers in mono" Two-Voice rule. Defensible for headline figures (the real machine numbers — terminal, cost, search — are mono).
- **`.cmp__caption` is a full prose sentence set in mono** — mild Two-Voice blur; mono should carry metadata, not prose.
- **Three terminal panels carry the terminal shadow at rest** (hero, commands, cost); the Flat-At-Rest rule names only the hero terminal. Defensible (all are "evidence" terminals) but an expansion of the rule.
- **One-Voice tension:** the hero green status pulse-dot and the green/amber comparison statuses introduce secondary saturated hues outside the terminal. Status use is defensible; the decorative hero dot is the weakest justification.
- **No scrollspy / active-section state** in the sticky nav on a 14-section page.
- **`data-od-sandbox-shim` script** (lines 3–60) is committed at HEAD and ships to getscribe.dev. It document-level-intercepts every click and shims `localStorage`. It reads like a preview-sandbox artifact, not production code — verify it's intended; if not, strip it from the deployed file.

## Questions to Consider

1. If you deleted one comparison table and four FAQs, would the skeptic trust you **more** — you said it once, with receipts — rather than less? What is the repetition actually buying: SEO, or doubt?
2. The hero shouts "$0/sync" while the Cost section's headline is "$103.91." Which is the truer hook for this audience — the $0 dream, or the brutally honest $103 receipt that proves you're not hiding the bill? **Could the honest bill be the hero?**
3. The site-wide animated graph is the only purely decorative element on an evidence-only page. Does an ambient particle field behind every section serve "only evidence raises its voice" — or quietly contradict it?
4. What if **Features were a single annotated `scribe doctor` / terminal output** instead of a 6-icon grid — letting the product's own output be the feature list, and killing the most template-y pattern on the page?
5. For a dev who has *already* dismissed RAG and Obsidian, does the hero's "compiled, LLM-written knowledge base / the 'LLM Wiki' pattern" abstraction land — or would one concrete sentence ("it reads your Claude Code history and writes the wiki, so the next session already knows what you decided") convert faster?
