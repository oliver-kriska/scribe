---
name: getscribe-site-sync
description: >-
  Audit and re-deploy the getscribe.dev marketing site whenever scribe ships a
  new release. Use this whenever a new scribe version is tagged or pushed, a new
  CHANGELOG entry lands, or the user says anything like "sync the site", "update
  getscribe.dev", "the site is stale", "check the site against the changelog",
  "did the landing page get updated for this release", or after running
  `git tag v*`. Also use it proactively when you notice the latest git tag /
  top CHANGELOG entry is newer than what the site copy reflects. The site is
  deliberately VERSION-FREE — this skill keeps it factually current with the
  CHANGELOG while enforcing that no version string ever leaks onto any surface,
  then deploys to Cloudflare and verifies the live result.
---

# getscribe.dev release sync

## Why this skill exists

`site/` is the source for <https://getscribe.dev>, deployed via Cloudflare
Workers static assets. It is **not** hand-edited on every patch release, so any
`v0.2.x` string baked into it rots the moment the next release ships — and a
stale version is the first thing a visitor (or a shared social card) sees. The
hard-won rule from past cleanups: **the site states current capability in the
present tense and carries no version number anywhere.** When a release changes
what is true, you update the *claim*, never add an "as of vX" note.

This skill is **invoked manually** — by the maintainer asking for it, or by
Claude noticing the page is stale. It is not wired to anything that runs on its
own. There is an optional, *warn-only* git hook that prints a reminder when a
release tag is created (see `references/release-automation.md`); it never
deploys and never runs an LLM — refreshing the site is always a deliberate act.

Once invoked, run the full loop end-to-end without pausing: figure out the
release delta from the CHANGELOG, audit every surface for version pins and
now-false claims, fix the copy in the evergreen voice, regenerate the social
card if its text changed, deploy, verify the live site, commit, then report.
The maintainer triggered it on purpose — don't stop midway to ask permission.

## The cardinal invariant

**No version string on any surface, ever.** That means: no `v0.2.x`, no bare
`0.2.x`, no `softwareVersion` in JSON-LD, no `Phase 4X` internal codenames, and
none of the version-pinned phrasings — `as of vX`, `since vX`, `complete in
vX`, `in vX+`, `Phase 4D (vX)`. The eyebrow, footer, stat labels, feature
cards, FAQ (visible `<dl>` *and* the FAQPage JSON-LD), and the `og.png` social
card pixels are the usual offenders. `scripts/audit.sh` greps for all of these;
treat any hit as a defect to fix, not to annotate.

Dates are allowed (sitemap `lastmod`, JSON-LD `dateModified`, the index.md
"Last updated" line) because they only need to be roughly current, not bumped
per release.

## Workflow

### 1. Establish the release delta

```sh
REPO="$(git rev-parse --show-toplevel)"
git -C "$REPO" describe --tags --abbrev=0          # newest tag, e.g. v0.2.20
```

Read the CHANGELOG top section(s). The newest entry is the first
`## [x.y.z] — YYYY-MM-DD` block in `CHANGELOG.md`. If several releases shipped
since the site was last synced (the site has no version, so check
`git log --oneline -- site/` for the last `feat(site)` sync and diff the
CHANGELOG since then), read every entry above that point — the page must
reflect the cumulative current state, not just the latest patch.

For each entry, extract the *user-visible capability changes* — new features,
renamed/removed flags or functions, changed counts (subcommands, cron jobs),
anything a sentence on the landing page might now contradict. Internal-only
refactors with no surface-copy impact can be skipped.

### 2. Deterministic audit (no LLM judgement)

```sh
bash "$REPO/.claude/skills/getscribe-site-sync/scripts/audit.sh"
```

This greps all four text surfaces + `og.png` metadata for version pins and
prints every hit, and cross-checks the page's hard counts (subcommands,
LaunchAgents) against the actual code. It exits non-zero if anything is wrong.
A clean exit means no pins and no count drift — but it cannot judge prose, so
step 3 still runs.

### 3. Reconcile the copy (LLM judgement)

Read `references/surface-map.md` (what each surface is, mirroring rules,
freshness fields) and `references/evergreen-voice.md` (how to turn a
version-pinned or now-false sentence into an evergreen present-tense one) before
editing.

For every capability change found in step 1, find the sentences on the page it
makes false, incomplete, or stale, and rewrite them to describe what scribe
does *now*, in the present tense, with no version reference. Mirror every change
across the surfaces in the order `surface-map.md` specifies. Regenerate
`llms-full.txt` as an exact copy of `index.md` (it is literally `cp`). If the
release changes anything stated on the social card (the eyebrow tagline or the
one-line lede — never a version), regenerate it:

```sh
bash "$REPO/.claude/skills/getscribe-site-sync/scripts/regen_og.sh"
```

### 4. Freshness stamps

Set sitemap.xml `<lastmod>`, the `index.html` JSON-LD `dateModified`, and the
`index.md` "Last updated:" line to today (`date +%F`). These are dates, not
versions — keep them current, don't remove them.

### 5. Deploy + verify

```sh
bash "$REPO/.claude/skills/getscribe-site-sync/scripts/deploy_verify.sh"
```

This runs the Cloudflare deploy (RTK-safe, credentials sourced from the
repo-root `.env`, wrangler is the global install — never `npm install`
anything in this repo) and then verifies the *live* site: zero version pins,
the new capability strings present, and `og.png` byte-identical to the local
file. It prints the Cloudflare Version ID and fails loudly if verification
does not pass. Read its log if it fails; do not declare success on a failed
verify.

### 6. Commit

Stage only `site/` files (never `.env`, never unrelated working changes) and
commit:

```
feat(site): sync to <one-line changelog summary> — evergreen, no version pins

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
```

### 7. Report

State concisely: which CHANGELOG entries drove the sync, the obsolete items
found and how each was rewritten, the Cloudflare Version ID, the verification
result, and the commit hash. Remind the user that already-shared social cards
stay cached on each platform until re-scraped (new shares can append `?v=N` to
bust it) — the skill cannot refresh third-party caches.

## Optional release reminder (warn-only)

There is no automation that runs this skill for you. The only optional piece
is a warn-only git hook: when a release tag is created it prints a one-line
reminder that getscribe.dev may be stale. It never deploys and never invokes
an LLM. Install it once if you want the nudge:

```sh
bash "$REPO/.claude/skills/getscribe-site-sync/scripts/install-release-hook.sh"
```

Details and the chain-safe install behaviour are in
`references/release-automation.md`.

## Hard constraints (do not violate)

- **No JS toolchain in the repo.** wrangler is a global install. Never
  `npm install` inside the repo; never commit `package.json`, `node_modules`,
  or `.wrangler/`. Source-only HTML/CSS + final assets.
- **Credentials only from repo-root `.env`** (gitignored). Never echo,
  print, or commit its contents.
- **Only `site/` files in the sync commit.** If the working tree has unrelated
  changes, stage paths explicitly — do not `git add -A`.
- **No absolute user paths in committed files.** Everything resolves via
  `git rev-parse --show-toplevel`; the scripts already do this.
- **Never deploy from a git hook or any unattended trigger.** Deploying
  getscribe.dev is always a manual, deliberate run of this skill. The release
  hook is warn-only by design — keep it that way.
