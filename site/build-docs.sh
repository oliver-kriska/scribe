#!/usr/bin/env bash
# build-docs.sh — render the markdown docs in site/public/ into styled HTML
# pages that humans can read, keeping the .md itself as the source of truth.
#
# The repo deliberately carries no npm toolchain (see CLAUDE.md), so this uses
# pandoc, a local dev tool, and COMMITS its output. CI only deploys static
# files. Re-run after editing any source .md, then commit both.
#
#   brew install pandoc
#   ./site/build-docs.sh
#
# --check verifies the committed HTML is current without writing (used by CI
# and by the getscribe-site-sync audit).

set -euo pipefail

cd "$(dirname "$0")"
TPL="templates/doc.html"
CHECK=0
[[ "${1:-}" == "--check" ]] && CHECK=1

command -v pandoc >/dev/null || { echo "pandoc not found — brew install pandoc" >&2; exit 1; }

# slug | source .md | kicker | <title> (page name only) | meta description
DOCS=$(cat <<'LIST'
setup|setup.md|Runbook|Setup runbook|Zero-to-running setup for scribe: personal, Ollama, hosted provider, team, several machines, and Linux scheduling.
topologies|topologies.md|Reference|Topologies|Every way to write to a scribe KB — one machine, several machines, a team — what each coordinates, and what it does not.
LIST
)

status=0
while IFS='|' read -r slug md kicker title desc; do
  [[ -z "$slug" ]] && continue
  src="public/$md"
  out="public/$slug.html"
  [[ -f "$src" ]] || { echo "missing $src" >&2; exit 1; }

  body=$(pandoc "$src" --from=gfm --to=html5 --syntax-highlighting=none)
  # Tables must scroll inside their own box, never the page body.
  body=$(printf '%s' "$body" | perl -0pe 's{<table>}{<div class="table-scroll"><table>}g; s{</table>}{</table></div>}g')

  rendered=$(
    TITLE="$title" DESC="$desc" SLUG="$slug" MD="$md" KICKER="$kicker" BODY="$body" \
    perl -0pe '
      s/__TITLE__/$ENV{TITLE}/g;
      s/__DESC__/$ENV{DESC}/g;
      s/__SLUG__/$ENV{SLUG}/g;
      s/__MD__/$ENV{MD}/g;
      s/__KICKER__/$ENV{KICKER}/g;
      s/__BODY__/$ENV{BODY}/g;
    ' "$TPL"
  )

  if [[ $CHECK -eq 1 ]]; then
    if ! printf '%s\n' "$rendered" | diff -q - "$out" >/dev/null 2>&1; then
      echo "STALE: $out is out of date — run ./site/build-docs.sh" >&2
      status=1
    else
      echo "ok  $out"
    fi
  else
    printf '%s\n' "$rendered" > "$out"
    echo "wrote $out  ($(wc -c < "$out" | tr -d ' ') bytes)"
  fi
done <<< "$DOCS"

exit $status
