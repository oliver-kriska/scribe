#!/usr/bin/env bash
# Deploy site/ to Cloudflare Workers and verify the LIVE result.
#
# Why each guard exists:
#  - wrangler is a GLOBAL install; we never add a JS toolchain to this repo.
#  - Credentials come only from the repo-root .env (gitignored). We source it,
#    never print it.
#  - RTK rewrites bash commands and can swallow wrangler's dynamic progress
#    output, so the deploy runs under `rtk proxy` when rtk is present, and all
#    output is tee'd to a logfile we can actually read.
#  - Local DNS sometimes lags a fresh Cloudflare record, so live verification
#    tries a plain request first and falls back to --resolve with a freshly
#    dig'd Cloudflare IP rather than hardcoding one (the A record rotates).
set -euo pipefail

REPO="$(git rev-parse --show-toplevel)"
SKILL_DIR="$REPO/.claude/skills/getscribe-site-sync"
SITE="$REPO/site"
PUB="$SITE/public"
DOMAIN="getscribe.dev"
LOG="${TMPDIR:-/tmp}/getscribe-deploy.$$.log"
# shellcheck source=_pins.sh
source "$SKILL_DIR/scripts/_pins.sh"

[ -f "$REPO/.env" ] || { echo "FATAL: $REPO/.env not found (Cloudflare creds)"; exit 1; }

echo "── deploy ──"
deploy_cmd='set -a; source "'"$REPO"'/.env"; set +a; cd "'"$SITE"'" && wrangler deploy'
if command -v rtk >/dev/null 2>&1; then
  rtk proxy bash -c "$deploy_cmd" >"$LOG" 2>&1 || true
else
  bash -c "$deploy_cmd" >"$LOG" 2>&1 || true
fi

VERSION_ID="$(grep -oE 'Current Version ID: [0-9a-f-]+' "$LOG" | awk '{print $4}' | tail -1)"
if [ -z "$VERSION_ID" ]; then
  echo "FATAL: wrangler deploy produced no Version ID. Log:"
  tail -25 "$LOG"
  exit 1
fi
echo "deployed — Cloudflare Version ID: $VERSION_ID"

echo "── live verify (https://$DOMAIN) ──"
fetch() { # $1=path $2=outfile ; plain curl, fall back to --resolve via fresh dig
  local path="$1" out="$2" ip
  if curl -fsS --max-time 12 "https://$DOMAIN$path" -o "$out" 2>/dev/null; then return 0; fi
  ip="$(dig +short "$DOMAIN" @1.1.1.1 2>/dev/null | grep -E '^[0-9.]+$' | head -1)"
  [ -n "$ip" ] || ip="$(dig +short "$DOMAIN" | grep -E '^[0-9.]+$' | head -1)"
  [ -n "$ip" ] || { echo "FATAL: cannot resolve $DOMAIN"; return 1; }
  curl -fsS --max-time 12 --resolve "$DOMAIN:443:$ip" "https://$DOMAIN$path" -o "$out"
}

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fetch "/" "$tmp/index.html"
fetch "/llms.txt" "$tmp/llms.txt"
fetch "/og.png" "$tmp/og.png"

verify_fail=0

if pins="$(grep -nEi "$PIN_REGEX" "$tmp/index.html" "$tmp/llms.txt" 2>/dev/null)"; then
  echo "FAIL live site still has version pins:"
  echo "$pins" | sed 's/^/     /'
  verify_fail=1
else
  echo "ok   live HTML + llms.txt carry zero version pins"
fi

if cmp -s "$tmp/og.png" "$PUB/og.png"; then
  echo "ok   live og.png byte-identical to local"
else
  echo "FAIL live og.png differs from local $(wc -c <"$tmp/og.png")B vs $(wc -c <"$PUB/og.png")B (CDN lag? re-check, then re-deploy)"
  verify_fail=1
fi

# Optional: caller may pass strings that MUST appear live (new capability copy).
if [ "$#" -gt 0 ]; then
  for needle in "$@"; do
    if grep -qF "$needle" "$tmp/index.html"; then
      echo "ok   live contains: \"$needle\""
    else
      echo "FAIL live is missing expected copy: \"$needle\""
      verify_fail=1
    fi
  done
fi

echo
if [ "$verify_fail" -ne 0 ]; then
  echo "DEPLOY+VERIFY: deployed ($VERSION_ID) but LIVE VERIFY FAILED — do not declare success."
  exit 1
fi
echo "DEPLOY+VERIFY: PASS — Version ID $VERSION_ID, live site clean."
echo "Note: third-party share caches (X, Bluesky, LinkedIn, Slack, iMessage)"
echo "      keep the OLD card until re-scraped; new shares can append ?v=N."
