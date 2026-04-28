You are Pass 1 of a chapter-aware two-pass absorb for a long, multi-chapter raw article. Your job is to produce a **plan** for **this single chapter only** — a list of distinct entities, decisions, tools, concepts, or principles that appear in this chapter. A separate plan is generated for each chapter; they will be merged before Pass 2 writes the wiki pages.

Source article (full): {{RAW_FILE}}
Chapter chunk (this is your input): {{CHUNK_FILE}}
Chapter title: {{CHAPTER_TITLE}}
Source PDF title: {{SOURCE_TITLE}}
Plan output path: {{PLAN_FILE}}

## Atomic facts already extracted for this chapter

The block below was produced by an earlier pass against this same chunk. It is the verbatim claim pool. When you propose entities, every `key_claim` you list **must** correspond to a fact in this block (paraphrased OK, but the fact's substance must be there). If a fact doesn't fit any entity, drop it — not every fact deserves its own wiki page. Empty block means atomic-fact extraction was off or produced nothing; in that case fall back to reading the chunk fresh.

{{FACTS}}

## Procedure

1. Read **the chapter chunk file** end to end. Do **not** read the full raw article — your scope is this chapter.

2. Identify the distinct topics worth their own wiki page: a named tool, a decision, a pattern, a solution, a research finding, a person, a concept that originates or is materially developed in this chapter. Skip cross-references to topics introduced elsewhere — the chapter that introduced them will plan them.

3. For each topic pick the best `type` from: `article | tool | decision | pattern | solution | research | person`.

4. Write the plan as JSON to {{PLAN_FILE}} using this exact schema:

```json
{
  "raw_file": "{{RAW_FILE}}",
  "source_title": "{{SOURCE_TITLE}}",
  "chapter": "{{CHAPTER_TITLE}}",
  "domain": "<domain from the raw article's frontmatter, or general>",
  "entities": [
    {
      "label": "Proposed Wiki Article Title",
      "type": "pattern",
      "one_line": "Single-sentence hook that orients the Pass 2 writer.",
      "key_claims": [
        "non-obvious claim 1 that Pass 2 must preserve",
        "numeric or named decision 2"
      ]
    }
  ]
}
```

Rules:
- **0–4 entities typical per chapter.** A short chapter may legitimately have zero — emit `"entities": []` rather than padding. A monograph chapter (>4 KB) might justify 3–4. Anything more is over-splitting; the merge step will combine fine-grained variants from multiple chapters.
- **Dedupe against existing wiki articles first.** Before proposing an entity, Grep the wiki for the proposed label (and close variants). If an article already exists, set `"label"` to its exact title so Pass 2 updates it instead of creating a duplicate.
- **Labels must be exact wiki article titles**, suitable for a `[[Wikilink]]`. Title Case, no trailing period.
- **`key_claims` is the verbatim preservation list.** Include any numeric, named, or non-obvious claim that would fail the reconstruction test (*"could a future query rebuild this from a summary alone?"*). Pass 2 is required to quote these verbatim.
- **Stay in your chapter.** If you need context from elsewhere in the paper to ground a claim, read it in the raw_file but do not propose entities for what you find there — those belong to other chapters' plans.

5. Do not write any wiki articles. Do not touch `_index.md` or `_backlinks.json`. Only write the plan JSON.

You are running non-interactively. Never ask questions — decide and act.
