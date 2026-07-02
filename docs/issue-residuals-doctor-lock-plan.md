# Residual gaps: stop-word doctor visibility + per-KB lock scoping

Base commit audited: `49bfd53` (main, clean). Both gaps below were verified
directly against the current source (not taken on faith from the dispatch
message) — see the "Verified against code" line in each finding.

This supersedes the doctor/status scope of
`docs/issue-27-kb-scoped-doctor-status-plan.md` (that plan found issue #27
and the #26 cron guard already fully shipped on `main`). These two gaps are
the actual remaining small residuals from the backlog sweep
(`docs/issues-master-plan.md:20-25`, Phase 1 row).

---

## 1. Problem & context

### Gap 1 — held stop-word files are invisible to `scribe doctor`

**Verified against code:** `cmd/scribe/doctor.go:894-911` has a
`secrets-in-articles` check backed by `findSecretsInKB` (`cmd/scribe/secrets.go:444-485`).
`cmd/scribe/stopwords.go` (the issue #25 gate, commit `4c2522e`) has
`holdStopWordFiles` (`stopwords.go:241-...`) which unstages matching files at
commit time and logs one `"STOPWORD HELD: %s:%d [%s] — ..."` line
(`stopwords.go:270`) — but there is **no doctor-visible record** of that
hold. `grep -rn "findHeldStopWord\|findStopWord\|StopWord.*doctor" cmd/scribe/doctor.go`
returns nothing. A file held back three weeks ago is invisible to
`scribe doctor` forever after; the only trace was that one sync-log line,
which cron doesn't retain past its log-rotation window.

The commit gate (`stopWordRules`, `stopwords.go:68-86`) unions two sources:
the shared `scribe.yaml`'s `stop_words:` block (`StopWordsConfig`,
`config.go:137-142`) and the personal `~/.config/scribe/config.yaml`'s own
`stop_words:` (never committed, per-machine). `applyStopWords`
(`stopwords.go:170-225`) is the pure matcher: hold wins over mask, returns a
`stopWordDecision{hold, line, label, ...}`.

Team-lead's two-part ask — "(a) articles in the KB matching current
stop-word rules and (b) held-back dirty files in the worktree" — turns out to
be **one scan, not two**. `findSecretsInKB`'s proven strategy (git-tracked +
untracked-non-ignored + `raw/` walk, content read from disk) already covers
both: (b) is a tracked-but-locally-modified file, which `git ls-files
--cached` lists and the disk read picks up the held content for; (a) is any
markdown file (including cleanly committed ones) re-checked against
*today's* rules, which matters when a hold word is added to the config
*after* an article already committed cleanly. Mirroring `findSecretsInKB`
exactly gets both for free — see Decision D1.

### Gap 2 — machine-wide lock names give false cross-KB contention

**Verified against code:** `cmd/scribe/config.go:588` — `LockDir: "/tmp"` is
the hardcoded default in `loadConfig`'s zero-value struct, and every fresh KB
scaffolded by `scribe init` renders `lock_dir: /tmp` into `scribe.yaml`
(confirmed by `cmd/scribe/team_integration_test.go:127-129`'s own comment:
*"The template hardcodes `lock_dir: /tmp`; point it at a per-test dir so
acquireLock never touches the live /tmp/scribe-sync.lock"*). `lockPathFor`
(`cmd/scribe/lockfile.go:49-51`) builds the lock file name from `lockDir` +
job name only:

```go
func lockPathFor(lockDir, name string) string {
	return filepath.Join(lockDir, "scribe-"+name+".lock")
}
```

No KB identity enters the path. Since no user manually sets a per-KB
`lock_dir:` override in practice, **every registered KB on a machine shares
the exact same lock files** — `/tmp/scribe-sync.lock`, `/tmp/scribe-dream.lock`,
`/tmp/scribe-capture-imessage.lock`. Five call sites confirmed via
`grep -rn "lockPathFor\|holdLocks\|withLock" cmd/scribe/*.go` (excluding
`lockfile.go` itself and tests): `sync.go:89`, `dream.go:53`,
`capture.go:65`, `commit.go:31` (via `holdLocks`, which takes ALL of
`lockNames = []string{"sync", "dream", "capture-imessage"}` at once —
`commit.go:14,31`), `projects.go:87` (via `withSyncLock` → `withLock`).

**Is this a correctness bug?** Verified `cmd/scribe/each.go:71-102`
(`EachCmd.Run`): the KB-agnostic scheduler loops over `registeredKBs()`
**sequentially** — `eachRunner` (`each.go:48-60`) runs `exec.CommandContext`
synchronously inside the `for` loop, so within one `scribe each -- <job>`
invocation only one KB's subprocess is ever alive at a time. That confirms
the team-lead framing: **not a correctness bug** in the sole-scheduler
steady state. The real, verified impact is two-fold:

1. A **manual** invocation on one KB (`SCRIBE_KB=kbB scribe sync`) races an
   **in-progress scheduled** run on a different KB (`scribe each -- sync`
   mid-loop on kbA, or a long-running legacy job) — both hit
   `/tmp/scribe-sync.lock`, and the second one silently no-ops
   (`sync.go:96`: `"another scribe sync is running — exiting"`) even though
   the two KBs are unrelated git repos.
2. **`commit.go`'s `holdLocks` cross-contends across job types AND KBs at
   once**: KB-A's hourly auto-commit (`commit.go:31`) requires ALL THREE of
   `/tmp/scribe-sync.lock`, `/tmp/scribe-dream.lock`,
   `/tmp/scribe-capture-imessage.lock` to be free. If KB-B (a different,
   unrelated KB) happens to be mid-sync at that moment, KB-A's commit is
   blocked (`commit.go:35`: `"blocked by active %s process"`) and skipped
   for that tick — a real, observable operational annoyance (delayed
   commits) though not data loss (the next hourly tick retries).

`watch.go` (`WatchCmd`, the multi-KB fsnotify watcher added in `d5bb056`) was
checked and does **not** call `acquireLock`/`lockPathFor` at all — it only
feeds the pending-sessions queue, so it's outside this gap's scope.

---

## 2. Design decisions

**D1 — One scan function for gap 1, mirroring `findSecretsInKB` line-for-line, living in `stopwords.go`.**
Matches the existing split (`secrets.go` owns `findSecretsInKB`;
`doctor.go` just calls it and formats the `check`). Rejected: writing the
scan logic directly in `doctor.go` — rejected for consistency with the
established convention, and because `stopwords.go` already owns every other
piece of stop-word logic (`stopWordRules`, `applyStopWords`,
`compileStopWords`) that this scan reuses directly.

**D2 — Report only `hold` matches, not `mask` matches.**
A masked file is sanitized in place and still commits
(`stopwords.go:164-166`: *"Hold wins... the sanitized document still
commits"*) — nothing about a mask match stays invisible; it's on disk,
redacted, in the commit. Only `hold` matches produce the "vanishes without a
trace" failure mode this gap is about. `findHeldStopWordsInKB` calls
`applyStopWords(content, hold, nil, "")` and only records `dec.hold` hits.

**D3 — The check is unconditional (no `cfg.Team` gate), unlike the secrets check.**
`SecretScanConfig` is explicitly team-mode-only (`config.go:132-136` doc
comment: *"the team-mode credential gate"*), so `doctor.go:899` gates it on
`cfg.Team`. `stopwords.go:15` explicitly documents the opposite for
stop-words: *"active for solo KBs too, not just team mode."* `stopWordRules`
naturally returns empty slices when nothing is configured, and
`findHeldStopWordsInKB` short-circuits on `len(hold) == 0`, so no extra
gating is needed or correct at the call site.

**D4 — Lock scoping: suffix the lock filename with a hash of the canonicalized KB root, keep `lockDir` (and its `/tmp` default) untouched.**
This is the minimal-blast-radius fix: only the filename changes
(`scribe-sync.lock` → `scribe-sync-<hash8>.lock`); the directory, the
`lock_dir:` config key, its default, and every log line that prints
`"lock %s: %w"` keep working unchanged. Rejected alternative: change
`LockDir`'s *default* to a KB-relative path (e.g. `<root>/output/.locks`) —
rejected because it's a larger surface (touches the scaffold template, every
existing KB's on-disk lock location moves, `team_integration_test.go`'s
`lock_dir: /tmp` string-replace patch would need rework) for no extra
correctness benefit over the filename-suffix approach, and it silently
changes behavior for KBs that already rely on `/tmp`'s reboot/cleanup
semantics.

**D5 — Hash source: `crypto/sha256`, not `hash/fnv`, truncated to 8 hex chars. No human-readable KB-name component in the filename.**
`crypto/sha256` is already an established in-repo pattern for exactly this
"short stable identifier from a string" purpose — used identically in
`config_trust.go:189`, `inbox.go:359`, `install_tools.go`,
`lint_content_dupes.go`, `lint_dupes.go`, `skill.go` (verified via `grep -rn
"crypto/sha256" cmd/scribe/*.go`). Reusing it means zero new import
patterns to review. 8 hex chars = 32 bits of a 256-bit digest — collision
probability is irrelevant at the scale of "a handful of KBs on one
developer's machine." Rejected: append a sanitized `kbName()` for
readability (`scribe-sync-scriptorium-<hash8>.lock`) — rejected because
`kb_name:` is free-form user text (`config.go:100-107`) with no guaranteed
filesystem-safety (could contain `/`, which would make
`filepath.Join(lockDir, ...)` write outside the intended flat directory via
`acquireLock`'s `os.MkdirAll(filepath.Dir(lockPath), ...)` at
`lockfile.go:20`), so it would need its own sanitizer — added surface for a
"small" fix, purely cosmetic benefit (an operator can still identify which
KB a lock belongs to via `lsof` / `fuser` on the path, or by comparing the
hash against `kbLockScope(root)` computed for a known KB root).

**D6 — Canonicalize the root via `filepath.Abs` then `filepath.EvalSymlinks` (best-effort) before hashing, so the same KB always maps to the same lock file regardless of how its path was spelled.**
Without this, `-C ./kb`, `SCRIBE_KB=/Users/x/kb`, a cwd-walk hit, and a
trailing-slash spelling could each hash to a *different* suffix for the
*same* KB — which would silently defeat the lock for that KB (two processes
on the same KB no longer see each other), the opposite of this fix's goal
and a strictly worse regression than the bug being fixed. `EvalSymlinks`
failure (dir vanished, permissions) falls back to the `Abs` form only,
matching the existing tolerant pattern in `isThrowawayPath`
(`init.go:772-774`: `if canon, err := filepath.EvalSymlinks(abs); err == nil
{ abs = canon }`).

**D7 — Backward compatibility during a `make install` upgrade: accept the one-upgrade window; do NOT also acquire the legacy (un-suffixed) lock name.**
The scenario: an old-binary process is mid-sync on KB-A holding
`/tmp/scribe-sync.lock` (old, un-suffixed name) at the moment `make install`
swaps the binary on disk (this doesn't kill the already-running old
process — Unix `execve`d processes keep running off their original inode).
A subsequent new-binary invocation for the *same* KB-A would compute
`/tmp/scribe-sync-<hash8>.lock` — a different file — and could start
concurrently with the still-running old process, briefly defeating the
lock's purpose for that one KB during that one transition window. Chosen
fix: **accept this window, document it, do nothing further.** Reasoning:
(1) `CLAUDE.md` already documents `make install` as a deliberate,
infrequent, single-maintainer-triggered event with an accepted disruptive
cost (it invalidates the chat.db FDA grant every time) — a narrow race
during that same already-disruptive window is consistent with the project's
existing risk posture, not a new category of risk; (2) the advisory lock is
a courtesy layer, not the only safety net — git's own `.git/index.lock`
still serializes the actual write that matters; worst case is duplicate
work and noisy logs, the same class already accepted for the
`foreign-agents` doctor check's "duplicated jobs run twice per slot"
finding (`doctor.go:736-742`); (3) the window closes itself as soon as the one
old process still running exits — it cannot recur or compound. Rejected
alternative: also acquire the legacy un-suffixed lock as a compatibility
shim — rejected because keeping it permanently **reintroduces the exact
cross-KB false-contention bug this fix exists to remove** (any two KBs
would again serialize through the shared legacy name), and keeping it only
"for one release then removing it" adds a deprecation-tracking burden and a
second code path to a "small" fix for a window that self-heals in minutes.

---

## 3. Implementation steps

### 3.1 `cmd/scribe/stopwords.go` — Gap 1

Add `"fmt"` to the import block (currently `bytes, os, path/filepath, regexp,
strings, unicode/utf8`). Add this function (suggested location: directly
after `holdStopWordFiles` and its helpers, before `commitGate`, keeping
gate-time and doctor-time logic adjacent):

```go
// findHeldStopWordsInKB scans markdown for doctor the same way
// findSecretsInKB (secrets.go:444) does for secrets. The stop-words commit
// gate holds matching files back from the commit (holdStopWordFiles above),
// but that hold previously left no persistent, doctor-visible record — only
// a transient sync-log "STOPWORD HELD" line at hold time. This reports
// every markdown file that currently matches a hold rule: files still
// dirty in the working tree because the gate held them, AND cleanly
// committed articles that match a hold word added to the config after they
// landed. Mask-only matches are not reported — masking sanitizes in place
// and the file still commits, so nothing about those stays invisible.
func findHeldStopWordsInKB(root string, cfg *ScribeConfig) []string {
	hold, _, _ := stopWordRules(cfg)
	if len(hold) == 0 {
		return nil
	}
	var findings []string
	seen := map[string]bool{}
	record := func(path string, content []byte) error {
		rel := relPath(root, path)
		if seen[rel] {
			return nil
		}
		seen[rel] = true
		if dec := applyStopWords(content, hold, nil, ""); dec.hold {
			findings = append(findings, fmt.Sprintf("%s:%d [%s]", rel, dec.line, dec.label))
		}
		return nil
	}
	if hasGit(root) {
		if out, err := runCmdRaw(root, "git", "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*.md"); err == nil {
			for rel := range strings.SplitSeq(string(out), "\x00") {
				if rel == "" {
					continue
				}
				content, rerr := os.ReadFile(filepath.Join(root, rel))
				if rerr != nil {
					continue
				}
				_ = record(filepath.Join(root, rel), content)
			}
		}
	}
	_ = walkAllMarkdown(root, record)
	rawDir := filepath.Join(root, "raw")
	_ = filepath.Walk(rawDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil //nolint:nilerr // skip unreadable, continue walk
		}
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil //nolint:nilerr // skip unreadable, continue walk
		}
		return record(path, content)
	})
	return findings
}
```

`relPath`, `hasGit`, `runCmdRaw`, `walkAllMarkdown` are existing
package-level helpers already used by `findSecretsInKB` — no new
dependencies.

### 3.2 `cmd/scribe/doctor.go` — wire Gap 1 into `checkState`

`checkState(root string) []check` is defined at `doctor.go:812` and called
once, at `doctor.go:79`: `all = append(all, checkState(root)...)`. Add a
`cfg *ScribeConfig` parameter (the manifest-loading body already only needs
`root`, so this is additive):

```go
func checkState(root string, cfg *ScribeConfig) []check {
```

and update the call site (`cfg` is already loaded at `doctor.go:59` before
the section dispatch):

```go
case "state":
	all = append(all, checkState(root, cfg)...)
```

Insert this block immediately after the `secrets-in-articles` block
(`doctor.go:894-911`), before the conflict-markers block at `doctor.go:913`:

```go
	// Stop-words hold scan: unlike the secret gate above, the stop-words
	// gate applies to solo KBs too (stopwords.go), so this check is
	// unconditional.
	if findings := findHeldStopWordsInKB(root, cfg); len(findings) > 0 {
		shown := findings
		if len(shown) > 5 {
			shown = append(append([]string{}, shown[:5]...), "…")
		}
		out = append(out, check{
			Section: "state", Name: "stopword-held-articles", Status: statusWarn,
			Detail: fmt.Sprintf("%d article(s) still held back by the stop-words gate: %s", len(findings), strings.Join(shown, ", ")),
			Fix:    "remove the held word, add 'scribe:allow' on the line, or delete the file if it shouldn't exist",
		})
	}
```

Before editing, re-grep `checkState(` across `cmd/scribe/*.go` and
`*_test.go` to confirm there are no other call sites that also need the new
`cfg` argument (only `doctor.go:79` was found at plan time).

### 3.3 `cmd/scribe/lockfile.go` — Gap 2

Add imports `"crypto/sha256"` and `"encoding/hex"` to the existing block
(`errors, os, path/filepath, syscall`).

Add, above `lockPathFor`:

```go
// kbLockScope returns a short, stable, filesystem-safe suffix identifying
// root, so two different KBs sharing the same lock_dir (the "/tmp" default
// every fresh KB is scaffolded with) get distinct lock files instead of
// silently serializing against each other's sync/dream/capture/commit
// runs. Canonicalizes via Abs then EvalSymlinks (best-effort) so the same
// KB always maps to the same suffix no matter how its path was spelled
// (-C, SCRIBE_KB, cwd walk, trailing slash, a symlinked home dir) —
// without this, the same KB could silently stop contending with itself,
// which would be worse than the bug this fixes.
func kbLockScope(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	if canon, err := filepath.EvalSymlinks(abs); err == nil {
		abs = canon
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:])[:8]
}
```

Change `lockPathFor` (`lockfile.go:49-51`):

```go
func lockPathFor(lockDir, name, root string) string {
	return filepath.Join(lockDir, "scribe-"+name+"-"+kbLockScope(root)+".lock")
}
```

Change `withLock` (`lockfile.go:61-70`) — add `root` and thread it through:

```go
func withLock(lockDir, name, root string, fn func() error) error {
	lf, ok, err := acquireLock(lockPathFor(lockDir, name, root))
	...
```

Change `holdLocks` (`lockfile.go:83-97`) — add `root` and thread it through:

```go
func holdLocks(lockDir string, names []string, root string) (release func(), busy string, err error) {
	...
	for _, name := range names {
		lf, ok, lerr := acquireLock(lockPathFor(lockDir, name, root))
		...
```

### 3.4 Call-site updates (mechanical, five sites)

| File:line | Before | After |
| - | - | - |
| `sync.go:89` | `lockPathFor(cfg.LockDir, "sync")` | `lockPathFor(cfg.LockDir, "sync", root)` |
| `dream.go:53` | `lockPathFor(cfg.LockDir, "dream")` | `lockPathFor(cfg.LockDir, "dream", root)` |
| `capture.go:65` | `lockPathFor(cfg.LockDir, "capture-imessage")` | `lockPathFor(cfg.LockDir, "capture-imessage", root)` |
| `commit.go:31` | `holdLocks(cfg.LockDir, lockNames)` | `holdLocks(cfg.LockDir, lockNames, root)` |
| `projects.go:87` (`withSyncLock`) | `withLock(loadConfig(root).LockDir, "sync", fn)` | `withLock(loadConfig(root).LockDir, "sync", root, fn)` |

`root` is already in scope at every one of these five call sites (each is
inside a `Run()`/helper that starts with `root, err := kbDir()` or, for
`withSyncLock(root string, fn func() error) error`, takes `root` as a
parameter already) — verified by reading each function's opening lines.

---

## 4. Test plan

All new tests are pure Go, hermetic (`t.TempDir()` only), and require no
network access.

| Test | File | Covers | Setup | Assertion |
| - | - | - | - | - |
| `TestFindHeldStopWordsInKB_HoldOnly` | `stopwords_test.go` (new func) | Gap 1 / D2 | `initTestGitRepo(t, ...)`; `writeKBFile` two files — one containing a configured hold word, one containing only a mask word; `cfg := &ScribeConfig{StopWords: StopWordsConfig{Hold: []string{"falcon"}, Mask: []string{"acme"}}}` | Findings include the hold-word file (`path:line [falcon]` shape); exclude the mask-only file |
| `TestFindHeldStopWordsInKB_MatchesGateScope` | `stopwords_test.go` (new func) | Gap 1 / D1, mirrors `TestFindSecretsMatchesGateScope` (`secrets_test.go`) | Files in `notes/` (outside wiki/raw), `wiki/`, and a `.gitignore`d `output/` dir, each containing a hold word | `notes/...` and `wiki/...` reported; `output/...` (gitignored) not reported |
| `TestFindHeldStopWordsInKB_EmptyConfigNoop` | `stopwords_test.go` (new func) | Gap 1 / D3 | No hold/mask config | Returns `nil` — regression guard on the `len(hold) == 0` short-circuit |
| `TestFindHeldStopWordsInKB_PersonalConfigUnion` | `stopwords_test.go` (new func) | Gap 1, matches `TestHoldStopWordFiles_PersonalConfigUnion`'s setup style | Hold word only in the personal `~/.config/scribe/config.yaml`, not in `scribe.yaml` | File is still reported (union honored, same as gate-time) |
| `TestCheckState_StopwordHeldSurfaced` | `doctor_test.go` (or new `doctor_state_test.go` if none exists — check first) | Gap 1 wiring | KB fixture with one held file on disk, `cfg` carrying the hold word | `checkState(root, cfg)` includes `check{Name: "stopword-held-articles", Status: statusWarn}` |
| `TestKBLockScope_StableAndUnique` | `lockfile_test.go` (new file) | Gap 2 / D5, D6 | Two fake KB dirs (`kb-a`, `kb-b`) | `kbLockScope(kbA) != kbLockScope(kbB)`; `kbLockScope(kbA)` called twice returns the same value; `kbLockScope(kbA + "/")` (trailing slash) equals `kbLockScope(kbA)` |
| `TestLockPathFor_DifferentKBsGetDifferentPaths` | `lockfile_test.go` | Gap 2 core fix | `lockPathFor(sharedDir, "sync", kbA)` vs `lockPathFor(sharedDir, "sync", kbB)` | Paths differ; both have basename prefix `scribe-sync-` |
| `TestAcquireLock_DifferentKBsDoNotContend` | `lockfile_test.go` | Gap 2 behavioral proof | Acquire `lockPathFor(dir, "sync", kbA)`, then try to acquire `lockPathFor(dir, "sync", kbB)` while the first is held | Second acquire succeeds (`ok == true`) — this is the exact scenario that silently failed before the fix |
| `TestAcquireLock_SameKBStillContends` | `lockfile_test.go` | Gap 2 regression guard (D6) | Acquire `lockPathFor(dir, "sync", kb)` twice for the *same* `kb` while the first is held | Second acquire fails (`ok == false`) — the fix must not accidentally let a KB race itself |

Existing tests to re-run (signature changes ripple into them):
`go test ./cmd/scribe/... -run 'TestTeamWorkflow_EndToEnd|TestTeamConflict'`
(`team_integration_test.go` calls `scaffoldTeamKB`, which patches
`lock_dir:` but not the lock *filename* — unaffected by this fix, should
stay green with zero changes; run it anyway since it's the one existing
test suite that exercises real lock acquisition end-to-end).

Full suite: `make test` (offline, `-tags sqlite_fts5`) must stay green.
`make check` (test+vet) before considering the branch done; `make ci` at
phase-end per `docs/issues-master-plan.md:70-71`.

---

## 5. Risks & edge cases

- **Signature churn is mechanical but touches 5 files outside `lockfile.go`
  and `doctor.go`** (`sync.go`, `dream.go`, `capture.go`, `commit.go`,
  `projects.go`) — grep `lockPathFor(\|withLock(\|holdLocks(` across
  `cmd/scribe/*.go` (including tests) one more time right before landing to
  catch any call site not enumerated in §3.4 (none were found at plan time,
  but re-verify against the live tree, not this document, since other
  in-flight branches from the same backlog sweep could add a new caller).
- **`checkState`'s new `cfg` parameter** is the same signature change
  already flagged as a risk in `docs/issue-27-kb-scoped-doctor-status-plan.md`
  §5 — if that plan's Finding D lands first, this plan's §3.2 becomes a
  no-op (already done); if this plan lands first, that plan's Finding D
  becomes redundant. **Coordinate: land whichever of the two plans is
  merged first, then rebase/skip the duplicate `checkState` edit in the
  other.**
- **D7's accepted upgrade window** only matters if a `make install` happens
  while a sync/dream/capture is actively running *and* another invocation
  for the *same* KB starts before the old process exits — narrow, self-healing,
  and already documented as accepted (not a new risk introduced here, an
  explicit call *not* to add complexity against it).
- **`findHeldStopWordsInKB`'s result depends on the machine's personal
  `~/.config/scribe/config.yaml`** (via `stopWordRules` → `loadUserConfig()`),
  not just the KB's `scribe.yaml` — so the same KB can show different
  `stopword-held-articles` results on different team members' machines.
  Intentional (mirrors gate-time behavior exactly); not a bug, not
  addressed further here.
- **Do not run `scribe sync`/`dream`/`cron install` for real** during this
  work (`docs/issues-master-plan.md:87-88`) — all verification is
  `go test`-only, consistent with the standing constraints.

---

## 6. Interactions with other issues

- Closes the one #25 leftover named in `docs/issues-master-plan.md:23-25`
  ("the doctor has `findSecretsInKB` but no stop-word equivalent... folds
  into the #27 doctor/status branch") — after this lands, #25 can close
  alongside #9/#26 per that doc's existing note.
- Gap 2 has no issue number of its own in the tracker as of this audit; it
  surfaced during the lock/cron portion of the backlog sweep. Recommend
  filing it (or noting it in the phase-1 commit message) so it has a
  citable reference for the `git log` audit trail future sweeps rely on —
  same pattern used for #9/#25/#26 in `docs/issues-master-plan.md`.
- No interaction with #26 (KB registry) beyond the one already covered:
  `each.go`'s sequential-processing behavior is the reason gap 2 is a
  "reduce false contention" fix rather than a "fix a race that corrupts
  state" fix — confirmed in §1, not re-litigated here, and this plan does
  **not** touch `each.go`'s sequential semantics (explicit non-goal, per
  dispatch).

---

## 7. Size estimate

**S** (small) — both gaps are narrow, additive fixes:

| File | Change | Approx. LOC |
| - | - | - |
| `cmd/scribe/stopwords.go` | `findHeldStopWordsInKB` + `fmt` import | ~50 |
| `cmd/scribe/doctor.go` | `checkState` signature + wiring | ~15 |
| `cmd/scribe/lockfile.go` | `kbLockScope` + 3 signature changes | ~30 |
| `sync.go`, `dream.go`, `capture.go`, `commit.go`, `projects.go` | One-line call updates each | ~5 |
| Tests: `stopwords_test.go` (+4 funcs), doctor state test (+1), new `lockfile_test.go` (+4 funcs) | | ~180 |
| **Total** | | **~280 LOC** |

No new `go.mod` dependencies (`crypto/sha256`, `encoding/hex` are stdlib and
`sha256` is already imported elsewhere in the package). No new files except
`lockfile_test.go` (test-only, consistent with `CLAUDE.md`'s "no internal/
split, keep it one package" guidance — this doesn't add a source file, only
a sibling test file, which is the established pattern for every other
`*.go`/`*_test.go` pair in `cmd/scribe/`).
