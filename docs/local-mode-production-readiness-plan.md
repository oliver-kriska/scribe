# Local-mode production readiness — plan for the next session

Status: **draft for implementation by next session**
Filed: 2026-05-13
Owner: Oliver
Parent: [[local-model-followup-plan]] (items 1–3 shipped in commit `4bd4ecf`)

This plan finishes the local-mode work to "production-ready" — the
state where the heavy crons (`com.scribe.sync-projects`,
`com.scribe.sync-sessions`, unloaded since 2026-05-12) can resume
running safely against the new local-mode stack.

Items are split into P0 (necessary; the prior work doesn't pay off
until these land) and P1 (good improvement; high value, can defer
under scope pressure). Phase 4C and other large-scope work are
explicitly out of scope.

---

# P0 — Necessary

## P0.1 — Validate fact-ID stripper on a real article

**Why:** We shipped `fact_citations.go` with 11 unit tests but never
re-ran the gemma3:27b + atomic_facts case to confirm the production
fabrication rate dropped from 1/22 to 0/22. If a fabricated ID
escapes the stripper, we need to know before crons run.

**No code change — this is a verification step.**

### Procedure

```sh
# Test KB at /tmp/scribe-pass2-test should still have:
#   pass2_provider: ollama
#   pass2_model: gemma3:27b
#   pass2_parallel: 1
#   pass2_timeout_min: 25
#   atomic_facts: true
#   facts_provider: ollama
#   facts_model: gemma3:4b

# Reset
rm -f /tmp/scribe-pass2-test/wiki/_absorb_log.json
rm -f /tmp/scribe-pass2-test/tools/{buffer,taplio,kleo,oliver-voice-skill,superx,supergrow,shield,authoredup}.md
rm -f /tmp/scribe-pass2-test/patterns/{voice-first-content-boundary,data-boundary-in-social-media-tools}.md
rm -f /tmp/scribe-pass2-test/research/*linkedin* /tmp/scribe-pass2-test/decisions/*linkedin*
rm -f /tmp/scribe-pass2-test/raw/articles/2026-05-13-*
rm -rf /tmp/scribe-pass2-test/output/facts /tmp/scribe-pass2-test/output/absorb-facts

# Run
cd /tmp/scribe-pass2-test
scribe -C /tmp/scribe-pass2-test absorb \
  /tmp/scribe-pass2-test/raw/articles/test-linkedin.md 2>&1 | tee /tmp/p0-1.log

# Audit
echo "=== fabricated IDs (should be 0) ==="
all_real=$(jq -r '.facts[].id' /tmp/scribe-pass2-test/output/facts/*.json | sort -u)
cited=$(grep -rho '\[c[0-9]*-f[0-9]*\]' /tmp/scribe-pass2-test/tools/*.md \
        /tmp/scribe-pass2-test/patterns/*.md 2>/dev/null \
        | sort -u | sed 's/[][]//g')
missing=""
while IFS= read -r id; do
  [ -z "$id" ] && continue
  echo "$all_real" | grep -qx "$id" || missing+="$id "
done <<< "$cited"
echo "fabricated_after_stripper: ${missing:-NONE}"

# Also confirm stripper log lines appeared
grep "stripped.*fabricated fact-ID bracket" /tmp/p0-1.log
```

### Acceptance criteria

- `fabricated_after_stripper: NONE` (the stripper caught everything)
- At least one `stripped N fabricated fact-ID bracket(s)` log line
  fired during pass-2 (proves the code path executed)

### If it fails

- Some ID escaped: inspect the surviving brackets manually. The
  regex `\s*\[c\d+-f\d+\]` may need broadening (e.g. brackets
  preceded by punctuation, embedded in larger strings).
- No log lines: the validator isn't being called. Check the
  wiring in `sync.go` json branch — `mergedFacts` may be nil when it
  shouldn't be.

### Estimate

10 min.

---

## P0.2 — Verify budget ceiling fires

**Why:** Plan item 4 (re-enable heavy crons) names a manual ceiling-
firing test as a prerequisite. Without confirming `ErrDailyBudgetExhausted`
aborts cleanly, re-enabling crons risks a 2026-05-11 repeat.

**No code change — verification step.**

### Procedure

```sh
# Use scriptorium so we hit a realistic ledger
cd /Users/oliverkriska/Projects/scriptorium

# Snapshot config
cp scribe.yaml scribe.yaml.bak

# Drop ceiling to a trivially small number
python3 -c "
import re
p='scribe.yaml'
s=open(p).read()
if 'daily_anthropic_output_token_ceiling' in s:
    s = re.sub(r'daily_anthropic_output_token_ceiling:\s*\d+', 'daily_anthropic_output_token_ceiling: 1000', s)
else:
    s = s.replace('sync:', 'sync:\n  daily_anthropic_output_token_ceiling: 1000', 1)
open(p,'w').write(s)
"
grep daily_anthropic /Users/oliverkriska/Projects/scriptorium/scribe.yaml

# Force a sync that will trip the gate within seconds
scribe sync 2>&1 | tee /tmp/p0-2.log

# Restore config
mv scribe.yaml.bak scribe.yaml
```

### Acceptance criteria

- `/tmp/p0-2.log` contains an `ErrDailyBudgetExhausted` log line or
  the error surface scribe uses for it ("daily anthropic output-token
  ceiling reached" per `budget.go`)
- Process exit code is 0 (clean exit, not a crash — so launchd
  doesn't retry endlessly)
- The cost ledger for today shows the calls that landed BEFORE the
  ceiling tripped, not corrupted state
- `SCRIBE_BYPASS_BUDGET=1 scribe sync` succeeds against the same
  ceiling (escape hatch works)

### If it fails

- Ceiling not enforced: the hook in `claude.go`/`llm.go` isn't
  loading config or reading the field. Inspect `loadConfig` output
  during a sync (add a one-shot debug log if needed).
- Process crashes instead of exit 0: the error is bubbling up
  uncaught. Wrap the sync outer loop to map `ErrDailyBudgetExhausted`
  → log + exit 0.
- Bypass env not honored: check string compare; should be `== "1"`,
  not truthy-ish.

### Estimate

5 min.

---

## P0.3 — Re-enable heavy crons

**Pre-conditions:** P0.1 and P0.2 both green.

**Why:** Heavy crons have been unloaded since 2026-05-12. ~13 days
of unmined sessions + unabsorbed articles backlog. All the local-
mode work shipped to make this safe.

**No code change — operational step.**

### Procedure

```sh
# 1. Sanity-check the production ceiling is reasonable
grep daily_anthropic /Users/oliverkriska/Projects/scriptorium/scribe.yaml
# Expected: daily_anthropic_output_token_ceiling: 2000000

# 2. Reload heavy crons
launchctl load ~/Library/LaunchAgents/com.scribe.sync-projects.plist
launchctl load ~/Library/LaunchAgents/com.scribe.sync-sessions.plist

# 3. Confirm they're listed
launchctl list | grep com.scribe

# 4. Watch the first 24h of cost ledger
tail -f /Users/oliverkriska/Projects/scriptorium/output/costs/$(date +%F).jsonl
```

### Acceptance criteria

- Both plists load without error
- First scheduled run completes (check `output/runs/$(date +%F).jsonl`
  for fresh entries within the cron interval)
- First day's `output/costs/$(date +%F).jsonl` shows pass-2 calls
  routed through `ollama/gemma3:27b` (not `sonnet`)
- Anthropic spend in the same ledger stays well under the 2M ceiling

### Rollback

```sh
launchctl unload ~/Library/LaunchAgents/com.scribe.sync-projects.plist
launchctl unload ~/Library/LaunchAgents/com.scribe.sync-sessions.plist
```

### Estimate

20 min (5 min to enable, 15 min to confirm first run lands).

---

## P0.4 — Make new knobs discoverable in `scribe.yaml`

**Why:** Two new knobs landed this week:
1. `absorb.pass2_mode` / `pass2_provider` / `pass2_model` /
   `pass2_parallel` / `pass2_timeout_min` (Phase 4B layer 2)
2. `sync.daily_anthropic_output_token_ceiling` (followup item 2)

Neither shows up in the YAML-block scaffolding that `scribe init`
writes or that `loadConfig` appends as a backfill hint. Users who
don't read source code can't find them. Other-session's observation
#1 calls this out explicitly.

### Files

- edit: `cmd/scribe/config.go` — `absorbDefaultYAMLBlock()` to
  include commented-out pass-2 knobs (currently has `pass2_mode`,
  `pass2_provider`, `pass2_model` from the Phase 4B commit; **verify**
  they actually emit — pre-flight read the function output)
- edit: `cmd/scribe/config.go` — new section or extension of an
  existing helper to surface `sync.daily_anthropic_output_token_ceiling`
  in the sync block of the YAML scaffold
- edit: `cmd/scribe/init.go` (or wherever `scribe init` writes the
  initial scribe.yaml) — confirm the new sync knob is emitted

### Design

For the sync knob, add the commented hint to whichever function
backfills sync defaults. Pattern matches the existing
`absorbDefaultYAMLBlock` approach:

```yaml
sync:
  # Daily anthropic output-token ceiling (Phase 4B reliability fix).
  # When the day's anthropic output tokens cross this number, scribe
  # aborts further claude -p calls with ErrDailyBudgetExhausted —
  # the cron logs the abort and exits clean, retrying the next day.
  # Local-provider calls (ollama) bypass the gate. Set to 0 to
  # disable entirely. Suggested production: 2_000_000 (≈ 30% of
  # the 2026-05-11 runaway).
  # env: SCRIBE_BYPASS_BUDGET=1 bypasses for emergency one-offs.
  # daily_anthropic_output_token_ceiling: 0
```

For the pass-2 knobs, the commit `125b88a` already added them to
`absorbDefaultYAMLBlock`. Verify by reading the function output
or running `scribe init` against a fresh `t.TempDir()` and grepping
the resulting scribe.yaml. If missing, add.

### Acceptance criteria

A `scribe init -p /tmp/freshkb` followed by
`grep -E "pass2_mode|pass2_provider|daily_anthropic" /tmp/freshkb/scribe.yaml`
prints all 4 lines (commented).

### Risks

- YAML escaping in Go raw strings — backticks inside backtick-
  delimited strings need the `+"`x`"+` workaround already used
  elsewhere in `absorbDefaultYAMLBlock`.

### Estimate

45 min, ~50 lines.

---

# P1 — Good improvement

## P1.1 — `SCRIBE_PASS2_MODE` env override

**Why:** Other-session's observation #4 — `absorb-compare.sh` needs
to flip `pass2_mode` between `tools` and `json` without editing
scribe.yaml. Editing yaml mid-script is brittle; an env override
is clean and pattern-matches `SCRIBE_BYPASS_BUDGET`.

### Design

In `applyAbsorbDefaults`, after the default merge but before the
auto-flip-mode logic:

```go
if env := os.Getenv("SCRIBE_PASS2_MODE"); env != "" {
    logMsg("config", "SCRIBE_PASS2_MODE=%q overriding scribe.yaml absorb.pass2_mode=%q", env, cfg.Pass2Mode)
    cfg.Pass2Mode = env
}
```

The existing auto-flip-mode-to-json-when-non-anthropic-provider
logic should still run AFTER this override so a misconfigured env
(`SCRIBE_PASS2_MODE=tools` + `pass2_provider: ollama`) still gets
flipped back to json with a log line.

Same shape for `SCRIBE_PASS2_PROVIDER` and `SCRIBE_PASS2_MODEL`
if we want full env-driven config; pick one consistent surface
(do all three or do none — half-overrides are confusing).

### Files

- edit: `cmd/scribe/config.go` — `applyAbsorbDefaults`
- edit: `scripts/absorb-compare.sh` — use the new env var instead of
  editing scribe.yaml
- new: test for the override in `cmd/scribe/config_absorb_test.go`
  using `t.Setenv`

### Acceptance criteria

- `SCRIBE_PASS2_MODE=json scribe sync` engages json mode regardless
  of scribe.yaml
- `SCRIBE_PASS2_MODE=` (empty) is a no-op
- The auto-flip still wins over a mis-set env

### Risks

- Three env vars (`SCRIBE_PASS2_MODE`, `_PROVIDER`, `_MODEL`) is
  config sprawl. Document them together in one block in scribe.yaml
  comments AND in the absorb-compare.md README.

### Estimate

30 min, ~25 lines + test.

---

## P1.2 — `scribe doctor` local-mode coherence checks

**Why:** Catches misconfigurations before a sync run wastes 20 min.
Pure read; no behavior change. High signal-to-effort.

### New checks

In whichever `cmd/scribe/doctor.go` section walks absorb config:

1. **pass2_provider=ollama but ollama daemon down**
   - Hit `/api/tags` on `Contextualize.OllamaURL` with a 2s timeout
   - WARN if unreachable: "absorb.pass2_provider=ollama but ollama
     is unreachable at <url> — `brew services start ollama`"

2. **pass2_model not pulled locally**
   - Parse `/api/tags` response
   - Use existing `modelListContains` helper
   - WARN if missing: "absorb.pass2_model=<name> not pulled — run
     `ollama pull <name>`"

3. **pass2_provider=ollama AND atomic_facts off**
   - Models fabricate fact-IDs at high rate without facts pass on
   - WARN: "absorb.pass2_provider=ollama but atomic_facts=false —
     model will fabricate [cNN-fM] citations; enable atomic_facts
     to ground them in real fact IDs"

4. **sync.daily_anthropic_output_token_ceiling=0 AND any provider stays anthropic**
   - INFO (not WARN): "no anthropic budget ceiling configured —
     after the 2026-05-11 incident this is recommended. Suggested
     value: 2_000_000."

### Files

- edit: `cmd/scribe/doctor.go` — new section "local mode" or
  inside an existing "absorb" section, with the 4 checks above
- edit: `cmd/scribe/doctor_test.go` — test each check fires under
  the right config (skip the ollama-reachability check in CI; gate
  with build tag or skip when `OLLAMA_URL` env unset)

### Design

Use existing `ollamaProvider.listedModels` for check #2 (refactor it
into a free function if it's currently a method, to avoid
constructing a provider for a doctor probe).

```go
// doctor.go pseudocode
func doctorLocalMode(cfg *ScribeConfig) []doctorFinding {
    var findings []doctorFinding
    if strings.EqualFold(cfg.Absorb.Pass2Provider, "ollama") {
        url := cfg.Absorb.Contextualize.OllamaURL
        models, err := probeOllamaTags(url, 2*time.Second)
        if err != nil {
            findings = append(findings, warn("ollama_unreachable", ...))
        } else if cfg.Absorb.Pass2Model != "" && !modelListContains(models, cfg.Absorb.Pass2Model) {
            findings = append(findings, warn("ollama_model_missing", ...))
        }
        if cfg.Absorb.AtomicFacts == nil || !*cfg.Absorb.AtomicFacts {
            findings = append(findings, warn("ollama_pass2_no_facts", ...))
        }
    }
    if cfg.Sync.DailyAnthropicOutputTokenCeiling == 0 {
        findings = append(findings, info("no_anthropic_ceiling", ...))
    }
    return findings
}
```

### Acceptance criteria

- Misconfigured KB shows all relevant warnings in `scribe doctor` output
- Well-configured KB shows none of the WARN findings (the INFO about
  ceiling=0 is acceptable noise)
- The reachability check finishes in under 2s even when ollama is down

### Risks

- 2s timeout per check could add to doctor wallclock if many checks
  hit ollama. One probe at the top of `doctorLocalMode` and reusing
  the result.

### Estimate

1h, ~80 lines + tests.

---

## P1.3 — Idempotent `absorbDefaultYAMLBlock` re-merge

**Why:** The append-on-missing behavior bit us TWICE this session
(duplicate `pass2_timeout_min` → entire absorb block wiped to
defaults). yaml.v3 errors on duplicate keys; `loadConfig` (post-fix)
now logs the error but still falls back to defaults. The right fix
is to make the backfill smart enough to never produce duplicates.

This is annoying to ship right — yaml.v3 doesn't preserve comment
placement on round-trip. Two approaches:

### Approach A — token-level merge (recommended)

Parse the existing scribe.yaml as a yaml.Node tree, traverse to
the `absorb:` mapping, set only keys not present, re-emit. Comments
attached to existing keys survive; new keys land at the end of the
mapping with the canned-comment line preserved from the template.

### Approach B — accept comment drift

Parse → set missing → marshal → write. Comments dropped. Document
clearly.

Approach A is the right one for a personal-KB tool — but it's
~150 lines including the node-walking helper. Approach B is ~30
lines but degrades the file aesthetically.

### Recommendation

**Approach A**, but only when the underlying YAML lib supports it
cleanly. `gopkg.in/yaml.v3` does; the helper to find a mapping by
key in a Node tree is the bulk of the code.

### Files

- edit: `cmd/scribe/config.go` — `appendAbsorbBlockQuiet` becomes
  `mergeAbsorbBlockQuiet`. Same call site in `loadConfig`.
- new: `cmd/scribe/config_yaml_merge_test.go` — test:
  - empty file → gets full absorb block
  - file with partial absorb block (e.g. only `strictness:`) →
    other keys appended, no duplicates
  - file with full absorb block + user-added pass2_mode → unchanged
  - file with duplicate keys → fixed (deduped, last value wins,
    log line emitted)

### Risks

- Wrong yaml.Node manipulation can corrupt the user's file. Mitigate
  with the existing tmp-file-then-rename pattern + a clear "this
  operation only adds, never removes" invariant tested explicitly.
- Comment loss on keys we DID modify. Less bad than data loss.

### Acceptance criteria

- After the rewrite, the duplicate-key bug from this session
  (adding `pass2_timeout_min` by hand when one already exists) is
  impossible — the merge either skips because the key already exists
  or replaces in-place.

### Estimate

2h, ~150 lines + tests.

---

# Sequencing

```
day 1 — P0 batch (all in one session, ~90 min total):
  P0.1 fact-stripper validation              — 10 min, no code
  P0.2 budget ceiling fire-drill             — 5 min, no code
  P0.3 re-enable heavy crons                 — 20 min, operational
  P0.4 knob discoverability in YAML scaffold — 45 min, ~50 lines

day 2 — P1 batch (one PR, ~2h total):
  P1.1 SCRIBE_PASS2_MODE env override        — 30 min, ~25 lines
  P1.2 scribe doctor local-mode checks       — 1h, ~80 lines

defer until P0+P1.1+P1.2 prove out for 24h:
  P1.3 idempotent YAML merge                 — only if duplicate-key
                                               bug bites again
```

P0 items are commit-able as one PR or even as ad-hoc shell sessions
(P0.1–P0.3 produce no code change; only P0.4 needs a commit). P1
items belong in one cohesive PR — the env override and the doctor
checks both touch config / scribe.yaml interpretation and benefit
from co-review.

---

# Out of scope (deliberately)

The following are real follow-ups but **not in this plan**:

- **Phase 4C session mining via envelope** — separate multi-week
  scope. Should be its own plan after a week of clean production
  data on items 1–3.
- **Pass-1 chaptered local migration** — Phase 4D. Pass-1 reads
  TOC sidecars, which is harder for small local models. Defer
  until there's a model proven good at that workload.
- **`loadConfig` per-call caching** (other-session obs #2) —
  premature without a profile. Look at it if `scribe doctor` reports
  ledger-write contention or sync wallclock grows mysteriously.
- **`--absorb-only` flag on `SyncCmd`** (other-session obs #3) —
  only matters if `absorb-compare.sh` gets used more than quarterly.
  Run the script as-is; if the project-extraction noise bothers
  someone, file then.
- **Multi-provider routing config normalization** — the per-op
  knobs are sprawling but each one has a clear job. Wait for a real
  second use case before normalizing.
- **Lint cleanup** (other-session obs #5, #6, #7, #8) — batch into
  a future PR when someone's already touching those files. None
  are load-bearing.

---

# What "production-ready" means after this plan

After all P0 items + P1.1/P1.2 land:

1. Heavy crons resume running with a hard ceiling backstop.
2. Local pass-2 fabrication rate stays at 0% (validated empirically).
3. New users discover the local-mode knobs by reading scribe.yaml.
4. Misconfigurations surface via `scribe doctor` before a 20-min
   sync wastes wallclock.
5. The `absorb-compare.sh` script works without yaml mutation.

That's the bar. Phase 4C and beyond can take their own plans.
