# Scribe Auto-Ingestion Implementation Plan

Companion to `scriptorium/research/scribe-auto-ingestion-redesign.md` (architectural decisions). This document is the work breakdown.

## Goals

1. Drop any supported file (PDF, DOCX, PPTX, XLSX, EPUB, HTML, MD, TXT) into `raw/inbox/` and have it appear as wiki articles after the next `scribe sync`. Zero manual conversion steps.
2. `brew install scribe` ships a single Go binary; non-text formats lazy-bootstrap marker via uv on first need. Power users can pre-install via `scribe install-tools`.
3. `brew upgrade scribe` never touches the marker venv. Marker upgrades are explicit (`scribe install-tools --upgrade`).
4. Pipeline is resumable, observable, and idempotent. Failed conversions quarantine cleanly; restarts pick up where they left off.

## Non-goals

- Daemon mode / inotify file-watching (cron is enough).
- Embedded MCP server (qmd handles that).
- Web UI.
- Bundling Python in the Go binary.
- Replacing scriptorium's existing dream-cycle / lint / contextualize pipelines (this plan extends them, not replaces).

## Sequencing rationale

Phases are ordered so each one ships independently, has user-visible value, and is reversible. Phase 1 fixes the immediate pain (manual `marker_single` workflow). Phase 2 makes it production-shaped (auto-bootstrap, idempotency, image preservation). Phase 3 deepens absorb quality (long-PDF tree, atomic facts, dedupe). Phase 4 reaches into wiki structure (graph, contradiction). Phase 5 is polish.

If a phase blocks (license issue, API change, scope creep), the prior phase remains shippable. No phase requires the next.

---

## Phase 1 — Watched inbox + tier 0/1 conversion (target: 1 week)

**Deliverable:** `cp file.pdf ~/scriptorium/raw/inbox/` followed by `scribe sync` produces a wiki article. marker is required (manual install via existing `pip install marker-pdf` for now); auto-bootstrap arrives in Phase 2.

### Files

- `cmd/scribe/convert.go` (new) — pluggable converter dispatch keyed on file type. Tier 0 in-process; tier 1 shells to marker.
- `cmd/scribe/convert_tier0.go` (new) — pure-Go converters: `ledongthuc/pdf` for PDF, `JohannesKaufmann/html-to-markdown` for HTML, stdlib `archive/zip` + `encoding/xml` for DOCX/EPUB.
- `cmd/scribe/convert_marker.go` (new) — marker subprocess driver. Detects `marker_single`/`marker_server` on PATH. Spawns `marker_server` lazily during sync; POSTs files; shuts down on completion. Per-file timeout from config.
- `cmd/scribe/ingest.go` — extend with `IngestFileCmd` mirroring `IngestURLCmd`. Flags: `--absorb`, `--dry-run`, `--converter` (override).
- `cmd/scribe/sync.go` — add Phase 1.5 (`drainFileInbox`) before existing Phase 1.6 URL drain. Reads `raw/inbox/*`, routes to converter, writes converted markdown to `raw/articles/`, moves original to `raw/inbox/.processed/`.
- `cmd/scribe/doctor.go` — new checks: `marker_single` present? `marker_server` reachable? tier 0 libs available? Report per-format coverage matrix.
- `scribe.yaml` schema — add `ingest.inbox_path`, `ingest.converters` map, `ingest.marker.timeout_seconds`, `ingest.marker.server_port`.

### Dependencies added to `go.mod`

- `github.com/ledongthuc/pdf` (BSD-3)
- `github.com/JohannesKaufmann/html-to-markdown/v2` (MIT)

Both are pure Go, no CGO.

### Tests

- `convert_tier0_test.go` — golden-file diffs for one PDF, one DOCX, one EPUB, one HTML in `testdata/convert/`.
- `convert_marker_test.go` — uses a stub marker_server (httptest.Server returning canned markdown) to test driver behavior without requiring marker installed in CI.
- `ingest_file_test.go` — end-to-end flow: drop file in temp inbox, run sync, assert article in raw/articles/.
- `sync_inbox_test.go` — assert idempotency (running twice produces no duplicates) and quarantine path on converter error.

### Exit criteria

- `cp paper.pdf raw/inbox/ && scribe sync` produces a wiki article with content.
- `scribe ingest file paper.pdf --absorb` produces a wiki article synchronously.
- `scribe doctor` correctly reports green/yellow per format.
- Tests cover happy path, failed conversion (quarantine), and missing-marker fallback to tier 0.

### Rollback

Phase 1 is additive. Removing `convert.go` and the new sync phase reverts to current behavior. The new dependencies are pure Go and drop cleanly.

---

## Phase 2 — Lazy bootstrap + idempotency + image sidecar + OCR gate (target: 2 weeks)

**Deliverable:** First-time PDF user runs `brew install scribe` and `cp paper.pdf raw/inbox/` — scribe transparently fetches uv and installs marker, prints progress, then converts. Crashed batches resume.

### Files

- `cmd/scribe/install_tools.go` (new) — `scribe install-tools` subcommand. Wraps:
  - Fetch static uv binary to `~/.scribe/bin/uv` if absent. URL pinned by version constant; verify SHA256.
  - macOS: `xattr -d com.apple.quarantine ~/.scribe/bin/uv`.
  - Run `~/.scribe/bin/uv tool install marker-pdf` (or `--upgrade` flag for upgrades).
  - Persist `~/.scribe/install-state.json` with installed marker version + timestamp.
- `cmd/scribe/install_tools_lazy.go` (new) — internal call site. When converter dispatch hits a non-text format and marker is absent, prompt user (or auto-proceed if `ingest.auto_install: true` in scribe.yaml), then call install-tools logic.
- `cmd/scribe/convert.go` — extend with smart routing: file size + page count → choose tier 0 or marker. Defaults: PDF <500 KB and <5 pages → tier 0; everything else → marker.
- `cmd/scribe/convert_marker.go` — image sidecar handling. After marker writes output, scan for `![](...)` references to extracted images, copy them to `raw/assets/<slug>/`, rewrite paths in markdown.
- `cmd/scribe/sync.go` — `raw/inbox/.state.json` with per-file states `queued|converting|converted|absorbing|absorbed|failed`. Resume picks up at the last successful state.
- `cmd/scribe/sync.go` — quarantine path: failed conversions move to `raw/inbox/.failed/<slug>/` with `<slug>.err.log`. `scribe inbox retry` requeues.
- `cmd/scribe/convert_marker.go` — OCR confidence parsing. Marker emits per-page metadata in JSON output mode; aggregate to a frontmatter line in the converted markdown:
  ```yaml
  ocr_pages: [3, 7]
  ocr_quality: low
  ```
- `cmd/scribe/absorb.go` — pass `ocr_quality` through to absorb prompt; system prompt instructs to hedge claims from OCR-flagged pages.

### Tests

- `install_tools_test.go` — mock HTTP fetch of uv, assert SHA256 verification, assert xattr removal on macOS.
- `convert_smart_routing_test.go` — assert tier selection logic for each size/page bucket.
- `image_sidecar_test.go` — golden-file: PDF with figures → markdown + `raw/assets/<slug>/` populated, image links rewritten.
- `sync_resume_test.go` — kill mid-batch, restart, assert no duplicate work.

### Exit criteria

- Fresh machine: `brew install scribe`, drop a PDF, `scribe sync` succeeds with no manual Python steps.
- `scribe install-tools --upgrade` bumps marker without touching scribe.
- Killing scribe mid-sync and restarting produces the same end state as an uninterrupted run.
- Images from PDFs survive into the wiki.

### Rollback

If lazy-bootstrap is unreliable, ship Phase 2 with manual `scribe install-tools` only. Phase 1 still works without it.

---

## Phase 3 — Long-PDF tree + atomic facts + dedupe + cost ledger (target: 3 weeks)

**Deliverable:** A 200-page paper produces a structured wiki without anti-cramming violations. Cross-source duplication is detected at absorb time, not at dream-cycle cleanup time.

### Files

- `cmd/scribe/longdoc.go` (new) — PageIndex-style tree builder. Triggered when converted markdown ≥ 30k tokens or PDF ≥ 20 pages.
  - Parse h1/h2/h3 → tree of nodes with `{title, level, line_start, line_end, children}`.
  - For each leaf: Claude API call → 1-paragraph summary + atomic-fact list.
  - Roll up internal nodes.
  - Persist `raw/articles/<slug>.tree.json`.
- `cmd/scribe/absorb.go` — long-doc path consumes tree, expands leaves on demand. Existing density-aware two-pass continues to handle short docs.
- `cmd/scribe/facts.go` (new) — atomic-fact extraction stage between contextualize and wiki write. Prompt borrowed from Beever Atlas `FACT_EXTRACTOR_INSTRUCTION` (five fact types, 0–1 quality on specificity+actionability+verifiability, drop below 0.5). Output to `raw/articles/<slug>.facts.json` with schema `{memory_text, quality_score, topic_tags, entity_tags, fact_type, source_line}`.
- `cmd/scribe/dedupe.go` (new) — cross-source fact dedupe. Embedding similarity + LLM-judge for top-K candidates above threshold. Marks duplicates so absorb writes the canonical one and links the alternates.
- `cmd/scribe/costs.go` (new) — per-absorb token ledger written to `wiki/_costs.json`. Schema `{date, file, model, input_tokens, output_tokens, dollars}`.
- `cmd/scribe/stats.go` (new) — `scribe stats` command reads ledger + sync logs. Reports cost per format, per file size bucket, per domain.

### Tests

- `longdoc_test.go` — fixture: 50-page markdown with nested headings → tree built correctly; long-doc threshold respected.
- `facts_test.go` — fixture markdown → expected fact JSON via stub LLM.
- `dedupe_test.go` — two raw articles saying the same thing differently → one canonical fact, one duplicate marker.

### Exit criteria

- A real long PDF (test against an academic paper Oliver has) produces multiple wiki pages instead of one giant cramming violation.
- `scribe stats` shows cost breakdown over a recent sync window.
- Cross-source dedupe catches at least one duplicate in scriptorium's existing raw corpus on a re-absorb run.

### Rollback

The long-doc path is gated by token threshold; setting threshold to ∞ disables it without removing code. The atomic-fact stage and dedupe are skippable via config flags.

---

## Phase 4 — Entity graph + contradiction detector + coreference + cross-batch validator (target: 3–4 weeks)

**Deliverable:** scriptorium's graph structure becomes machine-readable. Dream cycle has explicit contradiction signal instead of inferring from related: lists.

### Files

- `cmd/scribe/entities.go` (new) — entity + relationship extraction. Prompt borrowed from Beever's `ENTITY_EXTRACTOR_INSTRUCTION`. Types: Person, Technology, Project, Team, Decision, Meeting, Artifact (extensible). Relationships in SCREAMING_SNAKE_CASE with `{source, target, confidence, valid_from, context ≤120 chars}`. **No-orphan rule enforced.**
- `wiki/_graph.json` (new persistent index) — nodes (entities) and edges (relationships). Rebuilt incrementally during sync.
- `cmd/scribe/contradictions.go` — replace existing logic with two-threshold model. LLM judge per fact pair returns `{existing_fact_id, confidence, reason}`. `contradiction.auto_supersede_threshold` (default 0.85) auto-marks superseded; `contradiction.flag_threshold` (default 0.6) queues for review in `wiki/_contradictions_queue.json`.
- `cmd/scribe/coreference.go` (new) — formalize the existing `identities.go` work as a discrete pre-absorb pipeline stage. Resolve aliases (Anna ↔ Anna Kriskova) before contradiction detection runs.
- `cmd/scribe/sync.go` — add cross-batch validator at end of run. Catches duplicates and contradictions introduced across files in one sync.
- `cmd/scribe/dream.go` — consume `_graph.json` and `_contradictions_queue.json`. Auto-applies high-confidence supersedes (Rule 12: stale articles get `Status: superseded by [[X]]` line, not deletion).

### Tests

- `entities_test.go` — extraction prompt produces no-orphan output on fixture; relationships have valid sources/targets.
- `contradictions_test.go` — synthetic contradictory facts trigger correct threshold behavior.
- `cross_batch_test.go` — two files in one sync introducing a contradiction get flagged at end of run.

### Exit criteria

- `wiki/_graph.json` exists and is consistent (every edge target is a node).
- A re-absorb of scriptorium's raw corpus surfaces at least one real contradiction.
- Dream cycle uses graph signal, observable in `log.md` entries.

### Rollback

`_graph.json` is a sibling index (like `_backlinks.json`). Removing it doesn't break wiki articles. Config flag `entities.enabled: false` skips the stage.

---

## Phase 5 — Polish + plugin converters (target: 1 week, can interleave with any phase above)

### Items

- **Plugin converter declaration** in `scribe.yaml`:
  ```yaml
  converters:
    pdf: marker         # or: tier0 | docling | mineru | "custom: my-tool {input} {output}"
    docx: marker
    pptx: marker
  ```
  Future-proof against marker license changes. The lazy-bootstrap stays specific to marker; other choices opt into manual install.
- **Detect existing marker install** before bootstrap. Check order: `marker_single` on PATH → user-pip → pipx → brew formula → fall back to scribe-managed uv.
- **`scribe inbox` subcommand** — `status` (list pending/failed with sizes/ETA), `retry` (requeue from `.failed/`), `clear` (move all to `.processed/` without conversion, escape hatch).
- **`scribe uninstall-tools`** — symmetric to install. Removes `~/.scribe/` (with confirmation prompt unless `--force`).
- **`scribe stats`** — already in Phase 3. Polish: per-period rollups, per-domain breakdowns.
- **Test fixtures with golden-file diffs** in `testdata/convert/` — committed reference PDFs/DOCX with expected output. CI runs full conversion and diffs. Catches marker version regressions.
- **macOS notification on completion** — opt-in via `scribe.yaml: notify.completed: true`. Uses `osascript -e 'display notification ...'`. Off by default.

### Rollback

All polish items are independently deletable. None affect data on disk.

---

## Risk register

| Risk | Likelihood | Mitigation |
|------|------------|------------|
| marker license change excludes Oliver's use | Low | Plugin converter system (Phase 5) lets users swap in MinerU or docling without code change |
| uv changes its CLI surface | Low | Pin uv version in `~/.scribe/install-state.json`; upgrade path is explicit |
| marker_server is "not robust" per upstream | Medium | Per-file timeout, automatic restart on failure, fall back to per-file `marker_single` if server unstable |
| ~3 GB weights download blocks first run | Medium | Print clear progress; offer `scribe install-tools` as upfront opt-in; suggest tier 0 fallback for plain PDFs |
| macOS Gatekeeper blocks downloaded uv | High on macOS 14+ | xattr removal in install path, documented in `scribe doctor` output |
| Marker output schema changes (image link format, OCR metadata) between versions | Medium | Pin a tested marker version range; CI fixtures catch breakage; `scribe doctor` reports incompatible versions |
| Phase 4 graph structure conflicts with existing scriptorium frontmatter `related:` lists | Low | Graph is additive (new index); existing `related:` keeps working unchanged |

---

## Out-of-band tasks (no phase, do anytime)

- README updates per phase ship.
- `scribe install-tools` integration into Homebrew formula's caveats text (so `brew install scribe` prints "run `scribe install-tools` to enable PDF/DOCX support").
- Update `scriptorium/solutions/pdf-book-ingestion-scriptorium.md` after Phase 1 ships, marking the manual workflow superseded.

## Sequencing summary

```
Phase 1 (week 1)   — manual marker, auto routing, watched inbox, idempotency basics
Phase 2 (weeks 2–3) — lazy uv-bootstrap, smart routing, image sidecar, OCR gate
Phase 3 (weeks 4–6) — long-PDF tree, atomic facts, dedupe, cost ledger
Phase 4 (weeks 7–10) — entity graph, contradiction detector, coreference, cross-batch
Phase 5 (any time) — plugin converters, polish, fixtures, notifications
```

Each phase stands alone; pause/resume between phases is supported.
