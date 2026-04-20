#!/usr/bin/env bash
# scribe installer — fetches the right binary from the latest GitHub release
# into $HOME/.local/bin, falls back to `go build` if a Go toolchain is
# available and the release download fails.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/oliver-kriska/scribe/main/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/oliver-kriska/scribe/main/install.sh | bash -s -- --version v0.2.0
#   curl -fsSL https://raw.githubusercontent.com/oliver-kriska/scribe/main/install.sh | bash -s -- --prefix /usr/local
#
# Flags:
#   --version <tag>   Pin to a specific release (default: latest).
#   --prefix <dir>    Install prefix (default: $HOME/.local). Binary lands in <prefix>/bin.
#   --repo <oliver-kriska/scribe>  Override the GitHub repo (for forks / tests).
#
# Dependencies assumed present: curl or wget, tar, uname. Nothing else.

set -euo pipefail

REPO="${SCRIBE_REPO:-oliver-kriska/scribe}" # replace at publish time
VERSION="latest"
PREFIX="${HOME}/.local"

while [[ $# -gt 0 ]]; do
    case "$1" in
    --version)
        VERSION="$2"
        shift 2
        ;;
    --prefix)
        PREFIX="$2"
        shift 2
        ;;
    --repo)
        REPO="$2"
        shift 2
        ;;
    -h | --help)
        sed -n '1,20p' "$0"
        exit 0
        ;;
    *)
        echo "unknown flag: $1" >&2
        exit 1
        ;;
    esac
done

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
x86_64 | amd64) arch="amd64" ;;
arm64 | aarch64) arch="arm64" ;;
*)
    echo "unsupported arch: $arch" >&2
    exit 1
    ;;
esac
case "$os" in
darwin | linux) ;;
*)
    echo "unsupported os: $os" >&2
    exit 1
    ;;
esac

bindir="${PREFIX}/bin"
mkdir -p "$bindir"

fetch() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$1" -o "$2"
    elif command -v wget >/dev/null 2>&1; then
        wget -q "$1" -O "$2"
    else
        echo "need curl or wget" >&2
        exit 1
    fi
}

resolve_tag() {
    if [ "$VERSION" = "latest" ]; then
        # GitHub's /releases/latest redirects to /tag/<tag>; grab the Location header.
        api="https://api.github.com/repos/${REPO}/releases/latest"
        if command -v curl >/dev/null 2>&1; then
            tag="$(curl -fsSL "$api" | grep -o '"tag_name": *"[^"]*"' | head -1 | sed 's/.*"\([^"]*\)"$/\1/')"
        else
            tag="$(wget -qO- "$api" | grep -o '"tag_name": *"[^"]*"' | head -1 | sed 's/.*"\([^"]*\)"$/\1/')"
        fi
        [ -n "$tag" ] || {
            echo "could not resolve latest release tag"
            exit 1
        }
        echo "$tag"
    else
        echo "$VERSION"
    fi
}

build_from_source() {
    if ! command -v go >/dev/null 2>&1; then
        echo "no release binary available and no Go toolchain found — aborting" >&2
        exit 1
    fi
    echo "Falling back to: go install from source (requires CGO for sqlite_fts5)"
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' EXIT
    git clone --depth=1 "https://github.com/${REPO}.git" "$tmp/src"
    (cd "$tmp/src" && CGO_ENABLED=1 go build -tags "sqlite_fts5" -o "$bindir/scribe" ./cmd/scribe)
    echo "Built $bindir/scribe from source."
}

verify_checksum() {
    expected="$1"
    file="$2"
    if command -v sha256sum >/dev/null 2>&1; then
        actual="$(sha256sum "$file" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
        actual="$(shasum -a 256 "$file" | awk '{print $1}')"
    else
        echo "neither sha256sum nor shasum available — cannot verify checksum" >&2
        return 1
    fi
    if [ "$actual" != "$expected" ]; then
        echo "checksum mismatch for $(basename "$file"): expected $expected, got $actual" >&2
        return 1
    fi
}

main() {
    tag="$(resolve_tag)"
    asset="scribe_${tag#v}_${os}_${arch}.tar.gz"
    url="https://github.com/${REPO}/releases/download/${tag}/${asset}"
    sums_url="https://github.com/${REPO}/releases/download/${tag}/checksums.txt"
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' EXIT
    if fetch "$url" "$tmp/scribe.tar.gz"; then
        if fetch "$sums_url" "$tmp/checksums.txt"; then
            expected="$(awk -v name="$asset" '$2==name {print $1}' "$tmp/checksums.txt")"
            if [ -z "$expected" ]; then
                echo "checksums.txt does not list $asset — refusing to install unverified binary" >&2
                exit 1
            fi
            verify_checksum "$expected" "$tmp/scribe.tar.gz" || exit 1
        else
            echo "could not fetch checksums.txt — refusing to install unverified binary" >&2
            exit 1
        fi
        tar -xzf "$tmp/scribe.tar.gz" -C "$tmp"
        install -m 0755 "$tmp/scribe" "$bindir/scribe"
        echo "Installed $bindir/scribe ($tag, sha256 verified)"
    else
        echo "Release asset not reachable; trying source build." >&2
        build_from_source
    fi

    case ":${PATH}:" in
    *":${bindir}:"*) ;;
    *)
        echo
        echo "Add $bindir to your PATH:"
        echo "  export PATH=\"$bindir:\$PATH\""
        ;;
    esac

    echo
    echo "Next: scribe init --path ~/my-kb"
    echo "See README for cron + Full Disk Access (macOS) details."
}

main
