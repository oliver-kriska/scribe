# Issue #28 (residual) — `scribe projects add --from-sources`

## Status check (do this first)

`scribe projects add <path> [--local] [--domain] [--name]` is already shipped
and merged to `main` — commit `1c73501` ("feat(projects): scribe projects
add + fix sources.local merge footgun (#41)"), confirmed via
`git merge-base --is-ancestor 1c73501 HEAD`. `#28` and `#41` are closed per
`docs/issues-master-plan.md`'s sweep note. The only surviving gap, confirmed
by grep (`from-sources`/`FromSources` — zero hits anywhere in the repo), is
the optional bulk mode: `--from-sources`, "enroll every include-listed repo
in one go." That is the entire scope of this plan. **Do not touch anything
else in `projects_add.go` — it is working, tested code.**

Read `cmd/scribe/projects_add.go` in full before starting (299 lines). The
9 existing tests in `cmd/scribe/projects_add_test.go` must keep passing
unmodified — this plan adds to both files, it does not change existing
behavior.

---

## 1. Problem & context

`ProjectsAddCmd` (`projects_add.go:23-28`) currently requires exactly one
path per invocation. `ProjectsAddCmd.run(root string) error`
(`projects_add.go:38-88`) does the real work for one path: resolves
worktrees (`worktreeMainRoot`, line 54), refuses a path inside the KB itself
(`withinScribeKB`, line 68), refuses a path matching `sources.exclude` or
failing `allowed_remotes` (lines 76-81), widens `sources.include`/
`scribe.local.yaml` if needed (`widenSources`, line 83), and enrolls the
project in the manifest as approved (`c.enroll`, line 87). Critically, a
**non-git path is allowed through with just a warning**
(`projects_add.go:188-190`) — that's correct for one deliberately-typed
path, but wrong for a bulk sweep (see Design decision D1).

`sources.include` entries aren't necessarily repo roots — per the doc
comment on `SourcesConfig` (`sources.go:14-19`), a plain entry like `~/work`
covers everything beneath it, and a glob like `~/Projects/client-*` is
explicitly supported syntax (see the template example at
`cmd/scribe/templates/scribe.yaml:93-98`). `--from-sources` must expand both
shapes to concrete candidate directories, not assume each entry is itself a
repo.

`loadConfig(root)` already returns the **effective** include list — the
committed `scribe.yaml` unioned with `scribe.local.yaml` via `unionPaths`
(`config_trust.go:359-372`, `sources.go:44-63`). `cfg.Sources.Include` after
`loadConfig` is therefore exactly "committed ∪ local" with no extra work
needed to combine the two files.

---

## 2. Design decisions

**D1 — Reuse `run()` unmodified per candidate; add exactly one extra check
(git-repo-ness).** `run()` already turns every failure mode into either a
descriptive error (missing path, `sources.exclude` match, inside-KB) or a
graceful no-op print (`"%s already enrolled (%s)"`,
`projects_add.go:168`). Calling `(&ProjectsAddCmd{Path: cand}).run(root)`
per candidate gets all of that for free — "skip already-enrolled" and "skip
excluded" need **no new code**, they fall out of calling the existing
function and catching its error. The one behavior that must differ from
single-path `add` is non-git paths: single `add` enrolls them with a
warning (a deliberate, explicit user action); a bulk sweep over glob matches
must skip them instead, since a broad pattern like `~/work-*` will commonly
match non-repo siblings that shouldn't get silently onboarded. This is the
only pre-check `runFromSources` needs before delegating to `run()`.
Rejected alternative (what the earlier, now-discarded draft of this plan
did): extract a shared `addPath`/change `enroll`'s signature to report
"changed" — rejected here as unnecessary complexity now that the loop can
just call `run()` as-is and interpret its error/prints; it also keeps every
existing test (which calls `.run(root)` directly and asserts specific error
substrings) completely untouched.

**D2 — `--from-sources` is mutually exclusive with `<path>`, `--local`,
`--domain`, `--name`.** Mirrors the existing `ProjectsApproveCmd` pattern
(`Names []string \`arg:"" optional:""\`` + `All bool`, `projects.go:75-78`,
validated in `run()` at `projects.go:108-114`). `--local`/`--domain`/`--name`
are single values that can't apply to N enrolled projects; reject the
combination outright rather than silently ignoring a flag the user typed.

**D3 — Glob entries expand via `filepath.Glob`; malformed patterns are
swallowed, not fatal.** stdlib only (`path/filepath`), no new dependency.
Trailing `/**` is stripped first exactly like `sourcePatternMatches`
(`sources.go:143`) already does, so `--from-sources` reads the same pattern
syntax the rest of `sources.go` documents. A `Glob` error contributes zero
candidates for that entry rather than aborting the whole run — same
philosophy as skip-don't-abort throughout this command.

**D4 — De-duplicate resolved candidates across entries.** Two include
entries can resolve to the same directory (an exact duplicate, or a plain
entry inside a glob's match set). A `map[string]bool` seen-set before
calling `run()` avoids a double "already enrolled" print and a doubled
summary count. Four lines, cheap enough to include.

**D5 — Terse summary line, not a full breakdown.** The task calls for "a
terse summary line at the end," not a categorized report. One counter for
"enrolled or already-enrolled" (`run()` returned nil) and one for "skipped"
(non-git, or `run()` returned an error) is enough:
`from-sources: N enrolled/confirmed, M skipped`. Per-candidate detail is
still visible — each skip prints its own one-line reason as it happens.

---

## 3. Implementation steps

### `cmd/scribe/projects_add.go`

1. **Imports** (`projects_add.go:3-11`): add `"errors"` and `"strings"` (not
   currently imported in this file — verify before adding, `bytes`, `fmt`,
   `os`, `path/filepath`, `time`, `gopkg.in/yaml.v3` are the current set).

2. **Struct** (`projects_add.go:23-28`): change

   ```go
   Path   string `arg:"" help:"Project path to enroll (the repo, not the KB)."`
   ```

   to

   ```go
   Path   string `arg:"" optional:"" help:"Project path to enroll (the repo, not the KB). Omit with --from-sources."`
   ```

   and add a new field:

   ```go
   FromSources bool `help:"Bulk-enroll every git repo already covered by the effective sources.include. Skips non-git paths (one-line reason each); reuses the single-path enroll logic. Cannot be combined with a path, --local, --domain, or --name."`
   ```

3. **`Run()`** (`projects_add.go:30-36`): replace the closure body with:

   ```go
   func (c *ProjectsAddCmd) Run() error {
       root, err := kbDir()
       if err != nil {
           return err
       }
       return withSyncLock(root, func() error {
           if c.FromSources {
               if c.Path != "" || c.Local || c.Domain != "" || c.Name != "" {
                   return errors.New("--from-sources cannot be combined with a path, --local, --domain, or --name")
               }
               return c.runFromSources(root)
           }
           if c.Path == "" {
               return errors.New("pass a project path or --from-sources (see `scribe projects add --help`)")
           }
           return c.run(root)
       })
   }
   ```

   `run(root string) error` (`projects_add.go:38-88`) is otherwise
   **untouched**.

4. **New function** `runFromSources`, placed after `run()`:

   ```go
   // runFromSources bulk-enrolls every git repo already covered by the
   // effective (committed ∪ local) sources.include list. It resolves each
   // entry to candidate directories (resolveIncludeEntry) and calls run()
   // unmodified per candidate — run() already turns a missing path, a
   // sources.exclude match, or an inside-the-KB path into a descriptive
   // error, and already no-ops gracefully on an already-enrolled path, so
   // none of that needs duplicating here. The one thing run() does NOT do
   // that bulk mode must: skip a non-git candidate instead of enrolling it
   // with a warning, since a broad include pattern routinely matches
   // non-repo siblings a sweep shouldn't blindly onboard.
   func (c *ProjectsAddCmd) runFromSources(root string) error {
       cfg := loadConfig(root)
       if len(cfg.Sources.Include) == 0 {
           fmt.Println("note: sources.include is empty (allow-all) — nothing to bulk-enroll; use `scribe projects add <path>` for a specific repo")
           return nil
       }

       seen := map[string]bool{}
       var ok, skipped int
       for _, entry := range cfg.Sources.Include {
           for _, cand := range resolveIncludeEntry(entry) {
               if seen[cand] {
                   continue
               }
               seen[cand] = true
               if dirExists(cand) && !hasGit(cand) && worktreeMainRoot(cand) == "" {
                   fmt.Printf("skip %s: not a git repo\n", cand)
                   skipped++
                   continue
               }
               if err := (&ProjectsAddCmd{Path: cand}).run(root); err != nil {
                   fmt.Printf("skip %s: %v\n", cand, err)
                   skipped++
                   continue
               }
               ok++
           }
       }
       fmt.Printf("from-sources: %d enrolled/confirmed, %d skipped\n", ok, skipped)
       return nil
   }

   // resolveIncludeEntry expands one sources.include entry to concrete
   // directory candidates: a plain path is the candidate itself; a glob
   // (*, ?, [) expands via filepath.Glob. Mirrors the trailing "/**"
   // normalization sourcePatternMatches already applies (sources.go).
   func resolveIncludeEntry(pattern string) []string {
       p := strings.TrimSuffix(expandHome(strings.TrimSpace(pattern)), "/**")
       if !strings.ContainsAny(p, "*?[") {
           return []string{filepath.Clean(p)}
       }
       matches, _ := filepath.Glob(p)
       return matches
   }
   ```

No other file needs to change. `sources.go`, `config_trust.go`, `manifest.go`,
`sync_discover.go` are untouched — everything this plan needs already exists
there and is reused as-is.

---

## 4. Test plan

New tests in `cmd/scribe/projects_add_test.go`, using the existing
`addKB`/`makeProjectDir` helpers (`projects_add_test.go:14-38`). A repo
fixture needs an actual `.git` dir for `hasGit` to return true — check
`gitops.go:431`'s implementation and mirror however other tests in this
package fake a git repo (grep `hasGit` usages in `*_test.go` for the
existing convention before inventing a new one).

| Test | Setup | Assertion |
|---|---|---|
| `TestProjectsAdd_FromSources_EnrollsListedRepos` | `sources.include: [<repoA>, <repoB>]`, both real git dirs | Both approved, `discovered_from: manual`; no error |
| `TestProjectsAdd_FromSources_SkipsNonGitPath` | Include list has one repo + one plain (non-git) dir | Only the repo enrolls; non-git dir absent from manifest; no error returned |
| `TestProjectsAdd_FromSources_SkipsMissingAndExcluded` | Include has a path that doesn't exist, and a path also matching `sources.exclude` | Neither enrolls; command still returns nil (skips aren't fatal) |
| `TestProjectsAdd_FromSources_AlreadyEnrolledIsIdempotent` | Run `--from-sources` twice over the same include list | Second run enrolls nothing new; manifest identical before/after |
| `TestProjectsAdd_FromSources_EmptyIncludeNoop` | `sources.include` empty | No error, prints the allow-all note, manifest untouched |
| `TestProjectsAdd_FromSources_ExpandsGlobAndDedupes` | `sources.include: ["<parent>/client-*", "<parent>/client-a"]` with two matching subdirs | `client-a` enrolled once (not twice) despite matching both entries |
| `TestProjectsAdd_FromSources_RejectsCombinedFlags` | `&ProjectsAddCmd{FromSources: true, Local: true}` run through `.Run()` (needs `kbDir`/env setup via `addKB`, or test the validation as extracted logic if `Run()`'s `kbDir()` call makes direct testing awkward) | Returns an error naming the conflict, no manifest changes |
| Regression: all 9 existing tests in `projects_add_test.go` | unchanged | Must still pass — this is the check that `run()` truly wasn't modified |

`make test` (`go test ./... -tags sqlite_fts5`) must pass fully offline —
every fixture above is `t.TempDir()`-based, consistent with the rest of the
file.

---

## 5. Risks & edge cases

- **A glob matching hundreds of directories** prints one skip line per
  non-git match. Not mitigated here (same non-issue as `sync --discover`'s
  own walk) — `sources.include` is user-authored config, not untrusted
  input.
- **`resolveIncludeEntry`'s `filepath.Glob` error is silently swallowed**
  (`matches, _ := filepath.Glob(p)`) — deliberate per D3, but means a badly
  malformed glob in `sources.include` produces zero candidates with no
  visible complaint. Acceptable: `sourcePatternMatches` already treats a
  `filepath.Match` error as no-match with the same silence
  (`sources.go:151-153`), so this matches existing precedent in the same
  file rather than introducing a new silent-failure mode.
- **Recursive/self-referential candidate**: `(&ProjectsAddCmd{Path:
  cand}).run(root)` re-derives `cfg := loadConfig(root)` internally per
  call (`projects_add.go` inside `run`, via the same `loadConfig`), so each
  candidate sees a fresh config read — fine for correctness, mildly
  wasteful for a very long include list, not worth optimizing at this size.

---

## 6. Interactions with other open issues

- Sits in the "Phase 1 residuals" bucket per `docs/issues-master-plan.md`'s
  2026-07-02 sweep note (alongside the doctor stop-words visibility gap and
  the per-KB `/tmp` lock scoping) — small, isolated, no dependency on any
  other phase.
- No interaction with #8 (path-keyed manifest, later phase) — this plan
  only calls `run()`, which is already name-keyed exactly like every other
  path into `manifest.Projects`.
- No interaction with #26 (KB registry) — single-KB scoped, same as every
  other `projects` subcommand.

---

## 7. Size estimate

**S.** ~55-65 lines of production code in `projects_add.go` (struct field,
`Run()` branch, `runFromSources`, `resolveIncludeEntry`, two new imports),
~80-100 lines of new tests. Zero new dependencies. Zero changes to any file
other than `projects_add.go` and its test file.
