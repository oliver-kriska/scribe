#!/usr/bin/env bash
# Deterministic site audit. No LLM judgement — pure pattern checks.
#
# FATAL (exit 1): any version pin on any text surface, or a version pin in the
#   og.png SVG template. These are the cardinal-invariant violations.
# ADVISORY (printed, never fatal): hard-count claims on the page that disagree
#   with the code (subcommands, LaunchAgents). Counting code heuristically is
#   fragile, so this only flags for the LLM/automation to verify — it must not
#   block a deploy on a possibly-wrong heuristic.
set -euo pipefail

REPO="$(git rev-parse --show-toplevel)"
SKILL_DIR="$REPO/.claude/skills/getscribe-site-sync"
PUB="$REPO/site/public"
# shellcheck source=_pins.sh
source "$SKILL_DIR/scripts/_pins.sh"

SURFACES=(index.html index.md llms.txt llms-full.txt)
fatal=0

echo "── version-pin scan ($PUB) ──"
for f in "${SURFACES[@]}"; do
  if hits="$(grep -nEi "$PIN_REGEX" "$PUB/$f" 2>/dev/null)"; then
    echo "PIN  $f:"
    echo "$hits" | sed 's/^/     /'
    fatal=1
  else
    echo "ok   $f"
  fi
done

echo "── og.png social card ──"
TMPL="$SKILL_DIR/assets/og.svg.tmpl"
if [ ! -f "$TMPL" ]; then
  echo "PIN  missing og template ($TMPL) — cannot regenerate an evergreen card"
  fatal=1
elif grep -nEi "$PIN_REGEX" "$TMPL" >/dev/null 2>&1; then
  echo "PIN  og.svg.tmpl carries a version — the card would bake it into pixels:"
  grep -nEi "$PIN_REGEX" "$TMPL" | sed 's/^/     /'
  fatal=1
else
  echo "ok   og template present and version-free"
fi

# Hard-count claims. Deriving the true number from Go source is too fragile to
# fake a comparison (Kong nests sub-structs; cron labels are built at runtime),
# so we surface the CLAIMS the page makes and tell the operator to verify them
# against the authoritative source. Informational, never fatal — set +e so a
# no-match grep can't abort the script under pipefail.
echo "── hard-count claims (verify against code; advisory) ──"
set +e
claims="$(grep -rhoE '[0-9]+ (subcommands|LaunchAgents|input streams|cron jobs)' \
  "$PUB"/index.html "$PUB"/index.md 2>/dev/null | sort -u)"
set -e
if [ -n "$claims" ]; then
  echo "$claims" | sed 's/^/     claims: /'
  echo "     → confirm: subcommands via 'scribe --help'; LaunchAgents/cron in cmd/scribe/cron.go"
else
  echo "ok   no hard-count claims found on the page"
fi

echo
if [ "$fatal" -ne 0 ]; then
  echo "AUDIT: FAIL — version pins present (see PIN lines above)."
  exit 1
fi
echo "AUDIT: clean (no pins). Verify any hard-count claims listed above before declaring done."
