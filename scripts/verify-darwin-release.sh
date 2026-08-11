#!/usr/bin/env bash

# Validate the exact properties needed for a distributable macOS binary.
# Apple's notary service has accepted malformed Mach-O files before, so this
# deliberately checks the load-command flag and executes both architectures.

set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 BINARY GOARCH" >&2
  exit 2
fi

binary=$1
goarch=$2
expected_team=${MACOS_SIGN_TEAM_ID:-}
expected_minos=${MACOSX_DEPLOYMENT_TARGET:-12.0}

case "$goarch" in
  amd64) expected_arch=x86_64 ;;
  arm64) expected_arch=arm64 ;;
  *)
    echo "unsupported Darwin release architecture: $goarch" >&2
    exit 2
    ;;
esac

if [[ ! -f "$binary" ]]; then
  echo "Darwin release binary does not exist: $binary" >&2
  exit 1
fi

actual_arch=$(lipo -archs "$binary")
if [[ "$actual_arch" != "$expected_arch" ]]; then
  echo "unexpected architecture for $binary: got $actual_arch, want $expected_arch" >&2
  exit 1
fi

codesign --verify --strict --verbose=2 "$binary"

# Notarization acceptance can take a few seconds to propagate to the service
# queried by codesign even after notarytool returns Accepted.
notarized=false
for attempt in 1 2 3 4 5 6; do
  if codesign --verify --strict --verbose=2 \
    --test-requirement '=notarized' \
    --check-notarization "$binary"; then
    notarized=true
    break
  fi
  if [[ "$attempt" -lt 6 ]]; then
    echo "notarization ticket not visible yet; retrying in 10s ($attempt/6)" >&2
    sleep 10
  fi
done
if [[ "$notarized" != "true" ]]; then
  echo "Apple notarization verification failed for $binary" >&2
  exit 1
fi

signature_details=$(codesign -d --verbose=4 "$binary" 2>&1)
identifier=$(awk -F= '/^Identifier=/{print $2; exit}' <<<"$signature_details")
team=$(awk -F= '/^TeamIdentifier=/{print $2; exit}' <<<"$signature_details")
if [[ "$identifier" != "scribe" ]]; then
  echo "unexpected code-signing identifier: got $identifier, want scribe" >&2
  exit 1
fi
if [[ -z "$expected_team" || "$team" != "$expected_team" ]]; then
  echo "unexpected signing team: got $team, want ${expected_team:-<unset>}" >&2
  exit 1
fi
if ! grep -q 'flags=.*runtime' <<<"$signature_details"; then
  echo "hardened runtime is missing from the code signature" >&2
  exit 1
fi
if ! grep -q '^Timestamp=' <<<"$signature_details"; then
  echo "secure timestamp is missing from the code signature" >&2
  exit 1
fi

data_const_flags=$(
  otool -l "$binary" | awk '
    $1 == "segname" && $2 == "__DATA_CONST" { in_segment = 1; next }
    in_segment && $1 == "flags" { print $2; exit }
  '
)
if [[ "$data_const_flags" != "0x10" ]]; then
  echo "invalid __DATA_CONST flags: got ${data_const_flags:-<missing>}, want 0x10 (SG_READ_ONLY)" >&2
  exit 1
fi

minos=$(otool -l "$binary" | awk '$1 == "minos" { print $2; exit }')
if [[ "$minos" != "$expected_minos" ]]; then
  echo "unexpected minimum macOS version: got ${minos:-<missing>}, want $expected_minos" >&2
  exit 1
fi

case "$goarch" in
  arm64) "$binary" --version ;;
  amd64) arch -x86_64 "$binary" --version ;;
esac

echo "verified signed, notarized, executable Darwin/$goarch release binary"
