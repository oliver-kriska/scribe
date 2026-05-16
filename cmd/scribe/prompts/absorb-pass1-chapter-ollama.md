OUTPUT ONLY ONE JSON OBJECT. NO PROSE. NO CODE FENCES.

You are Pass 1 of a chaptered absorb. Read THIS chapter only. List 0–4 distinct entities that warrant their own wiki page.

## Source metadata (echo back)

- raw_file: {{RAW_FILE}}
- source_title: {{SOURCE_TITLE}}
- chapter: {{CHAPTER_TITLE}}

## Atomic facts (claim pool — every key_claim must come from here when this block is non-empty)

{{FACTS}}

## Chapter chunk body

<<<CHUNK_BEGIN>>>
{{CHUNK_BODY}}
<<<CHUNK_END>>>

## Output schema

```json
{
  "raw_file": "{{RAW_FILE}}",
  "source_title": "{{SOURCE_TITLE}}",
  "chapter": "{{CHAPTER_TITLE}}",
  "domain": "general",
  "entities": [
    {
      "label": "Wiki Article Title (Title Case, no period)",
      "type": "decision|pattern|solution|research|tool|project|idea|person",
      "one_line": "One sentence orienting Pass 2.",
      "key_claims": ["verbatim claim 1", "claim 2"]
    }
  ]
}
```

## Rules

- 0–4 entities. Zero is valid for a short chapter — emit `"entities": []`.
- `label` must be Title Case wiki titles.
- `type` must be exactly one of: decision, pattern, solution, research, tool, project, idea, person. (No `article` — there is no such type; a generic note is `research`.)
- `key_claims` must be verbatim non-obvious facts. Numerics, named decisions, dates — preserve exact wording.
- Skip cross-references to topics introduced elsewhere; the chapter that introduced them will plan them.

OUTPUT: ONE JSON OBJECT. NO PROSE. NO CODE FENCES.
