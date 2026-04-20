You are consolidating the output of 5 parallel assessment tracks for project '{{PROJECT}}' at '{{P_PATH}}' (domain: '{{DOMAIN}}').

The tracks have already written their findings to:
  - Structure: {{STRUCTURE_OUT}}
  - Features:  {{FEATURES_OUT}}
  - Docs:      {{DOCS_OUT}}
  - Decisions: {{DECISIONS_OUT}}
  - Gaps:      {{GAPS_OUT}}

Your job: read all five track outputs and produce a single consolidated project overview inside the KB at {{KB_DIR}}. Do NOT re-read the project source — the tracks already did that. You are a librarian, not a researcher.

Step 1: Read {{KB_DIR}}/CLAUDE.md for frontmatter conventions and writing standards.

Step 2: Read all 5 track files.

Step 3: Check if {{KB_DIR}}/projects/{{PROJECT_LOWER}}/overview.md already exists.
  - If it does: UPDATE it. Preserve the existing frontmatter's `created` date. Set `updated` to {{TODAY}}. Merge new findings with existing content, resolving contradictions in favor of the track outputs (reality wins per Rule 11). Do NOT duplicate sections.
  - If it does not: CREATE it at that path. Frontmatter per CLAUDE.md: title, type: project, created: {{TODAY}}, updated: {{TODAY}}, domain: {{DOMAIN}}, status (active | paused | completed | idea — infer from State of Play), stack (infer from Structure track), confidence: medium, tags, related: [], sources: ["assessed from {{P_PATH}} on {{TODAY}}"].

Step 4: Structure the overview body as:

  ## Summary
  (3-5 sentences. What this project is, for whom, in what state.)

  ## Architecture
  (Condensed from Structure track — directory layout, entry points, persistence, deployment. Half the length of the track output.)

  ## Features
  (Condensed from Features track — bullet list of capabilities and workflows.)

  ## Key Decisions
  (Condensed from Decisions track — locked-in choices with one-line rationale each.)

  ## Documentation
  (One sentence summarizing what exists + a short list of the most authoritative files.)

  ## Gaps & Open Work
  (Condensed from Gaps track — surfacing only what matters for a collaborator joining today.)

Step 5: For durable NEW findings that did not exist before:
  - Decisions worth their own article → write to {{KB_DIR}}/decisions/ (use wiki link from overview)
  - Append 1-3 entries to {{KB_DIR}}/projects/{{PROJECT_LOWER}}/learnings.md if the learnings file exists; skip if it does not exist (do not create)
  - Append 1-3 entries to {{KB_DIR}}/projects/{{PROJECT_LOWER}}/decisions-log.md if the decisions-log file exists; skip if it does not exist (do not create)

Step 6: Rebuild {{KB_DIR}}/wiki/_index.md and {{KB_DIR}}/wiki/_backlinks.json.

Writing standards (strict):
- Flat, factual, encyclopedic. Not AI-cheerful.
- One claim per sentence. Short sentences.
- Attribution over assertion: "project chose X because Y" not "X is the best choice".
- No em dashes, no peacock words, no rhetorical questions.
- Target length 60-100 lines for the overview. Hard cap 150 lines — split into sub-articles if you exceed it.

Do NOT delete or remove any existing articles. Rule 12 is append-only.
Do NOT git commit. The caller handles git.
You are running non-interactively. Never ask questions. Decide and act.
