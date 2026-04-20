You are clustering person-mentions across a knowledge base. The goal is to identify when multiple surface forms (emails, @handles, name variants) refer to the same real person.

## Inputs

Below you will see:

1. A list of existing `people/*.md` pages with their titles and any `aliases:` already declared.
2. A list of person-mentions harvested from the rest of the KB that do NOT match any existing title or alias.

## Rules

1. Cluster mentions that plausibly refer to the same person. Pair on:
   - Exact or near-exact name match (capitalization, "Lisa" vs "Lisa Chen").
   - Email local-part matching the name ("lisa.chen@acme.com" → "Lisa Chen").
   - Handle matching the name ("@lisa" near mentions of "Lisa Chen").
2. DO NOT cluster when the only evidence is a common first name and no other signal.
3. For each cluster, pick a proposed canonical form. Prefer full names over handles over emails.
4. If a cluster matches an existing `people/*.md` page, propose adding the new surface forms as `aliases:` to that page. Otherwise propose creating a new `people/<slug>.md`.
5. Do NOT write, modify, or delete any files. Output only the block format below.

## Output format

For each cluster output:

```
### [CANONICAL_NAME]
- Existing page: <people/slug.md if one matches, otherwise "none (propose new)">
- Surface forms:
  - <mention 1>
  - <mention 2>
  - ...
- Confidence: high | medium | low
- Suggested action: <add-aliases | create-new | skip>
```

If nothing clusters cleanly, output exactly: `no identity clusters found`.

## Existing people pages

{{PEOPLE}}

## Unmatched mentions

{{MENTIONS}}
