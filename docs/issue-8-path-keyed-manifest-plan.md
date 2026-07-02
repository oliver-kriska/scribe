# Issue #8 — path-keyed manifest identity

Status: planning only, no code written yet. Base commit for this plan: `49bfd53` (per
`docs/issues-master-plan.md`). **The implementing agent must run `git rev-parse HEAD`
first and confirm it matches main's current tip before touching anything** — agent
worktrees have snapshotted stale bases before (see
`docs/issues-master-plan.md` "Standing constraints").

This plan is self-contained: it does not assume the reader has read the GitHub issue
or explored the codebase. Every claim below cites the actual file/line it is based on.

---

## 1. Problem & context

`scripts/projects.json` (loaded by `loadManifest`, `cmd/scribe/manifest.go:134-165`) is
the discovery manifest: `Manifest.Projects` is a `map[string]*ProjectEntry` keyed by a
**derived name**, not by the project's real identity. The key comes from
`projectName(path)` (`manifest.go:376-383`):

```go
func projectName(path string) string {
	parent := filepath.Base(filepath.Dir(path))
	name := filepath.Base(path)
	if projectRoots[parent] {
		return name
	}
	return parent + "-" + name
}
```

This is lossy: `~/src/api` and `~/work/api` both derive different names only because
their parents differ, but `~/Projects/api` and `~/other/Projects/api` collide on `api`
because both parents happen to be recognized "project root" basenames
(`manifest.go:315-344`). A renamed parent directory, two sibling checkouts that happen
to share a leaf name, or a git worktree (whose basename is unrelated to the main
repo's) can all produce this collision.

Three interim patches already exist in the codebase, confirming this is a live,
previously-debugged problem, not a hypothetical:

1. **`samePath`** (`cmd/scribe/worktree.go:66-72`) — symlink-tolerant path comparison
   (macOS `/var` vs `/private/var`), used everywhere a raw string path-equality check
   would be wrong.
2. **`entryForPath`** (`manifest.go:352-373`) — a fallback resolver that, when the keyed
   lookup misses, linearly scans every entry's `Path` and `Worktrees` with `samePath`.
   Two call sites already depend on it because the keyed lookup is known to be wrong
   for their use case: `status.go:527` (session pending-count) and
   `sync_sessions.go:179` (pre-filter approval gate) — both have comments explaining
   they need `entryForPath` specifically because "a basename collision can't borrow
   another project's approval."
3. **Collision-refusal branches** — when a *second*, different repo would derive the
   same key as an already-known entry, discovery detects it (via `samePath`) and
   **refuses to enroll the second repo at all**, logging a warning instead:
   `sync_discover.go:69-77` (Claude discovery), `codex.go:312-317` (Codex discovery),
   `sync_discover.go:154-163` (worktree folding onto a colliding main entry). This is
   the actual user-visible bug: the second project can never be tracked until the user
   manually renames a directory or runs `scribe projects ignore`.

Meanwhile **15+ other call sites** do a *direct* keyed lookup (`manifest.Projects[name]`)
with no collision awareness at all, because they were written before `entryForPath`
existed and never migrated to it:
`codex.go:312,340`, `assess.go:73`, `deep.go:51`, `projects_add.go:152,183`,
`projects.go:53,130,161,226`, `sync_discover.go:69,101,150,154,183`,
`sync_extract.go:66,87`, `sync.go:226,359`.

**The extraction ledger is a separate file and is NOT part of this problem.**
`cmd/scribe/ledger.go` (`scripts/extraction-ledger.json`) already keys by
`repoLedgerKey(path)` (`ledger.go:92-93`), which normalizes the git origin remote URL
(`normalizeRemoteURL`, `ledger.go:101-129`) — a genuinely shared, machine-independent
identity. It has no basename problem and needs no migration. See Design Decision 6.

---

## 2. Design decisions

### 2.1 Canonicalization: absolute + cleaned + symlink-resolved, with a safe fallback

```go
// canonicalizePath is the sole identity normalization used for
// Manifest.Projects map keys.
func canonicalizePath(path string) string {
	abs, err := filepath.Abs(expandHome(path))
	if err != nil {
		abs = path
	}
	abs = filepath.Clean(abs)
	if resolved := evalSymlinksCached(abs); resolved != "" {
		return resolved
	}
	return abs
}
```

- Reuses `evalSymlinksCached` (`worktree.go:43-61`) — already a process-lifetime cache
  built for exactly this per-session × per-project comparison cost, with the doc
  comment already noting the macOS `/var` → `/private/var` gotcha this must handle
  (session cwds are logical paths; `git rev-parse` and worktree lookups return
  physical ones).
- `evalSymlinksCached` returns `""` on any resolution failure (path deleted, moved, or
  a dangling symlink component) — `canonicalizePath` falls back to the cleaned
  absolute path rather than `""`. **This is deliberate**: a project whose directory
  was since removed must stay addressable by its last-known path so existing manifest
  entries don't silently become orphaned/unfindable. `scribe doctor` already has a
  separate check for missing project dirs (via `dirExists` gates scattered through
  `doctor.go`, `sync_extract.go:177`, etc.) — identity resolution must not also break.
- **Rejected alternative**: `filepath.Clean` only, no symlink resolution. Rejected
  because it doesn't fix the exact bug `samePath`'s own doc comment names (macOS `/var`
  vs `/private/var`) — the whole point of "canonicalized" is that two spellings of the
  same real directory produce one key.

### 2.2 Schema: the map key becomes the canonical path; a new `Name` field carries the human label

```go
const manifestPathKeyedVersion = 2 // Projects keys are canonicalizePath(entry.Path);
                                    // below this, keys are legacy projectName()-derived.

type Manifest struct {
	Projects        map[string]*ProjectEntry `json:"projects"` // key: canonicalizePath(entry.Path)
	DomainAliases   map[string]string        `json:"domain_aliases"`
	IgnoredPaths    []string                 `json:"ignored_paths"`
	ManifestVersion int                      `json:"manifest_version,omitempty"`
	path            string
	migratedCount   int // set by migrateToPathKeys; save() logs+resets once (not persisted)
}

type ProjectEntry struct {
	Path   string `json:"path"`   // kept explicit and always == the map key; see rationale below
	Name   string `json:"name"`   // NEW: human display label (old projectName() derivation).
	                               // Used by CLI args, .repo.yaml, wiki dir naming, log lines.
	                               // No longer required globally unique for CORRECTNESS
	                               // (identity lives in the map key), but uniqueName()
	                               // still keeps it unique for CLI ergonomics — see 2.3.
	Domain string `json:"domain"`
	// ... every other existing field (LastSHA, LastExtracted, LastMDScan,
	// LastDropProcessed, LastResearchScanned, ExtractedDirs, DiscoveredFrom,
	// Worktrees, Status) is UNCHANGED.
}
```

**Why literal re-keying, not a "keep name-keyed, add a path field" patch.** A Go
map — and a JSON object — cannot have two entries with the same key. Re-keying by
canonical path makes the basename collision *structurally impossible*: two different
real directories can never produce the same key, full stop. Every patch already in the
codebase (samePath, entryForPath, collision-refusal) exists only because the key
itself is still wrong; re-keying removes the need for those patches at their call
sites (they either get simplified or deleted — see §3).

**Why `Path` stays a JSON field even though it's now redundant with the map key.**
`entry.Path` is read directly at 20+ call sites across the codebase (extraction,
`scribe deep`, `scribe assess`, doctor, drop/research collection, `.repo.yaml`
generation…). Removing the field and deriving it from context at every one of those
call sites is a large, unjustified blast-radius increase for zero behavioral benefit.
The redundancy costs one field; the alternative costs touching every existing reader.
`Path` is always kept `== ` the map key by construction (every write path sets both
together — see §3).

**Rejected alternative: keep the CLI/JSON key as the display name, add a separate
identity field for matching only.** This is what the three existing interim patches
already approximate (name-keyed map + `entryForPath`'s linear `samePath` scan as a
fallback). It was rejected as the long-term fix because it doesn't remove the actual
defect — two different repos can still be *permanently blocked* from both being
tracked, because the map key (the thing that must be unique) is still the lossy
derived name. Re-keying by path removes that failure mode by construction; the CLI
ergonomics are recovered separately via `Name` + a resolver (§2.3), not by leaving the
identity model broken.

### 2.3 CLI ergonomics: `Name` field + `manifest.resolve(arg)`, `manifest.uniqueName(...)`

Every CLI entry point that takes a project argument today does an exact-key lookup
against the old name-keyed map: `scribe assess <name>` (`assess.go:73`),
`scribe deep <name>` (`deep.go:51`), `scribe projects approve|ignore <name>`
(`projects.go:130,161`), `sync --extract <name>` / `--changed <name>`
(`sync_extract.go:163`, `sync.go:359`). These must keep accepting a short typed name —
requiring users to type full absolute paths on every invocation is a real UX
regression this plan must not introduce.

Two new `Manifest` methods:

```go
// resolve looks up a project by a CLI-typed reference: an absolute or
// relative filesystem path (canonicalized and matched against the map key
// directly), or a short display Name. Every CLI call site routes through
// this instead of a raw map index.
func (m *Manifest) resolve(arg string) (*ProjectEntry, error) {
	if m == nil || arg == "" {
		return nil, errors.New("empty project reference")
	}
	if looksLikePath(arg) {
		if e, ok := m.Projects[canonicalizePath(arg)]; ok {
			return e, nil
		}
	}
	var matches []*ProjectEntry
	for _, e := range m.Projects {
		if e != nil && e.Name == arg {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("project %q not in manifest (see `scribe projects list`)", arg)
	case 1:
		return matches[0], nil
	default:
		sort.Slice(matches, func(i, j int) bool { return matches[i].Path < matches[j].Path })
		paths := make([]string, len(matches))
		for i, e := range matches {
			paths[i] = e.Path
		}
		return nil, fmt.Errorf("project name %q is ambiguous — matches %d projects: %s (pass the full path instead)",
			arg, len(matches), strings.Join(paths, ", "))
	}
}

func looksLikePath(arg string) bool {
	return strings.ContainsRune(arg, filepath.Separator) || strings.HasPrefix(arg, "~") || arg == "." || arg == ".."
}
```

```go
// uniqueName returns a display Name for path guaranteed not to collide with
// any OTHER project's Name already in the manifest (a different canonical
// path). Called at discovery/enroll time so a newly-found project that
// happens to share a basename with an existing one still gets a name a
// human can type — instead of being refused entirely (the reported bug).
func (m *Manifest) uniqueName(base, path string) string {
	canon := canonicalizePath(path)
	if !m.nameCollides(base, canon) {
		return base
	}
	if qualified := filepath.Base(filepath.Dir(path)) + "-" + base; !m.nameCollides(qualified, canon) {
		return qualified
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !m.nameCollides(candidate, canon) {
			return candidate
		}
	}
}

func (m *Manifest) nameCollides(name, canon string) bool {
	for key, e := range m.Projects {
		if e != nil && e.Name == name && key != canon {
			return true
		}
	}
	return false
}
```

`uniqueName`'s escalation (`base` → `parent-base` → `base-2`, `base-3`, ...) mirrors
the disambiguation idea already in `projectName` itself (parent-qualification), just
applied one level further when even the parent-qualified form collides.

Every existing "not in manifest" error message at each CLI call site is preserved
verbatim (see §3) — only the *lookup mechanism* changes from a raw map index to
`manifest.resolve(arg)`.

### 2.4 Migration: automatic, silent (one-line notice), version-marked, idempotent, disk-write deferred to the next real save

```go
func (m *Manifest) migrateToPathKeys() {
	if m.ManifestVersion >= manifestPathKeyedVersion {
		return
	}
	byCanon := map[string][]*ProjectEntry{}
	for oldName, e := range m.Projects {
		if e == nil {
			continue
		}
		if e.Name == "" {
			e.Name = oldName // the old map key WAS unique — see reasoning below
		}
		e.Path = canonicalizePath(e.Path)
		byCanon[e.Path] = append(byCanon[e.Path], e)
	}
	migrated := make(map[string]*ProjectEntry, len(byCanon))
	for canon, entries := range byCanon {
		winner := entries[0]
		if len(entries) > 1 {
			sort.Slice(entries, func(i, j int) bool {
				if entries[i].LastExtracted != entries[j].LastExtracted {
					return newerExtracted(entries[i].LastExtracted, entries[j].LastExtracted)
				}
				return entries[i].Name < entries[j].Name
			})
			winner = entries[0]
			for _, loser := range entries[1:] {
				for _, w := range loser.Worktrees {
					winner.recordWorktree(w)
				}
			}
			logMsg("manifest", "migration: %d entries pointed at %s — kept %q's history, merged worktrees",
				len(entries), canon, winner.Name)
		}
		migrated[canon] = winner
	}
	m.migratedCount = len(m.Projects)
	m.Projects = migrated
	m.ManifestVersion = manifestPathKeyedVersion
}

// newerExtracted mirrors ledgerEntryNewer (gitmerge.go:85-92) exactly: parse
// RFC3339, fall back to a raw string compare if either side doesn't parse
// (covers "" for never-extracted entries).
func newerExtracted(a, b string) bool {
	ta, errA := time.Parse(time.RFC3339, a)
	tb, errB := time.Parse(time.RFC3339, b)
	if errA != nil || errB != nil {
		return a > b
	}
	return ta.After(tb)
}
```

Wired into `loadManifest` (`manifest.go:134-165`): call `m.migrateToPathKeys()` right
after `json.Unmarshal` succeeds (and skip it on the "fresh empty manifest" branch,
since a brand-new `Manifest{Projects: map[string]*ProjectEntry{}}` has nothing to
migrate — set `ManifestVersion: manifestPathKeyedVersion` there directly).

**Why Name collisions can't reappear after migration (no post-pass needed).** The old
map keys were, by construction (it's a Go/JSON map), already globally unique. Each
surviving entry inherits its old key as `Name` 1:1, except in the "N old entries
pointed at the same canonical path" collapse case, where exactly one `Name` survives
per canonical path. Two *different* canonical paths can therefore never end up with
the same inherited `Name`. No call to `uniqueName` is needed during migration — it
exists only for *new* discoveries after migration (§2.3), where a genuinely new path
can legitimately share a basename with something already in the manifest.

**Why the disk write is deferred, not immediate.** `loadManifest` is called by
read-only commands too: `ProjectsListCmd` (`projects.go:32`, `ReadOnly() → true`),
`DoctorCmd` (`doctor.go:52`), `StatusCmd` (`status.go:25`). `readonly_contract_test.go`
encodes the contract that read-only commands must not mutate KB state as a side
effect of being invoked (see `TestLoadConfigIsPure`, and `maybeBackfillAbsorbBlock`
never being called from `loadConfig` for the same reason). So `migrateToPathKeys`
transforms the **in-memory** struct only — every command, including read-only ones,
gets correct migrated data for its own computation — but writes nothing to disk.
`Manifest.save()` (`manifest.go:168-184`) gets one addition:

```go
func (m *Manifest) save() error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.path); err != nil {
		return err
	}
	if m.migratedCount > 0 {
		logMsg("manifest", "migrated %d project(s) to path-keyed identity (scripts/projects.json)", m.migratedCount)
		m.migratedCount = 0
	}
	return nil
}
```

The first *mutating* command that touches the manifest (any real `sync`, `projects
approve/ignore/add`) persists the migrated form transparently as a side effect of the
`save()` it already performs, and logs the one-line notice exactly once — read-only
commands never trigger a write or a log line. A KB whose manifest is only ever touched
by read-only commands stays legacy-shaped on disk indefinitely (harmless — every load
just re-derives the correct in-memory form again, cheaply, until something finally
saves it).

**No half-migrated on-disk state is possible.** `save()`'s only write path is the
existing atomic tmp-file + `os.Rename` (already true before this plan). A crash
between `migrateToPathKeys()` and the next `save()` leaves the on-disk file exactly as
it was (fully legacy); a crash *during* `save()` leaves either the old file (rename
never happened) or the new one (rename is atomic) — never a mix.

### 2.5 Worktrees fold into the main checkout (unchanged policy, simpler mechanism)

Already the policy today (`worktreeMainRoot`, `worktree.go:9-41`, and the doc comment
on `foldWorktree`, `sync_discover.go:128-134`: "every worktree of a repo shares the
repo's knowledge; enrolling each as its own project means duplicate extraction"). This
plan does not change *that* policy — it only removes the collision-refusal branch that
today can block a fold when the *main* repo's derived name collides with an unrelated
project (`sync_discover.go:154-163`). Under path-keyed identity, `foldWorktree` looks
up `manifest.Projects[canonicalizePath(main)]` directly; a name collision with some
other project is no longer possible to collide on, because it's not the lookup key
anymore (see §3 for the exact rewrite).

### 2.6 Team mode: no new identity field — the manifest already doesn't need one

Investigated and settled by two facts already in the codebase, not by adding
anything:

1. **`scripts/projects.json` is machine-local and gitignored in team KBs.**
   `special_files.go:55`: `"scripts/projects.json": {Class: classMachineLocal, ...}`.
   The doc comment on `classMachineLocal` (`special_files.go:25-27`): "per-machine
   state (the discovery manifest). Gitignored in team KBs; committed as-is in solo
   KBs. Never merged." Confirmed again in `manifest.go:138-142` and `init.go:170`
   (`init --team` gitignores it). **Two teammates' manifests are never the same file
   and are never reconciled with each other.** There is no cross-machine merge problem
   for `Manifest.Projects` to solve, because in team mode each machine's manifest is
   independent by design — a teammate's canonical local path for "their checkout of
   scribe" never needs to agree with mine.
2. **The cross-machine identity the issue is actually worried about — "don't
   double-extract the same repo seen at two different local paths" — is already solved
   by the extraction ledger**, keyed by normalized git remote URL
   (`repoLedgerKey`/`normalizeRemoteURL`, `ledger.go:89-129`), which the manifest
   doesn't need to duplicate. It's computed on demand from live git state (`git remote
   get-url origin`), not stored — so it can never go stale the way a persisted "team
   identity" field on `ProjectEntry` could (e.g. after a GitHub repo rename).

**Rejected alternative: add a stable `id`/remote-URL field to `ProjectEntry` "for
future team correlation."** Rejected because (a) nothing today needs it — per point 1,
there is no manifest-level cross-machine reconciliation happening or planned; (b) the
one legitimate cross-machine identity need (dedup extraction) is already served,
correctly, by the ledger; (c) a redundant snapshot of `repoLedgerKey(path)` on
`ProjectEntry` would go stale with no refresh mechanism, which is worse than not
having it. If a *future* issue needs cross-machine project correlation for something
the ledger doesn't cover, it should compute `repoLedgerKey` on demand at that call
site, exactly like `ledger.go` and `sources.go:126` (`remoteAllowed`) already do — not
add new persisted state here.

**Solo KB, multiple machines, committed manifest — real but pre-existing, not fixed
by this plan.** In solo (non-team) mode `scripts/projects.json` IS committed
(`special_files.go` comment above). If the same solo user syncs the same KB from two
machines where a project genuinely lives at two different absolute paths, both
machines' commits touch the same file and a normal git conflict results. It is *not*
in `derivedRegenerable` or `semanticMergers` (`special_files.go:46-56`), so
`autoResolveDerivedConflicts` (`gitops.go:147-204`) aborts the rebase and requires
manual resolution — this is **existing behavior, unrelated to key format**, and this
plan does not change it (still true whether keys are basenames or paths — the file
either merges cleanly because both sides only added new entries, one on each side of a
plain JSON diff, or it needs a human because both sides edited the same entry). Listed
under Risks (§5) as a known, deliberately out-of-scope edge case.

---

## 3. Implementation steps

### 3.1 `cmd/scribe/manifest.go`

1. Add `const manifestPathKeyedVersion = 2` near the top.
2. Add `ManifestVersion int json:"manifest_version,omitempty"` and unexported
   `migratedCount int` fields to `Manifest`.
3. Add `Name string json:"name"` field to `ProjectEntry` (place after `Path`).
4. Add `canonicalizePath(path string) string` (§2.1).
5. Add `(m *Manifest) migrateToPathKeys()` and `newerExtracted(a, b string) bool`
   (§2.4).
6. Add `(m *Manifest) uniqueName(base, path string) string`,
   `(m *Manifest) nameCollides(name, canon string) bool` (§2.3).
7. Add `(m *Manifest) resolve(arg string) (*ProjectEntry, error)` and
   `looksLikePath(arg string) bool` (§2.3).
8. In `loadManifest` (`manifest.go:134-165`): call `m.migrateToPathKeys()` on the
   parsed-file branch, right after the existing nil-map defaulting
   (`m.Projects == nil` / `m.DomainAliases == nil` checks). On the "fresh, file
   doesn't exist yet" branch (`manifest.go:144-150`), set
   `ManifestVersion: manifestPathKeyedVersion` directly in the returned struct literal
   (nothing to migrate).
9. In `save()` (`manifest.go:168-184`): add the `migratedCount` log-and-reset block
   after the successful `os.Rename`, per §2.4's snippet.
10. Simplify `entryForPath` (`manifest.go:352-373`):
    ```go
    func (m *Manifest) entryForPath(path string) *ProjectEntry {
    	if m == nil || path == "" {
    		return nil
    	}
    	canon := canonicalizePath(path)
    	if e, ok := m.Projects[canon]; ok {
    		return e
    	}
    	for _, e := range m.Projects {
    		if e == nil {
    			continue
    		}
    		for _, w := range e.Worktrees {
    			if canonicalizePath(w) == canon {
    				return e
    			}
    		}
    	}
    	return nil
    }
    ```
    (Behavior for existing callers — `status.go:527`, `sync_sessions.go:179` — is
    unchanged; this is strictly a correctness/perf simplification: O(1) canonical hit
    instead of a linear `samePath` scan for the common case.)
11. Rename `ignoreProject`'s parameter from `name` to `key` (body unchanged — it
    already operated on a raw map key; callers now pass a canonical path instead of a
    display name — see §3.4).
12. Change `pendingProjects()` (`manifest.go:69-78`) to sort by `Name` for
    human-friendly CLI ordering even though it now returns path keys:
    ```go
    func (m *Manifest) pendingProjects() []string {
    	var keys []string
    	for key, e := range m.Projects {
    		if e != nil && e.Status == statusPending {
    			keys = append(keys, key)
    		}
    	}
    	sort.Slice(keys, func(i, j int) bool { return m.Projects[keys[i]].Name < m.Projects[keys[j]].Name })
    	return keys
    }
    ```
13. `projectName(path)` (`manifest.go:376-383`) is UNCHANGED — it's still the
    base-candidate generator fed into `uniqueName`.

### 3.2 `cmd/scribe/worktree.go`

`recordWorktree` (`worktree.go:77-88`) currently does raw string equality (`path ==
e.Path`, `w == path`) with no symlink tolerance. Canonicalize the comparisons (store
the raw path as before — `collectionPaths` only needs a valid, `dirExists`-checked
directory, not a canonical one):

```go
func (e *ProjectEntry) recordWorktree(path string) bool {
	if e == nil || path == "" {
		return false
	}
	canon := canonicalizePath(path)
	if canon == canonicalizePath(e.Path) {
		return false
	}
	for _, w := range e.Worktrees {
		if canonicalizePath(w) == canon {
			return false
		}
	}
	e.Worktrees = append(e.Worktrees, path)
	return true
}
```

`samePath` itself, `evalSymlinksCache`/`evalSymlinksCached`, `worktreeMainRoot`,
`collectionPaths`, `describeWorktreeFold` are all UNCHANGED — `samePath` remains a
general-purpose utility used outside the manifest too (`cron.go:230`, `init.go:112`,
`kb.go:54`, `registry.go:50,54,147`, `status.go:300`).

### 3.3 `cmd/scribe/sync_discover.go`

**`discover`** (`sync_discover.go:17-126`): replace the block from the
`hasSignificantContent` check (line 64) through entry creation (line 116):

```go
if !hasSignificantContent(decoded) {
	continue
}

canon := canonicalizePath(decoded)
if existing, exists := manifest.Projects[canon]; exists {
	if existing.DiscoveredSource() != "claude" && existing.DiscoveredSource() != "both" {
		if !s.DryRun {
			existing.MergeDiscoveredFrom("claude")
			if err := manifest.save(); err != nil {
				logMsg("sync", "manifest save failed: %v", err)
			}
		}
	}
	continue
}

domain := manifest.resolveDomain(decoded)
status := discoveryStatus(cfg)
pname := manifest.uniqueName(projectName(decoded), decoded)
logMsg("sync", " DISCOVERED%s: %s -> %s (domain: %s)", pendingTag(status), pname, decoded, domain)
discovered++

if s.DryRun {
	continue
}

manifest.Projects[canon] = &ProjectEntry{
	Path:           canon,
	Name:           pname,
	Domain:         domain,
	DiscoveredFrom: "claude",
	Status:         status,
}
if err := manifest.save(); err != nil {
	logMsg("sync", "manifest save failed: %v", err)
}
if status != statusPending {
	ensureRepoYAML(root, decoded, pname, domain)
}
```

This **deletes** the "shadowed by existing project" refusal branch
(`sync_discover.go:70-77`) entirely — that's the actual bug fix. The rest of
`discover` (source filters, worktree-fold dispatch, `discoverCodex` call at the end)
is unchanged.

**`foldWorktree`** (`sync_discover.go:135-197`): replace from the "pre-existing entry
for the worktree itself" check (line 150) through entry creation (line 196):

```go
func (s *SyncCmd) foldWorktree(root string, manifest *Manifest, cfg *ScribeConfig, worktree, main, source string) (int, bool) {
	if manifest.isIgnored(main) || !sourceAllowed(cfg, main) {
		return 0, false
	}
	if top := runCmd(worktree, "git", "rev-parse", "--show-toplevel"); top != "" {
		worktree = top
	}
	worktreeCanon := canonicalizePath(worktree)
	mainCanon := canonicalizePath(main)

	if _, ok := manifest.Projects[worktreeCanon]; ok {
		return 0, false // pre-existing standalone entry for the worktree itself
	}

	if existing, ok := manifest.Projects[mainCanon]; ok {
		if s.DryRun || !existing.recordWorktree(worktree) {
			return 0, false
		}
		logMsg("sync", " [%s] %s — recorded for drop/research collection", existing.Name, describeWorktreeFold(worktree, main))
		if err := manifest.save(); err != nil {
			logMsg("sync", "manifest save failed: %v", err)
		}
		return 0, true
	}

	if !hasSignificantContent(main) {
		return 0, false
	}
	domain := manifest.resolveDomain(main)
	status := discoveryStatus(cfg)
	mname := manifest.uniqueName(projectName(main), main)
	logMsg("sync", " DISCOVERED (via %s worktree)%s: %s -> %s (domain: %s)", source, pendingTag(status), mname, main, domain)
	if s.DryRun {
		return 1, true
	}
	manifest.Projects[mainCanon] = &ProjectEntry{
		Path:           mainCanon,
		Name:           mname,
		Domain:         domain,
		DiscoveredFrom: source,
		Status:         status,
		Worktrees:      []string{worktree},
	}
	if err := manifest.save(); err != nil {
		logMsg("sync", "manifest save failed: %v", err)
	}
	if status != statusPending {
		ensureRepoYAML(root, main, mname, domain)
	}
	return 1, true
}
```

This **deletes** the "worktree main collides with existing project" refusal branch
(`sync_discover.go:159-163`) — same root fix. Everything else in this file
(`ensureRepoYAML`, `unprocessedDropFiles`, `collectDropFiles`, `filterNewerThan`,
`researchFile`, `comparableResearchBody`, `deriveResearchTitle`,
`collectOneResearchFile`, `collectResearchFiles`) is UNCHANGED — they all operate on
`*ProjectEntry` values or `pname`/`entry.Path` already passed in, not on manifest map
keys.

### 3.4 `cmd/scribe/codex.go`

`discoverCodex` (`codex.go:281-356`): mirror the `discover` rewrite exactly (its own
doc comment at `codex.go:275-280` already says it "mirrors the Claude branch"). Replace
lines 311-329 (the `pname := projectName(cwd)` block through the collision-refusal)
with the same canonical-path-keyed pattern as §3.3's `discover`, and the entry-creation
block at lines 340-348 accordingly (set `Path: canon, Name: pname`).

### 3.5 `cmd/scribe/projects.go`

- **`ProjectsListCmd.Run()`** (`projects.go:34-73`): iterate `manifest.Projects` by
  value, sort by `e.Name` (not the raw map key), display `e.Name` instead of the old
  `name` loop variable:
  ```go
  type projectRow struct {
  	key string
  	e   *ProjectEntry
  }
  rows := make([]projectRow, 0, len(manifest.Projects))
  for key, e := range manifest.Projects {
  	rows = append(rows, projectRow{key, e})
  }
  sort.Slice(rows, func(i, j int) bool { return rows[i].e.Name < rows[j].e.Name })
  // ... loop over rows, fmt.Fprintf using r.e.Name, r.e.Domain, r.e.Path ...
  ```
- **`approveProject(root string, manifest *Manifest, key string) error`**
  (`projects.go:129-140`): parameter renamed `name`→`key` (still a raw map lookup,
  unchanged body except the `ensureRepoYAML` call now passes `e.Name` where it used to
  pass `name` — same value in practice, just sourced from the field now).
- **`ProjectsApproveCmd.run`** (`projects.go:102-123`): resolve CLI-typed names to
  canonical-path keys before the approve loop:
  ```go
  keys := c.Names
  if c.All {
  	keys = manifest.pendingProjects() // already canonical-path keys, sorted by Name
  } else {
  	resolved := make([]string, 0, len(keys))
  	for _, arg := range keys {
  		e, err := manifest.resolve(arg)
  		if err != nil {
  			return err
  		}
  		resolved = append(resolved, e.Path)
  	}
  	keys = resolved
  }
  if len(keys) == 0 {
  	return errors.New("nothing to approve — pass project name(s) or --all (see `scribe projects list --pending`)")
  }
  for _, key := range keys {
  	if err := approveProject(root, manifest, key); err != nil {
  		return err
  	}
  	fmt.Printf("approved %s\n", manifest.Projects[key].Name)
  }
  return manifest.save()
  ```
- **`ProjectsIgnoreCmd.run`** (`projects.go:154-169`):
  ```go
  for _, arg := range c.Names {
  	e, err := manifest.resolve(arg)
  	if err != nil {
  		return err
  	}
  	manifest.ignoreProject(e.Path)
  	fmt.Printf("ignored %s (path blocked from re-discovery)\n", e.Name)
  	printOrphanedArticlesHint(root, e.Name)
  }
  return manifest.save()
  ```
- **`ProjectsReviewCmd.run`** (`projects.go:210-262`): `pending := manifest.pendingProjects()`
  now yields canonical-path keys (already sorted by Name); rename the loop variable
  `name`→`key`, fetch `e := manifest.Projects[key]`, print `e.Name`/`e.Path`/etc.,
  call `approveProject(root, manifest, key)` and `manifest.ignoreProject(key)` /
  `printOrphanedArticlesHint(root, e.Name)` as above.
- `printOrphanedArticlesHint` and `projectArticleCount` themselves are UNCHANGED — only
  their callers now pass `e.Name` explicitly instead of a manifest-key `name` that used
  to already be the display name.

### 3.6 `cmd/scribe/projects_add.go`

`enroll` (`projects_add.go:134-193`): rewrite to key by canonical path; `--name`
becomes a pure relabel (auto-disambiguated via `uniqueName` if it collides) instead of
a hard identity conflict:

```go
func (c *ProjectsAddCmd) enroll(root, enrollPath, worktreeOf string) error {
	manifest, err := loadManifest(root)
	if err != nil {
		return err
	}

	canon := canonicalizePath(enrollPath)
	domain := c.Domain
	if domain == "" {
		domain = manifest.resolveDomain(enrollPath)
	}

	manifest.unignorePath(enrollPath)

	if existing, ok := manifest.Projects[canon]; ok {
		changed := false
		if !existing.IsApproved() {
			existing.Status = statusApproved
			ensureRepoYAML(root, existing.Path, existing.Name, existing.Domain)
			fmt.Printf("approved existing project %s\n", existing.Name)
			changed = true
		}
		if c.Name != "" && c.Name != existing.Name {
			existing.Name = manifest.uniqueName(c.Name, existing.Path)
			changed = true
		}
		if worktreeOf != "" && existing.recordWorktree(worktreeOf) {
			fmt.Printf("recorded worktree %s\n", worktreeOf)
			changed = true
		}
		if !changed {
			fmt.Printf("%s already enrolled (%s)\n", existing.Name, existing.Path)
			return nil
		}
		return manifest.save()
	}

	pname := c.Name
	if pname == "" {
		pname = projectName(enrollPath)
	}
	pname = manifest.uniqueName(pname, enrollPath)

	entry := &ProjectEntry{
		Path:           canon,
		Name:           pname,
		Domain:         domain,
		DiscoveredFrom: "manual",
		Status:         statusApproved,
	}
	if worktreeOf != "" {
		entry.Worktrees = []string{worktreeOf}
	}
	manifest.Projects[canon] = entry
	if err := manifest.save(); err != nil {
		return err
	}
	ensureRepoYAML(root, enrollPath, pname, domain)
	if !hasGit(enrollPath) {
		fmt.Printf("note: %s is not a git repo — enrolled, but extraction records it as no-git\n", enrollPath)
	}
	fmt.Printf("enrolled %s -> %s (domain: %s, approved, via manual)\n", pname, enrollPath, domain)
	return nil
}
```

This **removes** the `"project name %q already maps to %s — pass --name..."` hard
error (`projects_add.go:153-155`) — it's now unreachable by construction (two
different paths can never collide on the map key). This is an intentional, visible
behavior change: run `scribe projects add` on a second same-basename repo and it now
succeeds with an auto-suffixed name instead of erroring. Update
`TestProjectsAdd_NameCollisionErrors` accordingly (§4).

Nothing else in this file changes: `widenSources`, `retrustAfterConfigEdit`,
`appendIncludePath`, `mappingValue`, `scalarNode` are all orthogonal to manifest
identity.

### 3.7 `cmd/scribe/sync.go`

- **`printProjectNames`** (`sync.go:217-231`):
  ```go
  func printProjectNames(manifest *Manifest) {
  	type row struct {
  		name     string
  		approved bool
  	}
  	rows := make([]row, 0, len(manifest.Projects))
  	for _, e := range manifest.Projects {
  		if e != nil {
  			rows = append(rows, row{e.Name, e.IsApproved()})
  		}
  	}
  	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
  	for _, r := range rows {
  		name := r.name
  		if !r.approved {
  			name += " (pending)"
  		}
  		fmt.Println(name)
  	}
  }
  ```
- **`showChanged`** (`sync.go:357-...`):
  ```go
  func (s *SyncCmd) showChanged(manifest *Manifest) error {
  	entry, err := manifest.resolve(s.Changed)
  	if err != nil {
  		return fmt.Errorf("project %q not in manifest — run --discover first", s.Changed)
  	}
  	fmt.Printf("Changed files in %s since %s:\n", entry.Name, coalesce(entry.LastExtracted, "never"))
  	patterns := []string{"*.md", "*.txt", "*.exs", "*.ex"}
  	for _, f := range gitChangedFiles(entry.Path, entry.LastSHA, patterns) {
  		fmt.Println(f)
  	}
  	return nil
  }
  ```
  (Original error wording preserved verbatim; only the lookup mechanism changes.)

### 3.8 `cmd/scribe/sync_extract.go`

**Critical correctness point**: `extractProject`'s `pname` parameter
(`sync_extract.go:288`) is used to build `output/scan-<pname>.md` and is passed into
the extraction LLM prompt for KB directory naming — it must always be fed
`entry.Name` (a short display label), **never** the canonical-path map key. This is
the single easiest mistake in an otherwise-mechanical find-replace across this file:
getting it wrong doesn't fail to compile, it silently corrupts wiki directory/file
naming with full filesystem paths.

**`projectsNeedingExtraction`** (`sync_extract.go:155-...`): resolve `s.Extract` once
up front to a `*ProjectEntry`, filter by pointer identity, keep the returned slice as
canonical-path keys (unchanged contract — callers already do `manifest.Projects[key]`):

```go
func (s *SyncCmd) projectsNeedingExtraction(root string, manifest *Manifest) []string {
	var result []string
	ledger := loadLedger(root)

	var filterEntry *ProjectEntry
	if s.Extract != "" {
		e, err := manifest.resolve(s.Extract)
		if err != nil {
			logMsg("sync", "%v", err)
			return nil
		}
		filterEntry = e
	}

	for key, entry := range manifest.Projects {
		if filterEntry != nil && entry != filterEntry {
			continue
		}
		if !entry.IsApproved() {
			if filterEntry == entry {
				logMsg("sync", " [%s] pending approval — run `scribe projects approve %s` first", entry.Name, entry.Name)
			}
			continue
		}
		if !dirExists(entry.Path) {
			continue
		}
		if withinScribeKB(entry.Path) {
			logMsg("sync", " [%s] is (inside) a scribe KB — skipping (KBs never harvest themselves or each other)", entry.Name)
			continue
		}
		if s.Force {
			result = append(result, key)
			continue
		}
		// ... existing ledger/SHA comparison logic below this point (sync_extract.go:195+)
		// is UNCHANGED except every remaining `pname` reference in a log line
		// becomes `entry.Name`, and every `result = append(result, pname)`
		// becomes `result = append(result, key)`.
	}
	return result
}
```

**`extract`** (`sync_extract.go:37-...`): rename the loop variable from `pname` to
`key` everywhere it indexes `manifest.Projects`, and introduce `entry.Name` for every
log line and for the `extractProject` call:

```go
for _, key := range toExtract[s.Max:] {
	logMsg("sync", " [%s] deferred (max %d reached, will extract next run)", manifest.Projects[key].Name, s.Max)
}
...
for _, key := range toExtract {
	entry := manifest.Projects[key]
	g.Go(func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		changed := gitChangedFiles(entry.Path, entry.LastSHA, extractScanPatterns)
		if exceedsExtractFileCap(cfg, len(changed)) {
			logMsg("sync", " [%s] SKIP: %d files > sync.max_extract_files (%d). Run: scribe deep %s",
				entry.Name, len(changed), cfg.Sync.MaxExtractFiles, entry.Name)
			return nil
		}
		logMsg("sync", " [%s] extracting (%d files to scan) from %s", entry.Name, len(changed), entry.Path)
		if err := s.extractProject(root, manifest, entry.Name, entry, changed); err != nil {
			// ... rate-limit / budget / generic-failure branches: replace every
			// `pname` with `entry.Name` in the log format strings, logic unchanged
		}
		mu.Lock()
		extracted++
		mu.Unlock()
		logMsg("sync", " [%s] done", entry.Name)
		return nil
	})
}
```

The `DryRun` branch (`sync_extract.go:64-75`) gets the same `pname`→`key`/`entry.Name`
split. `extractProject`'s own body (`sync_extract.go:288+`) is UNCHANGED — it already
receives `pname` as a parameter and only needs the CALLER to pass the right value now.

### 3.9 `cmd/scribe/assess.go` and `cmd/scribe/deep.go`

Both: replace `manifest.Projects[a.Project]` / `manifest.Projects[d.Project]` with
`manifest.resolve(...)`, preserving the exact existing error message:

```go
// assess.go:73-76
entry, err := manifest.resolve(a.Project)
if err != nil {
	return fmt.Errorf("project %q not in manifest — run 'scribe sync --discover' first", a.Project)
}
```
```go
// deep.go:51-54
entry, err := manifest.resolve(d.Project)
if err != nil {
	return fmt.Errorf("project %q not in manifest — run 'scribe sync --discover' first", d.Project)
}
```

Everything downstream in both files already operates on `entry` (a `*ProjectEntry`),
not on the manifest key — no further changes needed in either file.

### 3.10 `cmd/scribe/doctor.go`

`checkState` (`doctor.go:812-880`): three loops need their displayed identifier
switched from the old name-keyed loop variable to `entry.Name`:

- `pendingProjects()` names list (`doctor.go:822-831`): map each returned key through
  `m.Projects[key].Name` before truncating/joining.
- `kbProjects` loop (`doctor.go:838-843`): `for _, entry := range m.Projects { if
  entry != nil && withinScribeKB(entry.Path) { kbProjects = append(kbProjects,
  entry.Name) } }`.
- `worktreeProjects` loop (`doctor.go:857-865`): same pattern, `entry.Name` instead of
  the old `pname` key.

`checkState`'s own summary line (`doctor.go:816-819`, `"%d projects", len(m.Projects)`)
is unchanged. No other part of `doctor.go` touches manifest identity.

### 3.11 Not touched, and why

- `cmd/scribe/ledger.go`, `cmd/scribe/gitmerge.go`, `cmd/scribe/special_files.go` —
  §2.6: the ledger's identity model is already correct and independent; the manifest's
  git-merge class (`classMachineLocal`) is unaffected by what its keys look like.
- `cmd/scribe/status.go` (`countScopedPendingSessions`), `cmd/scribe/sync_sessions.go`
  (`preFilterSessions`) — both already call `entryForPath`, whose external contract is
  unchanged (§3.1 step 10 only changes its internals).
- `cmd/scribe/init.go` (`seedProjectsJSON`, `init.go:544-550`) — deliberately left
  emitting `{"domain_aliases":{},"ignored_paths":[],"projects":{}}` with no
  `manifest_version` key. A brand-new empty manifest migrates for free (0 entries, 0
  cost) the first time anything loads it; hardcoding the version number in a second
  place (the init template) would only create a place for the two constants to drift.
- `IgnoredPaths` matching (`manifest.go:214`, `isIgnored`'s `slices.Contains`, and
  `unignorePath`, `manifest.go:97-104`) stays exact-string-match, uncanonicalized.
  Noted, not fixed — see Risks §5.6.

---

## 4. Test plan

All new/updated tests use `t.TempDir()` KB fixtures per existing convention
(`worktree_test.go`'s `initRepoWithWorktree`/`initTestGitRepo` pattern, `codex_test.go`'s
`makeProjectWithMarkdown`). No network access; `make test -tags sqlite_fts5` must stay
green offline.

| # | Test | File | Scenario | Assertion |
|---|------|------|----------|-----------|
| 1 | `TestCanonicalizePath` | `manifest_test.go` (new) | real dir, dir with a symlinked ancestor, non-existent dir, relative + `~`-prefixed input | resolves through symlinks when it exists; falls back to cleaned-abs when `EvalSymlinks` fails; always returns an absolute path |
| 2 | `TestManifestMigrateToPathKeys_Basic` | `manifest_test.go` (new) | legacy fixture: `{"projects":{"scriptorium":{"path":"...","last_sha":"abc"}}}`, no `manifest_version` (same shape as the existing `TestLoadManifest` fixture, `manifest_test.go:266-276`) | after `loadManifest`, `Projects` keyed by `canonicalizePath(path)`; entry's `Name == "scriptorium"`; `ManifestVersion == manifestPathKeyedVersion` in memory |
| 3 | `TestManifestMigrateToPathKeys_CollapsesDuplicates` | `manifest_test.go` (new) | two legacy entries with different old names whose `Path` canonicalizes to the same real dir (one via a symlinked path, or literal duplicate), one with `LastExtracted` set and one without, different `Worktrees` on each | exactly one surviving entry, keyed once; `Name` is the one with `LastExtracted` set; `Worktrees` is the union of both |
| 4 | `TestManifestMigrateToPathKeys_Idempotent` | `manifest_test.go` (new) | load legacy fixture, call `save()`, reload, call `migrateToPathKeys()` again directly | second call is a no-op (`ManifestVersion` unchanged, `Projects` unchanged, no re-log — assert via a second `save()` producing a byte-identical file) |
| 5 | `TestLoadManifest_ReadOnlyDoesNotWriteMigratedFile` | `manifest_test.go` (new) | write a legacy fixture file, call `loadManifest` only (no `save()`) | on-disk file bytes unchanged after `loadManifest` returns (mirrors `TestLoadConfigIsPure`'s pattern, `readonly_contract_test.go:74-89`) |
| 6 | `TestManifestSave_LogsMigrationOnce` | `manifest_test.go` (new) | load legacy fixture, call `save()` twice in a row | `migratedCount` is 0 after the first `save()` (verify via a second `save()` not re-triggering the log branch — capture via a package-level log hook if one exists, else assert `m.migratedCount == 0` directly after the first `save()`) |
| 7 | `TestManifestResolve_ByPath` | `manifest_test.go` (new) | one entry, look up by its canonical path, by a non-canonical spelling (`.`-relative or symlinked) of the same path | both resolve to the same entry |
| 8 | `TestManifestResolve_ByName` | `manifest_test.go` (new) | one entry, look up by `Name` | resolves |
| 9 | `TestManifestResolve_AmbiguousName` | `manifest_test.go` (new) | two entries that (through a manually-crafted fixture) share a `Name` | error mentions both paths and says "ambiguous" |
| 10 | `TestManifestResolve_NotFound` | `manifest_test.go` (new) | empty manifest | error mentions `scribe projects list` |
| 11 | `TestManifestUniqueName` | `manifest_test.go` (new) | no existing entries (passthrough); one existing entry with `Name: "api"` at a different path, new candidate path whose parent also disambiguates to `"api"`; a pathological case where even `parent-api` collides (seed manifest with `api` and `parent-api` both taken) | passthrough case returns `base` unchanged; collision case returns the parent-qualified form; double-collision case returns `api-2` |
| 12 | `TestEntryForPath` (existing, update) | `worktree_test.go:421-454` | change fixture construction from `map[string]*ProjectEntry{"projects-api": entry}` to `map[string]*ProjectEntry{canonicalizePath(mainDir): entry}` | same assertions as today (main path, worktree path, unrelated path, basename-colliding-but-different-path all behave identically) |
| 13 | `TestFoldWorktreeIntoExistingEntry` (existing, update) | `worktree_test.go:126-165` | key the seed manifest by `canonicalizePath(main)` with `Name: pname` instead of `pname: {...}` | same assertions, using the canonical key to index `m.Projects` |
| 14 | `TestFoldWorktreeDiscoversMain` (existing, update) | `worktree_test.go:167-199` | same key-construction update | `entry := m.Projects[canonicalizePath(derived)]`; same field assertions |
| 15 | `TestFoldWorktreeSharedBasenameGetsUniqueName` (**replaces** `TestFoldWorktreeBasenameCollision`) | `worktree_test.go:201-232` | seed manifest has an unrelated project at `other` whose `Name == projectName(main)`; fold a worktree whose derived main is a *different* real path with the same basename | fold now SUCCEEDS (`n==1, changed==true`, inverting today's assertion); a NEW entry is created at `canonicalizePath(main)` with a `Name` distinct from `other`'s (e.g. parent-qualified); `other`'s entry (path, `Worktrees`) is byte-for-byte untouched |
| 16 | `TestFoldWorktreeFromSubdir` (existing, update) | `worktree_test.go:234-265` | key-construction update only | same assertions |
| 17 | `TestFoldWorktreeSkipsLegacyWorktreeEntry` (existing, update) | `worktree_test.go:267-289` | both entries keyed by their own canonical paths | same assertions |
| 18 | `TestFoldWorktreeDryRun` (existing, update) | `worktree_test.go:291-310` | key-construction update only | same assertions |
| 19 | `TestCollectDropFilesFromWorktree` / `TestCollectResearchFilesFromWorktree` (existing, update) | `worktree_test.go:312-383` | key-construction update only (these test `collectDropFiles`/`collectResearchFiles`, which iterate `manifest.Projects` by value and use `pname`/`entry.Path` already — only the seed `map[string]*ProjectEntry{...}` literal needs `canonicalizePath(main)` as the key plus `Name: pname` on the entry) | same assertions; staged file path still uses `pname` (now sourced from `entry.Name` in the updated `collectDropFiles`/`collectResearchFiles` call sites — no change needed there since those functions already take `pname` as a loop variable over the map, which callers derive from `entry.Name` per §3.3's note that those two collection functions are UNCHANGED — verify the test still passes by iterating `pname, entry := range manifest.Projects` and using `entry.Name` for the staged-path assertion instead of the loop key) |
| 20 | `TestDoctorWarnsOnWorktreeProjectEntry` (existing, update) | `worktree_test.go:385-419` | key by canonical path, `Name: projectName(...)` set explicitly | same assertions (`found.Detail` still contains `projectName(wt)`, now sourced from `Name`) |
| 21 | `TestRecordWorktree` (existing, extend) | `worktree_test.go:74-100` | add one sub-case: record a worktree path and its symlink-equivalent spelling | second call reports no-change (today's raw string-equality version can't express this case) |
| 22 | `TestDiscoverCodex_AddsNewProjects` (existing, update) | `codex_test.go:232-305` | seed manifest via `loadManifest` off a freshly-seeded empty file (already the pattern) — no fixture change needed, only the `for pname, entry := range manifest.Projects` assertion loop needs `pname` renamed (it's now a path) with the `DiscoveredFrom` check unaffected | same pass/fail behavior |
| 23 | `TestDiscoverCodex_PromotesClaudeOnlyToBoth` (existing, update — **this is the natural legacy-manifest migration fixture**) | `codex_test.go:307-353` | pre-seeds a raw legacy manifest JSON string (name-keyed, no `manifest_version`, `codex_test.go:328`) exactly like a real pre-upgrade file | `manifest.Projects[pname]` (`codex_test.go:346`) must change to `manifest.Projects[canonicalizePath(cwd)]` (or `manifest.resolve(pname)`); same `DiscoveredFrom == "both"` assertion |
| 24 | `TestDiscoverCodex_SameBasenameBothEnroll` (new) | `codex_test.go` | two Codex-discovered cwds under different parent dirs that derive the same basename (e.g. two `Projects/<x>/api` trees under different roots so `projectName` collides) | both get discovered as distinct, approved-or-pending entries; their `Name`s differ; `len(manifest.Projects) == 2` |
| 25 | `TestProjectsAdd_SameBasenameGetsUniqueName` (**replaces** `TestProjectsAdd_NameCollisionErrors`) | `projects_add_test.go:243-255` | enroll `a := makeProjectDir(t, "dup")`, then enroll `b := makeProjectDir(t, "dup")` (different parent) | both `run()` calls return `nil` (no error); manifest ends up with 2 entries; their `Name`s differ (one is `"dup"`, the other parent-qualified or suffixed) |
| 26 | `TestProjectsAdd_RenameViaName` (new) | `projects_add_test.go` | enroll a project, then `ProjectsAddCmd{Path: samePath, Name: "custom"}.run()` | existing entry's `Name` updates to `"custom"` (or a `uniqueName`-suffixed variant if `"custom"` happens to collide); still one entry, same canonical-path key |
| 27 | `TestProjectsApprove_ByNameAndByPath` (new) | `projects_test.go` | one pending project; approve by typed `Name`, and separately (new manifest) by full path | both succeed via `manifest.resolve` |
| 28 | `TestProjectsList_SortsByName` (new or extend existing) | `projects_test.go` | two entries whose canonical-path keys sort differently than their `Name`s | list output order follows `Name`, not path |
| 29 | `sync_extract_test.go` existing tests referencing `manifest.Projects[<name>]` fixtures (audit and update) | `sync_extract_test.go` | any fixture built as `map[string]*ProjectEntry{"myproj": {...}}` | rekey to `canonicalizePath(path)` + `Name: "myproj"`; assert `extractProject` receives `entry.Name`, not the path, as its `pname` argument (add an assertion/spy if none exists today — this is the regression guard for the §3.8 "critical correctness point") |
| 30 | `TestAssess_ResolvesProjectByPath` / `TestDeep_ResolvesProjectByPath` (new, small) | `assess_test.go`, `deep_run_test.go` | seed a manifest, call with `a.Project`/`d.Project` set to the entry's full path instead of its name | resolves the same entry (new capability, cheap to cover) |

Run `make test -tags sqlite_fts5` (per `Makefile`) after each file's edits; `make
check` (test + vet) before considering the branch done, per `CLAUDE.md`'s Build
section. No new `go.mod` dependency is introduced anywhere in this plan.

---

## 5. Risks & edge cases

1. **Stale/deleted project directories.** Handled by `canonicalizePath`'s
   `EvalSymlinks`-failure fallback to the cleaned absolute path (§2.1) — a
   since-removed project stays addressable by its last-known key. `doctor`'s existing
   `dirExists` gates (`doctor.go`'s worktree-projects loop, `sync_extract.go:177`,
   etc.) are untouched and keep flagging missing directories exactly as before.
2. **Half-migrated on-disk state.** Provably impossible — see §2.4's atomic-write
   argument (migration only ever mutates in-memory state; the only disk write is the
   pre-existing atomic tmp+rename in `save()`).
3. **Duplicate legacy entries collapsing on migration.** Deterministic: newest
   `LastExtracted` wins, ties broken by old name; loser's `Worktrees` are unioned in,
   not dropped, so no drop/research collection silently stops; exactly one summary log
   line per collapsed group (not one per project) to keep cron stdout terse per
   `CLAUDE.md`'s "Errors are logged to stderr, summarized to stdout... keep it terse."
4. **A teammate on an older scribe binary reading a manifest a newer binary already
   migrated and saved.** Only reachable in the narrow combination: solo (non-team) KB
   + multiple machines + a committed (not gitignored) `scripts/projects.json` — team
   KBs never share this file at all (§2.6, point 1). The old binary's JSON unmarshal
   doesn't crash (Go's `map[string]T` doesn't care what the keys look like), but its
   `manifest.Projects[projectName(path)]` lookups (basename-derived) will almost never
   hit path-shaped keys, so it re-discovers already-known projects under new
   basename-derived keys — a real but narrow regression. There is no version-negotiation
   mechanism anywhere in this codebase to build a guard against this affordably (no
   existing precedent for a binary refusing to run against a "too new" file format).
   **Mitigation: document, don't code.** Add a note to `CHANGELOG.md` for this release
   stating that machines sharing a committed manifest must upgrade together. Building a
   compatibility shim for a combination this narrow would be the kind of unjustified
   scope growth `CLAUDE.md`'s "New dependencies need justification" spirit argues
   against even though no dependency is involved — it's still complexity with no
   evidence anyone hits it.
5. **`scripts/projects.json` conflict on a solo multi-machine committed KB.** Already
   possible today (`classMachineLocal` is not in `derivedRegenerable` or
   `semanticMergers`, `special_files.go:46-56`; `autoResolveDerivedConflicts` aborts on
   it, `gitops.go:154-158`). This plan changes what the file's *contents* look like but
   not its git-merge class or the existing abort-and-ask-a-human behavior. Explicitly
   out of scope — not one of the "3+ distinct bugs" the issue names, and fixing git
   merge semantics for a machine-local file is a different, larger design question.
6. **`IgnoredPaths` / `isIgnored` stay exact-string-match, not canonicalized**
   (`manifest.go:97-104,214`). Noticed during this investigation, deliberately not
   touched — it predates and is orthogonal to the basename-keying bug this issue is
   about (discovery already passes real, `dirExists`-verified filesystem paths into
   `isIgnored`, so the exact-match gap is a much narrower, separate risk: only bites if
   the *same* path is spelled two different ways across two discovery runs, e.g. via a
   symlink change). Flagging so a reviewer doesn't read the silence as an oversight.
7. **The `extractProject`/`pname` naming trap** (§3.8). Called out as the single
   highest-risk mechanical-edit mistake in this plan: a find-replace that swaps every
   `pname` for the new canonical-path loop variable *without* special-casing the
   `extractProject` call and its surrounding log lines would silently write
   `output/scan-/Users/x/Projects/foo.md`-shaped filenames and feed a full filesystem
   path into KB directory naming — no compiler error, no test failure unless test #29
   (§4) is actually added and actually asserts on the argument value, not just on
   `extractProject` being called.
8. **Performance.** Net neutral-to-positive: `entryForPath`'s common case moves from an
   O(n) `samePath` scan (today) to an O(1) map hit; its worktree fallback stays an O(n)
   scan over a typically-tiny worktree list, same as today. No new I/O, no new
   `go.mod` dependency, no change to `sqlite_fts5`/CGO requirements.

---

## 6. Interactions with other issues

- **#41/#28 (`scribe projects add`) — already shipped** (`1c73501`, confirmed by
  `projects_add.go` existing in full on `main` and by `docs/issues-master-plan.md`
  line 22-23 listing it as closed). This plan modifies its already-landed `enroll()`
  function in place (§3.6); there is no coordination needed because there is no
  concurrent work on that file — it's finished, not in flight.
- **#26 (KB registry) — already shipped** (`29edaff` + follow-ups, per
  `docs/issues-master-plan.md` line 21, also confirmed by reading `registry.go` for
  this plan). `registry.go` manages a *different* file (`~/.config/scribe/config.yaml`'s
  `kbs:` list — KB *roots*, not per-project manifest entries) and already uses its own
  `samePath`-based comparisons (`registry.go:50,54,147`), independent of
  `Manifest.Projects`. **No code coupling exists or is created by this plan.** If a
  future issue wants the registry to canonicalize its own comparisons the way this plan
  canonicalizes the project manifest's, `canonicalizePath` (manifest.go) is the function
  to reuse for consistency — not a hard dependency, just a naming/behavior precedent
  worth following.
- **#23, #24** — per `docs/issues-master-plan.md`'s merge strategy (§"Merge strategy"),
  Phase 4 issues (#8, #23, #24) each get their **own worktree branch**, merged
  sequentially into local `main` with `make check` after each merge — they are not a
  single combined branch. This plan does not depend on #23 or #24's contents and
  found no reference to manifest identity in `docs/issue-23-adoption-metric-plan.md`
  during this research. The only shared surface is `main.go`'s command table and
  whatever files #23/#24 also touch — ordinary sequential-merge conflict risk, not a
  design dependency. No action needed from this plan beyond landing cleanly on its own
  branch.
- **Ledger (`ledger.go`) — confirmed independent, no migration needed** (§2.6, §3.11).

---

## 7. Size estimate

**M** — approximately 550-700 changed/added lines across 10 source files and 6 test
files, no new `go.mod` dependency, no new subsystem or JSON file:

| Bucket | Files | Approx. LOC |
|---|---|---|
| Core identity primitives (`canonicalizePath`, migration, `resolve`, `uniqueName`) | `manifest.go` | ~180 new |
| Call-site rewrites (mostly net-negative — deletes collision-refusal branches) | `sync_discover.go`, `codex.go`, `worktree.go` | ~110 changed |
| CLI command rewrites | `projects.go`, `projects_add.go`, `sync.go`, `sync_extract.go`, `assess.go`, `deep.go` | ~180 changed |
| Display-only fixes | `doctor.go` | ~15 changed |
| Tests (new + rewritten fixtures) | `manifest_test.go`, `worktree_test.go`, `codex_test.go`, `projects_add_test.go`, `projects_test.go`, `sync_extract_test.go`, `assess_test.go`, `deep_run_test.go` | ~300-350 |

No architectural risk (single well-understood identity primitive threaded through
existing call sites); the size driver is breadth (many small call sites), not depth.
