# Codex handshake — implementation plan

Status: **in progress**
Filed: 2026-05-15
Owner: Oliver
Sibling: [[codex-discovery-plan]] (`docs/codex-discovery-plan.md`, Phase C1 shipped in 0.2.15)

Goal: make the scribe KB *handshake* cross-agent. Today `scribe init`
appends a parameterised block only to `~/.claude/CLAUDE.md`, so only
Claude Code sessions know to (a) query the KB before decisions and
(b) write drop files. Codex CLI never reads that file. This plan adds
the analogous block to Codex's global instructions file so Codex
sessions participate in the same loop.

---

## The asymmetry (why this is small)

The handshake has two halves:

1. **Drop-file ingestion — already agent-agnostic.** Drop files are
   plain markdown written to `.claude/<kb>/`; `collectDropFiles`
   (`sync.go`) scans that dir by path, not by which agent wrote it.
   The `discovered_from: "codex"` field shipped in 0.2.15 proves
   scribe already ingests from Codex-touched projects. **No scribe
   pipeline change is needed for ingestion.**

2. **Instruction injection — Claude-only.** `installClaudeMD`
   (`init.go:661`) writes the block to `~/.claude/CLAUDE.md` only.
   Codex's equivalent global-instructions file is **`~/.codex/AGENTS.md`**
   (plus project-root `AGENTS.md`, merged up-tree). That's the gap.

So the entire feature is: render a Codex-flavoured variant of the
existing block and write it to `~/.codex/AGENTS.md` with the same
idempotency markers, plus a doctor row and an init summary line.

Prior KB research is consistent: the codex-plugin deep-dive
(`scriptorium/.../codex-plugin-deep-dive`) found "Codex forks Claude
Code's manifest pattern", which lowers the risk of mirroring the
convention.

---

## Phase H1 — init writes `~/.codex/AGENTS.md` (this plan)

### Template

New embedded `templates/codex-agents-md.md`. ~95% identical to
`claude-md-kb.md`. Three deliberate deltas:

1. **qmd access line.** The Claude block leads with the
   `mcp__plugin_qmd_qmd__query` MCP tool. Codex configures MCP servers
   in `~/.codex/config.toml [mcp_servers]` with different tool naming,
   and many installs won't have qmd wired as MCP at all. The Codex
   variant leads with `qmd query "<question>"` / `qmd get` via shell
   and mentions the MCP tool only as "if a qmd MCP server is
   configured in your Codex setup". Shell qmd works from any dir, so
   this is strictly safe.
2. **Drop-file path note.** Keep the path `.claude/<kb>/` verbatim —
   `collectDropFiles` only scans there, and changing the scan is out
   of scope. Add one sentence clarifying the `.claude/` dirname is the
   shared drop location both agents use, not a Claude-only thing, so a
   Codex user isn't confused writing into a `.claude/` folder.
3. **Opening paragraph.** Mention scribe also discovers Codex CLI
   projects (true since 0.2.15) so the provenance reads correctly to a
   Codex session.

Everything else (search-proactively triggers, one-hop graph rule,
storage-boundary rubric) is reused unchanged — it's agent-neutral.

### Code

- Generalise `installClaudeMD` → `installAgentMD(path, tmpl, label,
  vars, check, yes)`. Behaviour identical (same 4 cases: missing /
  present-without-markers / in-sync / drifted; user content outside
  the markers untouched). `installClaudeMD` becomes a one-line wrapper
  so the 2026-05-13 throwaway-path guard and all existing call sites
  keep working byte-for-byte.
- `buildClaudeMDBlock` → `buildAgentMDBlock(tmpl, vars)`. Markers are
  shared (`claudeMDMarkerBegin/End` — they're HTML comments, valid in
  any markdown file; renaming the consts is churn for no gain, leave
  them).
- New `codexAgentsMDPath()` → `~/.codex/AGENTS.md`.
- New `installCodexMD` wrapper.
- New `--no-codex-md` flag mirroring `--no-claude-md`. The existing
  `allowUserWrites` / throwaway-path gating applies to both — a
  `scribe init -p /tmp/...` smoke test must not write a global Codex
  file either.
- Wire into the init flow next to the CLAUDE.md block (init.go:195)
  and add a "Codex AGENTS.md (~/.codex/AGENTS.md):" summary section.

### Doctor

New `~/.codex/AGENTS.md block` row in `checkConfig` (`doctor.go`),
mirroring the existing `~/.claude/CLAUDE.md block` check:

- file missing → WARN ("not found", Fix: `scribe init`)
- markers present → OK ("installed")
- file present, no markers → WARN ("scribe block not found")

Never FAIL — Codex is optional, same stance as the
`codex_sessions_dir` row. AGENTS.md is a softer contract than
`~/.claude/CLAUDE.md` (Codex churned `codex.md` → `instructions.md` →
`AGENTS.md`, and Desktop / managed installs may manage their own), so
the row reports *presence of the scribe block*, not "Codex is reading
it" — we can't probe the latter.

### Tests

- `installAgentMD` table test against a temp `$HOME`: create / append
  to existing file with user content preserved / in-sync no-op /
  drift-refresh, for the Codex template. Mirrors the implicit contract
  the Claude path already relies on.
- Marker reuse: a file with the scribe block and surrounding user
  prose round-trips without touching the user prose.
- Throwaway-path: `scribe init -p <tempdir>` does not create
  `~/.codex/AGENTS.md` (extend the existing 2026-05-13 regression
  guard's assertions).

### Files touched

- new: `cmd/scribe/templates/codex-agents-md.md`
- new: tests in `init_test.go` (or `codex_test.go` — keep handshake
  tests next to discovery tests)
- edit: `cmd/scribe/init.go` (generalise installer, new path/flag/wrapper,
  flow + summary)
- edit: `cmd/scribe/doctor.go` (+ row)
- edit: `CHANGELOG.md`, `README.md`, `CLAUDE.md` (Key external surfaces
  already lists `~/.codex/sessions`; add the AGENTS.md handshake note)

**Estimate**: half a session including tests.

---

## Out of scope (deliberate)

- **Registering qmd as a Codex MCP server** (writing `[mcp_servers]`
  into `~/.codex/config.toml`). The shell `qmd query` fallback in the
  template already works everywhere; auto-editing a user's Codex TOML
  is a bigger blast-radius change with its own plan if wanted.
- **Codex hooks.** `~/.codex/hooks.json` supports
  SessionStart/UserPromptSubmit/Stop (same shape as Claude Code). A
  SessionStart hook that injects the KB context dynamically is a
  cleaner long-term mechanism than a static AGENTS.md block, but it's
  a separate stream — H1 ships the static block first because it
  matches the proven Claude path exactly.
- **Project-level `AGENTS.md` injection.** Only the global
  `~/.codex/AGENTS.md` is in scope. Per-project AGENTS.md is the
  user's to manage.
- **Codex session mining** — still deferred behind 100%-Ollama
  session-mine envelope (see [[codex-discovery-plan]] Phase C3).

---

## Edge cases (H1)

| Case | Behaviour |
|---|---|
| `~/.codex/` missing entirely | `installAgentMD` mkdir's it (same as the Claude `~/.claude` mkdir) |
| `~/.codex/AGENTS.md` exists, no scribe markers | append block to end, preserve user content (existing append path) |
| Block present but drifted | refresh in place, user content outside markers untouched |
| `scribe init -p /tmp/...` smoke test | skipped — throwaway-path guard covers Codex too |
| `--no-codex-md` | skip Codex write, still do Claude |
| Codex Desktop / superset-managed install with its own AGENTS.md handling | we still write `~/.codex/AGENTS.md`; doctor reports presence only — documented soft-contract caveat |

---

## Ship checklist

```
[ ] templates/codex-agents-md.md (qmd-via-shell + shared-drop-path note)
[ ] installAgentMD generic; installClaudeMD + installCodexMD wrappers
[ ] codexAgentsMDPath() + --no-codex-md flag
[ ] init flow + summary section
[ ] doctor ~/.codex/AGENTS.md block row (WARN not FAIL)
[ ] tests: create/append/in-sync/drift + throwaway-path guard
[ ] make check + race + lint green
[ ] dogfood: scribe init --check shows the Codex row on this machine
[ ] CHANGELOG 0.2.17 + README + CLAUDE.md
[ ] commit → push → CI green → tag → GH release
```
