#!/usr/bin/env bash
# Rasterise the bundled, version-free SVG into site/public/og.png.
#
# og.png is a social card with text baked into the pixels. It goes stale
# INVISIBLY — the HTML can be perfect while shares still show an old card —
# and third-party platforms cache the image hard. The template is the single
# source of truth and is committed with the skill, so the card is always
# reproducible (never depends on a /tmp scratch file surviving).
set -euo pipefail

REPO="$(git rev-parse --show-toplevel)"
SKILL_DIR="$REPO/.claude/skills/getscribe-site-sync"
TMPL="$SKILL_DIR/assets/og.svg.tmpl"
OUT="$REPO/site/public/og.png"
# shellcheck source=_pins.sh
source "$SKILL_DIR/scripts/_pins.sh"

command -v rsvg-convert >/dev/null 2>&1 || {
  echo "FATAL: rsvg-convert not found. Install: brew install librsvg"; exit 1; }

# The template must stay evergreen — refuse to bake a version into pixels.
if grep -nEi "$PIN_REGEX" "$TMPL" >/dev/null 2>&1; then
  echo "FATAL: og.svg.tmpl contains a version pin — fix the template first:"
  grep -nEi "$PIN_REGEX" "$TMPL" | sed 's/^/   /'
  exit 1
fi

rsvg-convert -w 1200 -h 630 "$TMPL" -o "$OUT"
echo "regenerated $OUT ($(wc -c <"$OUT" | tr -d ' ') bytes, 1200x630)"
echo "Tip: open it and eyeball the text — rsvg uses system font substitution."
