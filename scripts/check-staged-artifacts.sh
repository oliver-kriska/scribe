#!/usr/bin/env bash
# Reject compiled executables and oversized blobs from the staged index.
#
# Why this exists: on 2026-09-03 a 21 MB arm64 Mach-O (`cmd/scribe/scribe`)
# was committed by accident and sat on main until an OpenSSF Scorecard
# Binary-Artifacts alert surfaced it. Nothing in the repo could have caught
# it — every other lefthook command is `glob: "*.go"`, and `scribe commit
# --check` deliberately skips binaries (scanning them for secrets is
# meaningless). `.gitignore`'s `/scribe` is root-anchored, so `go build
# ./cmd/scribe/` run from inside the package drops a binary the ignore rule
# cannot see.
#
# Deliberately narrow. It matches executable magic bytes, not "binary" —
# the repo legitimately tracks PNG, JPEG, ICO and SVG assets, and a blanket
# binary ban would reject the site's own images.
#
# Escape hatch: ALLOW_LARGE_FILES=1 git commit ...
set -euo pipefail

# Largest legitimately tracked file is ~375 KB (docs/images/*.jpg), so 2 MB
# leaves generous headroom while still catching a build artifact by orders
# of magnitude.
MAX_BYTES=${MAX_STAGED_BYTES:-2097152}

fail=0
note() { printf '  %s\n' "$1" >&2; }

while IFS= read -r -d '' f; do
	# Read from the index, not the worktree: they can differ, and the
	# index is what is about to become a commit.
	size=$(git cat-file -s ":$f" 2>/dev/null) || continue

	# First 4 bytes, hex. ELF, every Mach-O variant (32/64, both
	# endiannesses, fat/universal), and PE/COFF.
	magic=$(git show ":$f" 2>/dev/null | head -c 4 | xxd -p 2>/dev/null || true)
	case "$magic" in
	7f454c46 | \
		cffaedfe | cefaedfe | feedfacf | feedface | \
		cafebabe | bebafeca | \
		4d5a*)
		note "compiled executable staged: $f (${size} bytes)"
		note "  → git rm --cached '$f' && add it to .gitignore"
		fail=1
		continue
		;;
	esac

	if [ "$size" -gt "$MAX_BYTES" ] && [ "${ALLOW_LARGE_FILES:-}" != "1" ]; then
		note "oversized file staged: $f (${size} bytes > ${MAX_BYTES})"
		note "  → intentional? re-run with ALLOW_LARGE_FILES=1"
		fail=1
	fi
done < <(git diff --cached --name-only --diff-filter=ACM -z)

if [ "$fail" -ne 0 ]; then
	printf '\nstaged-artifact gate failed — see above.\n' >&2
	exit 1
fi
