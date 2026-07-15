# scribe troubleshooting — symptom → section → fix

Start every diagnosis with `scribe doctor` (read-only). It runs eleven sections
in order and prints FAIL / WARN / OK per check, then the `status` scoreboard.
Below: what each section tells you and the command that clears the common WARNs.
Fix configuration and state — never fabricate KB content to make a check pass.

## Reading `scribe doctor`

Sections, in run order: **deps · config · localmode · convert · cron · state ·
freshness · errors · contradictions · stale · vault**. Exit code is non-zero
only on a hard FAIL; freshness drift and most WARNs are advisory. Run a single
section with `scribe doctor --section <name>` when you're chasing one thing.

## Common symptoms

### "Nothing is being extracted / KB feels empty or stale"
1. `scribe status` — look at `raw/articles` count, absorb/contextualize pending,
   and **last sync**. A stale/absent last-sync means the pipeline isn't running.
2. `scribe doctor --section cron` — if the LaunchAgents aren't installed, the
   daemon never runs. Fix: `scribe cron install`, then `scribe cron status`.
3. `scribe doctor --section freshness` — flags when the last run is older than
   expected (reads `output/runs/*.jsonl`).
4. If cron is healthy but backlog is high, run one pipeline pass by hand:
   `scribe sync` (respect the lock rule — one at a time, never backgrounded).
   Preview first with `scribe sync --estimate`.

### "Sessions aren't being mined"
- `scribe triage` (read-only) shows unprocessed sessions scored by density. If
  it's empty, ccrider hasn't indexed new sessions yet.
- `scribe sync --sessions` mines sessions only (skips the rest of the pipeline).
- Codex CLI mining only runs when `codex.mine` is enabled in `scribe.yaml`.

### "chat.db / iMessage capture fails" (macOS)
- `scribe doctor --section deps` shows the chat.db (FDA) check. Full Disk Access
  is dropped whenever the binary is **replaced** (`make install`, `brew
  upgrade`). Fix: `scribe fda` (interactive grant), then re-check with `doctor`.
- Confirm you're running the binary you think: `which scribe` + `scribe --version`.

### "ollama unreachable / local mode broken"
- `scribe doctor --section localmode` and the `status` footer show the
  contextualize provider + an ollama ping. If it's unreachable, start ollama and
  confirm the URL in `scribe.yaml` (`absorb.contextualize.ollama_url`).
- `scribe status` also prints "set provider=ollama for free local mode" when a
  hosted provider is configured — that's a tip, not an error.

### "qmd search returns nothing / index looks wrong"
- `scribe status` ends with the qmd collection line. "not indexed yet" → run
  `scribe sync` (which reindexes) or `qmd update` directly.
- `scribe doctor --section state` checks index/disk consistency (entries vs
  article count). A large mismatch means a reindex is needed.

### "PDF/DOCX/EPUB ingestion isn't working"
- `scribe doctor --section convert` reports whether the optional converters (uv
  + marker-pdf) are present. Fix: `scribe install-tools`.

### "lint shows errors or a wall of warnings"
- Frontmatter **errors** → `scribe lint --fix` (mechanical repairs). Re-run
  `scribe lint` to confirm they cleared.
- **Content-quality warnings** (bloated / thin / rolling-overgrown / self-named
  dir) have no auto-fix → hand off to the **scribe-kb-tidy** skill.
- `scribe doctor --section contradictions` / `--section stale` surface the
  ledgers; work them with `scribe contradictions resolve` / `scribe stale list`.

### "team KB: scribe refuses to run / config looks off"
- `scribe config diff` shows sensitive `scribe.yaml` keys changed since you last
  trusted them; `scribe config trust` approves the current values. `scribe config
  update` appends docs for options added since your file was scaffolded.

### "which KB am I even hitting?"
- Both `doctor` and `status` print the resolved KB root at the top. If it's the
  wrong one, scope explicitly with `-C <path>` or `SCRIBE_KB=<path>`. The
  machine-level registry is `scribe kb list`.

## When the daemon and you collide

`scribe sync` and `scribe dream` share a machine-wide lock with cron. If a
manual run hangs "waiting for lock," the cron job is probably mid-run — wait,
don't kill it. Never background these, and never start a second one.

## Escalation

If `doctor` is clean but behavior is still wrong, the internal plumbing
commands (`validate <file>`, `index`, `backlinks`, `orphans`) can be run
standalone to isolate the layer. Prefer the high-level command; drop to plumbing
only to debug.
