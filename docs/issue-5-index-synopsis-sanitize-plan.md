# Issue #5 — sanitize `wiki/_index.md` synopsis lines at generation time

GitHub issue #5: "Decide: scribe:allow markers vs regenerated `_index.md` synopsis lines."

## 1. Problem & context

`scribe index` (`IndexCmd.Run`, `cmd/scribe/index.go:22-128`) rebuilds `wiki/_index.md`
from scratch on every run by walking every article (`walkArticles`, skips
`_`-prefixed files) and, for each one, synthesizing a one-line synopsis:

```go
// cmd/scribe/index.go:42-52
body := extractBody(content)
if desc := firstSentence(body); desc != "" {
    if len(desc) > 80 {
        desc = truncateOutsideWikilink(desc, 80)
    }
    summary = desc + " (" + summary + ")"
} else {
    summary = "(" + summary + ")"
}
```

`firstSentence` (`cmd/scribe/index.go:166-187`) pulls the first content line/sentence
out of the article body verbatim. If that line contains a credential-shaped string
(an AWS key, a token, a password-bearing URL, …), the synthesized synopsis carries it
into `wiki/_index.md` — a file registered as `classDerivedRegenerable`
(`cmd/scribe/special_files.go:47`), meaning it is fully rebuilt, never hand-edited,
and always re-staged whole on every `scribe index` / `scribe sync` run.

`wiki/_index.md` is not exempt from the credential gates: `stagedMarkdown`
(`cmd/scribe/secrets.go:412-424`) lists every staged `.md` file repo-wide with no path
restriction, so both `holdSecretFiles` (`cmd/scribe/secrets.go:348-390`) and
`holdStopWordFiles` (`cmd/scribe/stopwords.go:241-282`) scan it like any other file at
commit time. `scanContentForSecrets` (`cmd/scribe/secrets.go:216-277`) honors an inline
`scribe:allow` / `gitleaks:allow` marker *on the offending line* to suppress a hit
(`cmd/scribe/secrets.go:219, 236-245`) — but `firstSentence` only ever copies sentence
text, never a trailing marker comment, into the synthesized synopsis. So:

- If a source article's line is a real detected pattern that a human deliberately
  allowed with `scribe:allow` (e.g. a documented example key), the **article** commits
  fine. The **synopsis** derived from that same line has no marker of its own, gets
  flagged as a fresh violation on every regen, and — in team mode — `holdSecretFiles`
  unstages the entire regenerated `wiki/_index.md` on every single run. There is no
  file-specific line to attach a marker to that would survive the next regen; that is
  the "half-state issue #5 rules out" as the only wrong answer.

The issue's own leaning is to sanitize at generation time — mask anything that matches
the lint credential patterns inside the synthesized synopsis — rather than attempt to
make `scribe:allow` survive into a file that is rebuilt from nothing every run. This
plan implements exactly that, reusing the existing rule set with zero new regexes.

## 2. Design decisions

**D1 — New shared function lives in `secrets.go`, not `index.go`.**
Add `maskSecretsInText(s string, includeGeneric bool) string` to
`cmd/scribe/secrets.go`, next to `secretValueAllowed` (currently
`cmd/scribe/secrets.go:294-313`). It iterates the existing `secretRules` slice
(`cmd/scribe/secrets.go:70-192`) directly and calls the existing
`secretValueAllowed` / `secretFromMatch` helpers — no new regex is defined anywhere.
*Rejected*: putting the function in `index.go` — it would need to reach into
`secrets.go`'s unexported `secretRules`/`secretValueAllowed` anyway (same package, so
technically legal), but colocating it with the rule set it depends on keeps the ruleset
and every consumer of it in one file, matching the existing `scanContentForSecrets` /
`findSecretsInKB` pattern in the same file.

**D2 — Whole-match replacement, not submatch-only replacement.**
`maskSecretsInText` replaces the *entire regex match* (not just the captured secret
group) with the shared `defaultRedaction` constant (`"[redacted]"`, already defined in
`cmd/scribe/stopwords.go:55`, reused as-is — no new redaction string). For tier-1 bare
rules (AWS key id, GitHub token, JWT, PEM header, …) the match *is* just the secret, so
this is a no-op distinction. For tier-2 contextual rules (`aws-secret-access-key`,
`azure-ad-client-secret`, `url-userinfo-password`, `generic-credential-assignment`) the
match includes a labeling prefix (`aws_secret_key: `, `postgres://user:...@host`); their
replacement swallows that prefix too. *Rejected*: extracting and replacing only the
captured group via manual index math on the submatch positions — meaningfully more
code for a synopsis-only teaser string, and it doesn't remove the value from the
picture, it removes the risk. Simplicity wins here.

**D3 — `scribe:allow` / `gitleaks:allow` markers on the source line are never honored
by `maskSecretsInText`.** This is not a gap to be filled later — it is the direct
resolution of issue #5's "either/or." A per-line marker suppresses the *commit gate for
that one line in that one file*. It cannot mean anything for a synthesized sentence in
a derived file that has no line of its own, and even if the literal marker substring
happened to be captured inside `firstSentence`'s extracted text (edge case, tested
below), `maskSecretsInText` still masks it. Teaching it to respect the marker text
would resurrect exactly the half-state the issue rejects, just laundered through a
substring match instead of a `scribe:allow` file. This must be called out explicitly
because it is the one place this plan deliberately diverges from
`scanContentForSecrets`'s behavior (which *does* honor the marker,
`cmd/scribe/secrets.go:236-245`) — same rule set, different suppression semantics, by
design.

**D4 — Masking is gated by the exact same condition doctor already uses for the
credential audit** (`cmd/scribe/doctor.go:899`): `cfg.Team && !cfg.SecretScan.Disable`,
with `includeGeneric = cfg.SecretScan.Generic`. Solo KBs (`team: false`, the default)
get no masking — `wiki/_index.md` synopsis text is untouched, exactly matching today's
behavior and the fact that `holdSecretFiles`/`findSecretsInKB` never run at all for
solo KBs (`cmd/scribe/secrets.go:358`: `if cfg == nil || !cfg.Team || cfg.SecretScan.Disable { return true }`).
*Rejected*: masking unconditionally for every KB. It would change solo-KB output for a
gate those users never opted into and that the rest of the codebase treats as
team-only; it would also silently diverge from what the commit gate would actually
flag, defeating "reuse the same pattern set" as a *consistency* guarantee, not just a
regex-sharing one. On `cfg.LoadErr != nil` (unparseable `scribe.yaml`), `loadConfig`
returns defaults (`Team: false`), so masking silently no-ops and `scribe index`
continues to generate `_index.md` — this matches every other non-entry command's
posture toward a broken config (only the commit *gate* itself fails closed,
`cmd/scribe/secrets.go:349-357`); `IndexCmd` is not in the `requireParseable` set today
and this plan does not add it there (out of scope, would be a bigger behavior change to
`scribe index` entirely on its own).

**D5 — Redaction runs on the already-extracted `desc` string, before the 80-char
truncation, not on the full article `body`.** `firstSentence` already bounds `desc` to
at most 120 characters before this point (`cmd/scribe/index.go:178-184`), so scanning
just `desc` is cheap and — critically — it means truncation (`truncateOutsideWikilink`)
only ever operates on already-safe text. There is no path where a partially-truncated
raw secret can appear in the output. *Rejected*: masking the whole `body` before
`firstSentence` runs — wasteful (bodies can be large; regexes would run over content
that never reaches the synopsis) and it doesn't change the outcome, since nothing
outside the extracted `desc` ever reaches `wiki/_index.md`.

**D6 — Scope is the synthesized synopsis sentence only — not `fm.Title`, `fm.Type`,
`fm.Domain`, or the `(type, domain)` suffix.** Those are frontmatter scalars copied
verbatim from the source article, and the source article's *raw file content*
(frontmatter included) is already scanned by `holdSecretFiles`/`findSecretsInKB` when
the article itself is staged — so a credential-shaped title or domain is already
caught and held at the source, before `scribe index` ever runs. Only the *synopsis*
text is a genuinely new derivation (a substring selected by `firstSentence`'s own
logic) that can carry a source line's content into a new file without the source
line's `scribe:allow` marker attached. Widening scope to the whole `articleEntry`
would be free-standing extra work solving a problem that doesn't exist.

**D7 — `maskSecretsInText` does not replicate `scanContentForSecrets`'s per-rule
first-hit dedup** (`fired[r.ID]`, `cmd/scribe/secrets.go:251-253`). That dedup exists
purely to keep the commit-gate's log line terse (one log entry per rule per file); it
is not a correctness or security boundary. `regexp.ReplaceAllFunc` naturally replaces
*every* non-overlapping match in one call, and every occurrence must be masked here,
so no dedup is carried over.

**D8 — A swallowed boundary character (a trailing quote/space consumed by `reBoundary`,
`cmd/scribe/secrets.go:68`, as part of a tier-2 rule's full match) is an accepted
cosmetic artifact, not fixed.** E.g. `"aws_secret_key: <40 chars> more text"` becomes
`"[redacted]more text"` (no space before "more") because the space was part of the
matched span. Reconstructing the exact boundary character after replacement would need
extra bookkeeping for a synopsis string that already accepts truncation-ellipsis
artifacts elsewhere (`truncateOutsideWikilink`). Only the four tier-2 contextual rules
are affected (`aws-secret-access-key`, `aws-bedrock-api-key`, `azure-ad-client-secret`,
`url-userinfo-password`, `generic-credential-assignment`, `gcp-api-key`,
`openai-api-key`, `anthropic-api-key`, `huggingface-token`, `npm-access-token`,
`sendgrid-api-key`, `jwt` all consume a trailing `reBoundary` char too, but most of
those are tier-1 rules where the match is *only* the secret, so eating one trailing
char right after the value is invisible in practice); the majority of tier-1 rules end
in a zero-width `\b`, which consumes nothing.

## 3. Implementation steps

### `cmd/scribe/secrets.go`

Insert a new function immediately after `secretValueAllowed` (currently ends at
`cmd/scribe/secrets.go:313`, right before the `shannonEntropy` function):

```go
// maskSecretsInText redacts every credential-shaped substring in s using
// the exact same secretRules, placeholder/stopword allowlist, and entropy
// floor as scanContentForSecrets — so a value that would hold a staged
// article back from committing never resurfaces unmasked in a derived
// file that regenerates from scratch on every run and has no line of its
// own to carry a scribe:allow marker (wiki/_index.md's synopsis lines,
// built by IndexCmd.Run in index.go, are the motivating case — issue #5).
// Unlike the commit gate, this never holds anything back: every match is
// replaced in place with defaultRedaction and the call always succeeds,
// because regeneration must stay unconditional and idempotent.
//
// A scribe:allow/gitleaks:allow marker on the source line has no bearing
// here, even if its literal text ends up inside s: the marker suppresses
// the commit gate for one line in one file, not every future derivation
// of that line's text. Honoring it here would resurrect the exact
// half-state issue #5 rules out.
func maskSecretsInText(s string, includeGeneric bool) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	for i := range secretRules {
		r := &secretRules[i]
		if r.Generic && !includeGeneric {
			continue
		}
		b = r.Re.ReplaceAllFunc(b, func(match []byte) []byte {
			if secretValueAllowed(secretFromMatch(r.Re.FindSubmatch(match), r.Group), r) {
				return match
			}
			return []byte(defaultRedaction)
		})
	}
	return string(b)
}
```

No other change to `secrets.go`. `defaultRedaction` is defined in `stopwords.go:55`
(same package, no import needed).

### `cmd/scribe/index.go`

1. In `(i *IndexCmd) Run() error`, right after the `kbDir()` error check
   (`cmd/scribe/index.go:22-26`), before the `// Collect articles grouped by directory`
   comment (line 28), add:

```go
	cfg := loadConfig(root)
	maskSynopsisSecrets := cfg != nil && cfg.Team && !cfg.SecretScan.Disable
	includeGenericSecrets := cfg != nil && cfg.SecretScan.Generic
```

2. Replace the block at `cmd/scribe/index.go:44-52`:

```go
		if desc := firstSentence(body); desc != "" {
			// Truncate to reasonable length without slicing through a wikilink.
			if len(desc) > 80 {
				desc = truncateOutsideWikilink(desc, 80)
			}
			summary = desc + " (" + summary + ")"
		} else {
			summary = "(" + summary + ")"
		}
```

   with:

```go
		if desc := firstSentence(body); desc != "" {
			if maskSynopsisSecrets {
				// Mask before truncating: truncation must only ever see
				// already-safe text, never a raw credential that could
				// get cut mid-value.
				desc = maskSecretsInText(desc, includeGenericSecrets)
			}
			// Truncate to reasonable length without slicing through a wikilink.
			if len(desc) > 80 {
				desc = truncateOutsideWikilink(desc, 80)
			}
			summary = desc + " (" + summary + ")"
		} else {
			summary = "(" + summary + ")"
		}
```

   `maskSynopsisSecrets` and `includeGenericSecrets` are read-only locals captured by
   the `walkArticles` closure — no signature changes needed anywhere else in the file.

No changes to `truncateOutsideWikilink`, `extractBody`, or `firstSentence`.

## 4. Test plan

`make test` must pass fully offline (no git repo needed for these — `IndexCmd.Run`
only touches the filesystem via `kbDir()`/`loadConfig`, same as the existing
`graphTestKB` tests in `cmd/scribe/index_test.go`).

### 4a. Unit tests — `cmd/scribe/secrets_test.go` (new `TestMaskSecretsInText`)

Reuse the existing `fakeAWSKey()` / `fakeGitHubToken()` / `fakeAnthropicKey()` /
`fakeJWT()` helpers already defined at the top of `secrets_test.go:13-21` (same
package, no redefinition needed).

| case | input | includeGeneric | want |
|---|---|---|---|
| empty string | `""` | false | `""` (identity, no panic) |
| plain prose, no secret | `"Set up the KB in five minutes."` | false | unchanged, byte-identical |
| real AWS key masked | `"the key was " + fakeAWSKey() + " in the env"` | false | contains `"[redacted]"`, does **not** contain the raw key substring |
| GitHub token masked | `"export GH_TOKEN=" + fakeGitHubToken()` | false | does not contain raw token |
| multiple occurrences, same rule | `fakeAWSKey() + " and later " + fakeAWSKey()` (two distinct fake AWS-shaped keys) | false | zero raw key occurrences remain (not just the first, per D7) |
| canonical AWS doc key stays visible | `"use AKIAIOSFODNN7EXAMPLE in docs"` | false | unchanged (stopword allowlist, `secrets.go:206`) |
| placeholder URL password stays visible | `"postgres://user:xxxx@localhost/db"` | false | unchanged (placeholder regex, `secrets.go:197`) |
| generic rule opt-out | `` `api_key = "x9K2mP8qL5nR3vT7wY1zB6cD4"` `` | false | unchanged |
| generic rule opt-in | same string | true | contains `"[redacted]"`, not the raw value |
| **scribe:allow marker text present but NOT honored (pins D3)** | `"real-looking " + fakeAWSKey() + " <!-- scribe:allow -->"` | false | still masked — contains `"[redacted]"`, raw key absent |
| PEM header masked | `"-----BEGIN RSA PRIVATE KEY-----"` | false | does not equal input (masked) |

### 4b. Integration tests — `cmd/scribe/index_test.go` (new `TestIndexCmdMasksSynopsisSecrets`)

Table-driven, one KB per case via `t.TempDir()` + `t.Setenv("SCRIBE_KB", root)`, same
pattern as `graphTestKB` (`cmd/scribe/index_test.go:92-108`). No git init required.

| case | `scribe.yaml` | article first sentence | assert on `wiki/_index.md` |
|---|---|---|---|
| team mode masks a real secret | `"team: true\n"` | `"Uses key " + fakeAWSKey() + " for auth."` | does **not** contain the raw fake key; **does** contain `"[redacted]"` |
| solo KB (default) leaves it unmasked | `"domains: [acme]\n"` (no `team:`) | same sentence | **contains** the raw fake key, unchanged (pins D4's team-only gate) |
| `secret_scan.disable: true` leaves it unmasked | `"team: true\nsecret_scan:\n  disable: true\n"` | same sentence | contains the raw fake key |
| canonical example key stays visible end-to-end | `"team: true\n"` | `"See AKIAIOSFODNN7EXAMPLE in the docs."` | contains `AKIAIOSFODNN7EXAMPLE` unmasked |
| masked text still truncates correctly | `"team: true\n"` | a >80-char sentence whose *first* 40 chars are a real fake AWS key | resulting synopsis line has no raw key, is ≤ ~83 chars (80 + `"..."`), and does not contain a dangling `[[` (existing `truncateOutsideWikilink` invariant, still holds post-mask) |

Use `captureLintStdout(t, func() { err = (&IndexCmd{}).Run() })` exactly as the existing
`TestIndexCmdRun` does (`cmd/scribe/index_test.go:110-144`), then
`os.ReadFile(filepath.Join(root, "wiki", "_index.md"))` and assert on its contents.

### 4c. Regression check

Run the existing `TestIndexCmdRun` and `TestIndexCmdDryRun` unmodified — they use no
`team:` config, so `maskSynopsisSecrets` is `false` for them and output must be
byte-for-byte identical to today.

## 5. Risks & edge cases

- **Boundary-char cosmetic artifact** (D8) — accepted, not fixed. Only affects tier-2
  contextual rules; the resulting synopsis reads slightly cramped
  (`"...[redacted]more text"`), never leaks partial secret bytes.
- **`cfg.Team` read via plain `loadConfig(root)`, not a trust-layer-resolved
  accessor** — this exactly matches the existing precedent at
  `cmd/scribe/doctor.go:899` for the same audit check. If there is a latent trust-layer
  gap in how `cfg.Team` gets resolved, it already affects doctor's existing
  `secrets-in-articles` check identically; this plan does not introduce a new one.
- **Sequential rule application mutates `b` between rules** — a later rule's regex
  runs against text that may already contain `"[redacted]"` from an earlier rule. Since
  `defaultRedaction` (`"[redacted]"`) doesn't resemble any credential prefix in
  `secretRules`, this can't accidentally re-trigger a different rule or hide an
  adjacent secret; each rule's `ReplaceAllFunc` call returns a fresh slice, so there's
  no shared-backing-array aliasing bug either.
- **Very short KBs / no team config at all** — `cfg` from `loadConfig` is never `nil`
  in practice (`cmd/scribe/config.go:582-595` always returns a populated struct), but
  the `cfg != nil` guard is kept for consistency with the rest of the codebase's
  defensive style (e.g. `doctor.go:899`), not because it's reachable.
- **Performance** — `desc` is bounded to ≤120 bytes before masking ever runs (D5); RE2
  is linear-time regardless, so no measurable cost even across ~27 rules per article.

## 6. Interactions with other open issues

- **#25 (Stop-words filter, `cmd/scribe/stopwords.go`, already implemented in this
  tree)** has an *analogous* unresolved gap: `holdStopWordFiles` also scans staged
  `wiki/_index.md` at the whole-file level (via `stagedMarkdown`, same as the secret
  gate), so a user-defined **hold**-mode stop word that happens to land inside a
  synthesized synopsis would hold back the regenerated `_index.md` on every run, same
  root cause as issue #5 but for the arbitrary stop-word list instead of the fixed
  credential rule set. This plan does **not** fix that — issue #5 is scoped to "the
  lint credential patterns" specifically, and the stop-words list is a different,
  user-configurable ruleset with its own hold/mask split. Worth flagging as a follow-up
  if not already tracked; not implemented here.
- **#27 (doctor/status: KB-scope the global-state checks)** and **#26 (cron:
  KB-agnostic scheduler)** — no code overlap with this change.
- **#42 (extraction prompts: failure traces)** — no overlap.
- No other open issue touches `index.go`, `secrets.go`, or `stopwords.go`.

## 7. Size estimate

**S** — roughly 25 new lines of production code (one function in `secrets.go`, ~10
lines; a config-read + one masking call in `index.go`, ~10 lines) and ~120-150 lines of
table-driven tests across the two `_test.go` files. No new files, no `go.mod` changes,
no new exported surface (everything stays unexported, same package).
