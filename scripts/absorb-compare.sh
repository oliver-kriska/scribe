#!/usr/bin/env bash
# absorb-compare.sh — runs `scribe sync` twice against the same raw
# article (once with pass2_mode=tools, once with pass2_mode=json) and
# emits a side-by-side report of the resulting wiki output.
#
# This is the Phase 4B layer 3 quality probe from the local-model
# follow-up plan. The pass-2 envelope path (json) shipped in commit
# 125b88a; this script lets us measure how much article quality the
# envelope path costs vs the historical claude -p tools path.
#
# REQUIRES:
#   - Run from the KB root (the directory holding scribe.yaml).
#   - jq, sed, diff, find, awk available on PATH.
#   - scribe binary on PATH or invoked via SCRIBE_BIN=/path/to/scribe.
#
# MUTATES STATE while running:
#   - sets SCRIBE_PASS2_MODE in the scribe sync env (no yaml mutation)
#   - deletes the target file's entry from wiki/_absorb_log.json so
#     it re-absorbs (restored at end)
#   - the wiki/ tree (snapshotted to OUT_DIR/baseline, restored
#     between runs and at end)
#   - the output/{plans,facts,absorb-facts}/ trees (restored too)
#
# USAGE: scripts/absorb-compare.sh <raw-file>
#   <raw-file>  Path to a markdown file already in raw/articles/.
#                Relative or absolute. Compared against itself.
#
# ENV:
#   SCRIBE_BIN   path to scribe binary (default: scribe on PATH)
#   OUT_DIR      where to keep snapshots and the report
#                  (default: /tmp/scribe-compare-$$)
#   KEEP_OUT     non-empty → keep OUT_DIR after the report prints

set -euo pipefail

SCRIBE_BIN="${SCRIBE_BIN:-scribe}"
OUT_DIR="${OUT_DIR:-/tmp/scribe-compare-$$}"

abort() {
    echo "ERROR: $*" >&2
    exit 1
}

# --- preflight --------------------------------------------------------

if [ "$#" -ne 1 ]; then
    echo "Usage: $0 <raw-article-path>" >&2
    exit 2
fi

RAW_FILE_ARG="$1"

[ -f scribe.yaml ] || abort "no scribe.yaml in cwd ($(pwd)) — run from KB root"
command -v "$SCRIBE_BIN" >/dev/null 2>&1 || abort "scribe binary not on PATH (set SCRIBE_BIN)"
command -v jq >/dev/null 2>&1 || abort "jq not on PATH"
command -v sed >/dev/null 2>&1 || abort "sed not on PATH"

# Resolve raw file to a canonical path under raw/articles/.
if [ -f "$RAW_FILE_ARG" ]; then
    RAW_FILE="$(cd "$(dirname "$RAW_FILE_ARG")" && pwd)/$(basename "$RAW_FILE_ARG")"
else
    RAW_FILE="$(pwd)/$RAW_FILE_ARG"
fi
[ -f "$RAW_FILE" ] || abort "raw file not found: $RAW_FILE_ARG"
case "$RAW_FILE" in
    "$(pwd)/raw/articles/"*) : ;;
    *) abort "raw file must live under raw/articles/ ($RAW_FILE)" ;;
esac
RAW_BASE="$(basename "$RAW_FILE")"

mkdir -p "$OUT_DIR"
echo "[$(date -u +%FT%TZ)] absorb-compare starting"
echo "  raw file:  $RAW_BASE"
echo "  out dir:   $OUT_DIR"
echo "  scribe:    $SCRIBE_BIN"

# --- snapshot & restore helpers ---------------------------------------
#
# snapshot_state writes the dirs we expect absorb to mutate into a
# named subdir of OUT_DIR. restore_state copies the named subdir back
# over the KB. Anything outside the listed paths is untouched.

WIKI_DIRS=(wiki projects research solutions tools decisions patterns ideas people sessions)

snapshot_state() {
    local label="$1"
    local target="$OUT_DIR/$label"
    mkdir -p "$target"
    for d in "${WIKI_DIRS[@]}" output; do
        if [ -d "$d" ]; then
            rsync -a --delete "$d/" "$target/$d/"
        fi
    done
}

restore_state() {
    local label="$1"
    local source="$OUT_DIR/$label"
    for d in "${WIKI_DIRS[@]}" output; do
        if [ -d "$source/$d" ]; then
            rsync -a --delete "$source/$d/" "$d/"
        else
            rm -rf "$d"
        fi
    done
}

# --- restore-on-exit guard --------------------------------------------

trap 'echo "[$(date -u +%FT%TZ)] restoring state"; restore_state baseline 2>/dev/null || true; if [ -z "${KEEP_OUT:-}" ]; then rm -rf "$OUT_DIR"; fi' EXIT

# --- baseline snapshot ------------------------------------------------

echo "[$(date -u +%FT%TZ)] snapshotting baseline"
snapshot_state baseline

# --- one run per mode -------------------------------------------------

# Remove the target file's entry from wiki/_absorb_log.json so the
# next sync run actually re-absorbs it. jq's `del(...)` is a no-op
# when the key is absent; we don't care either way.
forget_absorb_entry() {
    local log="wiki/_absorb_log.json"
    [ -f "$log" ] || return 0
    local tmp
    tmp="$(mktemp)"
    jq --arg k "$RAW_BASE" 'del(.[$k])' "$log" >"$tmp"
    mv "$tmp" "$log"
}

run_mode() {
    local mode="$1"
    echo "[$(date -u +%FT%TZ)] mode=$mode: restoring baseline"
    restore_state baseline
    forget_absorb_entry

    echo "[$(date -u +%FT%TZ)] mode=$mode: running scribe sync (this will take a while)"
    SCRIBE_PASS2_MODE="$mode" "$SCRIBE_BIN" sync >"$OUT_DIR/$mode.log" 2>&1 || {
        echo "WARN: scribe sync exited non-zero (see $OUT_DIR/$mode.log)" >&2
    }

    echo "[$(date -u +%FT%TZ)] mode=$mode: snapshotting wiki/ output"
    snapshot_state "$mode"
}

run_mode tools
run_mode json

echo "[$(date -u +%FT%TZ)] both runs complete — computing report"

# --- metric helpers ---------------------------------------------------

# Count files added by a run relative to baseline (paths only inside
# wikiDirs).
count_added_files() {
    local label="$1"
    local n=0
    for d in "${WIKI_DIRS[@]}"; do
        [ -d "$OUT_DIR/$label/$d" ] || continue
        while IFS= read -r f; do
            local rel="${f#"$OUT_DIR/$label/"}"
            if [ ! -e "$OUT_DIR/baseline/$rel" ]; then
                n=$((n + 1))
            fi
        done < <(find "$OUT_DIR/$label/$d" -type f -name '*.md')
    done
    echo "$n"
}

# Sum line counts across files added by a run.
sum_added_lines() {
    local label="$1"
    local total=0
    for d in "${WIKI_DIRS[@]}"; do
        [ -d "$OUT_DIR/$label/$d" ] || continue
        while IFS= read -r f; do
            local rel="${f#"$OUT_DIR/$label/"}"
            if [ ! -e "$OUT_DIR/baseline/$rel" ]; then
                local n
                n=$(wc -l <"$f" | tr -d ' ')
                total=$((total + n))
            fi
        done < <(find "$OUT_DIR/$label/$d" -type f -name '*.md')
    done
    echo "$total"
}

# Count distinct wikilink targets (case-sensitive) across added files.
count_wikilinks() {
    local label="$1"
    local files=()
    for d in "${WIKI_DIRS[@]}"; do
        [ -d "$OUT_DIR/$label/$d" ] || continue
        while IFS= read -r f; do
            local rel="${f#"$OUT_DIR/$label/"}"
            if [ ! -e "$OUT_DIR/baseline/$rel" ]; then
                files+=("$f")
            fi
        done < <(find "$OUT_DIR/$label/$d" -type f -name '*.md')
    done
    if [ "${#files[@]}" -eq 0 ]; then
        echo "0"
        return
    fi
    grep -hoE '\[\[[^]]+\]\]' "${files[@]}" 2>/dev/null \
        | sort -u | wc -l | tr -d ' '
}

# Count [cNN-fM] citation brackets across added files.
count_citations() {
    local label="$1"
    local files=()
    for d in "${WIKI_DIRS[@]}"; do
        [ -d "$OUT_DIR/$label/$d" ] || continue
        while IFS= read -r f; do
            local rel="${f#"$OUT_DIR/$label/"}"
            if [ ! -e "$OUT_DIR/baseline/$rel" ]; then
                files+=("$f")
            fi
        done < <(find "$OUT_DIR/$label/$d" -type f -name '*.md')
    done
    if [ "${#files[@]}" -eq 0 ]; then
        echo "0"
        return
    fi
    grep -hoE '\[c[0-9]+-f[0-9]+\]' "${files[@]}" 2>/dev/null | wc -l | tr -d ' '
}

# Orphan wikilinks = wikilink targets that don't resolve to a file
# already in baseline (i.e. would 404 against the snapshot's wiki).
# Heuristic: target's slug (lowercased, spaces→dashes) must match a
# basename without extension somewhere under wikiDirs in baseline.
count_orphans() {
    local label="$1"
    local files=()
    for d in "${WIKI_DIRS[@]}"; do
        [ -d "$OUT_DIR/$label/$d" ] || continue
        while IFS= read -r f; do
            local rel="${f#"$OUT_DIR/$label/"}"
            if [ ! -e "$OUT_DIR/baseline/$rel" ]; then
                files+=("$f")
            fi
        done < <(find "$OUT_DIR/$label/$d" -type f -name '*.md')
    done
    if [ "${#files[@]}" -eq 0 ]; then
        echo "0"
        return
    fi
    local known
    known="$(mktemp)"
    for d in "${WIKI_DIRS[@]}"; do
        [ -d "$OUT_DIR/baseline/$d" ] || continue
        find "$OUT_DIR/baseline/$d" -type f -name '*.md' \
            -exec basename {} .md \; \
            | awk '{print tolower($0)}' >>"$known"
    done
    sort -u -o "$known" "$known"
    local targets
    targets="$(mktemp)"
    grep -hoE '\[\[[^]]+\]\]' "${files[@]}" 2>/dev/null \
        | sed -E 's/^\[\[//; s/\]\]$//; s/\|.*$//' \
        | awk '{print tolower($0)}' \
        | sed -E 's/ /-/g' \
        | sort -u >"$targets"
    local orphan_count
    orphan_count="$(comm -23 "$targets" "$known" | wc -l | tr -d ' ')"
    rm -f "$known" "$targets"
    echo "$orphan_count"
}

# --- report -----------------------------------------------------------

T_FILES=$(count_added_files tools)
J_FILES=$(count_added_files json)
T_LINES=$(sum_added_lines tools)
J_LINES=$(sum_added_lines json)
T_LINKS=$(count_wikilinks tools)
J_LINKS=$(count_wikilinks json)
T_CITES=$(count_citations tools)
J_CITES=$(count_citations json)
T_ORPHANS=$(count_orphans tools)
J_ORPHANS=$(count_orphans json)

REPORT="$OUT_DIR/report.txt"
{
    echo "absorb-compare report"
    echo "raw file:    $RAW_BASE"
    echo "generated:   $(date -u +%FT%TZ)"
    echo
    printf '%-22s %10s %10s %12s\n' metric tools json delta
    printf '%-22s %10s %10s %12s\n' ---------------------- ---------- ---------- ------------
    printf '%-22s %10s %10s %+12d\n' added_files "$T_FILES" "$J_FILES" $((J_FILES - T_FILES))
    printf '%-22s %10s %10s %+12d\n' added_lines "$T_LINES" "$J_LINES" $((J_LINES - T_LINES))
    printf '%-22s %10s %10s %+12d\n' distinct_wikilinks "$T_LINKS" "$J_LINKS" $((J_LINKS - T_LINKS))
    printf '%-22s %10s %10s %+12d\n' citation_brackets "$T_CITES" "$J_CITES" $((J_CITES - T_CITES))
    printf '%-22s %10s %10s %+12d\n' orphan_wikilinks "$T_ORPHANS" "$J_ORPHANS" $((J_ORPHANS - T_ORPHANS))
    echo
    echo "logs:   $OUT_DIR/tools.log  $OUT_DIR/json.log"
    echo "wikis:  $OUT_DIR/tools/wiki  $OUT_DIR/json/wiki"
} | tee "$REPORT"

echo
echo "[$(date -u +%FT%TZ)] absorb-compare done — report at $REPORT"
