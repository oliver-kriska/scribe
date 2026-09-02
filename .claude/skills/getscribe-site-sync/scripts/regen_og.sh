#!/usr/bin/env bash
# Rasterise the bundled, version-free SVG into a cache-busted og-vN.png.
#
# The social card has text baked into the pixels. It goes stale
# INVISIBLY — the HTML can be perfect while shares still show an old card —
# and third-party platforms cache the image hard. The template is the single
# source of truth and is committed with the skill, so the card is always
# reproducible. Existing cards are never overwritten: a changed card always
# gets a new URL, and the caller then updates every active metadata reference.
set -euo pipefail

REPO="$(git rev-parse --show-toplevel)"
SKILL_DIR="$REPO/.claude/skills/getscribe-site-sync"
TMPL="$SKILL_DIR/assets/og.svg.tmpl"
PUB="$REPO/site/public"
INDEX="$PUB/index.html"
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

# If the page points at a new version whose file does not exist yet, render
# that path (the migration case). Otherwise increment the highest card number.
# Refuse to skip past an unreferenced card: that usually means the previous run
# succeeded but the metadata update was forgotten.
active="$(grep -oE 'og:image"[^>]*content="https://getscribe.dev/og-v[0-9]+\.png"' "$INDEX" \
  | grep -oE 'og-v[0-9]+\.png' | head -1 || true)"
active_n="$(printf '%s' "$active" | grep -oE '[0-9]+' || true)"
latest_n=1
for card in "$PUB"/og-v*.png; do
  [ -e "$card" ] || continue
  n="$(basename "$card" | grep -oE '[0-9]+')"
  [ "$n" -gt "$latest_n" ] && latest_n="$n"
done

if [ -n "$active_n" ] && [ "$active_n" -gt "$latest_n" ]; then
  next_n="$active_n"
elif [ -n "$active_n" ] && [ "$latest_n" -gt "$active_n" ]; then
  echo "FATAL: og-v$latest_n.png exists but index.html still references og-v$active_n.png."
  echo "       Review the newer card and update all metadata references before regenerating."
  exit 1
else
  next_n=$((latest_n + 1))
fi

OUT="$PUB/og-v$next_n.png"
[ ! -e "$OUT" ] || { echo "FATAL: refusing to overwrite $OUT"; exit 1; }

rsvg-convert -w 1200 -h 630 "$TMPL" -o "$OUT"
echo "regenerated $OUT ($(wc -c <"$OUT" | tr -d ' ') bytes, 1200x630)"
echo "Next: update og:image, twitter:image, and TechArticle.image to /og-v$next_n.png."
echo "Tip: open it and eyeball the text — rsvg uses system font substitution."
