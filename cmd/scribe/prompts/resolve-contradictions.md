You are resolving a set of contradiction pairs across a knowledge base. For each pair, decide which article should be treated as the authoritative one and which should be marked as superseded.

## Authority ranking (highest → lowest)

1. `authority: canonical` — intentional decisions/policies. Always wins over contextual/opinion.
2. `authority: contextual` — curated wiki pages (solutions, patterns, tools, research, projects).
3. `authority: opinion` — raw captures, tweets, excerpts. Loses by default.
4. If both pages have the same authority, the one with the most recent `updated:` wins.
5. If updates are the same day, the one with higher `confidence:` wins.
6. If still tied, prefer the page whose body is more specific (names, dates, numbers).

Prefer the rule that fires first; do NOT blend them.

## Output format

For each pair, output one block:

```
### [WINNER_PATH] supersedes [LOSER_PATH]
- Rule: <which rule from above fired>
- Reasoning: <one sentence on what specifically made this the winner>
- Suggested action: set `status: superseded` on [LOSER_PATH] and point it to [[WINNER_TITLE]].
```

If the pair is a framing difference rather than a true contradiction, output instead:

```
### [ARTICLE_A] and [ARTICLE_B] — not a real contradiction
- Reasoning: <one sentence on why they coexist>
```

Do NOT write, modify, or delete any files. Output only the blocks above.

## Input

{{PAIRS}}
