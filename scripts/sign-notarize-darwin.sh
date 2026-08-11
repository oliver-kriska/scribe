#!/usr/bin/env bash

# GoReleaser post-build hook for macOS artifacts. Native Apple signing avoids
# cross-platform Mach-O rewrites and runs before archives or checksums exist.

set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo "usage: $0 BINARY TARGET OS ARCH" >&2
  exit 2
fi

binary=$1
target=$2
os=$3
arch=$4

if [[ "$os" != "darwin" ]]; then
  exit 0
fi

if [[ "${SCRIBE_SKIP_APPLE_SIGNING:-}" == "1" ]]; then
  echo "warning: Apple signing explicitly skipped for local $target snapshot" >&2
  exit 0
fi

required=(
  MACOS_SIGN_IDENTITY
  MACOS_SIGN_TEAM_ID
  MACOS_NOTARY_KEY_PATH
  MACOS_NOTARY_KEY_ID
  MACOS_NOTARY_ISSUER_ID
)
for name in "${required[@]}"; do
  if [[ -z "${!name:-}" ]]; then
    echo "missing required release environment variable: $name" >&2
    exit 1
  fi
done

if [[ ! -f "$binary" ]]; then
  echo "Darwin release binary does not exist: $binary" >&2
  exit 1
fi
if [[ ! -f "$MACOS_NOTARY_KEY_PATH" ]]; then
  echo "App Store Connect API key does not exist: $MACOS_NOTARY_KEY_PATH" >&2
  exit 1
fi

echo "Developer ID signing $target with native codesign"
codesign --force --timestamp --options runtime \
  --identifier scribe \
  --sign "$MACOS_SIGN_IDENTITY" \
  "$binary"
codesign --verify --strict --verbose=2 "$binary"

notary_archive="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/scribe-${target}-notary-$$.zip"
trap 'rm -f "$notary_archive"' EXIT
ditto -c -k --keepParent "$binary" "$notary_archive"

echo "Submitting $target to Apple notary service"
notary_result=$(
  xcrun notarytool submit "$notary_archive" \
    --key "$MACOS_NOTARY_KEY_PATH" \
    --key-id "$MACOS_NOTARY_KEY_ID" \
    --issuer "$MACOS_NOTARY_ISSUER_ID" \
    --wait \
    --output-format json
)

read -r submission_id submission_status < <(
  python3 -c '
import json
import sys

result = json.load(sys.stdin)
print(result.get("id", "unknown"), result.get("status", "unknown"))
' <<<"$notary_result"
)
echo "Apple notarization $submission_status for $target (submission $submission_id)"
if [[ "$submission_status" != "Accepted" ]]; then
  echo "$notary_result" >&2
  exit 1
fi

"$(dirname "$0")/verify-darwin-release.sh" "$binary" "$arch"
