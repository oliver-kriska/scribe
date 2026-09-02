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

# The committed HTML is compared byte-for-byte in --check mode, so the renderer
# is part of the contract: two pandoc versions produce different markup from the
# same markdown and the check would fail for a reason nobody could see. CI
# installs this exact version (see .github/workflows/ci.yml); keep the two in
# lockstep, and bump both in the same commit as the regenerated HTML.
PANDOC_PIN="3.10.2"
PANDOC_HAVE="$(pandoc --version | head -1 | awk '{print $2}')"
if [[ "$PANDOC_HAVE" != "$PANDOC_PIN" ]]; then
  echo "warning: pandoc $PANDOC_HAVE, pinned $PANDOC_PIN — output may differ from CI" >&2
fi

# slug | source .md | kicker | <title> (page name only) | meta description
DOCS=$(cat <<'LIST'
setup|setup.md|Runbook|Setup runbook|Zero-to-running setup for scribe: personal, Ollama, hosted provider, team, several machines, and Linux scheduling.
topologies|topologies.md|Reference|Topologies|Every way to write to a scribe KB — one machine, several machines, a team — what each coordinates, and what it does not.
vs|vs.md|Comparison|scribe compared with RAG, AnythingLLM, and Obsidian|When to choose scribe (getscribe.dev), a DIY vector-DB RAG pipeline, AnythingLLM, or an Obsidian markdown vault.
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

  schema=""
  if [[ "$slug" == "vs" ]]; then
    # Keep FAQPage answers derived from the visible source instead of maintaining
    # a second copy. The four comparison H2s are a deliberate public contract.
    schema=$(BODY_HTML="$body" python3 - <<'PY'
import html
import json
import os
import re
import sys

body = os.environ["BODY_HTML"]
pairs = re.findall(r'<h2[^>]*>(.*?)</h2>\s*<p>(.*?)</p>', body, flags=re.DOTALL)

def text(fragment):
    return ' '.join(html.unescape(re.sub(r'<[^>]+>', '', fragment)).split())

questions = [(text(question), text(answer)) for question, answer in pairs]
expected = [
    "What's an alternative to RAG for a personal developer knowledge base?",
    "AnythingLLM alternative for a local markdown knowledge base",
    "scribe vs a vector-DB RAG pipeline",
    "Is this the same as Scribe (scribehow)?",
]
if [question for question, _ in questions] != expected:
    print("vs.md must contain exactly the four contracted comparison H2s", file=sys.stderr)
    sys.exit(1)

schema = [
    {
        "@context": "https://schema.org",
        "@type": "WebPage",
        "@id": "https://getscribe.dev/vs#webpage",
        "url": "https://getscribe.dev/vs",
        "name": "scribe (getscribe.dev) compared with RAG, AnythingLLM, and Obsidian",
        "description": "When to choose scribe (getscribe.dev), a DIY vector-DB RAG pipeline, AnythingLLM, or an Obsidian markdown vault.",
        "datePublished": "2026-09-02",
        "dateModified": "2026-09-02",
        "isPartOf": {"@id": "https://getscribe.dev/#website"},
    },
    {
        "@context": "https://schema.org",
        "@type": "FAQPage",
        "@id": "https://getscribe.dev/vs#faq",
        "url": "https://getscribe.dev/vs",
        "dateModified": "2026-09-02",
        "mainEntity": [
            {
                "@type": "Question",
                "name": question,
                "acceptedAnswer": {"@type": "Answer", "text": answer},
            }
            for question, answer in questions
        ],
    },
]
print('<script type="application/ld+json">')
print(json.dumps(schema, ensure_ascii=False, indent=2))
print('</script>')
PY
    )
  fi

  rendered=$(
    TITLE="$title" DESC="$desc" SLUG="$slug" MD="$md" KICKER="$kicker" BODY="$body" SCHEMA="$schema" \
    perl -0pe '
      s/__TITLE__/$ENV{TITLE}/g;
      s/__DESC__/$ENV{DESC}/g;
      s/__SLUG__/$ENV{SLUG}/g;
      s/__MD__/$ENV{MD}/g;
      s/__KICKER__/$ENV{KICKER}/g;
      s/__BODY__/$ENV{BODY}/g;
      s/__SCHEMA__/$ENV{SCHEMA}/g;
    ' "$TPL"
  )

  if [[ $CHECK -eq 1 ]]; then
    if ! printf '%s\n' "$rendered" | diff -q - "$out" >/dev/null 2>&1; then
      echo "STALE: $out is out of date — run ./site/build-docs.sh" >&2
      [[ "$PANDOC_HAVE" != "$PANDOC_PIN" ]] && \
        echo "       (rendered with pandoc $PANDOC_HAVE; the committed HTML was built with $PANDOC_PIN — mismatched versions alone can cause this)" >&2
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
