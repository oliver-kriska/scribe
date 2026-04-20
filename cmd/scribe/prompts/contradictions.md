You are reviewing a knowledge base for contradictions. You will be given a set of wiki articles. Your job is to identify pairs of articles (or passages inside the same article) that disagree with each other on a factual or decisional claim.

Output format: one finding per line, in this exact shape:

```
[ARTICLE_A | ARTICLE_B] <one-sentence description of the disagreement>
```

Rules:

1. Report only **factual contradictions**, not stylistic differences or overlapping-but-complementary framings.
2. Prefer specificity. "A says X is done; B says X is blocked by Y" is a real finding. "A emphasizes speed; B emphasizes quality" is a framing difference, not a contradiction.
3. If you cannot find any clear contradictions, output exactly: `no contradictions found`.
4. Do NOT try to resolve anything. Do NOT write or modify any files. Do NOT use any tools. Just list the findings.
5. Max 20 findings. Pick the most important if there are more.

## Articles to review

{{ARTICLES}}
