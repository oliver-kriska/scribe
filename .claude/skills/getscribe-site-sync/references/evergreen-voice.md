# The evergreen voice

The site states what scribe **does now**, in the present tense, with no
reference to the version that introduced it. This is not a style preference —
it is what keeps the page from rotting between the rare times anyone hand-edits
it. A reader (or a cached social card) seeing "as of v0.2.16…" three releases
later learns nothing useful and trusts the page less.

## The transform

When a CHANGELOG entry makes a sentence on the page false or incomplete, ask:
**"What is simply true now?"** — and write that. Drop the version scaffolding.

| Don't write | Write instead |
|---|---|
| "100% Ollama (complete in v0.2.16)" | "Runs 100% on local Ollama" |
| "As of v0.2.16 every LLM op…" | "Every LLM op…" |
| "Since v0.2.15 it also walks `~/.codex/sessions/`" | "It also walks `~/.codex/sessions/`" |
| "Phase 4D (v0.2.14) ported Dream to Ollama" | "Dream runs 100% on Ollama" |
| "v0.2.16 closed the last `claude -p` callsite" | "There is no remaining `claude -p` callsite in a normal `scribe sync`" |
| "The asterisk is gone — for real as of v0.2.16" | "No asterisk." (then state the present-tense fact) |

The pattern: a release is an *event*; the page describes a *state*. Translate
"X happened in vN" → "X is true". If something is only partially done, say what
is true today plainly ("Dream runs on Ollama; session-mining still calls
Anthropic") rather than "as of vN, partially…".

## What a new release usually changes

- **A feature shipped** → some card/FAQ/loop-stage that said "Claude-only" or
  "soon" or "not yet" is now wrong. Rewrite it to the new reality, present
  tense. (e.g. the Codex handshake: a sentence that said "tells every Claude
  Code session" became "tells every Claude Code *and* Codex session".)
- **A flag/function/path was renamed** → grep the surfaces for the old name;
  replace. The page name-drops real identifiers (`isNonFastForward`,
  `collectDropFiles`, `discovered_from: both`) — they must match the code.
- **A count changed** → subcommands ("N subcommands"), LaunchAgents
  ("installed N LaunchAgents"). `audit.sh` flags drift advisorily; verify the
  true number (`scribe --help`, `cron.go`) and update the prose.
- **Internal refactor only** → often no surface change. Don't invent copy for
  it. The page sells capability, not changelog.

## Tone

Match the existing page: confident, concrete, technical, no hype. It quotes
real file paths and command output. New copy should read like it was always
there — not like a patch note bolted on. If you'd need a version number to make
a sentence make sense, the sentence is wrong; rephrase it as a standing fact.

## Hard "never"

Never reintroduce: `vX.Y.Z`, bare `X.Y.Z`, `softwareVersion`, `Phase 4X`,
"as of vX", "since vX", "complete in vX", "in vX+". `scripts/audit.sh` and the
live verify both reject these; they are the cardinal invariant, not nits.
