# Shared definition of what a "version pin" looks like on the site.
# Sourced by audit.sh and deploy_verify.sh so the rule lives in exactly one place.
#
# A pin is any of:
#   - a semver triple (v0.2.20 or bare 0.2.20) — the main offender
#   - JSON-LD softwareVersion
#   - internal phase codenames (Phase 4A..4E)
#   - version-pinned phrasings that rot on the next release
#
# Semver triple is \b[0-9]+\.[0-9]+\.[0-9]+\b so it never matches num_ctx
# values (16384), pixel sizes (1200), or prices ($0.0001).

PIN_REGEX='(\bv?[0-9]+\.[0-9]+\.[0-9]+\b|softwareVersion|Phase 4[A-E]|as of v[0-9]|since v[0-9]|complete in v[0-9]|in v[0-9]+(\.[0-9]+)*\+|Phase 4[A-E][, ]*v[0-9])'

# pin_scan FILE — print "line:match" pins in FILE, but first strip SVG
# coordinate attributes (path d="...", viewBox="...", points="...") whose
# decimal sequences false-match the semver triple — e.g. the GitHub icon's
# path data contains "4.07.55". Real version strings never live inside those
# attributes. Returns grep's exit status (0 = pins found). Both audit.sh and
# deploy_verify.sh call this so the SVG carve-out lives in one place.
pin_scan() {
  sed -E 's/ (d|viewBox|points)="[^"]*"//g' "$1" | grep -nEi "$PIN_REGEX"
}
