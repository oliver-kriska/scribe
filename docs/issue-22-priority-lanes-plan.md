# Issue #22 — Priority lanes for the pending-session queue (roadmap-03)

Part of roadmap umbrella #6, second in suggested order. Base commit: `49bfd53`.

This plan is self-sufficient: it does not assume you have read the GitHub
issue, `.claude/research/2026-06-11-roadmap-03-priority-lanes-pending-queue.md`,
or any other conversation. Every design decision below is settled — implement
it as written rather than re-deriving the design. Cite `file.go:line` from
this plan when you need to locate something; line numbers are as of base
commit `49bfd53`.

## 1. Problem & context

`scribe` mines Claude Code sessions from ccrider's FTS5 index into KB
articles. Two independent producers feed session IDs into a shared queue
file, and one consumer drains it:

- **Producers**: `SessionEndHookCmd.Run` (`cmd/scribe/hook.go:66`) fires
  when a Claude Code session ends and appends a line to
  `~/.config/scribe/pending-sessions.txt` if the session clears a cheap
  message-count + FTS5-score bar. `scanForCandidates` in `cmd/scribe/watch.go`
  (the `scribe watch` background daemon, used for Codex CLI sessions which
  have no SessionEnd hook) does the same via `appendPending`
  (`cmd/scribe/watch.go:289`).
- **Consumer**: `(s *SyncCmd) mineSessions` (`cmd/scribe/sync_sessions.go:548`)
  drains the file via `readAndClearPendingSessions` (`cmd/scribe/hook.go:409`),
  merges the drained IDs **ahead of** whatever `scribe triage` would pick next
  (`cmd/scribe/sync_sessions.go:625-641`), then hands the merged list to
  `mineSessionBatches`.

Today's queue is a flat FIFO: whichever session got appended to the file
first (or whichever `scribe triage` ranks first among the non-queued
remainder) mines first, full stop. Arrival order — not actual value — decides
what gets processed when the queue is long.

Separately, `mineSessions` hardcodes **two size-based lanes**:

- **Normal** (`message_count <= 300`): batched, `cfg.Sync.ParallelExtractions`
  concurrent, 10-minute timeout each, budget = `s.SessionsMax` (CLI flag
  `--sessions-max`, default 3).
- **Large** (`message_count > 300`): one at a time (concurrent writes to the
  wiki aren't safe), 20-minute timeout each, budget =
  `(s *SyncCmd) largeSessionBudget()` (`cmd/scribe/sync_sessions.go:539`) =
  `max(1, s.SessionsMax/3)`, **on top of** the normal budget.

GitHub issue #22: *"Replace the single FIFO `pending-sessions.txt` with
scored priority lanes, so high-signal sessions mine first regardless of
arrival order. This subsumes the current normal/large two-lane budget in
`sync --sessions`... which was kept honest-but-simple in PR #1 precisely
because this redesign was coming."*

There is a 2026-06-11 research doc
(`.claude/research/2026-06-11-roadmap-03-priority-lanes-pending-queue.md`)
that sketched a **different** two-lane idea: lanes derived from
`subscriptions.domains`/`owners` (a *team-relevance* axis, for surfacing
teammates' preferred domains first). That feature (`SubscriptionsConfig`,
`cmd/scribe/subscriptions.go`) exists today but only governs which *pulled
articles* get logged/notified after `git pull` — it has no session→domain
mapping, and building one is explicitly listed as still-to-do in that doc.
**This plan does not adopt that design.** The issue text and every
implementation instruction reference *score* and the *normal/large size
split* — not subscriptions — and building a session→domain mapping would add
a new config surface (`sources.repos[].domain` or similar) this issue was
never scoped for. The research doc is prior art on the *interleave/no-starve*
shape of the solution (see §2.4), not on the axis lanes are built along.
Domain-based lanes remain a valid, separate future idea; not in scope here.

## 2. Design decisions

### 2.1 Lane axis: score, not arrival order or domain

Two lanes, **Hot** and **Normal**, split by a score threshold on the same
FTS5 knowledge-density score `scribe triage` and the hook already compute
(`runScoring` in `cmd/scribe/triage.go:127`, `quickScoreSession` in
`cmd/scribe/hook.go:275` — these two scorers are already required to stay in
lockstep per the comment at `hook.go:267-274`; this plan does not change
that).

- **Hot**: `score >= cfg.PriorityLanes.HotThreshold` (default `90`).
- **Normal**: everything else.

**Why a fixed threshold, not top-K-per-run:** top-K only has meaning relative
to whatever happens to be queued *this run* — the score that qualifies as
"top" shifts every cycle based on unrelated volume, so it can't be described
or tested as a stable lane. A score is either clearly valuable in isolation
or it isn't; a threshold lets `scribe.yaml` document what "valuable" means
for this KB (mirrors the existing `hook.go` `MinScore` default of 50 — Hot at
90 means "well above the bar that got you queued at all", not an arbitrary
second number).

**Why exactly two lanes, not three+:** the two-lane precedent already exists
(normal/large) and is what the issue says this subsumes. More tiers add
config surface and edge cases with no requested use case. The threshold is a
config field (`priority_lanes.hot_threshold`), so a KB that wants more
granularity later can still get it without a redesign — it would just need
another PR, not a different queue format.

### 2.2 Queue file format: 4-column TSV, tolerant of 3 legacy shapes

New format written by every producer going forward:

```
sessionID\tscore\tmsgCount\tISO8601timestamp\n
```

This inserts `msgCount` as the third field and pushes the timestamp to
fourth. `msgCount` is embedded directly (not re-queried at drain time)
because it is what size-pool partitioning needs (§2.3), and both current
producers already have it in scope for free: `hook.go`'s `Run` already reads
`msgCount` via `queryMessageCount` at `hook.go:111` (currently used only for
the `MinMessages` gate and the verbose log at `hook.go:167`); `watch.go`'s
`scanForCandidates` already scans `c.msgs` per candidate (`watch.go:215`).

**Back-compat / migration rule** (exact, deterministic, no separate
migration step needed — old lines only exist as leftovers from *before* the
binary upgrade, since every producer writes the new format from the moment
this ships):

Parse each line by splitting on `\t`, then by field count:

| Fields | Meaning | Rule |
| --- | --- | --- |
| 4+ | `id, score, msgCount, timestamp` (current format) | `score`/`msgCount` parsed as int; on parse failure of either, fall back to that field's "unknown" sentinel below |
| 3, and field[2] parses as an int | `id, score, msgCount` (no timestamp) | defensive case; treat as unknown timestamp |
| 3, and field[2] does **not** parse as an int | `id, score, timestamp` (the format shipped before this change — the common migration case) | `msgCount` unknown (`-1`) |
| 2 | `id, score` | `msgCount`, timestamp unknown |
| 1 | `id` (oldest legacy / hand-written) | `score`, `msgCount`, timestamp unknown |
| 0 (blank) | — | skip line entirely |

"Unknown" sentinels: `Score` unset → `HasScore=false` (treated as `0` for
lane math, see §2.4); `MsgCount` unset → `-1`; timestamp unset →
`HasEnqueuedAt=false`.

**One extra rule for the pure single-column legacy shape** (row "Fields: 1"
above): mark it `LegacyUnknownAge: true`. This is the only shape with *no*
signal at all about when it was queued, and existing pre-upgrade files can
realistically contain it (`hook.go`'s own `parsePendingLine` already treats
bare IDs as valid — see `hook.go:335-344` — so it is not hypothetical).
Treating it as "maximally old" (§2.4) means any such leftover clears on the
very first post-upgrade drain instead of silently rotting because it can
never satisfy a numeric age comparison.

### 2.3 Large-session budget: kept as an outer size cap, not folded into score

The issue says priority lanes "subsume" the normal/large two-lane budget.
This does **not** mean deleting the size distinction — large sessions are
capped because they cost 2x the wall-clock time and must run serially
(concurrent wiki writes aren't safe, see the comment above
`mineSessionsSerial` at `sync_sessions.go:480`), which is a *capacity safety*
concern, orthogonal to *relevance*. Merging two orthogonal concerns into one
number is the exact mistake the 2026-06-11 research doc's "why not
score-based priority instead" section warns against for the domain axis; the
same logic applies here for the size axis.

**Settled design:** two independent size pools, each with its own budget
(`s.SessionsMax` for normal, `s.largeSessionBudget()` — **unchanged**,
`sync_sessions.go:539` — for large), and **each pool internally ordered by
the same Hot/Normal priority-lane logic**. This is what "subsumes" means
here: the old two lanes were pure size-based FIFO/score-desc; the new two
*pools* keep the size-based caps (still necessary for wall-clock safety) but
replace the flat ordering inside each with real priority-lane semantics.
`largeSessionBudget()` itself is not touched — a giant, very-high-scoring
backlog still cannot consume more than `max(1, SessionsMax/3)` slots per run,
which is the literal mechanism that prevents giant sessions from starving
the run budget.

### 2.4 Starvation guarantee: floor-reservation admission + aging promotion

Realistic budgets are tiny (`SessionsMax` defaults to 3; the large pool's
budget defaults to `max(1, 3/3) = 1`). A literal interleave pattern (e.g. "7
Hot then 3 Normal, repeat") is meaningless at single-digit scale — with a
budget of 3, every slot falls inside the first block of any 7:3 pattern, so
short runs would always look 100% Hot. Two mechanisms fix this at any scale:

**A. Floor-reservation at admission time** (this is the "interleave", just
expressed as slot reservation instead of a positional pattern — equivalent
at any run length, and testable independent of budget size):

```go
// splitBudgetByLane reserves floor(budget * normalRatio), rounded to the
// nearest integer, for the Normal lane, so Normal always gets *some* slots
// on a run whose budget is nonzero — never zero slots just because Hot had
// enough candidates to fill the whole run. Ties round up (0.5 -> 1) so a
// budget of 1 or 2 with a nonzero ratio still occasionally favors Normal
// rather than deterministically starving it every single run.
func splitBudgetByLane(budget int, normalRatio float64) (hotSlots, normalSlots int) {
    normalSlots = int(math.Round(float64(budget) * normalRatio))
    if normalSlots > budget {
        normalSlots = budget
    }
    return budget - normalSlots, normalSlots
}
```

`normalRatio = 0.3` (70/30 Hot/Normal), reusing the exact ratio the
2026-06-11 research doc proposed for its domain-axis lanes ("Drain
interleaved 70/30 (A/B) per batch... The interleave ratio can be a constant
first") — same shape of problem, same defensible default, avoids inventing a
second arbitrary constant.

Admission: take up to `hotSlots` from the Hot lane (already sorted, see
below) and up to `normalSlots` from the Normal lane. If either lane has
fewer candidates than its slot count, **backfill the shortfall from the
other lane** (Hot backfills into unused Normal slots first, then vice versa)
so budget is never left on the table just because one lane is thin this run.

```go
// admitByLaneFloor returns up to `budget` entries, reserving normalSlots
// for the Normal lane per splitBudgetByLane, backfilling any shortfall
// from whichever lane still has spare candidates. Input slices must
// already be sorted highest-priority-first (see sortWithinLane).
func admitByLaneFloor(hot, normal []pendingEntry, budget int, normalRatio float64) []pendingEntry {
    hotSlots, normalSlots := splitBudgetByLane(budget, normalRatio)
    admitted := make([]pendingEntry, 0, budget)
    hi, ni := 0, 0
    for hi < len(hot) && hi < hotSlots {
        admitted = append(admitted, hot[hi])
        hi++
    }
    for ni < len(normal) && ni < normalSlots {
        admitted = append(admitted, normal[ni])
        ni++
    }
    // Backfill leftover budget: Hot's unused overflow first, then Normal's.
    for len(admitted) < budget && hi < len(hot) {
        admitted = append(admitted, hot[hi])
        hi++
    }
    for len(admitted) < budget && ni < len(normal) {
        admitted = append(admitted, normal[ni])
        ni++
    }
    return admitted
}
```

**B. Aging promotion** — the actual anti-starvation guarantee for a
*specific old entry*, independent of pool size. Floor-reservation guarantees
Normal gets throughput in general; it does not guarantee any one Normal
entry ever reaches the front if newer Normal entries keep arriving with
equal or higher scores. So: at classification time (not admission time), an
entry moves from Normal into Hot if it's stale, regardless of its raw score:

```go
// classifyLanes buckets candidates into Hot/Normal. An entry is forced
// into Hot if its own score clears the bar OR it has been waiting long
// enough that it should stop waiting on score alone (age promotion) OR
// its age is entirely unknown (the single-column legacy queue format —
// treated as maximally old so any leftover from before this upgrade
// clears on the very next drain instead of silently rotting forever).
func classifyLanes(entries []pendingEntry, cfg PriorityLanesConfig, now time.Time) (hot, normal []pendingEntry) {
    for _, e := range entries {
        forceHot := e.Score >= cfg.HotThreshold || e.LegacyUnknownAge ||
            (e.HasEnqueuedAt && now.Sub(e.EnqueuedAt) >= time.Duration(cfg.AgeDays)*24*time.Hour)
        if forceHot {
            hot = append(hot, e)
        } else {
            normal = append(normal, e)
        }
    }
    sortWithinLane(hot)
    sortWithinLane(normal)
    return hot, normal
}
```

`cfg.PriorityLanes.AgeDays` defaults to `7`.

**Intra-lane sort** (`sortWithinLane`, used only to decide *processing
order* among an already-admitted or already-classified set — never affects
*whether* something is admitted, only in what order): `Score` descending,
tie-broken by `EnqueuedAt` ascending. For the tiebreak, three states:

- `LegacyUnknownAge == true` → sorts as oldest (epoch zero) — first among
  ties.
- `HasEnqueuedAt == true` → sorts by the real timestamp.
- Neither (a fresh `scribe triage` discovery that was never queued —
  see §2.5) → sorts as **newest** (now) — last among ties. This is
  deliberate: a session `scribe triage` just noticed this run has no
  wait-time history, so it should not out-rank a same-scored session that
  has genuinely been sitting in the queue.

### 2.5 Merging pending-queue entries with live `scribe triage` picks

`mineSessions` has always merged two sources per size pool: whatever's
sitting in `pending-sessions.txt` (hook/watch queued) and whatever
`scribe triage` would rank next (live FTS5 query, not queue-based). That
merge stays, but the **old behavior of unconditionally putting pending IDs
ahead of triage IDs is removed** — that was exactly the "arrival order,
not value" behavior issue #22 asks to replace. Under the new design, a
pending-queue entry and a live-triage entry compete on the same Hot/Normal
classification; a pending-queue entry with a mediocre score no longer
automatically wins over a fresh, higher-scoring live-triage pick.

Dedup rule when the same session ID appears in both sources: **the
pending-queue entry wins** (it carries a real `EnqueuedAt` and the score the
hook/watcher computed at enqueue time; the live-triage rediscovery of the
same ID would otherwise silently erase that history and reset it to
"newest" for tiebreak purposes).

### 2.6 `scribe status` queue visibility

Add one new backlog row to `renderBacklog` (`cmd/scribe/status.go:379`)
showing the current (undrained — peek only, exactly like the existing
`--dry-run` peek) lane breakdown:

```
  backlog (run `scribe sync` to process):
    session queue (hooked):  3 hot, 5 normal (1 aged→hot)
```

Only printed when the queue file has entries (mirrors the existing
`if pending, ok := ...` guard pattern already used for the sessions row at
`status.go:447`).

## 3. Implementation steps

### 3.1 `cmd/scribe/hook.go`

1. Add a package-level type near the existing pending-queue helpers
   (below the doc comment block starting at `hook.go:38`, e.g. right above
   `parsePendingLine` at `hook.go:333`):

   ```go
   // pendingEntry is one parsed line from pending-sessions.txt. Producers
   // (hook.go, watch.go) always write the current 4-column format;
   // consumers must tolerate the 3 legacy shapes documented in
   // parsePendingEntry. HasScore/MsgCount==-1/HasEnqueuedAt/
   // LegacyUnknownAge record exactly what was actually present on the
   // line so lane classification (sync_sessions.go) can apply the right
   // fallback instead of guessing from a zero value.
   type pendingEntry struct {
       ID               string
       Score            int
       HasScore         bool
       MsgCount         int // -1 = unknown, backfilled later from ccrider
       EnqueuedAt       time.Time
       HasEnqueuedAt    bool
       LegacyUnknownAge bool // true only for the bare single-column legacy line
   }
   ```

2. Replace `parsePendingLine` (`hook.go:333-344`) with:

   ```go
   // parsePendingEntry parses one pending-sessions.txt line per the format
   // table in docs/issue-22-priority-lanes-plan.md §2.2. Returns ok=false
   // for a blank line.
   func parsePendingEntry(line string) (pendingEntry, bool) {
       line = strings.TrimSpace(line)
       if line == "" {
           return pendingEntry{}, false
       }
       fields := strings.Split(line, "\t")
       e := pendingEntry{ID: fields[0], MsgCount: -1}
       switch {
       case len(fields) >= 4:
           if v, err := strconv.Atoi(fields[1]); err == nil {
               e.Score, e.HasScore = v, true
           }
           if v, err := strconv.Atoi(fields[2]); err == nil {
               e.MsgCount = v
           }
           if t, err := time.Parse(time.RFC3339, fields[3]); err == nil {
               e.EnqueuedAt, e.HasEnqueuedAt = t, true
           }
       case len(fields) == 3:
           if v, err := strconv.Atoi(fields[1]); err == nil {
               e.Score, e.HasScore = v, true
           }
           if v, err := strconv.Atoi(fields[2]); err == nil {
               // 3-column shape with an int in field[2]: msgCount, no timestamp.
               e.MsgCount = v
           } else if t, err := time.Parse(time.RFC3339, fields[2]); err == nil {
               // 3-column legacy shape (current production format): id, score, timestamp.
               e.EnqueuedAt, e.HasEnqueuedAt = t, true
           }
       case len(fields) == 2:
           if v, err := strconv.Atoi(fields[1]); err == nil {
               e.Score, e.HasScore = v, true
           }
       default: // len(fields) == 1: bare ID, oldest legacy shape
           e.LegacyUnknownAge = true
       }
       return e, true
   }

   // parsePendingLine returns just the session ID — the subset every
   // pre-existing caller (pendingContainsID, dedup loops) needs. Kept as a
   // thin wrapper so those callers and their tests are untouched.
   func parsePendingLine(line string) string {
       e, ok := parsePendingEntry(line)
       if !ok {
           return ""
       }
       return e.ID
   }
   ```

   Add `"strconv"` to the import block (`hook.go:3-18`) — not currently
   imported there.

3. Update the SessionEnd write (`hook.go:159-164`) to the 4-column format,
   reusing the already-in-scope `msgCount` from step 5 of `Run`
   (`hook.go:111`):

   ```go
   fmt.Fprintf(f, "%s\t%d\t%d\t%s\n", sessionID, score, msgCount, time.Now().UTC().Format(time.RFC3339))
   ```

   Update the comment block above it (`hook.go:153-154`) to describe 4
   columns: `sessionID<TAB>score<TAB>msgCount<TAB>ISO8601-UTC`.

4. Add entry-returning variants, implemented via one shared scan helper so
   peek/drain logic isn't duplicated:

   ```go
   // scanPendingEntries reads f line by line, returning unique entries in
   // file order (first occurrence of a given ID wins — matches the
   // existing dedup semantics of peekPendingSessions/readAndClearPendingSessions).
   func scanPendingEntries(f *os.File) ([]pendingEntry, error) {
       var out []pendingEntry
       seen := make(map[string]bool)
       sc := bufio.NewScanner(f)
       for sc.Scan() {
           e, ok := parsePendingEntry(sc.Text())
           if !ok || e.ID == "" || seen[e.ID] {
               continue
           }
           seen[e.ID] = true
           out = append(out, e)
       }
       return out, sc.Err()
   }

   // peekPendingEntries is peekPendingSessions but returns full entries
   // (score/msgCount/age) instead of bare IDs, for lane-aware callers
   // (mineSessions dry-run, status.go's queue summary). Non-consuming.
   func peekPendingEntries() []pendingEntry {
       path := pendingSessionsFile()
       f, err := os.Open(path)
       if err != nil {
           return nil
       }
       defer f.Close()
       entries, err := scanPendingEntries(f)
       if err != nil {
           logMsg("sync", "peek pending queue: %v", err)
           return nil
       }
       return entries
   }
   ```

   Rewrite `peekPendingSessions` (`hook.go:378-400`) to call
   `peekPendingEntries` and map to `[]string` (preserves its existing
   signature, nil-on-missing-file behavior, and dedup order exactly — no
   test changes needed for it).

   Rewrite `readAndClearPendingSessions` (`hook.go:409-453`) to add a
   sibling `readAndClearPendingEntries() ([]pendingEntry, error)` that does
   the claim/rename/scan/remove dance exactly as today but calls
   `scanPendingEntries` instead of the old inline loop, then makes
   `readAndClearPendingSessions` a thin wrapper mapping entries to IDs
   (same nil/nil-on-empty-queue contract, verified by
   `TestPeekAndDrainPendingSessions`).

### 3.2 `cmd/scribe/watch.go`

1. `appendPending` (`watch.go:289-300`): add a `msgCount int` parameter,
   write it as the new 3rd column:

   ```go
   func appendPending(path, sessionID string, score, msgCount int) error {
       ...
       _, err = fmt.Fprintf(f, "%s\t%d\t%d\t%s\n", sessionID, score, msgCount, time.Now().UTC().Format(time.RFC3339))
       return err
   }
   ```

2. Update the doc comment ("Format matches the SessionEnd hook...") and the
   call site at `watch.go:256`:

   ```go
   if err := appendPending(pendingFile, c.id, score, c.msgs); err != nil {
   ```

   `c.msgs` is already scanned into the `candidate` struct at `watch.go:215`
   — no new query needed.

### 3.3 `cmd/scribe/config.go`

1. Add a new struct near `TriageConfig` (`config.go:399-402`):

   ```go
   // PriorityLanesConfig tunes how sync drains pending-sessions.txt (issue
   // #22). An entry scoring at or above HotThreshold admits before Normal-
   // lane entries; AgeDays promotes a stale Normal entry into Hot so a
   // queue that's perpetually fed high scorers can't starve an old
   // low-scorer forever. See docs/issue-22-priority-lanes-plan.md.
   type PriorityLanesConfig struct {
       HotThreshold int `yaml:"hot_threshold"`
       AgeDays      int `yaml:"age_days"`
   }
   ```

2. Add the field to `ScribeConfig` (near `Triage TriageConfig
   \`yaml:"triage"\`` — find its exact line via
   `grep -n 'Triage .*TriageConfig' config.go`, it sits alongside `Sync`,
   `Deep`, `Capture` around `config.go:115-118`):

   ```go
   PriorityLanes PriorityLanesConfig `yaml:"priority_lanes"`
   ```

3. Add the defaulting function next to `TriageConfig.Resolve`
   (`config.go:432-451`):

   ```go
   // applyPriorityLanesDefaults fills PriorityLanesConfig. 90 sits well
   // above the hook's own MinScore default (50, hook.go) — Hot means
   // "clearly high value", not merely "cleared the queue bar". AgeDays=7
   // is the starvation backstop: a Normal entry waits at most a week
   // before it's promoted regardless of score.
   func applyPriorityLanesDefaults(cfg *PriorityLanesConfig) {
       if cfg.HotThreshold <= 0 {
           cfg.HotThreshold = 90
       }
       if cfg.AgeDays <= 0 {
           cfg.AgeDays = 7
       }
   }
   ```

4. Call it in **both** branches of `loadConfig` (`config.go:582-686`),
   matching the placement of `applyMetaDefaults(&cfg.Meta)`:
   - No-`scribe.yaml` branch (`config.go:624-635`): add
     `applyPriorityLanesDefaults(&cfg.PriorityLanes)`.
   - Normal branch (`config.go:667-677`): add the same call.

### 3.4 `cmd/scribe/triage.go`

Hoist the JSON result struct out of `runScoring` so `sync_sessions.go` can
reuse it without duplicating field/JSON-tag definitions (a drift risk: if
someone renames a JSON field in one copy and not the other, the sync-side
decoder silently gets zero values).

1. Move the local `type result struct { ... }` (`triage.go:178-192`, inside
   `runScoring`) to package scope, renamed `triageResult` (same fields,
   same JSON tags — pure rename, zero behavior change):

   ```go
   // triageResult is one row of `scribe triage --json` output. Shared with
   // sync_sessions.go's triageSessionsScored, which decodes the same JSON
   // shape when pulling live-triage candidates into the priority-lane
   // admission path (issue #22) — one struct, one set of JSON tags, no
   // drift between the two call sites.
   type triageResult struct {
       SessionID string  `json:"session_id"`
       Project   string  `json:"project"`
       Msgs      int     `json:"msgs"`
       Score     int     `json:"total_score"`
       Dec       int     `json:"dec"`
       Arch      int     `json:"arch"`
       Res       int     `json:"res"`
       Learn     int     `json:"learn"`
       Eval      int     `json:"eval"`
       Deep      int     `json:"deep"`
       Date      string  `json:"date"`
       Hours     float64 `json:"hours"`
       Summary   string  `json:"summary"`
   }
   ```

2. Update the two references inside `runScoring` (`triage.go:194`
   `var results []result` → `[]triageResult`, and `triage.go:196`
   `var r result` → `var r triageResult`) to the renamed type. No other
   changes to `runScoring`'s logic.

### 3.5 `cmd/scribe/sync_sessions.go` — the core rewrite

1. New scored-triage fetcher, sibling to `triageSessionIDs`
   (`sync_sessions.go:285-305`):

   ```go
   // triageSessionsScored calls `scribe triage --json` (not --ids) so the
   // result carries score + message count, the two signals priority-lane
   // classification and size-pool partitioning need. Mirrors
   // triageSessionIDs exactly except for the --json flag and the parse.
   func (s *SyncCmd) triageSessionsScored(top int, extraArgs ...string) []pendingEntry {
       scribeExe, _ := os.Executable()
       if scribeExe == "" {
           scribeExe = "scribe"
       }
       args := make([]string, 0, 7+len(extraArgs))
       args = append(args, "triage", "--json", "--top", strconv.Itoa(top), "--sort", s.SessionSort)
       args = append(args, extraArgs...)
       out, err := runCmdErr("", scribeExe, args...)
       if err != nil {
           return nil
       }
       var rows []triageResult
       if err := json.Unmarshal([]byte(out), &rows); err != nil {
           return nil
       }
       entries := make([]pendingEntry, 0, len(rows))
       for _, r := range rows {
           entries = append(entries, pendingEntry{
               ID: r.SessionID, Score: r.Score, HasScore: true, MsgCount: r.Msgs,
               // No queue history for a fresh triage discovery — sorts as
               // "now" in the intra-lane tiebreak (see sortWithinLane).
           })
       }
       return entries
   }
   ```

   Add `"encoding/json"` to the import block if not already present (it
   is not currently imported in `sync_sessions.go`).

2. Size-pool partitioning + msgCount backfill:

   ```go
   // backfillMsgCounts fills MsgCount for entries whose queue line didn't
   // carry one (the 3-column legacy timestamp shape, or bare-ID lines) via
   // one ccrider lookup per unknown entry — cheap and rare (only exists
   // for leftovers queued before this upgrade shipped; see
   // docs/issue-22-priority-lanes-plan.md §2.2). On DB-open or per-row
   // lookup failure, MsgCount stays -1, which partitionBySize treats as
   // "normal" — fail open toward the cheaper, more-parallel pool rather
   // than the expensive serial one.
   func backfillMsgCounts(dbPath string, entries []pendingEntry) []pendingEntry {
       var db *sql.DB
       for i := range entries {
           if entries[i].MsgCount != -1 {
               continue
           }
           if db == nil {
               var err error
               db, err = sql.Open("sqlite3", dbPath+"?mode=ro")
               if err != nil {
                   return entries
               }
               defer db.Close()
           }
           if n, ok := queryMessageCount(db, entries[i].ID); ok {
               entries[i].MsgCount = n
           }
       }
       return entries
   }

   // partitionBySize splits entries into the normal (<=300 msgs, or
   // unknown — fail open) and large (>300 msgs) size pools that carry
   // largeSessionBudget's wall-clock safety cap forward (see
   // docs/issue-22-priority-lanes-plan.md §2.3).
   func partitionBySize(entries []pendingEntry) (normal, large []pendingEntry) {
       for _, e := range entries {
           if e.MsgCount > 300 {
               large = append(large, e)
           } else {
               normal = append(normal, e)
           }
       }
       return normal, large
   }
   ```

   `queryMessageCount` already exists at `hook.go:238-249` and returns
   `(int, bool)` — reuse it verbatim, no changes needed there.

3. Lane classification + admission + sort (`classifyLanes`,
   `splitBudgetByLane`, `admitByLaneFloor` from §2.4 above — add them as
   new functions in `sync_sessions.go`), plus:

   ```go
   // mergeCandidates unions pending-queue and live-triage candidates for
   // one size pool, deduped by ID. The pending-queue entry wins a
   // collision: it carries real EnqueuedAt/Score history the triage
   // rediscovery of the same ID doesn't have (see
   // docs/issue-22-priority-lanes-plan.md §2.5).
   func mergeCandidates(pending, triaged []pendingEntry) []pendingEntry {
       seen := make(map[string]bool, len(pending)+len(triaged))
       out := make([]pendingEntry, 0, len(pending)+len(triaged))
       for _, e := range pending {
           if !seen[e.ID] {
               seen[e.ID] = true
               out = append(out, e)
           }
       }
       for _, e := range triaged {
           if !seen[e.ID] {
               seen[e.ID] = true
               out = append(out, e)
           }
       }
       return out
   }

   // sortWithinLane orders a classified lane for processing: score desc,
   // then EnqueuedAt asc (oldest first) as the tiebreak. LegacyUnknownAge
   // sorts as oldest (epoch); an entry with no queue history at all
   // (fresh triage discovery) sorts as newest. See
   // docs/issue-22-priority-lanes-plan.md §2.4.
   func sortWithinLane(entries []pendingEntry) {
       sort.SliceStable(entries, func(i, j int) bool {
           if entries[i].Score != entries[j].Score {
               return entries[i].Score > entries[j].Score
           }
           return entryAge(entries[i]).Before(entryAge(entries[j]))
       })
   }

   // entryAge returns a sortable proxy for "how long has this been
   // waiting": the real timestamp when known, epoch (oldest) for the
   // legacy bare-ID shape, or "now" (newest) when there's no queue
   // history to speak of.
   func entryAge(e pendingEntry) time.Time {
       switch {
       case e.LegacyUnknownAge:
           return time.Time{}
       case e.HasEnqueuedAt:
           return e.EnqueuedAt
       default:
           return time.Now()
       }
   }
   ```

   Add `"sort"` to the import block if not already present.

4. Rewrite `mineSessions` (`sync_sessions.go:548-708`). Structure per size
   pool is identical; write a shared helper to avoid duplicating it twice:

   ```go
   // admitForPool runs one size pool's full pipeline: merge pending +
   // live-triage candidates, classify into Hot/Normal, admit up to
   // budget by floor-reservation, and return the final processing-order
   // ID list. scopeFilter is preFilterSessions for the normal pool or
   // filterSessionsByScope for the large pool — kept as a parameter so
   // each pool's existing scope-gate call (with its existing side
   // effects, e.g. preFilterSessions marking skipped IDs processed) is
   // untouched; only the ORDERING feeding into it changes.
   func admitForPool(pending, triaged []pendingEntry, budget int, cfg PriorityLanesConfig, scopeFilterIDs func([]string) []string) []string {
       merged := mergeCandidates(pending, triaged)
       ids := make([]string, len(merged))
       byID := make(map[string]pendingEntry, len(merged))
       for i, e := range merged {
           ids[i] = e.ID
           byID[e.ID] = e
       }
       kept := scopeFilterIDs(ids)
       filtered := make([]pendingEntry, 0, len(kept))
       for _, id := range kept {
           filtered = append(filtered, byID[id])
       }
       hot, normal := classifyLanes(filtered, cfg, time.Now())
       admitted := admitByLaneFloor(hot, normal, budget, 0.3)
       sortWithinLane(admitted)
       out := make([]string, len(admitted))
       for i, e := range admitted {
           out[i] = e.ID
       }
       return out
   }
   ```

   Then in `mineSessions`:

   - Replace `pendingIDs, err := readAndClearPendingSessions()`
     (`sync_sessions.go:591`) with
     `pendingEntries, err := readAndClearPendingEntries()`.
   - Keep the existing already-processed filter
     (`sync_sessions.go:595-613`), adapted to filter `pendingEntries` by
     `.ID` instead of plain strings.
   - `cfg := loadConfig(root)` already exists at `sync_sessions.go:644` —
     move it earlier (right after the processed-filter block) since it's
     now needed before size partitioning too.
   - `pendingEntries = backfillMsgCounts(cfg.CcriderDB, pendingEntries)`
   - `pendingNormal, pendingLarge := partitionBySize(pendingEntries)`
   - Normal pool, replacing `sync_sessions.go:621-689`:
     ```go
     triagedNormal := s.triageSessionsScored(s.SessionsMax*3, "--message-limit", "300")
     normalIDs := admitForPool(pendingNormal, triagedNormal, s.SessionsMax, cfg.PriorityLanes,
         func(ids []string) []string {
             kept, skipped := preFilterSessions(root, cfg.CcriderDB, ids)
             if len(skipped) > 0 {
                 // ... existing skipped-marking block, unchanged, using `skipped`
             }
             return kept
         })
     if len(normalIDs) > 0 {
         logMsg("sync", "priority lanes found %d normal sessions (<=300 msgs)", len(normalIDs))
         mined, rateLimited := s.mineSessionBatches(root, normalIDs, cfg.Sync.ParallelExtractions, 10*time.Minute, "session-extract.md", "session")
         totalMined += mined
         if rateLimited { /* existing early-return block, unchanged */ }
     }
     ```
     (The existing "mark skipped sessions processed" block at
     `sync_sessions.go:648-667` moves verbatim inside the closure passed to
     `admitForPool` — same JSON update logic, same log line, unchanged.)
   - Large pool, replacing `sync_sessions.go:691-703`:
     ```go
     if largeMax := s.largeSessionBudget(); largeMax > 0 {
         triagedLarge := s.triageSessionsScored(largeMax*3, "--min-messages", "301")
         largeIDs := admitForPool(pendingLarge, triagedLarge, largeMax, cfg.PriorityLanes,
             func(ids []string) []string { return filterSessionsByScope(root, cfg.CcriderDB, ids) })
         if len(largeIDs) > 0 {
             logMsg("sync", "priority lanes found %d large sessions (>300 msgs)", len(largeIDs))
             mined, _ := s.mineSessionBatches(root, largeIDs, 1, 20*time.Minute, "session-extract-large.md", "large-session")
             totalMined += mined
         }
     }
     ```
     Note the large pool's triage fetch changes from `largeMax` (exact) to
     `largeMax*3` (over-fetch), matching the normal pool's existing `*3`
     over-fetch (`sync_sessions.go:621`) — needed so there's enough raw
     material in both lanes for the 70/30 split to mean something instead
     of silently collapsing to 100% Normal whenever Hot's candidate pool is
     thin.
   - `s.mineSessionBatches` and `s.mineSessionsSerial` (`sync_sessions.go:358-533`)
     are **not modified** — checkpointing, envelope-mode dispatch
     (`sync_sessions.go:376-378`), and `recordBatchOutcome` all keep
     operating on plain `[]string` IDs exactly as today.
   - Dry-run branch (`sync_sessions.go:556-573`): swap
     `peekPendingSessions()` for `peekPendingEntries()` and add a lane
     summary line before the existing triage table print:
     ```go
     if peeked := peekPendingEntries(); len(peeked) > 0 {
         hot, normal := classifyLanes(peeked, cfg.PriorityLanes, time.Now())
         logMsg("sync", "DRY RUN -- hook queue: %d pending session(s), %d hot / %d normal",
             len(peeked), len(hot), len(normal))
     }
     ```
     (Replaces the existing `if peeked := peekPendingSessions(); ...` block;
     `cfg` must be loaded before this point in the dry-run branch too — it
     currently isn't, so hoist `cfg := loadConfig(root)` above the dry-run
     `if s.DryRun` check, same pattern as done for the main path above.)

### 3.6 `cmd/scribe/status.go`

1. Add, near `countScopedPendingSessions` (`status.go:489-540`):

   ```go
   // pendingQueueSummary classifies the current (undrained) hook/watch
   // queue into Hot/Normal for `scribe status` visibility (issue #22).
   // Peek-only — never consumes the queue. ok=false when the queue file
   // doesn't exist (nothing to report, not an error).
   func pendingQueueSummary(cfg PriorityLanesConfig) (hot, normal, aged int, ok bool) {
       entries := peekPendingEntries()
       if entries == nil {
           return 0, 0, 0, false
       }
       now := time.Now()
       for _, e := range entries {
           isAged := e.LegacyUnknownAge || (e.HasEnqueuedAt && now.Sub(e.EnqueuedAt) >= time.Duration(cfg.AgeDays)*24*time.Hour)
           if e.Score >= cfg.HotThreshold || isAged {
               hot++
               if isAged && e.Score < cfg.HotThreshold {
                   aged++
               }
           } else {
               normal++
           }
       }
       return hot, normal, aged, true
   }
   ```

   Requires `"time"` — already imported in `status.go` (used elsewhere).

2. In `renderBacklog` (`status.go:379`), right after the existing sessions
   backlog block (`status.go:441-450`):

   ```go
   if hot, normal, aged, ok := pendingQueueSummary(cfg.PriorityLanes); ok && hot+normal > 0 {
       agedNote := ""
       if aged > 0 {
           agedNote = fmt.Sprintf(" (%d aged→hot)", aged)
       }
       fmt.Fprintf(w, "    %-22s %d hot, %d normal%s\n", "session queue (hooked):", hot, normal, agedNote)
   }
   ```

### 3.7 `cmd/scribe/templates/scribe.yaml`

Add a commented block after the existing `triage:` section
(`templates/scribe.yaml:333-351`), so `scribe config update`
(`config_update.go`, keyed by top-level YAML key via
`templateConfigSegments`/`missingTemplateBlocks`) offers it to existing KBs
automatically — no changes needed in `config_update.go` itself, it walks the
template file generically:

```yaml
# Priority lanes for the pending-session queue (scored, not FIFO). A
# session scoring at or above hot_threshold mines before Normal-lane
# sessions; age_days promotes a stale Normal entry into Hot so it can't
# be starved forever by a steady stream of higher scorers. Defaults
# below match the built-in fallback — uncomment only to tune.
# priority_lanes:
#   hot_threshold: 90              # default: 90
#   age_days: 7                    # default: 7
```

## 4. Test plan

All new tests are table-driven where the function under test has more than
2 meaningfully distinct cases, per repo convention. ccrider-backed tests use
`newCcriderDB`/`insertFixtureSession` (`cmd/scribe/sessions_test.go:15-44`)
against `cmd/scribe/testdata/ccrider_schema.sql` — never
`~/.config/ccrider/`. `make test` must pass with no network access (no test
here needs one — everything is local SQLite + in-process function calls).

### 4.1 `hook_test.go` (existing file — extend, don't replace)

| Test | Covers |
| --- | --- |
| `TestParsePendingEntry` (new) | Table-driven over the 6 format rows in §2.2: 4-col current, 3-col-int (defensive), 3-col-timestamp (today's shipped format), 2-col, 1-col bare ID (asserts `LegacyUnknownAge=true`), blank line (`ok=false`). Assert exact `Score`/`HasScore`/`MsgCount`/`HasEnqueuedAt`/`LegacyUnknownAge` per row. |
| `TestParsePendingLineStillWorks` (new, or extend existing `TestPendingContainsID`) | `parsePendingLine` on a 4-column line still extracts only the ID — regression guard now that a 4th column exists (existing `TestPendingContainsID` "tab-separated lines match first field only" subtest at `hook_test.go:35-51` already has 3 columns; add a 4-column variant). |
| `TestPeekAndDrainPendingSessions` (existing, `hook_test.go:80-133`) | Must pass **unchanged** — this is the regression guard that `peekPendingSessions`/`readAndClearPendingSessions` kept their exact `[]string` contract. Add fixture lines using 4-column format to the `content` string at `hook_test.go:96` to also exercise the new format through the old string-returning API. |
| `TestPeekAndDrainPendingEntries` (new) | Same lifecycle as above (`peekPendingEntries` doesn't consume, `readAndClearPendingEntries` does and clears the file) but asserting full `pendingEntry` fields survive the round trip, using a content string mixing 4-col, 3-col-timestamp, and bare-ID lines. |
| `TestSessionEndHookWritesFourColumns` (new, or extend hook integration test if one exists — check for a `TestSessionEndHookCmd_Run`-shaped test first) | After a `Run()` that queues a session, the appended line has exactly 4 tab fields and field[2] equals the message count returned by the fixture DB. |

### 4.2 `watch_test.go` (existing file — extend)

| Test | Covers |
| --- | --- |
| `TestAppendPending` (existing, `watch_test.go:15-48`) | Update: call `appendPending(path, id, score, msgCount)` with a msgCount argument; change the field-count assertion from `!= 3` to `!= 4`; add an assertion that field[2] equals the passed `msgCount`. |
| `TestAppendPendingReaderCompat` (existing, `watch_test.go:53-79`) | Update call site to pass a `msgCount` argument; assertions otherwise unchanged (still just checks `pendingContainsID` finds the ID). |

### 4.3 `config_test.go` (existing file — extend)

| Test | Covers |
| --- | --- |
| `TestApplyPriorityLanesDefaults` (new, table-driven) | Zero-value input → `{90, 7}`. Partial input (only `HotThreshold` set) → `AgeDays` still defaults. Fully-set input passes through unchanged. Negative/zero values on either field fall back to default (matches the `<= 0` guard). |
| Extend whatever test currently asserts `loadConfig` defaults (search for the existing "defaults" test near `TestLoadConfig`) | Add an assertion that a fresh `loadConfig(tmpKB)` with no `priority_lanes:` in `scribe.yaml` yields `cfg.PriorityLanes == PriorityLanesConfig{90, 7}`. |

### 4.4 `sync_sessions_test.go` (new file — the bulk of the new logic lives here)

| Test | Covers |
| --- | --- |
| `TestSplitBudgetByLane` | Table: `budget=0→(0,0)`, `budget=1,ratio=0.3→(1,0)` (rounds down to 0 for Normal — documents the known small-budget edge case), `budget=3,ratio=0.3→(2,1)`, `budget=10,ratio=0.3→(7,3)`, `budget=2,ratio=0.5→(1,1)`. |
| `TestAdmitByLaneFloor` | Table covering: enough candidates in both lanes (floor respected exactly); Hot lane empty (100% backfilled from Normal); Normal lane empty (100% from Hot); both lanes together short of budget (returns everything, no panic); budget=0 (returns empty). |
| `TestClassifyLanes` | Table covering: score above threshold → Hot; score below → Normal; `HasEnqueuedAt` + age >= `AgeDays` → Hot regardless of score; `LegacyUnknownAge=true` → Hot regardless of score; fresh entry (no age signal, score below threshold) → Normal. |
| `TestSortWithinLane` | Same score, mixed age states (`LegacyUnknownAge`, real timestamp, no-signal) → asserts the epoch/real/now ordering from §2.4 exactly. Different scores → higher score always first regardless of age. |
| `TestMergeCandidates` | Collision on ID: pending entry's fields win (assert the *pending* `EnqueuedAt`/`Score` survive, not the triaged one's). No-collision case: both preserved, pending-first ordering. |
| `TestBackfillMsgCounts` | Against a fixture ccrider DB (`newCcriderDB` + `insertFixtureSession`): entry with `MsgCount=-1` gets the real count filled in; entry with a known `MsgCount` is left untouched (no DB call made — can assert via a session ID that doesn't exist in the fixture DB and confirming no panic/error); DB-open failure (bad path) leaves `MsgCount=-1` for all entries (fail open). |
| `TestPartitionBySize` | `MsgCount=300` → normal (`<=300` per existing `sync_sessions.go` semantics carried forward — check `preFilterSessions`'s message-limit flag usage uses the same boundary); `301` → large; `-1` (unknown) → normal. |
| `TestAdmitForPool_NoStarvation` (the invariant test — table-driven against multiple scenarios, matching the rigor the research doc asked for even though the axis changed) | Construct a synthetic queue of e.g. 20 entries, 18 scoring 95+ (Hot) and 2 scoring 40 (Normal), run `admitForPool` across several simulated consecutive "runs" (each run re-classifies the *remaining* un-admitted entries — simulate by removing admitted IDs between calls) with a small budget (3): assert the 2 Normal entries are admitted within a bounded number of simulated runs (not "eventually, unbounded") — this is the direct regression guard for the starvation class of bug the interleave/aging design exists to prevent. |
| `TestAdmitForPool_EmptyConfigNoSubscriptions` (golden-ish, not literal domain subscriptions — renamed from the research doc's framing to match this axis) | With `PriorityLanesConfig` at its zero value fed through `applyPriorityLanesDefaults` (i.e. real defaults, 90/7) and a queue where every entry scores below 90 and is fresh (no aging triggered): confirms admission ordering degrades to pure score-descending (today's *effective* behavior minus the old "pending always first" rule) — i.e. verifies the new code path doesn't silently reorder things when nothing unusual (no Hot scorers, no aged entries) is present. |
| `TestMineSessionsAdmitsPendingAndTriageEntries` (integration-shaped, using the existing sync integration-test scaffolding — check `sync_extract_test.go`/`sync_discover_test.go` for the KB-in-`t.TempDir()` + fixture-DB pattern already used and follow it) | End-to-end through `mineSessions` with a stub/no-op LLM path (search for how existing session-mining tests avoid real `claude -p` calls — likely via `cfg.SessionMine.Mode` or a `runClaude` seam; if no existing seam mocks this cleanly, scope this test down to asserting the *ID list order* `admitForPool` would produce feeding into `mineSessionBatches`, rather than a full mined-KB assertion, to avoid needing network/LLM access). |

### 4.5 `status_sessions_test.go` (existing file — extend)

| Test | Covers |
| --- | --- |
| `TestPendingQueueSummary` (new) | Using `t.Setenv("XDG_CONFIG_HOME", ...)` (per the pattern in `hook_test.go:82`) to redirect `pendingSessionsFile()`: no file → `ok=false`; mixed Hot/Normal/aged entries → correct counts; empty file → `ok=true, hot=0, normal=0`. |
| Extend `TestCountScopedPendingSessions` suite or add a sibling | Confirm `renderBacklog` prints the new "session queue (hooked):" line only when `pendingQueueSummary` returns entries (use a `bytes.Buffer` + `renderBacklog(buf, root, cfg)`, `strings.Contains` check — matches how other `renderBacklog` rows are likely already tested; check for an existing `TestRenderBacklog`-shaped test first and extend it rather than adding a parallel one). |

### 4.6 `triage_test.go` (existing file — minimal touch)

| Test | Covers |
| --- | --- |
| Existing tests referencing the local `result` type | Should compile unchanged after the rename to `triageResult` — this is a mechanical rename, not a behavior change, so no new test is strictly required. Add one `TestTriageJSONOutputShape` if none exists that asserts `--json` output round-trips through `triageResult`'s JSON tags (guards the contract `triageSessionsScored` in `sync_sessions.go` now depends on). |

### 4.7 Full-suite regression

- `TestLargeSessionBudget` (`sessions_test.go:241`) must still pass
  unchanged — `largeSessionBudget()` itself is untouched (§2.3).
- `TestProjectScopeAllowed` (`sync_sessions_scope_test.go`) must still pass
  unchanged — `projectScopeAllowed`/`preFilterSessions`/
  `filterSessionsByScope` signatures and logic are untouched; only what
  feeds their `[]string` input changes.
- `make test` (`go test ./... -tags sqlite_fts5`) and `make check`
  (test + vet) must both be green before this is considered done, per
  `CLAUDE.md`.

## 5. Risks & edge cases

- **Queue-file migration is the sharpest edge.** Solved by tolerant
  parsing (§2.2), not a migration script — the format table must be
  implemented exactly as specified, especially the field[2]-parses-as-int
  disambiguation between the current shipped 3-column format (`id, score,
  timestamp`) and the new 3-column defensive shape (`id, score, msgCount`).
  Get this rule wrong and every session queued before the upgrade either
  loses its score (falls back to `HasScore=false` → treated as `0`,
  Normal lane, safe but wastes prior signal) or crashes on `time.Parse`
  (it won't — parse failures fall through to "unknown", never panic; but a
  reviewer should specifically test a raw file copied from a running KB
  pre-upgrade, not just synthetic fixtures).
- **Two independent writers (`hook.go`, `watch.go`) must change in lockstep.**
  `watch_test.go`'s `TestAppendPendingReaderCompat` exists precisely because
  this handoff broke silently once before (see its doc comment). Do not
  ship a change to one producer without the other — verify by grep for
  `appendPending(` and the `Fprintf` in `hook.go` both landing on the same
  4-column shape.
- **`admitForPool`'s closure-based `scopeFilterIDs` must preserve
  `preFilterSessions`'s side effect** (marking mechanically-skipped
  sessions as processed in `_sessions_log.json`, `sync_sessions.go:648-667`)
  exactly where it happens today, or a session that fails the mechanical
  gate will be re-triaged forever instead of being marked skipped once.
  This is why §3.5 step 4 keeps that block inside the closure verbatim
  rather than trying to generalize it away.
- **`splitBudgetByLane` at budget=1** (the large pool's default) always
  reserves 0 slots for Normal (`round(1*0.3)=0`). This is intentional and
  documented (§2.4) — the Normal lane's protection at this scale comes from
  aging promotion (an old large session eventually becomes Hot and wins the
  single slot on its own), not from the floor. Do not "fix" this by forcing
  `normalSlots >= 1` — with a budget of exactly 1, that would mean the large
  pool *never* mines a genuinely fresh Hot giant session, which is worse.
- **`triageSessionsScored` shells out to `scribe triage --json`,
  duplicating the process-spawn pattern `triageSessionIDs` already uses**
  (`runCmdErr`) — no new dependency, but it does mean two `scribe triage`
  invocations per `mineSessions` run per size pool instead of one (was
  already two: one per pass). Cost is one extra process spawn + one FTS5
  query; negligible next to the LLM calls this whole pipeline exists to
  gate.
- **No new `go.mod` dependencies.** Everything above uses `encoding/json`,
  `sort`, `strconv`, `math`, `time` — all stdlib, already imported
  elsewhere in the package.
- **Interaction with #23 (KB-first adoption metric, roadmap-01):** #23
  needs "ccrider-side signal extraction: detect qmd query/search tool
  calls early in sessions." It reads ccrider's `messages`/`messages_fts`
  tables independently of the pending-queue file — no overlap with this
  plan's `pendingEntry`/queue-file work. The only shared surface is that
  both features may eventually add rows to `scribe status`'s output; #23
  should add its own row rather than touching `pendingQueueSummary`
  (§3.6) — keep the two status additions independent so either can land
  without the other.
- **Interaction with #6 (roadmap umbrella):** this issue is umbrella item
  2 of 4. #21 (drop authoring CLI) is umbrella item 1 and touches an
  unrelated surface (input authoring, not session mining) — no ordering
  dependency either way, but per `docs/issues-master-plan.md` §Rationale
  ("Phase 2 follows the maintainer's own roadmap order... Phase 3: #22
  subsumes the two-lane budget"), #21 is expected to land first in the
  actual merge sequence; nothing in this plan requires that, it's a
  scheduling choice, not a code dependency.

## 6. Size estimate

**M/L**, matching `docs/issues-master-plan.md`'s Phase 3 sizing
("Engineering quality: stub LLM harness, priority lanes | M/L").

Rough LOC:

- `hook.go`: +90 / -15 (new struct, `parsePendingEntry`, `scanPendingEntries`,
  entry-returning peek/drain variants, format-string + comment updates).
- `watch.go`: +3 / -3 (signature + call-site change).
- `config.go`: +25 (new struct, field, defaulting function, two call sites).
- `triage.go`: +15 / -14 (struct hoist — net near zero, mostly moved lines).
- `sync_sessions.go`: +180 / -90 (the core rewrite: `triageSessionsScored`,
  `backfillMsgCounts`, `partitionBySize`, `classifyLanes`,
  `splitBudgetByLane`, `admitByLaneFloor`, `mergeCandidates`,
  `sortWithinLane`, `entryAge`, `admitForPool`, plus `mineSessions`'s
  rewritten body).
- `status.go`: +25 (`pendingQueueSummary` + one `renderBacklog` block).
- `templates/scribe.yaml`: +8 (commented block).
- Tests: +350-450 across `hook_test.go`, `watch_test.go`, `config_test.go`,
  new `sync_sessions_test.go`, `status_sessions_test.go` extensions
  (table-driven tests dominate the new-code line count here, as expected
  for this kind of scheduling logic).

**Total: roughly 700-800 lines touched (code + tests), 8 files.** No new
files except `sync_sessions_test.go` if one doesn't already exist (check
first — `sync_sessions_scope_test.go` exists but covers a different
function; a fresh `sync_sessions_test.go` for the new functions is cleaner
than overloading the scope-test file).
