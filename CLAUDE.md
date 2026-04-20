# scribe — agent guide

`scribe` is a single-binary Go CLI that runs an LLM-written knowledge-base pipeline. It extracts reusable knowledge from git repos, mines Claude Code sessions via ccrider's FTS5 index, absorbs captured URLs, and reindexes the KB with `qmd`. Designed to run on cron against a private KB repo. `scribe init` scaffolds a fresh KB anywhere; the user picks the KB's display name (defaulting to the directory basename).

This file is the agent guide for hacking on scribe itself. End-user docs live in `README.md`.

---

## Repo layout

```
cmd/scribe/          Single Go main + every subcommand in one package
  main.go            Kong CLI root
  sync.go            `scribe sync` — discover → extract → absorb → reindex
  triage.go          FTS5 session scoring
  sessions.go        Session log debug/repair
  capture.go         iMessage chat.db reader + 3-tier URL fetcher
  dream.go           Weekly memory consolidation driver
  lint.go            Frontmatter + size + orphan checks
  doctor.go          Read-only health audit
  link.go            Orphan linker (See Also injection)
  cron.go            macOS LaunchAgent install/status/uninstall
  hook.go            SessionEnd hook: score + queue to pending-sessions.txt
  init.go            Bootstrap a new KB from embedded templates
  ingest.go          Drain inbox → raw/articles/
  fda.go             macOS Full Disk Access probe + interactive grant
  prompts/           Embedded LLM prompt templates
  templates/         Embedded KB scaffold (scribe.yaml, CLAUDE.md, dirs)

Formula/             Homebrew tap formula (brew install oliver-kriska/scribe/scribe)
scripts/             pre-commit hook shipped into user KBs
install.sh           curl-piped installer
Makefile             build/install/test with -tags sqlite_fts5
Justfile             dev shortcuts
.goreleaser.yml      release pipeline
```

One Go package under `cmd/scribe/`. No internal/ split yet — keep it that way until the package breaks 3000 LOC or a second binary is needed.

---

## Build

```sh
make build        # CGO_ENABLED=1, -tags sqlite_fts5
make install      # builds + drops binary in $HOME/.local/bin
make test         # go test ./... -tags sqlite_fts5
make check        # test + vet
```

**FTS5 is mandatory.** ccrider's `messages_fts` virtual table uses it, and `scribe triage` runs BM25 queries against it. `go-sqlite3` ships without FTS5 — the `sqlite_fts5` build tag is what flips it on. Never drop that tag from the Makefile.

**CGO is required** for go-sqlite3. Cross-compilation across OS/arch needs a C toolchain; GoReleaser handles this in the release workflow.

---

## Key external surfaces

| Input | Path | Notes |
|---|---|---|
| ccrider sessions DB | `~/.config/ccrider/sessions.db` | FTS5, read-only access |
| Claude Code session folders | `~/.claude/projects/*` | keyed by project cwd |
| iMessage chat DB | `~/Library/Messages/chat.db` | needs Full Disk Access |
| scribe user config | `~/.config/scribe/config.yaml` | global defaults |
| pending sessions queue | `~/.config/scribe/pending-sessions.txt` | drained by `sync --sessions` |
| LaunchAgents | `~/Library/LaunchAgents/com.scribe.*.plist` | installed by `cron install` |
| KB root | `$SCRIBE_KB` or `scribe.yaml` in cwd | every command resolves this first |

Everything under `$SCRIBE_KB` belongs to the user's private KB repo and is never committed from this codebase.

---

## Conventions

- **Kong for CLI.** Every subcommand is a struct with `Run(ctx *kong.Context) error`. Shared deps go on the root struct.
- **Embedded prompts and templates.** `go:embed` under `prompts/` and `templates/`. Never read LLM prompts from disk at runtime — that breaks single-binary distribution.
- **`claude -p` calls always pass `--no-session-persistence`.** Session-mining Claude invocations must not pollute the user's auto-memory. Search the codebase before adding a new `claude` call to make sure you follow this.
- **No LLM calls in the hot path of `triage`, `hook`, `doctor`.** These are budgeted at <1s, <2s, <1s respectively. Keep them deterministic.
- **Writes are checkpointed.** Session-mining commits after each extracted session so an interrupted run doesn't lose work. Don't regress this.
- **Run records** are appended to `$KB/output/runs/*.jsonl` by every invocation (see `writeRunRecord`). `scribe doctor --section freshness` reads them. Don't skip recording.
- **Errors are logged to stderr, summarized to stdout.** Cron captures stdout; keep it terse.

## Testing

- `*_test.go` sit next to the code. Table-driven where possible.
- Integration tests that need a KB build one in a `t.TempDir()` via the embedded templates. Don't assume a user KB exists.
- Tests that need ccrider's DB use a minimal fixture schema under `testdata/`. Never point at `~/.config/ccrider/`.
- `make test` must pass with no network access.

## Release

GoReleaser builds darwin/linux × amd64/arm64 via the workflow in `.github/`. Tag and push to trigger. The Homebrew formula in `Formula/` is updated by the same release.

---

## Reference KB: scriptorium

The maintainer's private KB is called `scriptorium`. It's the first and primary user of scribe and is used as the integration-test KB when local development needs a populated setup. When working on scribe, prefer abstractions that don't hardcode any specific KB name, domain, or owner context. Anything KB-specific belongs in the user's `scribe.yaml`, not in Go code.

Do not commit absolute user paths or private domain names into this repo.

## Not in scope here

- Wiki content, frontmatter schema, or writing standards — those live in the user's own KB (written by `scribe init` from embedded templates).
- Cron job schedules — documented in README.md and embedded in `cron.go`.
- `qmd` itself — separate tool; scribe only shells out to it for reindexing.
