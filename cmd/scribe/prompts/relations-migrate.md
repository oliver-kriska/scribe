You are classifying edges in a personal knowledge base. Each edge points from one article (the *source*) to another article it references (the *target*). Your job is to label the relationship that best describes the edge, drawing only from the closed set provided.

The source article's `type:` field constrains which kinds are allowed. Only emit a kind that appears in the **Allowed kinds** list. If no listed kind clearly fits, emit `null` — that signals "leave the link in untyped `related:`."

## Allowed kinds (closed set)

- `supersedes` — the source replaces or makes the target obsolete (decisions only)
- `superseded_by` — the source has been replaced by the target (decisions only)
- `contradicts` — the source asserts the opposite of the target on a factual point (decisions only)
- `applies_to` — the source is a concrete application of the target (solutions, patterns)
- `derived_from` — the source is built on or extracted from the target (solutions, tools)
- `instance_of` — the source is a specific instance of the target's general pattern (patterns, ideas)
- `specializes` — the source is a more specific version of the target (patterns)
- `extends` — the source builds on prior work in the target (research)
- `cited_by` — the source cites the target as a reference (research)
- `informs` — the source provides background that shapes the target (research)

## Rules

1. **Conservative by default.** When in doubt, emit `null`. Bad typed edges are worse than missing ones.
2. **Use only `Allowed kinds` for this source type.** Anything outside that subset is invalid output.
3. **Direction matters.** `A supersedes B` means A came after B and replaces it; `A superseded_by B` is the reverse.
4. **Confidence is part of the contract.** Emit `"high"` only when the source body explicitly describes the relationship. `"medium"` for strong implication. `"low"` for plausible but unstated.
5. **One reasoning line per edge.** ≤200 characters, factual, no flourish.

## Output format

Strict JSON array, one object per candidate, in the same order as the candidates were given. No preamble, no trailing prose. If you cannot produce valid JSON, output exactly `[]`.

```json
[
  {"target": "Target Title", "kind": "supersedes", "confidence": "high", "reasoning": "Source body says 'this replaces Target Title' explicitly."},
  {"target": "Other Title", "kind": null, "confidence": null, "reasoning": "No clear typed relationship; loose conceptual link."}
]
```

## Source

Title: {{SOURCE_TITLE}}
Type: {{SOURCE_TYPE}}
Allowed kinds: {{ALLOWED_KINDS}}

Body excerpt:
{{SOURCE_BODY}}

## Candidates

{{CANDIDATES}}
