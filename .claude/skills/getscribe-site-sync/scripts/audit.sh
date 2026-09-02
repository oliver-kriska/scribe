#!/usr/bin/env bash
# Deterministic site audit. No LLM judgement — pure pattern checks.
#
# FATAL (exit 1): any version pin on any text surface, or a version pin in the
#   social-card SVG template. These are the cardinal-invariant violations.
# FATAL (exit 1): a command-path count on the page that disagrees with the
#   binary. Kong's root --help already prints every full command path, so this
#   is an exact comparison, not a heuristic — a stale count is the one defect
#   the page's own "every figure is checkable" caption dares a reader to find.
# ADVISORY (printed, never fatal): other hard-count claims (LaunchAgents, input
#   streams) whose true value is built at runtime and cannot be compared exactly.
set -euo pipefail

REPO="$(git rev-parse --show-toplevel)"
SKILL_DIR="$REPO/.claude/skills/getscribe-site-sync"
PUB="$REPO/site/public"
# shellcheck source=_pins.sh
source "$SKILL_DIR/scripts/_pins.sh"

SURFACES=(index.html index.md llms.txt llms-full.txt setup.md topologies.md)
fatal=0

echo "── version-pin scan ($PUB) ──"
for f in "${SURFACES[@]}"; do
  if hits="$(pin_scan "$PUB/$f" 2>/dev/null)"; then
    echo "PIN  $f:"
    echo "$hits" | sed 's/^/     /'
    fatal=1
  else
    echo "ok   $f"
  fi
done

echo "── brand and social-card assets ──"
TMPL="$SKILL_DIR/assets/og.svg.tmpl"
if [ ! -f "$TMPL" ]; then
  echo "PIN  missing og template ($TMPL) — cannot regenerate an evergreen card"
  fatal=1
elif tmpl_hits="$(pin_scan "$TMPL")"; then
  echo "PIN  og.svg.tmpl carries a version — the card would bake it into pixels:"
  echo "$tmpl_hits" | sed 's/^/     /'
  fatal=1
else
  echo "ok   og template present and version-free"
fi

active_og="$(grep -oE 'og:image"[^>]*content="https://getscribe.dev/og-v[0-9]+\.png"' "$PUB/index.html" \
  | grep -oE 'og-v[0-9]+\.png' | head -1 || true)"
if [ -z "$active_og" ]; then
  echo "DRIFT homepage og:image is not a cache-busted og-vN.png"
  fatal=1
elif [ ! -f "$PUB/$active_og" ]; then
  echo "DRIFT homepage references missing social card: $active_og"
  fatal=1
else
  echo "ok   active social card exists ($active_og)"
fi

if grep -nE '(content=|"image":)[^>]*og\.png' "$PUB/index.html" "$REPO/site/templates/doc.html" >/dev/null 2>&1; then
  echo "DRIFT active metadata still references legacy og.png:"
  grep -nE '(content=|"image":)[^>]*og\.png' "$PUB/index.html" "$REPO/site/templates/doc.html" | sed 's/^/     /'
  fatal=1
elif [ -n "$active_og" ] && ! grep -qF "https://getscribe.dev/$active_og" "$REPO/site/templates/doc.html"; then
  echo "DRIFT document template does not use the active social card ($active_og)"
  fatal=1
else
  echo "ok   homepage and document metadata use the versioned social card"
fi

for asset in favicon.svg favicon-32.png favicon.ico apple-touch-icon.png logo-512.png; do
  if [ -s "$PUB/$asset" ]; then
    echo "ok   fetchable brand asset present ($asset)"
  else
    echo "DRIFT missing or empty brand asset: $asset"
    fatal=1
  fi
done

if ! grep -qF '"logo": "https://getscribe.dev/logo-512.png"' "$PUB/index.html"; then
  echo "DRIFT Organization.logo must point at the square logo-512.png"
  fatal=1
else
  echo "ok   Organization.logo uses the square logo"
fi

# Command-path count. Kong prints every full command path in the root help at
# exactly two spaces of indent, so the true number is an exact count, not a
# heuristic. Prefer the repo-local build (may be ahead of the released binary);
# fall back to whatever is on PATH. Skip only if neither exists.
echo "── command-path count (exact) ──"
SCRIBE_BIN=""
if [ -x "$REPO/bin/scribe" ]; then SCRIBE_BIN="$REPO/bin/scribe"
elif command -v scribe >/dev/null 2>&1; then SCRIBE_BIN="$(command -v scribe)"
fi

if [ -z "$SCRIBE_BIN" ]; then
  echo "skip no scribe binary (build with 'make build') — command-path count unverified"
else
  real="$("$SCRIBE_BIN" --help 2>&1 | grep -cE '^  [a-z]')"
  set +e
  claimed="$(grep -rhoE '(<b>)?[0-9]+(</b>)? command paths' \
    "$PUB"/index.html "$PUB"/index.md 2>/dev/null \
    | grep -oE '[0-9]+' | sort -u)"
  set -e
  if [ -z "$claimed" ]; then
    echo "ok   no command-path claim on the page"
  else
    bad=0
    for c in $claimed; do
      if [ "$c" != "$real" ]; then
        echo "DRIFT page claims $c command paths; $SCRIBE_BIN reports $real"
        bad=1
      fi
    done
    if [ "$bad" -eq 1 ]; then
      grep -rn "command paths\|see all [0-9]" "$PUB"/index.html "$PUB"/index.md | sed 's/^/     /'
      echo "     → fix every occurrence, including the 'see all N' caption"
      fatal=1
    else
      echo "ok   command-path claim ($real) matches $SCRIBE_BIN"
    fi
  fi
  # the 'see all N' caption carries the number without the noun — check it too
  set +e
  loose="$(grep -rhoE 'see all [0-9]+' "$PUB"/index.html "$PUB"/index.md 2>/dev/null | grep -oE '[0-9]+' | sort -u)"
  set -e
  for c in $loose; do
    if [ "$c" != "$real" ]; then
      echo "DRIFT 'see all $c' disagrees with $real"; fatal=1
    fi
  done
fi

# Other hard-count claims are built at runtime (cron labels, stream names), so
# these stay advisory. set +e so a no-match grep cannot abort under pipefail.
echo "── other hard-count claims (verify against code; advisory) ──"
set +e
claims="$(grep -rhoE '[0-9]+(</b>)? (subcommands|LaunchAgents|input streams|cron jobs|session providers|typed-edge kinds|inference modes)' \
  "$PUB"/index.html "$PUB"/index.md 2>/dev/null | sort -u)"
set -e
if [ -n "$claims" ]; then
  echo "$claims" | sed 's/^/     claims: /'
  echo "     → confirm: LaunchAgents/cron in cmd/scribe/cron.go"
else
  echo "ok   no other hard-count claims found on the page"
fi

echo
if [ "$fatal" -ne 0 ]; then
  echo "AUDIT: FAIL — version pins, brand drift, and/or count drift (see PIN/DRIFT lines above)."
  exit 1
fi
echo "AUDIT: clean (no pins, command-path count matches). Verify any advisory claims above before declaring done."
