# Changelog

All notable changes to scribe are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning follows [SemVer](https://semver.org/) (pre-1.0 — minor bumps may include breaking changes).

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
