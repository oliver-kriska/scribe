# Changelog

All notable changes to scribe are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning follows [SemVer](https://semver.org/) (pre-1.0 — minor bumps may include breaking changes).

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
