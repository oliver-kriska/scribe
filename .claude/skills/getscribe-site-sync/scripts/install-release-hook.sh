#!/usr/bin/env bash
# Install the WARN-ONLY reference-transaction release hook, chain-safely.
#
# The hook only PRINTS a reminder when a release tag is created — it never
# deploys and never runs an LLM. Refreshing getscribe.dev stays a manual
# decision (invoke the getscribe-site-sync skill when you choose to).
#
# This repo already ships custom pre-commit / pre-push hooks, so clobbering is
# unacceptable. We target reference-transaction (currently unused here):
#   - no hook present            -> install ours
#   - our hook already present   -> idempotent no-op (re-stamp exec bit)
#   - a DIFFERENT hook present   -> back it up, install a dispatcher that runs
#                                   the original first (stdin tee'd) then ours
set -euo pipefail

REPO="$(git rev-parse --show-toplevel)"
SRC="$REPO/.claude/skills/getscribe-site-sync/scripts/hooks/reference-transaction"
HOOKS="$(git rev-parse --git-path hooks)"
DST="$HOOKS/reference-transaction"
MARKER="GSS-WARN-v1"

mkdir -p "$HOOKS"
chmod +x "$SRC" \
  "$REPO/.claude/skills/getscribe-site-sync/scripts/"*.sh

if [ ! -e "$DST" ]; then
  cp "$SRC" "$DST"; chmod +x "$DST"
  echo "installed: $DST"
elif grep -q "$MARKER" "$DST" 2>/dev/null; then
  cp "$SRC" "$DST"; chmod +x "$DST"
  echo "refreshed (already ours): $DST"
else
  ORIG="$HOOKS/reference-transaction.pre-gss"
  [ -e "$ORIG" ] || cp "$DST" "$ORIG"
  chmod +x "$ORIG"
  cat >"$DST" <<EOF
#!/usr/bin/env bash
# GSS dispatcher [marker: $MARKER] — runs the pre-existing hook, then ours.
# reference-transaction reads refs from stdin, so we capture stdin once and
# feed the same bytes to both hooks.
set -euo pipefail
_in="\$(cat)"
printf '%s' "\$_in" | "$ORIG" "\$@" || true
printf '%s' "\$_in" | "$SRC" "\$@"
EOF
  chmod +x "$DST"
  echo "chained: original preserved at $ORIG; dispatcher installed at $DST"
fi

echo
echo "Done. On the next 'git tag vX.Y.Z' you'll see a reminder to refresh"
echo "getscribe.dev. Nothing deploys automatically — run the"
echo "getscribe-site-sync skill yourself when you're ready."
echo "(.git/hooks isn't version-controlled; re-run this after a fresh clone.)"
