# Changelog

All notable changes to scribe are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning follows [SemVer](https://semver.org/) (pre-1.0 — minor bumps may include breaking changes).

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
