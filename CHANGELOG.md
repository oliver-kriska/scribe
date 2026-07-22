# Changelog

All notable changes to scribe are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning follows [SemVer](https://semver.org/) (pre-1.0 — minor bumps may include breaking changes).

## [Unreleased]

### Changed — multi-provider coding-agent session mining
- `scribe watch` now admits every provider indexed by ccrider by default instead
  of silently limiting the near-real-time queue to Codex. Explicit
  `--provider` filtering remains available. This covers Claude Code, Codex CLI,
  GitHub Copilot CLI, OpenCode, Pi, Antigravity, Amp, and future ccrider
  providers without requiring another scribe change. Amp sessions already
  imported by a ccrider build with Amp support follow the same path.
- Session prompts, CLI help, the embedded command reference, README, and site
  copy now describe the ccrider-backed path as provider-neutral rather than
  implying it is Claude-only.

### Fixed
- Recent Codex subagent rollouts can encode `session_meta.payload.source` as an
  object rather than a string. Scribe no longer rejects those rollouts; the
  unused field is deliberately left untyped.
- `scribe sync --sessions --dry-run` now previews the normal and large-session
  lanes with the same message-count bounds used by a real run, and
  `--skip-large` also suppresses the large preview.
- Dry-run syncs no longer rebuild indexes or invoke qmd, so the documented
  no-write guarantee applies to generated KB artifacts as well as extraction.

### Fixed — docs only, no behavior change
- Public copy (README, getscribe.dev + its `index.md`/`llms-full.txt` mirrors and
  OG card, the embedded `scribe.yaml` template comment, and CLAUDE.md) described
  triage scoring as "BM25". It never was: the gate is a weighted per-category
  FTS5 `MATCH` hit count computed in pure SQL (`buildTriageSQL`, triage.go).
  All triage copy now says FTS5 keyword scoring; qmd's lexical search remains
  correctly described as BM25. The zero-LLM-tokens-on-reject claim is unchanged
  — that part was always true.

## [0.4.2] — 2026-07-15

An agent-skills release. The single embedded `scribe-kb` skill becomes a
three-skill bundle — `scribe-kb` (author/search content), `scribe-kb-tidy` (work
the `scribe lint` content-quality queue), and `scribe-cli` (drive the CLI
itself) — and `scribe skill install` now writes them to every major coding
agent's discovery path (Claude Code, Codex, OpenCode, Pi), since the Agent
Skills format is byte-identical across them. Alongside, `scribe lint` learned to
explain the warnings it can't auto-fix and to hand that work to an agent.

### Added — `scribe-cli` agent skill + the CLI now advertises the skills
- New third bundled skill **`scribe-cli`**: how to drive the scribe CLI itself.
  It leads with the read-only `scribe doctor` / `scribe status` diagnose-first
  flow, carries a goal→command quick map, and routes content work to `scribe-kb`
  and the lint content-quality queue to `scribe-kb-tidy` (one skill, one job). Two
  references ship with it: `COMMANDS.md` (the full subcommand map, grouped) and
  `TROUBLESHOOT.md` (symptom → doctor section → fix for the common failure modes:
  extraction stalled, cron off, FDA lost after an upgrade, ollama down, qmd not
  indexed, lint errors). Includes the lock rule for `sync`/`dream`.
- The CLI now tells humans an agent can do this work. `scribe skill install`
  closes with a plain-language next step (open an agent in the KB and ask it —
  naming what each skill does); `scribe status` / `scribe doctor` print a
  one-line `scribe-cli` pointer at the end of the scoreboard; and the `scribe
  lint` "Needs review" footer closes with a blank-line-separated "An agent can
  work these for you" paragraph — a numbered `scribe skill install` → `scribe
  lint -v` → ask-an-agent sequence (readability fix over the earlier cramped
  inline pointer), and the footer's three buckets are now spaced apart.

### Added — `scribe skill install` targets every agent, not just Claude Code
- `scribe skill install` now writes the bundle to each agent's skill-discovery
  directory, defaulting to **both** `.claude/skills/` (Claude Code) and
  `.agents/skills/` (the agentskills.io cross-tool standard that **Codex CLI,
  Pi, and OpenCode** all read). Two directories cover every known agent because
  the `SKILL.md` body and `references/` are byte-identical across them — the
  format is the open Agent Skills standard, so no per-vendor translation is
  needed.
- New `--agent <list>` flag selects targets: `claude`, `codex` (alias of
  `agents`), `agents`, `opencode`, `pi`, or `all` (repeatable / comma-separated;
  default `claude,agents`). `codex` and `agents` de-duplicate to the same
  `.agents/skills/`. `--target <dir>` remains as the explicit, agent-agnostic
  override. `--check` now verifies drift across every resolved target.
- The Codex-specific `agents/openai.yaml` composer metadata is deliberately
  omitted — it's not part of the standard, and implicit description-matching
  surfaces the skills in Codex without it.

### Added — `scribe-kb-tidy` agent skill
- The embedded skill bundle (`scribe skill install`) now ships a second skill,
  **`scribe-kb-tidy`**, alongside `scribe-kb`. It's a procedure for working the
  `scribe lint` content-quality queue an agent can't auto-fix: split bloated
  articles, expand/merge thin stubs, archive overgrown rolling files, and merge
  KB-self-named directories. Grounded in real KB cleanups — it's honest about
  which classes have a battle-tested recipe (rolling-archive, self-named-dir
  triage) versus which are greenfield (split, merge), enforces "mechanical is
  safe, judgment is propose-first," and carries a `references/FIELD_NOTES.md`
  with the archive-naming/frontmatter reality, the 150-vs-200 threshold gap, and
  the parser-vs-scanner convergence trap. `scribe skill install` writes each
  skill to its own `<target>/<skill-name>/` subdirectory; `scribe skill list`
  enumerates both.
- The `scribe lint` "Needs review" footer now names the skill: it points at
  `scribe lint -v` for the file list and the `scribe-kb-tidy` skill to walk an
  agent through the fixes.

### Added — `scribe lint` explains what it can't auto-fix
- The remediation footer gains a **"Needs review (no automatic fix):"** section.
  The content-quality warning classes with no fix command (bloated / thin /
  rolling-overgrown / self-named directory) each print their remediation action
  (split / expand / archive / merge), and the footer points at `scribe lint -v`
  to list the files — so a passing run with N warnings never leaves them silently
  unexplained (previously the footer went blank when no class had a command).
- `scribe lint` now surfaces **`--fix`-able frontmatter** as its own warning
  class. A file that passes validation but whose frontmatter `--fix` would still
  backfill (a tolerated-but-unset `authority:`, list defaults, a slash date) is
  counted and routed to `scribe lint --fix` — so `lint` predicts what `--fix`
  will change instead of reporting "clean" and then rewriting N files on the next
  run (the "lint said nothing, --fix fixed 12" gap). The check is a pure dry-run
  of the fixer, so the two can't disagree.

## [0.4.1] — 2026-07-15

A reliability patch centered on frontmatter convergence: the deterministic
fixers (`scribe lint --fix`, `scribe tier write`) now reach a fixpoint on
malformed frontmatter instead of silently no-op'ing or oscillating, the
extraction seam that produced the malformed frontmatter is closed at the source,
and `scribe lint` gained a "how to fix" footer plus a regression net. Also clears
two hosted-model (MiniMax M3 / Together) truncation and empty-envelope losses.

### Fix — the frontmatter fixers converge
- One root cause spanned the family: `parseFrontmatter` and the line-level
  scanner read a malformed opening fence differently, so `scribe lint --fix`
  reported "clean" while `scribe tier write --missing-only` rewrote the same file
  every run (`wrote=1` forever). Three fence classes now normalize to a bare
  `---`: duplicate top-level keys (collapsed last-wins to match the validator),
  a joined `--- title:` fence (split), and a trailing-whitespace `--- ` fence
  (~36 files on the reference KB). A nested `frontmatter:` map — the extraction
  artifact where a source file's own frontmatter was wrapped instead of merged —
  is stripped, promoting the more specific nested `domain` first. `tier write`
  now skips any file it can't parse cleanly and flags it for `--fix` instead of
  oscillating. `autoFixArticle`'s honesty guard re-parses its own output, so a
  file is never "fixed" into valid-looking garbage. Verified end-to-end on a
  6.5k-article KB: `lint --fix` converges to 0 and `tier write` reports 0
  malformed-fence skips. (4870562, d285702)

### Fix — the malformed-frontmatter producer seam is closed
- Traced the origin: raw source articles are clean; both artifacts (the nested
  map and the trailing-whitespace fence) came from the off-Anthropic extraction
  envelope path, where a local/hosted model has no tool-use schema enforcement so
  its raw action `content` reached disk verbatim. `clampEnvelopeFrontmatter` now
  normalizes a malformed opening fence at write time — sharing the exact
  `normalizeOpeningFence` helper with `--fix` so the write-time seam and the
  on-disk healer can't disagree — and the 8 envelope prompts were reworded off
  the literal `frontmatter` placeholder a weak model echoed as a nested key.
  (ff69ff0)

### Fix — hosted-model truncation and empty envelopes
- Hosted requests now send an explicit `max_tokens`; omitting it let Together's
  2048-token default silently truncate dream envelopes (`ok=true`, output exactly
  2048), and a `finish_reason=length` now logs a warning. Pass-2 moved to a
  strict `json_schema` envelope with a token raise, closing a MiniMax M3
  empty-`{}` loss that dropped ~20-40% of envelopes under `json_object`. A dead
  Together FP8 slug was retired from the docs. (a620b92, 49b4468, 65bc759)

### Added — lint tells you how to fix what it found
- `scribe lint` now closes with a **"To fix, run:"** footer naming the exact
  command(s) for whatever it surfaced (`scribe lint --fix`, `scribe tier write
  --missing-only`), plus the residue no command repairs (missing title, invalid
  confidence). The `--fix` hint shows on any error now, not only mechanical ones.
  (22858ce, c19db60)
- **Nested-`frontmatter:` warning (Phase 5).** Lint warns on a stray nested
  `frontmatter:` map so a regression is visible instead of silently valid (these
  PASS frontmatter validation). Detection reuses the fixer's own predicate — lint
  warns *iff* `--fix` would strip it — so the check and the fixer can't drift
  apart. Routes to `scribe lint --fix` via the remediation footer; 0 false
  positives on the clean reference KB. (8cb9fd6)

## [0.4.0] — 2026-07-10

Everything that accreted on `main` after the v0.3.0 team-KB release, cut as one
minor: an agent-facing `scribe drop` CLI, daily `dream --hot` consolidation,
priority lanes for the pending-session queue, self-healing cron plists, a
KB-first adoption metric, and a Go 1.26.5 security bump. Path-keyed project
identity is the one behavior change — migrated automatically, idempotently, on
first load.

### Security — Go 1.26.5 clears the Encrypted Client Hello advisory
- The Go toolchain moves 1.26.4 → 1.26.5, single-sourced in `go.mod` and honored
  by CI's `go-version-file` and the release container's `GOTOOLCHAIN=auto`. This
  clears stdlib advisory **GO-2026-5856** — an Encrypted Client Hello privacy
  leak in `crypto/tls` that `govulncheck` flagged as reachable from scribe's own
  code, turning the `vuln` CI job red. Direct and indirect `golang.org/x` deps
  ride along in the same pass: `x/sync` 0.21.0 → 0.22.0, `x/net` 0.56.0 → 0.57.0,
  `x/sys` 0.46.0 → 0.47.0. `make ci` (test + race + golangci-lint + govulncheck)
  is green again.

### Added
- **Cron plists self-heal across upgrades.** `cron install` digest-stamps every
  plist it writes; a scribe-authored plist whose content no longer matches what
  the current binary would generate is refreshed without `--force`, while
  unstamped or hand-edited files are never auto-touched. `scribe doctor` gains a
  `cron-plists` drift row (ok/stale/hand-edited/missing per job), and the brew
  formula's `post_install` runs the new `scribe cron install --if-installed` —
  a silent no-op unless cron was already opted into — so `brew upgrade` picks up
  new or changed scheduled jobs automatically. Existing installs need one
  `scribe cron install --force` to adopt stamping. Install also heals load
  state — a current plist that isn't loaded into launchd gets bootstrapped —
  and bootstrap retries once to absorb launchd's bootout-teardown race
  (exit 5), both hit live on the first adoption run. (#54)
- **`scribe drop`** — validated drop-file authoring CLI. Agents (and humans)
  author well-formed drop files from any project without hand-rolling
  frontmatter; the scribe-kb skill and both handshake templates now point at it,
  with hand-written YAML kept as the no-scribe-on-PATH fallback. (#21)
- **`scribe projects add --from-sources`** enrolls every `sources.include`-listed
  repo in one pass — globs expand, non-git paths are skipped with a one-line
  reason. (#28)
- **Priority lanes for the pending-session queue.** High-signal sessions mine
  first regardless of arrival order (Hot/Normal lanes inside each size pool);
  floor-reservation admission plus 7-day aging promotion prevent starvation, and
  `scribe status` shows the lane split. The queue file gains a 4th column with a
  back-compat parser for all legacy line shapes — no migration needed. (#22)
- **`scribe dream --hot`** — daily hot-domain mini consolidation between weekly
  dreams. Auto-selects the single most-touched domain since the last pass,
  self-gates on churn / recent-dream / budget so idle days cost nothing, and
  ships its own scoped prompt pair. Scheduled daily at 03:10. (#24)
- **KB-first adoption metric.** `scribe sync` measures how often agent sessions
  query the KB before their first edit (derived from ccrider, marker format
  pinned against ccrider's own importer source), caches it in run records, and
  surfaces 7d/30d ratios in `scribe status` and per-machine digest notes —
  never in the committed team digest. (#23)
- **Failure traces in extraction prompts.** All 15 extraction templates now ask
  for approaches that were tried and rejected (and why) plus the conditions
  under which the chosen fix breaks; a matching KB-wide convention rule ships in
  the embedded kb-CLAUDE.md. (#42)
- **Progress heartbeat during long LLM calls** — one stderr line every 30s, so
  a hung run and a slow run are distinguishable in cron logs. (#19)

### Changed
- **Project manifest identity is path-keyed** instead of basename-keyed, with
  automatic, idempotent in-place migration on first load. Two checkouts sharing
  a basename no longer collide; worktrees still fold into their main checkout;
  same-basename projects get unique display names. (#8)

### Fixed
- `update_frontmatter` envelope actions tolerate the alternate merge-map keys
  small local models invent (`set`/`fields`/`updates`) instead of erroring with
  "empty frontmatter map"; the ollama dream prompts also now spell out the
  required shape. Found on the first real `dream --hot` run (gemma3:12b). (#53)
- Regenerated `_index.md` synopsis lines mask credential-shaped strings in
  team KBs, so a reindex can never resurface a value the commit gate held. (#5)
- `scribe doctor` now surfaces held stop-word files (previously only visible
  via `git status`), and run locks are scoped per-KB — an unrelated KB's sync
  no longer blocks another KB's runs or the hourly auto-commit. (follow-ups to
  #25/#26)

### Also in 0.4.0 — landed on main after the v0.3.0 tag was cut
- **Stop-words commit gate** (#25): user-defined words hold (or mask) a document
  out of the KB at commit time — whole-word case-insensitive by default,
  `/regex/` opt-in, union of the shared `scribe.yaml` list and a personal
  never-committed one. Same fail-closed posture as the secret gate.
- **`scribe projects add <path> [--local] [--domain] [--name]`** (#41/#28):
  one-step enrollment for repos with no session history, with
  comment-preserving `scribe.yaml` editing — and the `scribe.local.yaml`
  merge footgun fixed: local `sources.include`/`exclude` now UNION with the
  committed list instead of silently replacing it.
- **doctor/status KB-scoping completed** (#27): cron kb-scope headline,
  manifest-scoped session backlog, per-collection qmd status.
- **#26 follow-ups**: per-KB scheduler cadence gating, machine-level
  `daily_output_token_ceiling` across KBs, multi-KB watcher.
- Internal: the three `nolint:gocognit` LLM drivers decomposed on top of a
  scripted stub-provider harness (#9).

## [0.3.0] — 2026-06-30

The big one: scribe grows from a single-user tool into one a small team can
share. A handful of devs can point scribe at the same git-backed KB —
fresh-clone bootstrap, pull-merge-reindex on every sync, one machine elected to
dream, per-teammate subscriptions, and `scribe promote` to move an article from
your personal KB into the shared one. A config-trust layer keeps a shared
`scribe.yaml` from quietly widening what gets ingested or where content is sent.

Alongside that: offload the pipeline to hosted inference when local Ollama is
inconvenient (e.g. a Mac mini running hot in summer) — without losing the
local-Ollama or `claude -p` options; a machine-level KB registry so one cron job
set serves every KB; and a `scribe cost` overhaul so multi-KB, multi-provider
spend is legible and reconciles with the provider's own dashboard.

### Added — shared team knowledge bases
- **Multi-writer shared KB.** A team can point scribe at one git-backed KB.
  `scribe init --team` scaffolds it; teammates fresh-clone and scribe rebuilds
  the local manifest from committed state. Every `scribe sync` pulls, merges,
  and reindexes before extracting, so each run sees teammates' new pages.
  Derived/coordination files get semantic auto-merges and scribe never leaves a
  half-finished rebase behind.
- **`scribe promote <article> --to <kb>`** copies an article from your personal
  KB into another (e.g. the shared team KB) with provenance recorded — the
  personal → team curation path. Derived/coordination files are refused as
  promotion sources.
- **Team dashboard** at `wiki/_digest.md` — a generated activity/ownership view
  with per-domain owners routing.
- **One machine dreams.** `scribe dream` takes a leader lease via a committed
  lease file, so on a shared KB only one machine runs the weekly consolidation
  instead of every teammate's cron racing.
- **Pull subscriptions** surface teammates' new articles in domains you care
  about; a **shared extraction ledger** lets teammates skip revisions another
  machine already extracted.
- **Project approval gate.** Discovered projects start pending; `scribe projects
  {list,approve,ignore,review}` controls what enters the pipeline, and cron /
  `scribe init --team` surface pending approvals. Discovery is further gated by
  `allowed_remotes` (git-remote identity) and `sources` include/exclude filters
  (`scribe init --allow/--disallow`).
- **Contributor attribution** stamps a `contributor:` frontmatter field on new
  articles at commit time — including scribe's own debounced sync commits.
- **Team-mode secret-scan gate** holds any staged file with a likely secret
  (entropy-confirmed) out of the commit and logs it loudly — never the secret
  itself. Covers staged `raw/` markdown, not just wiki dirs.

### Added — hosted OpenAI-compatible providers (#43)
- A new `openai-compat` provider path speaks `/v1/chat/completions`, so
  `llm.provider: together` (or `groq`, `fireworks`, `huggingface`, or a generic
  `openai-compat` with `llm.base_url`) routes the **whole** pipeline through a
  hosted endpoint. Local Ollama stays the default and `claude -p` is untouched —
  all three backends coexist, and nothing routes to a paid provider without
  explicit config.
- The API key resolves, in order, from the provider's env var (e.g.
  `TOGETHER_API_KEY`), the generic `SCRIBE_LLM_API_KEY`, or — the stable,
  cron-friendly path — `llm_api_key` / `llm_api_keys` in
  `~/.config/scribe/config.yaml`, so the key lives per-machine and never in a
  shareable KB repo.
- Optional `llm.pricing` map (USD per 1M input/output tokens) bakes real dollar
  costs into the cost ledger for hosted models.
- Non-anthropic providers auto-flip absorb pass-2 to JSON mode (the `claude -p`
  tools path is anthropic-only), and every seam parsing hosted-model JSON
  defends against shape drift.

### Added — single-KB quality of life
- `scribe config update` appends commented docs for options added since the KB
  was scaffolded, so an older `scribe.yaml` can adopt new settings without a
  hand-merge (defaults unchanged).
- Extraction prompts now carry a research-before-create dedup protocol — the
  model checks for an existing article before proposing a new page, cutting
  near-duplicate fan-out.
- `scribe sync` folds linked git worktrees into their main project entry instead
  of enrolling each worktree as a separate project.
- `scribe doctor` flags foreign LaunchAgents that drive the same binary or KB
  (duplicate jobs running twice per slot); `scribe lint`/`doctor` detect
  unresolved git conflict markers left in articles by a botched merge.
- Quieter startup, a `--version` flag, and a one-line strictness-hold summary in
  place of one held-article line per file.

### Changed — runaway-spend backstop now covers every metered provider
- `sync.daily_anthropic_output_token_ceiling` is generalized to
  `sync.daily_output_token_ceiling` (the old key is still honored). It halts the
  day's run once the output-token ceiling is hit across anthropic **and** hosted
  providers; local Ollama is exempt (free). `doctor`'s ceiling check follows.

### Changed — `scribe cost` is multi-KB and provider-aware
- Aggregates **every registered KB by default** — hosted providers bill per API
  key and one key usually serves all of a machine's KBs, so the combined total
  matches the provider dashboard (verified to the cent against Together). Scope
  to one KB with `--kb <name|path>`, `-C`, `SCRIBE_KB`, or by running inside a KB.
- Adds **By provider** and **By KB** rollups, so spend per backend (anthropic /
  hosted / local) and per KB is visible at a glance.
- Measured dollars are now the headline number; the estimate for un-instrumented
  calls moved to the Coverage footnote instead of the old unreadable
  `$92.80+~$0.45` cell. Numbers are comma-grouped, wallclock is humanized
  (`47h20m`), and the tables carry header/total rules.

### Added — KB registry + KB-agnostic scheduler (#26)
- A machine-level KB registry (`kbs:` in the user config) drives
  `scribe each -- <subcommand>`, which runs a command in every registered KB.
  Cron LaunchAgents now invoke `scribe each` instead of a hardcoded KB, so one
  job set serves every KB; per-KB behavior still comes from each KB's own
  `scribe.yaml`/`scribe.local.yaml`.
- `scribe.local.yaml` (gitignored) is the per-machine override layer — provider
  routing, capture handles, subscriptions — applied after team-config trust
  enforcement, so it always wins locally without leaking to a shared KB.

### Security — team-mode trust now locks the full LLM routing surface
- The team-KB trust layer locks `llm.provider`/`llm.model` and every per-op
  provider/model (absorb passes, contextualize, dream, extract, session-mine,
  assess, deep-ingest, relations) — not just the URLs. A *named* hosted
  provider (`together`/`groq`/…) carries a built-in base URL, so the previous
  "every URL is locked, so a provider flip can only reach a trusted endpoint"
  reasoning was false once #43 landed: a pushed `provider: together` redirected
  a teammate's KB content to Together with nothing to catch it. All twelve
  (provider, model) pairs now revert to the trusted snapshot on drift and show
  up in `scribe config diff`. KBs trusted before this shipped keep working
  unchanged — the next `scribe sync` silently records the current routing as
  trusted (no re-approval needed unless other keys also drifted).
- `ingest.inbox_path` is now containment-checked: a value that resolves outside
  the KB root is refused, so a repo-controlled config can't make `sync` drain
  an out-of-tree directory and ingest arbitrary local files.
- KBs never harvest each other: ingestion and discovery walk up to the nearest
  `scribe.yaml`, so a session or project nested inside another KB is never mined
  into this one (no more `readme.md → readme.md.md` self-extract fan-out).
- `scribe` fails closed on an unparseable `scribe.yaml` for gate and LLM
  commands rather than silently proceeding with defaults.

### Fixed — hosted-provider follow-ups
- Top-level `llm.model` now cascades into the absorb passes for hosted
  providers, so the documented "move the whole pipeline to the cloud" config no
  longer fails on the absorb/contextualize stage with an empty/`haiku` model.
  Contextualize also cascades `llm.provider` like the other passes.
- `scribe init` re-pointing `kb_dir` preserves the user config's api keys, KB
  registry, and contributor instead of rewriting the file wholesale.
- Hosted-provider 429s are recorded as `rate_limit` (not `other`) in the cost
  ledger; `scribe cost` rows sort by the dollar figure they actually display
  and the table renderer tolerates ragged rows.

### Added — output & CLI ergonomics (#15, #16, #17)
- `scribe lint` groups warnings by class in the default view — e.g.
  `412× index_tier missing  (run: scribe tier write --missing-only)` — instead
  of a flat per-file stream that buried real signal under hundreds of identical
  lines. `--verbose`/`-v` restores the per-file lines; `--quiet`/`-q` (used by
  `sync` mid-extract) prints only per-file ERRORs plus the verdict. Errors stay
  per-file in every mode. (#15)
- `scribe --help` groups the 60+ subcommands into titled sections — Core,
  Content, Quality, Team, System, Debug — via Kong `group:` tags, with a
  regression test that fails if a new command lands ungrouped. (#16)
- `scribe status` separates the backlog **held by policy** (strictness=high with
  no absorb opt-in, or projects over `sync.max_extract_files`) from genuinely
  **pending** work, so a steady-state held backlog no longer reads as actionable.
  (#17)

### Changed — build/deploy split & quieter discovery (#18, #20)
- `make build` now compiles only to the repo-local `./bin/scribe` and never
  touches `~/.local/bin`; the deploy moved to a separate `make install`. On
  macOS this stops a routine rebuild from invalidating the chat.db Full Disk
  Access grant — only `install` replaces the live binary (and reminds you to
  re-run `scribe fda`). (#18)
- `sync --discover` collapses the per-project "unchanged, skipping" lines into a
  single summary instead of one line per project, so the cron log surfaces real
  activity. (#20)

### Internal — lint ratchet, coverage, dependencies
- The lint ratchet enables 17+ more golangci-lint analyzers (gocritic, intrange,
  perfsprint, errchkjson, usestdlibvars, usetesting, wastedassign,
  rowserrcheck/sqlclosecheck, …) and fixes every hit; `nolintlint` now requires
  each `//nolint` to name its linter and a reason. (#32)
- Substantially expanded `cmd/scribe` test coverage — structural lint phases,
  identity proposals, session-log repair/pre-filter, graph subcommands, capture
  I/O, cost estimation and retry policy. (#34)
- Dependency bumps: `go-sqlite3` 1.14.47, `html-to-markdown` 2.5.2, plus CI
  actions (`actions/checkout`, `actions/setup-go`, `golangci-lint-action`, …)
  via Dependabot (#44–#49).

> Note: features #15–#18 and #20 above were originally developed before the
> hosted-provider work but were accidentally closed unmerged when a base branch
> was deleted; they were relanded (patch-equivalent, reconciled against current
> `main`) via #50.

## [0.2.29] — 2026-06-03

Harden the shared LLM-action executor against the failure class behind a real
KB-corruption incident: destructive ops (overwrite, provenance writes, decay
flags) are now gated by how much the model that proposed them actually saw.
Plus a contextualize-quality fix and a clean security baseline.

### Fix — only a content-grounded consumer may overwrite a curated doc
- `applyWikiActions` is shared by ~8 consumers; the `AllowOverwrite: true`
  option — designed for pass-2 absorb, which regenerates a page from that
  page's *own* full source — had been copy-pasted to every call site. A
  session-mine run consequently overwrote an 88-line, 14-study research hub
  with a thin session-grounded reconstruction.
- New `entityWriterApplyOptions()` policy: dream, session-mine, codex-mine,
  assess, project-extract, and deep-extract all run with `AllowOverwrite`
  off and `ProtectProvenance` on. **pass-2 absorb is now the only consumer
  that overwrites** — a single, defensible line.
- `ProtectProvenance` restricts `update_frontmatter` to a safe key allowlist
  (`updated`, `status`, `tags`, …) and drops identity/provenance keys
  (`sources`, `created`, `title`, …) a blind consumer cannot have grounded.

### Fix — decay markers can no longer land on fresh docs
- The decay-candidate marker asserts "stale, >60 days old, deletion
  candidate." A dream run that ignored the (often empty) stale list had
  stamped 114 markers on docs updated <60 days prior. `applyWikiActions` now
  refuses a decay-marker append when the target's `updated:` is within 60
  days, and skips a double-stamp when a marker is already present. Both
  guards run before the dry-run gate, so `dream --dry-run` surfaces them.
- The legacy monolithic `dream.md` delete step now re-verifies staleness from
  the article's own `updated:` and never deletes on a marker alone — closing
  the only path that could act destructively on a bogus marker.

### Fix — contextualize stops leaking the capture date and inverting stats
- The raw article's YAML frontmatter (which carries an ingest `captured:`
  date) is stripped before the model sees the body; the authoritative source
  and publication date are passed via `SOURCE_META` instead. Kills the
  "study dated June 2026" class where a small model grabbed the capture month
  as the publication date.
- The prompt adds a "name the topics, not the numbers" rule, and a new
  `degenerateContextReason` check rejects breadcrumb/echo output instead of
  splicing it — the file is retried next run rather than persisting garbage.

### Build — one Go-version source, clean vuln baseline
- Go toolchain pinned to 1.26.4 from a single place (go.mod `toolchain` plus
  `go-version-file` in CI); clears stdlib advisories GO-2026-5037 /
  GO-2026-5039 and bumps x/net, x/sys, x/crypto, x/text, and
  html-to-markdown/v2. `make ci` (test + race + golangci-lint + govulncheck)
  is green.

## [0.2.28] — 2026-05-27

Make `contextualize` attribute from fact, not guesswork — and recommend a
Mixture-of-Experts local model that delivers 27–30B-class quality at
small-model speed on Apple Silicon.

### Fix — contextualize attributes from captured source metadata
- New `contextualizeSourceMeta` (contextualize.go) pulls `title` +
  `source_url`/`source_path` from each raw article's frontmatter and feeds it
  into the `contextualize.md` prompt as an authoritative `{{SOURCE_META}}`
  field, instructing the model to take the publisher from the source URL's
  domain — not from an organization merely *discussed inside* the article.
- Removes the small-model failure where the retrieval paragraph invented the
  source (e.g. a 4B model attributing a Turing Post newsletter to "Jack Clark
  at Anthropic", or copying the prompt's own example paragraph). Both raw and
  wiki contextualize paths flow through the one fixed function; the change is
  provider/model-agnostic (Anthropic or Ollama).
- No regression: articles without a source field (~30% of a real corpus) fall
  back to body inference exactly as before, and `loadPrompt`'s placeholder
  guard strips an unsupplied `{{SOURCE_META}}` cleanly.

### Docs — recommend qwen3:30b-a3b for high-quality local writes
- README local-mode now recommends `qwen3:30b-a3b-instruct-2507` (MoE, ~3B
  active params) for the high-quality `absorb.pass2` role on Apple Silicon:
  benchmarked ~4× faster than dense `gemma3:27b` (≈41 vs ≈10 tok/s on an M4
  Pro, ≈90 tok/s via MLX) at comparable quality, because LLM decode is
  memory-bandwidth-bound and MoE activates far fewer params per token.
  `gemma3:4b` remains the default contextualize model.

## [0.2.27] — 2026-05-25

Stop a KB from ingesting itself, and ship the cleanup tooling for KBs already
hit. Reported by a user whose `scribe.yaml` repo was being processed as one of
its own source projects, producing duplicate wiki pages (`readme.md`,
`readme.md.md`, `readme_md.md`). The trigger had **six** independent entry
points, plus a feedback loop — every sync auto-commits, so the KB's git SHA
changes and it re-extracts on every run — so the duplicates grew without bound.
Empirically the contamination came through **session mining**, not project
discovery, so the session-side guards are load-bearing, not just the obvious
"detect `scribe.yaml` and skip".

### Fix — a KB can no longer ingest itself (all six vectors)
- Shared predicate `isScribeKB` / `isWithinKB` / `sessionInKB` (manifest.go),
  consulted at every ingestion door:
  - project discovery, Claude + Codex — `manifest.isIgnored`
  - already-manifested re-extraction — `projectsNeedingExtraction` (sync.go)
  - cron triage SQL — `buildKBExcludeClause` (triage.go)
  - SessionEnd hook queue, which mines by ID and bypassed triage — `hook.go`
  - legacy IDs already in the pending queue — `preFilterSessions` (sync.go)
  - Codex session mining — `codex_mine.go`
- `validateActionPath` (wiki_actions.go) refuses to *write* a doubled `.md.md`
  page — closes the filename-as-title fingerprint at the seam.

### Add — cleanup tooling for KBs already contaminated
- `scribe lint` flags self-ingestion artifacts: `*.md.md` (ERROR), `<x>_md.md`
  shadowing `<x>.md` (WARN), and directories named after the KB itself (WARN).
- `scribe lint --fix` (full run) now also, all git-recoverable:
  - removes/renames the filename-as-title duplicates (`foo.md.md`, `foo_md.md`),
  - collapses **byte-identical** duplicate pages, keeping the shallower
    canonical path (frontmatter-differing near-twins stay a human call),
  - removes paths carrying an unsubstituted `{{VAR}}` template placeholder
    (e.g. `projects/{{DOMAIN}}/…`).
- `scribe lint --duplicates` (new): structural, no-LLM content-duplicate scan —
  exact normalized-body hashes + token-overlap near-dups via an inverted index
  — writes `wiki/_duplicates.md` for review. Report-only.
- `scribe doctor` adds `kb-as-project` (manifest lists a scribe KB) and
  `placeholder-artifacts` (a `{{VAR}}` leaked into a KB path) warnings.
- New weekly `lint-duplicates` LaunchAgent (Sat 1:15am); `scribe cron install`
  picks it up. The existing weekly `lint --fix` job runs the cleanups above.

### Fix — alias YAML corruption (surfaced during the sweep)
- `apply-identities` wrote unquoted `- @handle` alias entries — invalid YAML
  that silently corrupted `people/*.md`. `yamlQuoteScalar` (new) quotes at every
  alias-write site; `normalizeAliasesBlock` (lint_fix.go) repairs already-broken
  files via `lint --fix`, conservatively (already-valid quoting is left intact).

### Tests
- New coverage: KB-detection predicates, triage exclude clause, doubled-ext
  path rejection, doctor `kb-as-project` + `placeholder-artifacts`,
  self-ingestion / byte-identical / placeholder dup detection + fix, YAML scalar
  quoting, alias-block repair, content-duplicate exact + near tiers. `make ci`
  clean (golangci-lint 0 issues, `-race`, govulncheck).

## [0.2.24] — 2026-05-16

Unify envelope content sanitization into one apply-time seam. 0.2.23
fixed the path/op robustness class centrally (every envelope consumer
inherits `validateActionPath`), but the two content guards — fabricated
`[cNN-fM]` fact-ID strip and `related:` YAML normalize — were hand-wired
into only the two absorb call sites. The other six envelope consumers
(dream, assess, extract, deep, session-mine, codex-mine) could still
write local-model-corrupted content straight to disk. Under a 100%-ollama
config every one of those runs on a local model, so this was the exact
0.2.23 corruption class, one field over, on the callers that never got
the fix.

### Fix — content robustness now centralized like the path guards
- New `sanitizeEnvelopeContent` seam runs at the top of
  `applyWikiActions`, beside the existing path-guard boundary, gated by
  `ApplyOptions.SanitizeContent` (+ `ValidFactIDs`; nil ⇒ strip every
  `[cNN-fM]`, matching the prompt's drop-the-bracket fallback).
- pass-2's ~40-line inline strip block and absorb-single's hand-rolled
  `normalizeEnvelopeRelated` call are deleted — both route through the
  seam with identical behavior.
- dream / assess / extract / deep / session-mine / codex-mine opt in
  with one line each and inherit both content guards; the
  local-model corruption gap on those six paths is closed.
- Backward compatible: callers that don't set `SanitizeContent` are
  byte-identical to before; repairs log through the `envelope` channel
  (no `ApplyResult` schema change). single-pass now also strips
  fabricated fact-IDs (it runs no facts pass, so any `[cNN-fM]` there
  was always fabricated).

### Tests
- `wiki_actions_test`: opt-in strips all IDs + normalizes `related:`;
  `ValidFactIDs` keeps grounded IDs and strips the rest; opt-out is
  byte-identical; already-correct `related:` survives intact.

## [0.2.23] — 2026-05-16

Pass-1 entity fan-out cap + local-model pass-2 hardening. Three bugs
surfaced by the 2026-05-15 gemma3:27b-vs-12b pass-2 A/B (see
`docs/entity-fanout-cap-plan.md` + the `local-model-followup-plan.md`
2026-05-16 addendum). The headline fix is a chunker floor, not a model
downgrade — `pass2_model` stays `gemma3:27b`.

### Fix — runaway entity fan-out (thermal root cause)
- `chunkByHeadings` split at *every* heading with no minimum size: a
  588-word note became 13 ~45-word "chapters" → ~35 entities → pass-2
  ground the GPU ~80 min serially.
- New `chunkOptions.MinChunkBytes` (default 6 KB) coalesces
  sub-minimum heading sections; large sections are untouched; `0`
  disables. Also clears the `gemma3:4b` pass-1 timeout/parse
  instability on big docs.

### Fix — local models corrupt `related:` frontmatter
- Local models emit invalid YAML for `related:` (bracket-stripped
  bare lists, escaped garbage). `normalizeRelatedFrontmatter` rewrites
  any form back to canonical quoted `"[[X]]"`, applied via
  `normalizeEnvelopeRelated` before the pass-2 and single-pass
  applies. Model-agnostic; passthrough when there is no frontmatter /
  no `related:`.

### Fix — model-invented top dir dropped the entity
- A hallucinated top directory (`middleware/foo.md`) silently dropped
  the entity. New `errUnknownTopDir` sentinel + opt-in
  `ApplyOptions.RemapUnknownTopToWiki` re-homes it under `wiki/`. The
  remap is re-validated, so absolute / traversal / `_`-prefixed paths
  stay hard-rejected and the shared sandbox is unchanged for every
  other envelope caller.

### Tests
- `chunker_test`, `wiki_actions_test`; existing chaptered/headings
  fixtures realigned to the corrected contract.

### Verdict
- Keep `pass2_model: gemma3:27b` — `gemma3:12b` corrupts ~47% of
  `related:`. The thermal fix is the fan-out cap, not a model
  downgrade. Fix B (a hard `MaxEntities` cap) is deliberately
  deferred.

## [0.2.22] — 2026-05-15

Codex session mining (C3) — closes the Codex loop. Discovery (0.2.15)
finds projects touched only via Codex; the AGENTS.md handshake
(0.2.17) makes Codex sessions write drop files; **0.2.22 distills the
Codex sessions themselves into the KB**, the same triage→envelope→wiki
treatment ccrider sessions get. Scribe now has full (not partial)
Codex support.

### Why this was small
The only ccrider-specific surface in the session-mine path was the
transcript source. Everything downstream (renderTranscriptForPrompt,
runSessionEnvelopeOnce, applyWikiActions, the session-extract prompt
family) is transcript-source-agnostic and reused unchanged.

### Schema (verified, not guessed)
A real Codex CLI "review this project" session run in this repo on
2026-05-15 produced a 176-event rollout that served as the schema
probe. The earlier C3 plan's event-type table was materially wrong;
the parser is built against ground truth (fixture pinned at
`testdata/codex/rollout-transcript.jsonl`):

- Codex writes the **OpenAI Responses-API item schema**. `response_item`
  is the canonical model I/O stream and the **only** one consumed.
- `event_msg` is a parallel UI/telemetry stream whose content-bearing
  events (`user_message`, `agent_message`) **duplicate**
  `response_item/message`; the rest is noise (`token_count` is the
  single most frequent event). Consuming both double-counts every
  turn — the trap the guessed table fell into.
- `reasoning` items are `encrypted_content` only — unrecoverable,
  skipped. The synthetic leading `<environment_context>` user turn is
  dropped so it can't skew triage scoring.

### Added
- `fetchCodexTranscript` (codex_transcript.go) → `[]sessionTurn`,
  the same shape ccrider mining consumes. Malformed line → skip
  (non-fatal); empty rollout → empty slice.
- `walkCodexRollouts` — lookback-windowed rollout walk; unlike
  `walkCodexSessions` it does **not** dedupe by cwd (each session is a
  distinct minable transcript).
- `scoreText` — pure, in-process triage scorer (ccrider scores via
  FTS5 BM25; Codex rollouts aren't in any DB). Shares
  `TriageConfig.Resolve()` so there is one keyword/weight definition.
- `wiki/_codex_sessions_log.json` — durable processed-set keyed by the
  rollout's session_meta id (survives session resume); mirrors
  `_sessions_log.json`, reuses its generic helpers.
- `mineCodexSessions` driver + sync Phase 2.55 hook. Reuses the
  session-mine provider/config and the `session-extract-*` prompt
  family (no new prompts). Respects the 0.2.21 `--dry-run` contract.
- `codex:` config block (`mine`, `sessions_max`, `lookback_hours`,
  `min_score`); surfaced in `scribe doctor`.

### Opt-in
Mining spends LLM tokens per session, so it is **opt-in**: set
`codex: { mine: true }` in scribe.yaml (matches `absorb.atomic_facts`'
opt-in precedent). It is a silent no-op without `codex_sessions_dir`.
Discovery and the handshake remain on by default as before.

## [0.2.21] — 2026-05-15

Read-only / portability contract hardening. Three contract violations
surfaced by a Codex CLI review of this repo (2026-05-15), all verified
against the code before fixing.

### Why
- `scribe doctor` / `scribe status` and any `--dry-run` appended a run
  record to `output/runs/` (which auto-commits to the KB repo) — so
  diagnostics and previews were self-modifying.
- `loadConfig` rewrote `scribe.yaml` (temp-write + rename) on *any*
  invocation missing a top-level `absorb:` key, including read-only
  ones — a hidden write on every inspect.
- `doctor` hard-FAILed when `~/Library/Messages/chat.db` was
  unreadable and pointed at the macOS-only `scribe fda`, even on
  Linux (README-supported) or on macOS with capture intentionally
  unused — a false hard failure off the capture happy-path.

### Change
- New optional `ReadOnly()` interface; `main()` skips the run-record
  write when the selected command is read-only. `doctor`/`status`
  implement it; every `--dry-run` invocation is detected generically
  via the `DryRun` field (no per-command boilerplate).
- `loadConfig` is now pure — it never writes. The first-use `absorb:`
  discoverability backfill moved to `maybeBackfillAbsorbBlock`,
  invoked only from the mutating entrypoints (`sync` on a real run;
  `init` already had its own explicit, confirmable backfill). UX is
  unchanged (cron sync backfills within one cycle).
  `SCRIBE_NO_CONFIG_BACKFILL=1` still forces strict purity everywhere.
- `doctor`'s FDA probe is capability-aware: only a hard FAIL on
  `darwin` *and* with capture configured; otherwise a skip (non-
  darwin) or `warn` (macOS, capture unconfigured). Never blocks
  Linux / capture-off setups.

### Tests
- `commandIsReadOnly` gate (doctor/status/`--dry-run` true; real
  `sync` false), `loadConfig` purity, `maybeBackfillAbsorbBlock`
  (append / `SCRIBE_NO_CONFIG_BACKFILL` / existing-key no-op), and
  FDA capability matrix (Linux emits no FAIL via a testable
  `runtimeGOOS` seam; macOS+capture-off warns, never FAILs).

## [0.2.20] — 2026-05-15

FxTwitter empty-tweet fix. An empty-text tweet (deleted, protected,
media-only, or one FxTwitter couldn't extract) is now a **failed**
fetch instead of a fake success.

### Why
- `fetchFxTwitter` returned success whenever `code==200 && tweet.id
  != ""` — it never checked that the tweet actually had text. A
  deleted/protected tweet still has a valid id, so it produced a
  ~12-word attribution-only body (`> ` empty blockquote + `— @user
  on <date>` + original-link) reported as `fetched_via: fxtwitter`.
- That fake-success article landed in `raw/articles/`, cleared the
  stub heuristics just enough to look real, and absorb burned a
  gemma3:27b two-pass on zero content. Worse, `capture --refetch`
  re-"succeeded" on it every run (non-empty boilerplate body) so the
  dead link was never parked — an effective per-link refetch loop.
  This was a major contributor to scriptorium's x.com backlog (60
  such captures) and the "Tweet content was never captured (empty
  blockquote)" absorb log noise.

### Change
- Extracted the response→result mapping into `fxTweetToResult` (pure,
  unit-testable without an HTTP round-trip) and added an
  empty/whitespace `tweet.text` guard that returns an error.
- An empty tweet now falls through to the next fetch tier
  (trafilatura → jina) and ultimately the intentional stub/park/
  refetch path: written as `fetched_via: stub`, skipped by absorb,
  parked in `wiki/_unfetched-links.md`, terminal (no loop).
- `trafilatura` and `jina` already rejected empty output; this brings
  FxTwitter in line with the other two tiers.

### Operational
- Existing fake-success x.com articles already in a KB are unaffected
  on disk, but with scriptorium at `strictness: high` (0.2.19) they
  no longer auto-absorb. They can be cleaned up by hand or left for
  `capture --refetch`, which will now correctly park the dead ones
  instead of re-"succeeding" forever.

## [0.2.19] — 2026-05-15

Eager absorb-log heal. Follow-up to 0.2.18's salvage: `loadAbsorbLog`
now rewrites a recovered log clean **immediately**, instead of relying
on the caller's article-completion `saveAbsorbLog`.

### Why
- 0.2.18 salvaged the corrupt log in memory but only persisted the
  repair when an article finished pass-2 and the caller saved. On
  scriptorium's config (gemma3:27b, `pass2_parallel: 1`) the first
  article takes 60-90 min of serial pass-2; a run interrupted before
  then left the file corrupt, so the next run re-evaluated the whole
  backlog from the same broken state — an effective cross-run loop
  that looked like "sync is stuck" (observed: a 77-min run with the
  log still unhealed because no article had completed yet).

### Change
- `loadAbsorbLog`, on successful single-value salvage, calls
  `saveAbsorbLog(path, salvaged)` before returning. The heal is now
  durable the instant *any* code path (absorb, ingest, estimate)
  opens the log — interruption-safe.
- The total-garbage fail-open path deliberately does **not** rewrite:
  overwriting an unrecoverable file with an empty log would destroy
  whatever a human might want to inspect. Only files we successfully
  recovered the real data from get clobbered.
- Clean files still take the strict-parse fast path with zero writes
  (idempotent — no rewrite churn on every load).

### Tests
- `TestLoadAbsorbLog_SalvagesAndEagerHeals`: file on disk is clean
  immediately after `loadAbsorbLog` with no caller save; a second
  load is byte-stable.
- `TestLoadAbsorbLog_TotalGarbageFailsOpenAndLeavesFileIntact`:
  unrecoverable file is left untouched for inspection.

### Operational note (not code)
- scriptorium `scribe.yaml`: `absorb.strictness` flipped `medium →
  high`. The raw/ backlog was 328 articles dominated by 59 x.com
  captures (many empty-blockquote failed fetches) all getting the
  full two-pass gemma3:27b absorb. `high` restricts auto-absorb to
  `absorb: true` / named-domain articles. Config-only, in the user's
  KB, not this repo.

## [0.2.18] — 2026-05-15

Envelope executor hardening. A single misbehaving envelope action could
silently corrupt scribe-generated artifacts and take the whole absorb
phase offline until the file was hand-repaired. Root-caused from a real
scriptorium incident: the dream/extract envelope `append`-ed an
LLM-fabricated record onto `wiki/_absorb_log.json`, which then failed
`json.Unmarshal` (`invalid character '{' after top-level value`) on
every subsequent `scribe sync`, aborting absorb each run.

### Fix — executor rejects writes to generated artifacts (Layer 1)
- `validateActionPath` now rejects any action whose basename starts
  with `_` (`_index.md`, `_backlinks.json`, `_absorb_log.json`,
  `_hot.md`, `_staleness.jsonl`, …). Every underscore-prefixed KB file
  is derived — regenerated by `scribe index` / `backlinks` / `stale
  build` / `sections build` / `contextualize` / the absorb-log writer.
  A model has no legitimate reason to author one. Deterministic guard;
  doesn't depend on prompt obedience. The bad action is recorded in
  `res.Errors` instead of mutating disk. Basename-checked, so a
  `_`-file in any wiki subdir is caught.

### Fix — append-to-missing promotes to create (Layer 2)
- `applyWikiActions` "append" case: when the target is missing
  (`errors.Is(err, fs.ErrNotExist)`), the content is written via
  `writeFileAtomic` as a new file and logged, instead of erroring.
  The model's intent ("this content belongs at this path") is still
  satisfiable; honoring it beats discarding a generation because the
  model picked the wrong op for a new file. Layer 1 runs first, so a
  `_`-prefixed missing target is still rejected, never promoted.

### Fix — absorb log is no longer abortable (Layer 3)
- `loadAbsorbLog` attempts single-value salvage on parse failure:
  `json.Decoder.Decode` reads exactly one JSON value and ignores
  trailing bytes, recovering the valid leading object. The caller's
  `saveAbsorbLog` then rewrites the file clean — self-healing on the
  next `scribe sync`, no manual step, no data loss (only the
  fabricated trailing fragment is dropped). If even the leading object
  is unparseable, it fails open with an empty log (absorb re-evaluates
  by content hash; `checkAbsorbDecision` dedupes) rather than aborting
  the phase. Verified against the real corrupt scriptorium file: 331
  entries recovered, fragment dropped.

### Hardening — prompt rule (Layer 4, defense in depth)
- 11 envelope prompts (`dream-*`, `extract-*`, `assess-ollama`,
  `deep-extract-*`, `session-mine-*`, `session-extract-*`,
  `absorb-pass2-json`) gained an explicit "NEVER target a file whose
  basename starts with `_`; use `create` for new, `append` only for a
  file you were shown exists" rule. Layer 1 is the actual guarantee;
  this reduces wasted generations (rejected actions still cost tokens)
  and improves local-model output.

### Tests
- `wiki_actions_test.go`: `_`-prefix rejection table (incl. subdir +
  nested cases, and underscore-mid-name still accepted); append→create
  promotion; existing-append unchanged; `_`-prefixed append still
  rejected (Layer 1 wins over Layer 2). The pre-existing
  `TestApplyWikiActions_AppendMissingFails` was rewritten to assert
  the new promote-not-fail contract.
- `absorb_log_test.go`: trailing-garbage salvage + self-heal
  round-trip, total-garbage fail-open, clean-file regression guard.

## [0.2.17] — 2026-05-15

Cross-agent KB handshake. `scribe init` now writes its KB block to **`~/.codex/AGENTS.md`** as well as `~/.claude/CLAUDE.md`, so OpenAI Codex CLI sessions get the same loop Claude Code sessions get: query the KB before decisions, write drop files for reusable knowledge. Pairs with the Codex *project discovery* shipped in 0.2.15 — discovery finds the projects, the handshake makes Codex sessions populate them. The drop-file half was always agent-agnostic (`scribe sync` scans `.claude/<kb>/` by path regardless of which agent wrote the file); this closes the instruction-injection half.

### `scribe init` — Codex AGENTS.md block
- New embedded `templates/codex-agents-md.md` — ~95% identical to `claude-md-kb.md`, three deliberate deltas: (1) leads with `qmd query`/`qmd get` via shell instead of the `mcp__plugin_qmd_qmd__*` MCP tool (Codex configures MCP differently in `~/.codex/config.toml`; shell qmd is always available), (2) an explicit note that `.claude/<kb>/` is the *shared* drop location both agents use, not Claude-specific, (3) opening paragraph mentions Codex project discovery.
- `installClaudeMD` generalised to `installAgentMD(path, tmpl, vars, check, yes)`; `installClaudeMD` + new `installCodexMD` are thin wrappers. Behaviour byte-identical — same four cases (missing / present-without-markers / in-sync / drifted), user content outside the markers never touched. The shared `<!-- scribe:begin … -->` markers are HTML comments, valid in any markdown file, so they work unchanged in AGENTS.md.
- New `--no-codex-md` flag mirroring `--no-claude-md`. The 2026-05-13 throwaway-path guard covers both handshakes — `scribe init -p /tmp/...` writes neither global file.
- Status-mode init collects template vars once and reuses them for both handshakes (was double-prompting in interactive mode).

### Doctor — `~/.codex/AGENTS.md block` row
- New row mirroring `~/.claude/CLAUDE.md block`: OK when the scribe markers are present, WARN when the file or block is missing. Never FAIL — Codex is optional. AGENTS.md is a softer contract than `~/.claude/CLAUDE.md` (Codex churned `codex.md` → `instructions.md` → `AGENTS.md`, and Desktop/managed installs may manage their own), so the row reports *presence of the scribe block*, not "Codex is reading it" — the latter isn't probeable.

### Tests
- `TestInstallCodexMD_Lifecycle` exercises create / in-sync no-op / drift-refresh / user-content-preserved against a fake `$HOME`. `TestInstallCodexMD_CheckModeNeverWrites` locks `--check` read-only. The existing throwaway-path and `--bind` regression guards extended to assert Codex AGENTS.md behaviour too.

### Out of scope (deliberate)
- Registering qmd as a Codex MCP server (auto-editing `~/.codex/config.toml`) — the shell `qmd query` fallback works everywhere; bigger blast radius, separate plan if wanted.
- Codex SessionStart hooks (`~/.codex/hooks.json`) as a dynamic alternative to a static AGENTS.md block — cleaner long-term, but H1 ships the static block first because it matches the proven Claude path exactly.
- Project-level `AGENTS.md` injection — only the global `~/.codex/AGENTS.md` is scribe-managed.

Plan: `docs/codex-handshake-plan.md`.

## [0.2.16] — 2026-05-14

Two follow-up fixes to the 100%-Ollama landing in 0.2.14. Closes the last `claude -p` callsite that fired during a normal `scribe sync` run, and stops the auto-flip log lines from repeating on every `loadConfig`.

### Fix — project extraction was still hitting Anthropic

- `extractProject` (sync.go) was the last `runClaude` callsite in the normal sync flow. Even with `llm.provider: ollama` set in `scribe.yaml`, sync silently billed Anthropic for every project's extraction step — `ollama ps` stayed empty during sync runs because none of the actual LLM work was hitting the local server.
- New `ExtractConfig` (`extract:` block in scribe.yaml) routes the per-project extraction step the same way dream/assess/deep do. Fields: `provider`, `model`, `ollama_url`, `mode` (tools | envelope), `max_file_chars`, `max_total_chars`, `timeout_min`, `num_ctx`. Defaults inherit from `llm:`.
- Auto-flip: non-anthropic provider forces `mode: envelope` (legacy tools path requires `claude -p`).
- New `cmd/scribe/extract_envelope.go` mirrors `deep_orchestrator.go` at project granularity. Go gathers the context (drops → KB `CLAUDE.md` → project `CLAUDE.md`/`README` → changed files → doc dirs), inlines it into a bounded prompt under `MaxTotalChars` (default 32 K), the model emits one `EnvelopeV2`, scribe applies through `applyWikiActions`.
- New `prompts/extract-anthropic.md` (preserves the legacy tool-mode prompt) and `prompts/extract-ollama.md` (envelope-mode JSON-out prompt) — picked by `promptForProvider("extract", …)`.
- Verified end-to-end against scriptorium: `xuku-enaia-main` (0 changed files) and `phd_knowledge-temp` extracted via gemma3:12b @ num_ctx=16384, ~1 minute per project, 3-4 envelope actions applied each, zero Anthropic calls.

### Fix — auto-flip log lines flooded sync output

- The "<op>.provider forces mode=…" log lines fired on every `loadConfig` call. A single `scribe sync` calls `loadConfig` 5+ times across its subcommand entry points, so the same 5 lines printed 25-30 times per run; subprocess scribes (lint, scan, index) added another batch per spawn.
- New `logAutoFlipOnce(key, …)` (config.go) prints each (op, provider) auto-flip exactly once per process. Mutex-guarded map keyed by `<op>:<provider>` so a config that flips multiple ops still surfaces every meaningful state change.
- New `SCRIBE_QUIET_CONFIG=1` env var suppresses every auto-flip log call. `runCmdErr` automatically sets it when the spawned binary's basename is `scribe`, so child processes inherit silence without affecting unrelated tools (`qmd`, `git`).
- Net effect on scriptorium's `scribe sync` output: the 5 auto-flip lines that previously printed 25-30 times per run now print exactly once at startup. Subprocess output (lint warnings, sections-build stats) is unaffected.

### Tests
- 5 new tests in `ollama_followup_test.go`: `extract` joins the inheritance + auto-flip + num_ctx-fallback test matrix that already covered dream/assess/deep/session-mine/relations; `TestLogAutoFlipOnce_DedupesPerKey` locks the dedup contract.

## [0.2.15] — 2026-05-14

Codex CLI project discovery. `scribe sync --discover` now walks `~/.codex/sessions/` alongside `~/.claude/projects/`, picking up projects you've only ever touched from Codex. Discovery only — Codex session *mining* is a separate, larger track (deferred behind the 100%-Ollama session-mine envelope).

### Codex sessions are first-class for discovery
- New `codex_sessions_dir: ~/.codex/sessions` config key (default points at the standard Codex CLI rollout root). The scribe.yaml template, `init` summary, and `applyDefaults` home-expansion all pick it up.
- New `cmd/scribe/codex.go`: `readCodexSessionMeta` parses just the first JSONL line of a rollout (the `session_meta` event) to extract the verbatim `cwd` — no `decodeClaudePath`-style ambiguous-dash rebuild needed. 1 MB scanner buffer ceiling defends against future growth of `base_instructions.text`.
- `walkCodexSessions` enumerates rollouts in descending YYYY/MM/DD partition order and yields each unique `cwd` once. Most-recent rollout wins on cwd disagreements (mid-project renames).
- `SyncCmd.discoverCodex` mirrors the existing Claude branch in `discover` — same `manifest.isIgnored`, `hasSignificantContent`, `manifest.resolveDomain`, and `ensureRepoYAML` flow. Codex failures log and continue; never block the Claude pass.

### Manifest schema — `discovered_from`
- New `discovered_from` field on `ProjectEntry` (`"claude" | "codex" | "both"`). Back-compat: empty reads as `"claude"` since every pre-existing entry came in through the Claude scanner.
- Idempotent `MergeDiscoveredFrom` promotes the field to `"both"` when a project is seen from a second source, so cross-agent projects stay one manifest entry.

### Doctor — `codex_sessions_dir` row
- New row alongside `claude_projects_dir`. OK when the dir exists and a recent rollout still parses with a non-empty `cwd` (schema-drift sentinel — a future Codex rename of the field shows up as a WARN here instead of silently breaking discovery). WARN on missing dir, zero rollouts, or empty config. Never FAIL — Codex is optional.
- Rollout count is shown in the OK detail (capped at 5000+).

### Tests
- 13 new tests in `codex_test.go`: `readCodexSessionMeta` happy/empty/malformed/wrong-type, fixture-pin against the on-disk shape, `walkCodexSessions` dedup + descending-order race, `discoverCodex` adds-new + Claude→both promotion, `DiscoveredSource` back-compat + `MergeDiscoveredFrom` idempotency, `codexRolloutCount` limit honouring, `codexProbeRollout` recency.
- `testdata/codex/rollout-fixture.jsonl` locks the parser to today's session_meta layout.

### Out of scope
- Codex session *mining* (ccrider-equivalent FTS5 indexer + sync wiring) — deferred behind the 100%-Ollama session-mine envelope so we don't reinvent the schema. Plan in `docs/codex-discovery-plan.md` Phase C3.
- Codex `memories/`, `rules/`, `skills/` cross-agent sharing — different problem.
- Reading Codex's SQLite (`state_5.sqlite`, `logs_1.sqlite`, `session_index.jsonl`). Desktop-private; walking rollouts directly is the robust contract.

## [0.2.14] — 2026-05-14

100% Ollama. Every LLM-driven subcommand — `dream`, `assess`, `deep`, `session-mine`, `relations migrate`, and the four absorb passes that weren't already local — now runs end-to-end against a local Ollama server with zero `claude -p` calls. The Anthropic path stays the default; flipping the whole pipeline to free/offline is a single `llm.provider: ollama` line in `scribe.yaml`.

### `llm:` top-level routing block
- New `llm:` block in `scribe.yaml` sets provider/model/ollama_url/num_ctx defaults that every per-op section inherits when its own provider/model is empty. The pre-existing per-op blocks (`absorb.pass2_provider`, `absorb.facts_provider`, `contextualize.provider`) still win when set.
- Six per-op default-resolvers (`applyDreamDefaults`, `applyAssessDefaults`, `applyDeepIngestDefaults`, `applySessionMineDefaults`, `applyRelationsDefaults`, `applyAbsorbDefaultsWithLLM`) inherit from `LLMConfig` in a consistent order: per-op value → `llm.*` → built-in default.
- Auto-flip: a non-anthropic top-level provider forces the right *mode* on every op that has one. `dream.mode → orchestrator`, `assess.mode → envelope`, `deep_ingest.mode → envelope`, `session_mine.mode → envelope`, `absorb.pass2_mode → json`. The legacy `claude -p` paths can't drive Ollama; the auto-flip prevents silent no-ops and logs the override.
- Claude alias under `provider: ollama` (e.g. forgetting to update `model: sonnet`) gets coerced to `gemma3:4b` with a log line via `coerceProviderModel`, so a half-finished migration never 404s the local server.

### Phase 4C/4D/4E orchestrators
- `dream` orchestrator (Phase 4D): Go walks `dreams/`, builds the orient packet (recent log + inventory + stale list + contradictions), inlines it into a single bounded prompt, parses one `EnvelopeV2` JSON document back. The legacy hour-long monolithic `claude -p` path remains available via `dream.mode: monolithic` for users who want it.
- `assess` orchestrator (Phase 4E): Go walks the source tree (200 entry cap), inlines top-level docs (12K char cap) + recent git log, asks the model for one envelope. The new `errAssessWalkDone` sentinel replaces `filepath.SkipDir` from a file callback so partial walks return what they gathered instead of empty.
- `deep` orchestrator (Phase 4E): per-directory envelope subtask. Each directory's .md files are concatenated (24K char cap, head/tail trim) and the model emits one envelope per directory. `runDeepExtractEnvelope` accumulates `envelope_actions_applied` / `envelope_actions_errored` into `runStats` for the per-run JSONL row.
- `session-mine` envelope path (Phase 4C): the ccrider FTS5 path now reads transcripts in Go (no MCP), inlines them into a bounded prompt, parses one envelope. Two-prompt-per-task wrapper picks `session-mine-anthropic.md` vs `session-mine-ollama.md` based on resolved provider. Once-only fallback warning logs which prompt was missing so users notice mis-named variants.

### `WikiActionEnvelope` V2 — `MetaAction` (Phase 4C)
- New `MetaAction` ops cover the side-channel writes that don't fit inside the wiki-dirs sandbox: `log_append` (top-level `log.md`), `sessions_log_append` (`sessions/<YYYY>/<MM>/<DD>.md`), `rolling_memory_append` (`<domain>/<stem>.md` under a closed-stem allowlist).
- `meta.rolling_targets` in `scribe.yaml` lets each KB pin its own stems. Empty defaults to `[learnings, decisions-log]` (the pre-4C set). Path-traversal validation drops anything containing slashes or `..` with a log line.
- `parseEnvelopeV2` is the new entry point — accepts both V1 and V2 envelopes, logs a hint when a V1 envelope carries `meta` (forgot to set `version: 2`), warns on unknown future versions.

### `num_ctx` plumbing
- Every envelope op tags its `context.Context` with `withOllamaNumCtx(ctx, …)` before calling the LLM provider. Ollama reads the value from the request body; Anthropic ignores it.
- Per-op `num_ctx` defaults: dream 16384, assess 32768, deep 16384, session-mine 16384, absorb.pass2 16384, absorb.single_pass 16384. Each falls through `LLMConfig.NumCtx` → built-in floor when unset.
- Absorb pass-2 + single-pass got new `pass2_num_ctx` / `single_pass_num_ctx` fields. The pre-4C pass-2 path loaded Ollama with the default 8192 even on a 27B model, silently truncating the tail of dense articles.

### HTTP client timeout
- Removed the 10-minute `http.Client.Timeout` cap from the Ollama provider. Every caller already wraps the request in a `context.WithTimeout` carrying the per-op `TimeoutMin` (dream 20m, session-mine 8m, deep 10m, assess 10m, absorb.pass2 25m). With `Stream: false` on `/api/generate`, the client cap had to cover full generation — a 12B model cold-loading + a sizable orient packet routinely blew past 10 min and surfaced as the misleading "Client.Timeout exceeded while awaiting headers". Context timeout is now the single source of truth; users tune via `TimeoutMin` per op.

### Per-task prompt variants
- Each task that needs different framing on local vs Anthropic ships two prompts: `<task>-anthropic.md` + `<task>-ollama.md`. `promptForProvider(task, provider)` picks the right one; a missing ollama variant logs a one-time fallback warning (mutex-guarded) and uses the anthropic prompt. New prompt pairs: `absorb`, `absorb-pass1`, `absorb-pass1-chapter`, `assess`, `deep-extract`, `dream`, `session-extract`, `session-mine`.

### Observability
- Every orchestrator populates `runStats` (mode, project, envelope_actions_applied, envelope_actions_errored) which `writeRunRecord` flushes into `output/runs/<YYYY-MM-DD>.jsonl`. `scribe doctor --section freshness` reads those rows.
- `dream` log line now shows the resolved provider/model/mode (`model: ollama/gemma3:12b, mode: orchestrator`) instead of always printing the CLI flag default ("sonnet"), which was misleading on orchestrator-mode runs.

### Doctor — `localmode` resolves inheritance
- `scribe doctor --section localmode` now reads the *effective* `absorb.pass2` provider/model after `LLMConfig` inheritance, not the raw `absorb.pass2_provider` field. A KB that pinned `llm.provider: ollama` and left `absorb.pass2_provider:` empty used to look misconfigured to doctor; it now reports the correctly inherited value and warns only when the resolved provider isn't ollama-on-an-actually-pulled-model.
- New check: `llm_model_pulled` — warns when `llm.model` is set but not pulled locally. Fix line: `ollama pull <model>`.

### Tests
- 51 new tests in `ollama_followup_test.go` covering the inheritance chain (`LLMConfig.Model` reaches dream/assess/deep/session-mine/relations), per-op model wins, `Pass2NumCtx`/`SinglePassNumCtx` fallback chain, prompt-pair resolution, envelope V1↔V2 compatibility, MetaAction config-driven allowlist, transcript role mapping for `tool`/`tool_use`/`tool_result`, and `coerceProviderModel` alias swaps.
- `wiki_actions_meta_test.go` covers the three MetaAction ops + the rolling-targets allowlist boundary.

### Verified end-to-end against scriptorium
- 3445-article KB, dream cycle completed in ~70 seconds on gemma3:12b at `num_ctx=16384`. 3 envelope actions applied, dream committed + pushed cleanly. Same KB, `absorb.pass2` continues to run gemma3:27b at the bumped context window; `ollama ps` now correctly shows `CONTEXT 16384` instead of the silently-truncating 8192.

## [0.2.13] — 2026-05-14

### Fix — `scribe init -p` no longer foot-guns global state on temp paths
- `scribe init -p /tmp/...` (and any path under `/tmp`, `/private/tmp`, `/var/folders`, or `$TMPDIR`) used to silently retarget `~/.claude/CLAUDE.md`'s scribe-managed block and rewrite `~/.config/scribe/config.yaml`'s `kb_dir` when invoked with `--yes`. That's wrong for what is almost always a smoke test, and on 2026-05-13 it triggered a cascade: a stray `scribe init -p /tmp/freshkb --yes` repointed the user config; `scribe watch` noticed the kb_dir change and ran a 76-minute `scribe sync --max 2` against the throwaway KB overnight, billing anthropic tokens against a directory that had nothing to absorb.
- New `isThrowawayPath` check refuses to retarget global state when the bootstrap target is under one of those temp prefixes. The KB scaffold (`scribe.yaml`, `wiki/`, `.gitignore`, etc.) is still written; only the two destructive global writes are skipped.
- New `--bind` flag is the explicit opt-in for the rare case where a temp-pathed KB really is meant to be primary. Without `--bind`, throwaway paths print a clear `Re-run with --bind if you really want this to be your primary KB.` notice and stop.
- New `--no-claude-md` flag suppresses the `~/.claude/CLAUDE.md` block refresh regardless of mode (works in both bootstrap and status paths). Useful when you want to scaffold a real KB but defer the global-context wiring.
- Normal (non-temp) paths still bind globals as before when `--yes`/`--force`/`--bind` is set. No behavior change for `scribe init -p /Users/me/Projects/my-kb --yes`.

## [0.2.12] — 2026-05-14

Local-mode production readiness. The Phase 4B + followup work shipped in 0.2.11 made the absorb pipeline runnable end-to-end against local Ollama models; this release closes the operational gaps so the heavy crons (`com.scribe.sync-projects`, `com.scribe.sync-sessions`) can resume running safely against the new stack.

### Doctor — `localmode` section
- New `scribe doctor --section localmode` validates the absorb pipeline's local-provider knobs against the runtime environment so misconfigurations surface *before* a 20-min sync wastes wallclock.
- Four checks: ollama daemon reachable at `absorb.contextualize.ollama_url`; `absorb.pass2_model` actually pulled locally (parses `/api/tags`); `absorb.atomic_facts` is on when `pass2_provider=ollama` (model fabricates `[cN-fM]` citations without ground-truth fact IDs to cite); `sync.daily_anthropic_output_token_ceiling` is configured (recommended ≥2_000_000 after the 2026-05-11 runaway).
- A single 2s `/api/tags` probe is shared across the checks that need the model list. `SCRIBE_DOCTOR_SKIP_OLLAMA=1` skips the network call for offline CI.
- Doctor enumerates the section in `--section` help and runs by default.

### Config — `SCRIBE_PASS2_{MODE,PROVIDER,MODEL}` env overrides
- Three new env vars let one-shot scripts flip pass-2 routing without mutating `scribe.yaml`. Empty values are no-ops. The existing auto-flip-to-json-when-provider-is-not-anthropic logic still wins, so a misconfigured `SCRIBE_PASS2_MODE=tools` combined with `SCRIBE_PASS2_PROVIDER=ollama` still engages json mode with a log line.
- Use case: `scripts/absorb-compare.sh` now switches modes per run via env var instead of `sed`-editing the yaml + a `scribe.yaml.bak` restore-on-trap. One source of truth, no half-rolled-back state on crash.

### scribe.yaml scaffold — surface every local-mode knob
- `scribe init` template (`templates/scribe.yaml`) and `absorbDefaultYAMLBlock()` now emit commented hints for every knob that landed in 0.2.11: `pass2_parallel`, `pass2_mode`, `pass2_provider`, `atomic_facts`, `facts_model`, `facts_timeout_min`, `facts_provider`, and the new `sync.daily_anthropic_output_token_ceiling`. Users editing scribe.yaml by hand can now discover the full local-mode surface without reading source.
- `pass1_timeout_min` template value bumped 3 → 5 to match the in-code default (the bump landed in 0.2.11's `absorbDefaults()` after dense long-form articles SIGKILLed on haiku at 3 min).

### Lint
- `// #nosec G101` on `budgetBypassEnv = "SCRIBE_BYPASS_BUDGET"` — gosec G101 flagged the env-var-name literal as a "potential hardcoded credential" because the string contains `BUDGET`. It is not a credential; the comment documents the rationale and keeps `make ci` green.

### Verified empirically against scriptorium
- Fact-ID stripper: 0 fabricated `[cNN-fM]` brackets survived a real `gemma3:27b` + `atomic_facts` absorb run (10 entities, 63 real facts, 42 distinct cited IDs). The defense-in-depth path fired once mid-run on entity "AuthoredUp" and stripped 2 brackets cleanly.
- Budget ceiling: with `daily_anthropic_output_token_ceiling: 1000` and today's anthropic output already at 19,499 tokens, sync aborted on the first claude-extraction call with `daily anthropic output-token ceiling reached: used 19499 / limit 1000` and exited 0 (cron-safe). `SCRIBE_BYPASS_BUDGET=1` bypasses cleanly. Production value set to 2_000_000 in scriptorium before re-enabling heavy crons.

## [0.2.11] — 2026-05-11

### Lint — relax per-type relation allowlist
- `specializes` and `instance_of` are now permitted on `decision`, `solution`, `research`, `tool`, `project`, `idea` (previously restricted, in some cases empty). Both kinds are universally meaningful: a research paper can specialize another research paper; a tool can be an instance of a pattern; a decision can specialize a broader decision.
- Motivation: the Phase 6A v2 LLM relation classifier produced semantically valid `specializes`/`instance_of` edges across these types, which the old per-type schema then flagged as errors. Real-world classifications matched the relaxed schema.
- `pattern` and `person` allowlists unchanged.
- No data migration required — existing typed edges in scriptorium frontmatter immediately validate clean.

## [0.2.10] — 2026-05-07

### Phase 6A v2 — LLM relation classifier
- New `scribe relations migrate` walks every wiki article with a non-empty `related:` list, batches the wikilinks per source, and asks the LLM to classify each into the closed set of typed kinds (`supersedes`, `applies_to`, etc.). High-confidence classifications move from `related:` to the typed field; everything else stays in `related:` as before.
- Closed-set guarantees: post-parse validation rejects any kind not allowed for the source's `type:` (e.g. `supersedes` on a research article never writes). `--min-confidence` (default `medium`) skips low-confidence verdicts. `--no-reverse` opts out of auto-injecting the inverse edge on the target.
- `--dry-run` previews without writing. `--assisted` prompts for `[Y/n]` per edge. `--limit N` caps articles per run for cost control. `--model` defaults to `haiku` (cheapest path that still produces well-formed JSON in our tests).
- Per-article opt-out: `relations_locked: true` in frontmatter skips the article entirely. Hand-curated KBs can pin specific articles before running.
- Audit trail: every change writes `wiki/_relations_migration_<ts>.jsonl` (committed). `scribe relations migrate-revert <log>` replays the file in reverse to undo a run — including the auto-reverse edges. Sidecar cache lives at `wiki/_relations_classifier/<source>.json` (gitignored) so re-runs short-circuit already-classified edges.
- Reasoning rationale stored per-edge: model, confidence, and a one-line "why" survive in both the migration log and the classifier sidecar.
- 11 unit tests covering candidate collection, dry-run no-op, threshold enforcement, kind rejection on type mismatch, null-kind preservation, log round-trip, and full revert.

## [0.2.9] — 2026-05-07

### Phase 7B — Declarative views (v1)
- New `wiki/_views/<name>.scribe-view.yaml` files declare reusable KB slices: filter expression + sort spec + column projection. `scribe view <name>` evaluates the file and prints the result.
- Schema (subset of Obsidian Bases semantics, scribe-flavored): `filters` (and/or/not tree with leaf clauses `{field, op, value}`), `sort` (per-field asc/desc), `view.columns` (positional), `view.limit`. Closed-set ops: `eq`, `ne`, `lt`, `le`, `gt`, `ge`, `in`, `has`, `contains`, `exists`, `missing`. Mixed container/leaf shapes and unknown ops fail at parse time.
- `scribe view --list` enumerates registered views with their `description:`. `scribe view <name> --show` dumps the parsed schema (no evaluation). `scribe view <name> --json | --csv` switches output format from the default Markdown table.
- Frontmatter-only filters by design — no body content scan, no joins, no aggregates. Adding ops is a one-line switch arm; new shapes belong in v2.
- Three reference views shipped in `scriptorium/wiki/_views/`: `active-decisions-enaia`, `stale-research`, `orphan-patterns`.
- `--no-extract` shortcut not needed — sync's `--max=0` already does it.

### Sync — `--max-absorb` flag
- `scribe sync --max-absorb N` overrides `absorb.max_per_run` from scribe.yaml for a single run. Useful for one-shot backlog drains; default 0 keeps the config-driven behavior. `--dry-run --estimate` honors the override too.

## [0.2.8] — 2026-05-07

### Phase 6C — Staleness ledger (v1: date + source signals)
- New `wiki/_staleness.jsonl` ledger captures one entry per article that fires at least one signal. Two signals in v1: `date` (article's `updated:` is older than its type's half-life) and `source` (opt-in HEAD probe of `source_url:` returned 4xx/5xx or a network error).
- Type half-lives (defaults, scribe.yaml override deferred): decision 180d, pattern 365d, solution 365d, research 90d, tool 365d, idea 90d, project 60d, anything else 365d. Articles with `status: superseded` (research or decision) are never date-stale.
- `scribe stale build [--check-urls] [--max-urls N]` rebuilds the ledger. URL probes are off by default (network), capped at 100/run, parallel-bounded to 8. Idempotent — preserves `first_observed_at`. Removes the file when no entries remain.
- `scribe stale list [--signal date|source] [--type ...]` prints triaged candidates. `scribe stale show <id-or-path>` prints the JSON entry.
- `scribe doctor --section stale` summarises one warn line per active signal kind.
- Ledger is a derived artifact — gitignore template + scriptorium .gitignore already cover it under `output/` patterns; explicit `wiki/_staleness.jsonl` ignore added to template.
- Reference staleness (citing `[[X]]` where X has been superseded) ships in v2 once Phase 6A v2 LLM migrator populates typed `supersedes:` chains across the corpus.
- **Known limitation**: scribe's own `lint --fix` and `tier write` bump `updated:` on every touch, so a freshly-backfilled KB will show 0 date-stale articles even when content is genuinely old. v2 will baseline against git-derived "last content change" instead. The signal becomes useful again as soon as the KB stops being mass-rewritten.

### Doctor — `vault` section
- New `scribe doctor --section vault` flags stray vault-tool scaffolding directories (`logseq/`, `pages/`) in a non-Logseq KB. Surfaces file count + directory size with a one-line `rm -rf … && echo … >> .gitignore` remediation.
- Catches the case where Logseq ran once and left a thousand-file `bak/` autosave tree behind, which silently bloats the Obsidian graph and pads commit diffs.

## [0.2.7] — 2026-05-07

### Phase 6B — Contradiction ledger (v1: derived from typed edges)
- New `wiki/_contradictions.jsonl` ledger built from typed `contradicts:` edges. Each entry pairs two articles canonically (sorted), assigns a stable FNV-1a pair ID, and tracks `first_observed_at` / `last_seen_at` / `resolved_at` / `resolution_note`. Symmetric A↔B edges collapse to one entry; one-directional edges still produce one entry with a single source.
- `scribe contradictions build` rebuilds the ledger from disk. Idempotent — preserves `first_observed_at` across rebuilds, drops entries whose underlying edges are gone (deletes the file when empty). Wired into `scribe sync` next to `sections build`.
- `scribe contradictions list` shows pair / resolved-state / first-observed / sources. `scribe contradictions show <id>` prints one entry. `scribe contradictions resolve <id> <note>` marks an entry resolved while keeping it on file as a paper trail.
- `scribe doctor --section contradictions` warns once per unresolved pair. The intent is gentle nagging, not blocking commits.
- Ledger is a derived artifact — added to gitignore template + scriptorium .gitignore.

LLM pass-2 contradiction discovery (auto-detected, not just declared) ships in 6B v2.

### Phase 6A — Typed relations (v1: schema + manual surface)
- Frontmatter gains 10 typed-edge keys: `supersedes, superseded_by, contradicts, applies_to, derived_from, instance_of, specializes, extends, cited_by, informs`. Each carries `[[Wikilink]]` payloads, same shape as `related:`. Untyped `related:` stays as the easy-out for genuinely loose connections.
- Closed set per article type validated by `scribe lint`: decision (supersedes/superseded_by/contradicts), solution (applies_to/derived_from), pattern (instance_of/specializes/applies_to), research (extends/cited_by/informs), tool (derived_from), idea (instance_of).
- `scribe relations get|set|rm <article>` — manual editing surface with idempotent set, list-shape preservation, and per-type kind validation.
- `scribe relations graph <article>` — prints typed neighborhood (outbound + inbound) for orientation before touching a heavily-referenced article.
- `scribe relations check [--fix]` — bidirectional integrity audit. Verifies that every supersedes has a superseded_by, every instance_of has a specializes, etc. `--fix` auto-injects the missing reverse on the target's frontmatter.
- New `relations_locked: true` frontmatter key reserved for the LLM migration step (Phase 6A v2): pre-commits which articles the migrator must skip.
- `resolveArticleArg` now also tries `wiki/<arg>` so commands work whether the user passes `decisions/foo.md` or `wiki/decisions/foo.md` (KBs sometimes carry both).

LLM-driven migration (`scribe relations migrate` and `--assisted` mode) ships in 6A v2.

## [0.2.6] — 2026-05-07

### Phase 7A — Skill bundle
- Embedded `scribe-kb` agent-skill bundle (7 files, ~30 KB): top-level `SKILL.md` + 6 reference docs (`FRONTMATTER`, `WIKILINKS`, `STRUCTURE`, `DROP_FILES`, `QUERY`, `COMPAT`). Follows the [agentskills.io specification](https://agentskills.io/specification) so any Claude Code / Codex CLI / OpenCode session can adopt it without per-vendor adaptation.
- `scribe skill install [--target <dir>] [--check] [--force]` writes the embedded tree to `<KB-root>/.claude/skills/scribe-kb/` by default. Idempotent (SHA-256 short-circuit on unchanged files). Hand edits flagged with `<!-- scribe-skill: hand-edited, do not overwrite -->` are preserved unless `--force`.
- `scribe skill install --check` reports drift between the embedded version and what's on disk. Non-zero exit on drift, suitable for pre-commit / CI.
- `scribe skill list` prints the bundle contents (path + size).
- Source of truth: `cmd/scribe/skills/scribe-kb/`. Update content there; embed picks it up on `make build`.

## [0.2.5] — 2026-05-07

### Phase 5A — Section sidecar
- New `wiki/_sections/<dir>/<slug>.json` parallel tree captures every wiki article's H1/H2/H3 structure (id, title, level, line range, byte range, token estimate). Anchor IDs follow Obsidian/Logseq `^slug` convention so wikilinks like `[[Article#^methods]]` work in either vault tool.
- `scribe sections build` recomputes every sidecar (regex pass, no LLM, ~1s for 1500 articles). Wired into `scribe sync` next to `backlinks`/`index`.
- `scribe sections list <article>` prints the section index. `scribe sections get <article> <id>` prints one section's body. Both accept either a file path or a frontmatter title.
- Sidecars are derived artifacts — added to gitignore template + scriptorium .gitignore.

### Phase 5B — Tiered index hint
- New `index_tier:` frontmatter field with closed set `stub | brief | standard | deep | reference`. Computed from body word count + section count + (for raw articles) `fetched_via`. `index_tier_override:` lets a human pin a value that survives recomputes.
- `scribe tier list [--tier X] [--missing]` shows tier per article with counts. `scribe tier compute <article>` prints the rationale (words, sections, computed value). `scribe tier set <article> <tier>` writes the override. `scribe tier write --missing-only|--all [-n]` backfills the computed tier into frontmatter.
- Lint warns (not errors) on missing tier so the field rolls out without a flag-day migration.
- `validate.go` rejects out-of-set values for `index_tier` and `index_tier_override`.

### Phase 7C — Defuddle fetcher tier
- New tier between trafilatura and jina in the cascade: `arxiv → fxtwitter → trafilatura → defuddle → jina`. Picks up JS-heavy modern sites where trafilatura returns empty. Optional dependency — silently skipped when `defuddle` isn't on PATH.
- `--fetcher defuddle` is now a valid forced choice.

## [0.2.4] — 2026-05-07

### Capture
- Cross-device iMessage now captured. The self-chat query dropped `is_from_me = 1` and added DISTINCT. A user signed into iPhone (handle = phone) and Mac (handle = Apple-ID email) sends from one device to the other; the Mac's chat.db records `is_from_me = 0` even though it's the user's own message → previously the link was silently dropped on the receiving device.

### Refetch
- `rewriteRawArticleBody` now updates `title:` when the existing value is the URL-derived slug stub-capture stamps in. Slug = no whitespace inside the quotes; any space marks a human-edited title and is preserved. Past behavior left the slug forever even after a successful trafilatura/jina fetch returned a real `<title>`.

## [0.2.3] — 2026-05-07

### Lint
- `idea` added to `validTypes`. `ideas/` is in `wikiDirs` but the type was rejected, so any idea-typed article failed lint by construction.
- `superseded` added to research's allowed status set, mirroring the same value already accepted for decision. A research note replaced by a follow-on plan has the same lifecycle shape as a superseded decision.

## [0.2.2] — 2026-05-07

### Dependencies
- `github.com/mattn/go-sqlite3` 1.14.42 → 1.14.44
- `github.com/fsnotify/fsnotify` 1.9.0 → 1.10.1
- `golang.org/x/net` 0.47.0 → 0.53.0 (indirect)
- `golang.org/x/sys` 0.38.0 → 0.43.0 (indirect)

## [0.2.1] — 2026-05-07

### Capture
- `capture.self_chat_handles` (list) replaces `self_chat_handle` (singular). iMessage creates a distinct chat per address you message yourself with — accounts using both phone + Apple-ID email lost half their links to the unconfigured chat. Legacy singular still honored; `SCRIBE_SELF_CHAT_ID` env override now accepts comma-separated values.
- macOS `handle` table fanout fix: each (id, service) pair gets its own ROWID. Capture now collects every ROWID per id and queries with `IN (?,?,...)` so messages joined via the iMessage-vs-SMS ROWID stop disappearing.

### Fetch
- arxiv-aware tier ahead of trafilatura/jina. Routes any `arxiv.org/{abs,pdf,html}/<id>` URL to the richest available source — `/html/<id>v1` first (full paper, ~1s), `/pdf/<id>` + marker fallback (universal, ~10–30s), jina last-resort. Frontmatter enriched with title/authors/published/categories from `export.arxiv.org/api/query`. Honors HTTP 429 Retry-After with one polite retry.

### Absorb
- Chapter-aware path now accepts the `headings` strategy, not just `toc`. Markdown articles with H1-H6 structure get chapter-paralleled pass-1 instead of falling through to a single 17K+ shot at haiku. Same `chapter_threshold: 3` gate applies.
- Rate-limit matcher scoped to stderr only. Scanning combined stdout+stderr produced massive false positives whenever the model's response or article content discussed rate-limiting as a topic (~10% of real-world articles). Genuine API rate-limit *responses* still surface structurally via the JSON envelope.
- `pass1_timeout_min` default bumped 3 → 5 minutes for the dense long-tail.
- 1.5s polite pacing between successful contextualize calls — bursty Haiku quotas were stranding the rest of the queue mid-run.

### Lint
- `Frontmatter.Stack` is now `any` so list-valued `stack: [Go, SQLite, ...]` frontmatter parses cleanly. Lint also surfaces the actual yaml.v3 error instead of the catch-all "missing or invalid YAML frontmatter".

### Observability
- New `output/errors/<date>.jsonl` ledger captures full stderr/stdout tails (~50 lines each) when `claude -p` fails on non-rate-limit paths. Terminal output stays terse; debugging gets the context.

## [0.2.0] — 2026-04-28

### Ingest pipeline
- Watched inbox + Go-native DOCX/EPUB conversion; `doctor convert` section.
- Phase 2A: lazy `uv` bootstrap, smart routing, image sidecar.
- Phase 2B: OCR confidence gate + inbox idempotency.
- Phase 2C: `TORCH_DEVICE` knob + MPS crash-retry.
- Phase 3A: TOC sidecar + 3-tier chunker (data layer).
- Auto-route PDF/HTML through tier 0 + tier 1 dispatcher.
- Surface marker stats in `scribe ingest` file frontmatter.

### Absorb pipeline
- Phase 3A.5: chapter-aware pass-1 with merge.
- Phase 3B: atomic-fact extraction + pass-1 grounding.
- Phase 3B.5: pass-2 verbatim citation enforcement.
- Phase 3C: content-aware absorb idempotency.
- Phase 4A: facts pass via `llmProviderGenerator` (local-model groundwork).
- Phase 4B groundwork: wiki action envelope.

### Observability
- Phase 3D: cost ledger + `scribe cost` subcommand.
- Phase 3D.5: `claude -p --output-format json` for token accounting.
- Track `contextualize` / `contradictions` calls; tag ops in ledger.
- Distinguish cancel-cascade from real errors.
- Line-scan stdout for JSON envelope (CMUX hooks compatibility).

### Status / KB ops
- KB-wide backlog (projects + sessions + drop files) in `scribe status`.

### Docs
- Paste-and-run install commands for `qmd`, `ccrider`, `claude` in README.
- Local-model support plan with Phase 4A/4B progress.

## [0.1.5] — 2026-04-24

- `goreleaser` brews block generates the published formula correctly.

## [0.1.4] — 2026-04-24

- Pull `ccrider` via brew; updated install hints.

## [0.1.3] — 2026-04-24

- Earlier release-pipeline fixes.

## [0.1.2] — 2026-04-22

- Initial published release iteration.

## [0.1.1] — 2026-04-21

- Early packaging fixes.

## [0.1.0] — 2026-04-21

- First tagged release.
