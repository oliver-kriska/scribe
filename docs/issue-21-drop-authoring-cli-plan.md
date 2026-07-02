# Issue #21 — Drop authoring CLI + agent skill (roadmap-02)

Status: planning complete, ready for implementation. This plan is self-sufficient — the
implementing agent does not need to read the GitHub issue.

## 1. Problem & context

scribe's "drop file" pattern lets an agent working in a **non-KB project** hand reusable
knowledge to the user's KB: it writes a markdown file with YAML frontmatter to
`.claude/<kb_name>/YYYY-MM-DD-{slug}.md` in the current project, and the KB owner's
`scribe sync` cron absorbs it later.

Today authoring a drop file means an agent hand-writes the YAML frontmatter from memory
or from documentation, with **zero validation until absorb time** — and absorb time
validation doesn't really exist either:

- `cmd/scribe/sync_discover.go:267` (`unprocessedDropFiles`) and `:286`
  (`collectDropFiles`) only glob `.claude/<kb>/*.md`, copy bytes into
  `output/drops-<project>/`, and record `last_drop_processed`. No frontmatter is parsed.
- `cmd/scribe/sync_extract.go:317-332` (`extractProject`) builds a **prose instruction**
  ("Step 1.5: Process drop files... For 'create': make a new article. For 'update':
  merge...") and inlines the raw drop file bytes into an LLM prompt
  (`cmd/scribe/prompts/extract.md` / `extract-anthropic.md` / `extract-ollama.md`). The
  LLM — not Go — interprets `action`, `type`, `domain`, `tags`, `rolling_target`.
- `cmd/scribe/extract_envelope.go:107-211` (`gatherExtractFiles`) does the same thing on
  the envelope/JSON path: drop files are read as raw bytes and labeled `"DROP: " + rel`
  in the prompt. Same story — no structural parsing.
- The only place a "closed schema" for drop-file frontmatter exists today is
  **documentation**, in two places that already agree with each other:
  - `cmd/scribe/skills/scribe-kb/references/DROP_FILES.md` (an **already-shipped** agent
    skill, embedded via `cmd/scribe/skill.go:30`
    `//go:embed skills/scribe-kb/SKILL.md skills/scribe-kb/references/*.md`, installable
    via `scribe skill install`).
  - `cmd/scribe/templates/claude-md-kb.md:25-38` and
    `cmd/scribe/templates/codex-agents-md.md:25-38` (the handshake block `scribe init`
    writes into `~/.claude/CLAUDE.md` / `~/.codex/AGENTS.md` — see
    `cmd/scribe/init.go`).

**Conclusion that reshapes scope:** deliverable (b) from the issue — "a Claude Code skill
that teaches agents the schema" — **already exists and is already wired** (embed +
`scribe skill install` + drift-check via `--check`, see `cmd/scribe/skill.go:44-136`).
What's missing is deliverable (a), the CLI, plus updating the skill/handshake docs to
teach agents to call it instead of hand-authoring YAML. This plan is scoped accordingly:
mostly new code for `scribe drop`, plus surgical doc edits — not a new skill bundle.

Because absorb-time parsing of drop-file frontmatter doesn't exist (it's all
LLM-interpreted prose), `scribe drop` cannot "hook into" a validator on the consumer
side — there isn't one. Its entire value is **authoring-side**: guarantee the frontmatter
that lands in `.claude/<kb>/*.md` is well-formed YAML with values from the same closed
sets scribe already enforces elsewhere (`validTypes`, `validDomainsForRoot`), so the LLM
at absorb time is never handed a malformed or invented value.

## 2. Design decisions

### 2.1 Command shape: new top-level `scribe drop`, not a subcommand of `write`

**Decision:** Add `Drop DropCmd` as a new root field on `CLI` in `cmd/scribe/main.go`,
grouped `"content"` (next to `Write`, `Ingest`). New file `cmd/scribe/drop.go`.

**Why:** `scribe write` (`cmd/scribe/write.go`) writes *into the KB itself* and requires
`kbDir()` to resolve via the CWD-walk (i.e., you're standing inside the KB). `scribe drop`
is the opposite: it's designed to run from **outside** the KB, in an arbitrary project,
and it writes into *that project's* `.claude/<kb_name>/` directory, not the KB. Bolting
this onto `WriteCmd` would conflate two different root-resolution stories and violate the
existing `Rule 12 (append-only)` framing `write.go`'s doc comment establishes. A sibling
top-level command mirrors how `scribe ingest` and `scribe write` already sit side by side
as distinct "content commands."

**Rejected:** `scribe write --drop` flag on the existing command — rejected because
`WriteCmd.Run()` (`write.go:50`) hard-requires `kbDir()` to succeed via the full 4-step
resolution in `config.go:846` `kbDir()`, and `runCreate`/`runRolling` both write relative
to `root` (the KB), never relative to `cwd`. Retrofitting a second "write relative to cwd"
code path into the same struct is more confusing than a new 200-line file.

### 2.2 Where does the drop file go, and how is `<kb_name>` resolved from outside the KB?

**Decision:** The target directory is **`<cwd>/.claude/<kb_name>/`** — `cwd` is
`os.Getwd()` at invocation time (the current, non-KB project), never the KB root. This
matches the existing contract in `DROP_FILES.md:10-13` and how `scan.go:123`
(`scanPrintDropFiles`) and `sync_discover.go:267-281` (`unprocessedDropFiles`) already
read the same location for the same directory name.

`<kb_name>` resolution reuses `kbDir()` (`config.go:846`) exactly as every other command
does — **no new resolution logic**:

1. Call `kbDir()`. Its existing priority order (`-C` flag / `SCRIBE_KB` env / CWD-walk /
   `~/.config/scribe/config.yaml` `kb_dir` default) already handles "I'm in a random
   project but scribe knows my default KB" — this is precisely the common case (single KB
   per machine) `announceDefaultKB` (`config.go:887`) exists for. When invoked from a
   non-KB project, the CWD-walk step naturally finds nothing and falls through to the
   user-config default, so **no code change to `kbDir()` is needed.**
2. **If `kbDir()` resolves** (`root`, `err == nil`): `kb_name` = `kbName(root)`
   (`config.go:538`) unless `--kb-name` overrides it; valid `--domain` values =
   `validDomainsForRoot(root)` (`validate.go:46`); valid `--type` values = the existing
   package-level `validTypes` map (`validate.go:19` — already includes `idea`, matching
   `DROP_FILES.md`'s documented type list once corrected, see §2.6).
3. **If `kbDir()` fails** (no KB anywhere — no default registered, no `-C`, no
   `SCRIBE_KB`): `--kb-name` becomes **required** (error out with a clear message if
   absent). `--domain` is accepted **free-form** with a one-line stderr warning ("no KB
   resolved — domain not validated against any config; pass -C/SCRIBE_KB/--kb-name to
   validate"). `--type` is **still validated** against `validTypes` regardless — that's a
   scribe-wide fixed taxonomy, not a per-KB config value, so there's no reason to relax it
   even when the KB itself can't be found.

**Why split domain vs. type this way:** `validTypes` is a closed set baked into the
binary (mirrors `wikiDirs`, `validate.go:14-19`); it doesn't depend on any specific KB's
`scribe.yaml`, so it can always be enforced. `domain` is genuinely per-KB
(`scribe.yaml`'s `domains:` list, `config.go:70` + `AllDomains()` at `config.go:691`), so
it can only be validated when a KB is actually resolved. This directly answers the open
question from the assignment ("read from KB config when resolvable, else free-form with a
warning") using existing infrastructure instead of inventing a new one.

**Rejected:** requiring the user to always pass `--kb-name` explicitly — rejected because
it breaks the single-KB, zero-config common case that `kbDir()`'s fallback chain was
built for (`config.go:836-845`'s own comment explains why the CWD-walk-then-default
order matters). Also rejected: resolving `<kb_name>` by reading a KB config file located
via some new lookup — `kbDir()` already is that lookup; duplicating it would drift.

### 2.3 Nested-inside-KB guard

**Decision:** If `kbDir()` resolves and the *resolved root* is also an ancestor of (or
equal to) `cwd` — i.e., the agent is running `scribe drop` from inside the KB checkout
itself — print a stderr warning ("you're inside the KB (<root>) — `scribe write` writes
directly into the KB and skips the absorb queue; `scribe drop` is for cross-project
handoffs") but **do not block the write**. Detect via `strings.HasPrefix` on cleaned
absolute paths (same idiom `write.go:107-110` already uses for its `--path` escape
guard).

**Why warn, not error:** the skill (`SKILL.md:65-70`, "What NOT to do") already documents
this as a soft rule ("Don't write directly to the KB from a non-KB project"), and there
are legitimate edge cases (testing, a KB that intentionally treats itself as a "project"
in `projects/`) where a hard block would just get worked around. A warning satisfies the
"validate/normalize" spirit of the issue without adding a new failure mode nobody asked
for.

### 2.4 Flag set

```
scribe drop --title <str> --type <type> --domain <domain> [--action create|update|append]
            [--tags a,b,c] [--rolling-target learnings|decisions-log]
            [--body -|file:<path>|<inline>] [--slug <str>] [--date YYYY-MM-DD]
            [--kb-name <str>] [--force] [--dry-run|-n]
```

| Flag | Required | Default | Validation |
|---|---|---|---|
| `--title` `-T` | yes | — | non-empty |
| `--type` `-t` | yes | — | member of `validTypes` (`validate.go:19`) |
| `--domain` `-d` | yes | — | member of `validDomainsForRoot(root)` when KB resolves; free-form + warning otherwise |
| `--action` | no | `create` | one of `create`, `update`, `append` |
| `--tags` `-g` | no | none | repeatable or comma-separated (reuse `joinTags`, `write.go:236`) |
| `--rolling-target` | no | none | one of `learnings`, `decisions-log`; **requires** `--action append` |
| `--body` | no | `-` (stdin) | `-` = stdin, `file:<path>` = read file, else inline literal (reuse pattern from `write.go:273-292` `readBody`) |
| `--slug` | no | `slugify(title)` (`ingest.go:505`) | non-empty after slugify |
| `--date` | no | today (UTC, `2006-01-02`) | matches `dateRE` (`validate.go:38`) when given |
| `--kb-name` | conditionally (required iff `kbDir()` fails) | `kbName(root)` | non-empty; sanitized for use as a YAML key (see §2.6) |
| `--force` | no | `false` | — |
| `--dry-run` `-n` | no | `false` | field literally named `DryRun` so `main.go:194`'s `commandIsReadOnly` reflection convention picks it up for free (no `ReadOnly()` method needed) |

**Why `--body` mirrors `write.go`'s three-way spec instead of inventing a new one:**
consistency for anyone who has already learned `scribe write`'s body convention, and it
lets step 3.2 below extract a single shared helper instead of two copies of the same
stdin/file/inline branching.

**Rejected:** an `$EDITOR`-spawning mode — rejected because `scribe drop` is meant to be
called *by agents*, not typed interactively by a human at a keyboard; every other
CLI-write-surface command in this codebase (`write.go`, `ingest.go`) is non-interactive
for the same reason, and an editor spawn would hang a scripted/agent invocation.

### 2.5 Collision behavior

**Decision:** Target path is `<cwd>/.claude/<kb_name>/<date>-<slug>.md`. If that file
already exists: refuse with a clear error (`"%s already exists — pass --force to
overwrite, or vary --slug/--date"`) unless `--force` is set, in which case overwrite
unconditionally.

**Why refuse-by-default:** matches the existing convention in `write.go:112-114`
(`runCreate` refuses an existing target with a similarly-worded error). Drop files are
lower-stakes than KB articles (they're staging artifacts, never committed to the KB
itself), so unlike `write.go` there's a legitimate reason to want an override — hence
`--force`, which `write.go`'s create path deliberately doesn't offer ("Rule 12 is
append-only, edit directly"). A same-day repeat drop about a related topic is a realistic
agent workflow (e.g., two follow-up insights in one session); `--slug` differentiation is
the normal path, `--force` is the escape hatch.

### 2.6 Frontmatter shape emitted

```yaml
---
<kb_name>: true
action: create | update | append
title: "<title>"
type: <type>
domain: <domain>
tags: [tag1, tag2]
rolling_target: learnings | decisions-log   # only when set
---

<body>
```

- The **key name itself** is `<kb_name>` (matching `DROP_FILES.md:22-24`'s "the first key
  is the kb-name marker" contract, and `sync_discover.go`/`scan.go`'s directory-naming
  convention which is also keyed on `kbName(root)`). Emit it as a bare YAML key when it
  matches `^[A-Za-z_][A-Za-z0-9_-]*$`; otherwise quote it (`"<kb_name>": true`). This is
  defensive only — real KB names are always simple slugs — but it costs one regex check
  and prevents ever emitting invalid YAML for an edge-case name.
- `title` is always double-quoted (mirrors `write.go:213`'s `%q`).
- `tags` renders as `[a, b, c]` (reuse `joinTags`, `write.go:236-247`, already
  comma-splits and trims).
- **Fix a doc bug while here:** `DROP_FILES.md:27` and both templates
  (`claude-md-kb.md:32`, `codex-agents-md.md:32`) currently list
  `type: project | tool | person | decision | pattern | solution | research` — missing
  `idea`, which `validTypes` (`validate.go:19`) and `FRONTMATTER.md:10` both already
  include. `scribe drop --type idea` must be accepted; the doc edits in step 3.5 correct
  the listed set to match `validTypes` exactly everywhere it's written out.

### 2.7 No `Sources`/`related`/`confidence`/`created`/`updated` fields on drop files

**Decision:** `scribe drop` does not accept or emit any of the wiki-article-only fields
from `requiredFields` (`validate.go:13`: `created`, `updated`, `confidence`, `related`,
`sources`). Those are populated by the LLM at absorb time when it turns the drop into a
real wiki article (`extract.md`'s prompt already tells it to do so); the drop file itself
is a handoff document, not a wiki article, and `validateFile` (`validate.go:104`) is never
run against files under `.claude/<kb>/` (they never reach `wiki/`, `decisions/`, etc.
until the LLM writes an article from them) — confirmed by grepping `validateFile`'s
callers and `walkArticles`, neither of which touches `.claude/`.

### 2.8 Reuse `write.go`'s body-reading helper instead of duplicating it

**Decision:** Extract `WriteCmd.readBody()` (`write.go:275-292`) into a free function
`readBodySource(spec string) (string, error)` in `write.go`, and have both
`WriteCmd.readBody()` (now a one-line wrapper: `return readBodySource(w.Body)`) and the
new `DropCmd` call it directly. Zero behavior change to `write.go`.

**Why:** avoids a second stdin/`file:`/inline implementation drifting from the first;
matches the "New dependencies need justification... prefer reuse" spirit of the repo's
`CLAUDE.md` conventions section, applied here to intra-package reuse.

## 3. Implementation steps

### 3.1 `cmd/scribe/main.go` — register the command

- Add `Drop DropCmd \`cmd:"" group:"content" help:"Author a validated drop file for the current project's KB handoff directory."\`` to the `CLI` struct (`main.go:26-85`), placed after `Write` (line 38) and before `Ingest` (line 39) to keep the "content" group's write-surface commands adjacent.
- No other change to `main.go` — `TestRootCommandsAreGrouped` (referenced in the `commandGroups` comment, `main.go:87-91`) will pass automatically since the new field carries `group:"content"`.

### 3.2 `cmd/scribe/write.go` — extract the shared body-reader

- Replace the body of `WriteCmd.readBody()` (`write.go:275-292`) with a new free function:

```go
// readBodySource resolves a --body flag value shared by scribe write and
// scribe drop: "-" = stdin, "file:path" = read file, anything else = inline
// literal.
func readBodySource(spec string) (string, error) {
	if spec == "-" {
		data, err := io.ReadAll(bufio.NewReader(os.Stdin))
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	if after, ok := strings.CutPrefix(spec, "file:"); ok {
		data, err := os.ReadFile(after)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	return spec, nil
}

func (w *WriteCmd) readBody() (string, error) {
	return readBodySource(w.Body)
}
```

- No import changes needed (`bufio`, `io`, `os`, `strings` are already imported in
  `write.go`).
- Existing `write_test.go` tests for `readBody` continue to pass unchanged (behavior is
  identical, just refactored).

### 3.3 New file `cmd/scribe/drop.go`

```go
// drop.go — `scribe drop`: author a validated drop-file handoff for the
// current (non-KB) project's `.claude/<kb_name>/` directory. See
// cmd/scribe/skills/scribe-kb/references/DROP_FILES.md for the schema this
// enforces and cmd/scribe/skills/scribe-kb/SKILL.md for the agent-facing
// workflow this replaces hand-authored frontmatter for.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// dropValidActions and dropValidRollingTargets are drop-file-only closed
// sets — distinct from validTypes/validDomainsForRoot (validate.go), which
// govern wiki articles, not the handoff documents that precede them.
var (
	dropValidActions        = map[string]bool{"create": true, "update": true, "append": true}
	dropValidRollingTargets = map[string]bool{"learnings": true, "decisions-log": true}
	// bareYAMLKeyRE matches a key that never needs quoting.
	bareYAMLKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
)

// DropCmd writes a schema-validated drop file to the current project's
// .claude/<kb_name>/ directory, for a later `scribe sync` in the target KB
// to absorb. Unlike WriteCmd, this never requires standing inside a KB
// checkout — kbDir() resolution is used only to learn the KB's name and
// (when resolvable) its valid domain set; the file itself is always
// written relative to the CURRENT directory, never the KB root.
type DropCmd struct {
	Title          string   `help:"Article title (required)." short:"T"`
	Type           string   `help:"Article type: project | tool | person | decision | pattern | solution | research | idea." short:"t"`
	Domain         string   `help:"Domain tag. Validated against the resolved KB's scribe.yaml when one is found." short:"d"`
	Action         string   `help:"create | update | append." default:"create"`
	Tags           []string `help:"Tags (repeatable or comma-separated)." short:"g"`
	RollingTarget  string   `help:"learnings | decisions-log. Requires --action append." name:"rolling-target"`
	Body           string   `help:"Body source: '-' for stdin, 'file:path' for file, or inline string." default:"-"`
	Slug           string   `help:"Override the filename slug (default: derived from --title)."`
	Date           string   `help:"Override the YYYY-MM-DD date prefix (default: today)."`
	KBName         string   `help:"Override the resolved KB name (required if no KB can be resolved)." name:"kb-name"`
	Force          bool     `help:"Overwrite an existing drop file at the target path."`
	DryRun         bool     `help:"Print the drop file without writing." name:"dry-run"`
}

func (d *DropCmd) Run() error {
	if err := d.validateStatic(); err != nil {
		return err
	}

	root, kbResolved := "", true
	r, err := kbDir()
	if err != nil {
		kbResolved = false
	} else {
		root = r
	}

	kbNameResolved, domains, err := d.resolveKBContext(root, kbResolved)
	if err != nil {
		return err
	}
	if !domains[d.Domain] {
		if kbResolved {
			return fmt.Errorf("invalid --domain %q (expected one of: %s)", d.Domain, strings.Join(sortedKeys(domains), ", "))
		}
		fmt.Fprintf(os.Stderr, "scribe drop: warning: no KB resolved — --domain %q not validated against any config\n", d.Domain)
	}

	body, err := readBodySource(d.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return fmt.Errorf("body is empty (read from %s)", d.Body)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	d.warnIfInsideKB(root, kbResolved, cwd)

	date := d.Date
	if date == "" {
		date = time.Now().UTC().Format("2006-01-02")
	}
	slug := d.Slug
	if slug == "" {
		slug = slugify(d.Title)
	}
	if slug == "" {
		return errors.New("--title produced an empty slug; pass --slug explicitly")
	}

	dropDir := filepath.Join(cwd, ".claude", kbNameResolved)
	target := filepath.Join(dropDir, fmt.Sprintf("%s-%s.md", date, slug))

	if fileExists(target) && !d.Force {
		return fmt.Errorf("%s already exists — pass --force to overwrite, or vary --slug/--date", target)
	}

	content := d.buildFrontmatter(kbNameResolved) + "\n" + body + "\n"

	if d.DryRun {
		fmt.Fprintf(os.Stderr, "# would write: %s\n", target)
		fmt.Print(content)
		return nil
	}

	if err := os.MkdirAll(dropDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dropDir, err)
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	logMsg("drop", "wrote %s (%d bytes)", target, len(content))
	fmt.Printf("dropped: %s\n", target)
	return nil
}

// validateStatic checks everything that doesn't require KB resolution:
// required fields and the two drop-file-only closed sets.
func (d *DropCmd) validateStatic() error {
	if d.Title == "" {
		return errors.New("--title is required")
	}
	if !validTypes[d.Type] {
		return fmt.Errorf("invalid --type %q (expected one of: %s)", d.Type, strings.Join(sortedKeys(validTypes), ", "))
	}
	if d.Domain == "" {
		return errors.New("--domain is required")
	}
	if !dropValidActions[d.Action] {
		return fmt.Errorf("invalid --action %q (expected: create, update, append)", d.Action)
	}
	if d.RollingTarget != "" {
		if !dropValidRollingTargets[d.RollingTarget] {
			return fmt.Errorf("invalid --rolling-target %q (expected: learnings, decisions-log)", d.RollingTarget)
		}
		if d.Action != "append" {
			return fmt.Errorf("--rolling-target requires --action append (got --action %s)", d.Action)
		}
	}
	if d.Date != "" && !dateRE.MatchString(d.Date) {
		return fmt.Errorf("--date %q not in YYYY-MM-DD format", d.Date)
	}
	return nil
}

// resolveKBContext returns the effective kb_name and the valid-domain set to
// check --domain against. When kbResolved is false, domains is nil (every
// domain check becomes a warn-only pass in Run).
func (d *DropCmd) resolveKBContext(root string, kbResolved bool) (string, map[string]bool, error) {
	if kbResolved {
		name := d.KBName
		if name == "" {
			name = kbName(root)
		}
		return name, validDomainsForRoot(root), nil
	}
	if d.KBName == "" {
		return "", nil, errors.New("no scribe KB could be resolved (no -C, SCRIBE_KB, or default kb_dir) — pass --kb-name explicitly")
	}
	return d.KBName, map[string]bool{}, nil
}

// warnIfInsideKB nudges toward `scribe write` when cwd is inside the
// resolved KB itself — a drop file there would just wait for a sync that's
// already standing in the same checkout. Non-fatal by design; see plan
// §2.3.
func (d *DropCmd) warnIfInsideKB(root string, kbResolved bool, cwd string) {
	if !kbResolved || root == "" {
		return
	}
	cleanRoot := filepath.Clean(root) + string(os.PathSeparator)
	cleanCwd := filepath.Clean(cwd) + string(os.PathSeparator)
	if strings.HasPrefix(cleanCwd, cleanRoot) || filepath.Clean(cwd) == filepath.Clean(root) {
		fmt.Fprintf(os.Stderr, "scribe drop: warning: %s is inside the KB (%s) — consider `scribe write` instead\n", cwd, root)
	}
}

// buildFrontmatter renders the drop-file YAML block including the closing
// delimiter. kbNameKey is quoted only when it isn't a bare-safe YAML key.
func (d *DropCmd) buildFrontmatter(kbNameKey string) string {
	key := kbNameKey
	if !bareYAMLKeyRE.MatchString(key) {
		key = fmt.Sprintf("%q", key)
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "%s: true\n", key)
	fmt.Fprintf(&b, "action: %s\n", d.Action)
	fmt.Fprintf(&b, "title: %q\n", d.Title)
	fmt.Fprintf(&b, "type: %s\n", d.Type)
	fmt.Fprintf(&b, "domain: %s\n", d.Domain)
	b.WriteString("tags: [")
	b.WriteString(joinTags(d.Tags))
	b.WriteString("]\n")
	if d.RollingTarget != "" {
		fmt.Fprintf(&b, "rolling_target: %s\n", d.RollingTarget)
	}
	b.WriteString("---\n")
	return b.String()
}
```

Notes for the implementer:

- `fileExists`, `slugify`, `joinTags`, `kbName`, `validDomainsForRoot`, `validTypes`,
  `dateRE`, `sortedKeys`, `logMsg` all already exist package-wide (`ingest.go`,
  `write.go`, `config.go`, `validate.go`, `claude.go`) — no new helpers beyond what's
  written above.
- `ReadOnly()` is intentionally **not** implemented — `DryRun` is a literal field named
  `DryRun`, which `main.go:190-197`'s reflection-based `commandIsReadOnly` already
  detects generically (same convention `IngestURLCmd.DryRun` etc. rely on).
- Keep `Run()`'s error returns wrapped with enough context (`%w`) to match the rest of
  the codebase's error style — every existing command in this file set does this.

### 3.4 `cmd/scribe/write.go` — no functional change beyond §3.2

Just the extraction described in 3.2. Double-check `write_test.go` still compiles and
passes as-is (it should — `readBody()`'s signature and behavior are unchanged).

### 3.5 Doc edits — point the skill and handshake blocks at `scribe drop`

Four files, all content-only edits (no Go, no template variable changes):

**`cmd/scribe/skills/scribe-kb/references/DROP_FILES.md`:**
- Replace the "Required frontmatter" section's manual YAML instructions (lines 20-33)
  with: lead with a `scribe drop` invocation as the **primary** method, keep a shortened
  version of the manual YAML block as a **fallback** ("if `scribe` isn't on PATH, write
  the file directly with this exact schema:"). Example primary block:

  ```
  scribe drop --title "PgBouncer Transaction Mode Pool Sizing" \
    --type pattern --domain general --tags postgres,pgbouncer,connection-pooling \
    --body file:/tmp/pgbouncer-notes.md
  ```

  (Agents commonly have the body text in hand already, not a file — also document the
  stdin form: `scribe drop --title ... | ... <<'EOF' ... EOF` is awkward for multi-line
  markdown, so recommend `--body -` with a heredoc, or writing the body to a scratch file
  first and using `file:path`, matching `write.go`'s existing documented convention.)
- Fix the `type:` list (line 27) to include `idea`, matching `validTypes` (§2.6).
- Update the "Optional fields for rolling-target appends" section (lines 42-61) to show
  the `--rolling-target` flag form alongside the existing manual-YAML example.
- Update the "A complete worked example" section (lines 72-104) to show the equivalent
  `scribe drop` command producing the same file, immediately above the existing
  hand-written version (keep the hand-written version — it's still valid, still what
  `scribe drop` generates under the hood, and useful for an agent auditing the output).
- Leave "What NOT to file" (lines 106-113) unchanged — orthogonal to authoring mechanics.

**`cmd/scribe/skills/scribe-kb/SKILL.md`:**
- Update the "Operations cheat sheet" table row (line 29) — change the "Command" column
  from `Write to .claude/<kb_name>/YYYY-MM-DD-{slug}.md — see references/DROP_FILES.md`
  to `scribe drop --title ... --type ... --domain ... — see references/DROP_FILES.md`.
- Update "Workflow: file a drop file from another project" (lines 33-42), step 2: change
  "Write the frontmatter" to "Run `scribe drop` with the required flags (or hand-write
  the frontmatter per references/DROP_FILES.md if `scribe` isn't installed)."

**`cmd/scribe/templates/claude-md-kb.md`** and **`cmd/scribe/templates/codex-agents-md.md`**
(identical edit, both files — lines 25-38 in each):
- Replace the fenced YAML block + surrounding prose with a shorter primary instruction
  pointing at `scribe drop`, e.g.:

  > **How to contribute from other projects — drop files.** When a session in a non-KB
  > project produces reusable knowledge, run:
  >
  > `scribe drop --title "..." --type <project|tool|person|decision|pattern|solution|research|idea> --domain {{.DomainsPipe}} --tags a,b --body file:<scratch-file>`
  >
  > This validates the frontmatter and writes to `.claude/{{.KBName}}/YYYY-MM-DD-{slug}.md`
  > in the current project. If `scribe` isn't on PATH, write that file directly — schema
  > in `.claude/skills/scribe-kb/references/DROP_FILES.md` if the skill is installed, or
  > ask {{.OwnerName}}. Add `--rolling-target learnings` or `--rolling-target
  > decisions-log` when the insight belongs to a specific project's memory log.
  > `scribe sync` running on cron in the KB will absorb these automatically. Tell
  > {{.OwnerName}} what you filed and why — don't fabricate drop files for trivial facts.

  Keep every existing `{{.KBName}}` / `{{.OwnerName}}` / `{{.DomainsPipe}}` template
  variable usage — these are rendered by `init.go` (`templateVars` at `init.go:147`,
  populated around `init.go:451` and `init.go:689`); do not introduce new template
  variables (none are needed — `scribe drop`'s `--domain` takes any one value, so
  `{{.DomainsPipe}}` in the example is illustrative text, same role it plays today).
- No change needed to `init.go`, `init_plan.go`, or `init_test.go` — the templates are
  rendered with `text/template` and this edit only touches literal Markdown/prose inside
  them.

No change needed to `cmd/scribe/skills/scribe-kb/references/FRONTMATTER.md`,
`STRUCTURE.md`, `WIKILINKS.md`, `QUERY.md`, `COMPAT.md` — none of them describe drop-file
frontmatter (that's `DROP_FILES.md`'s sole job); confirmed by reading all five during
research for this plan.

### 3.6 README.md

Check `README.md` for any existing `scribe write` documentation block and add a
one-paragraph `scribe drop` entry alongside it, in the same style (command synopsis +
one example). If no such section exists for `write`, skip this — don't invent new
top-level README structure for this issue; a doc gap in README pre-existed and isn't
this issue's job to fix wholesale. (Implementer: grep `README.md` for `scribe write`
first; mirror whatever's found or skip.)

## 4. Test plan

New file `cmd/scribe/drop_test.go`, package `main`, mirroring `write_test.go`'s
structure (table-driven where the case count justifies it; scenario funcs otherwise).
`make test` (`go test ./... -tags sqlite_fts5`) must pass fully offline — every case
below uses only `t.TempDir()`, env vars, and in-process `DropCmd.Run()` calls; no
network, no `claude -p`, no real filesystem outside the tempdir.

Reuse `testKBRoot(t)` (`write_test.go:204-225`) for "KB resolves" scenarios, but note it
sets `SCRIBE_KB` to the KB root — for drop tests, the command must be exercised with
`cwd` **outside** that root (drop files are written relative to `cwd`, not `SCRIBE_KB`).
Use `t.Chdir(<separate tempdir>)` (the established idiom in this package — see
`config_test.go:55,75,90,107,121` and `convert_phase2a_test.go:190`) after calling
`testKBRoot(t)`, so `SCRIBE_KB` still resolves the KB while `cwd` is a distinct "project"
tempdir.

For "KB does not resolve" scenarios, follow `config_test.go:18-45`'s exact pattern
(`TestKBDirResolution`'s `setUserConfigKB` helper): `t.Setenv("XDG_CONFIG_HOME",
t.TempDir())` to sandbox `loadUserConfig()` away from any real
`~/.config/scribe/config.yaml`, `t.Setenv("SCRIBE_KB", "")`, leave `globalRoot` at its
zero value (save/restore it exactly as `TestKBDirResolution`'s `reset` helper does at
`config_test.go:42-46`, since it's a package-level var mutated by `-C` parsing), and
`t.Chdir(t.TempDir())` to a bare directory with no `scribe.yaml` anywhere in its parent
chain. This reuses the exact isolation mechanism already proven correct for `kbDir()`
resolution — don't invent a second one.

| Test | Setup | Assertion |
|---|---|---|
| `TestDropRun_CreateBasic` | `testKBRoot`, cwd = separate tempdir "project" (not the KB), all required flags | file written at `<project>/.claude/<kb_name>/<today>-<slug>.md`; frontmatter has correct `<kb_name>: true`, `action: create`, `title`, `type`, `domain`, `tags` |
| `TestDropRun_MissingTitle` | required flags minus `--title` | error containing `--title is required` |
| `TestDropRun_InvalidType` | `--type bogus` | error listing valid types (includes `idea`) |
| `TestDropRun_InvalidDomainKBResolved` | KB resolves, `--domain nonexistent` | error, no file written |
| `TestDropRun_FreeformDomainKBUnresolved` | KB unresolved, `--kb-name` given, arbitrary `--domain` | file written; stderr contains a warning; no error |
| `TestDropRun_KBUnresolvedNoKBName` | KB unresolved, `--kb-name` omitted | error containing `--kb-name` |
| `TestDropRun_RollingTargetRequiresAppend` | `--rolling-target learnings --action create` | error containing `requires --action append` |
| `TestDropRun_InvalidRollingTarget` | `--rolling-target bogus --action append` | error listing `learnings, decisions-log` |
| `TestDropRun_InvalidAction` | `--action delete` | error listing `create, update, append` |
| `TestDropRun_InvalidDateFormat` | `--date 2026-5-7` | error containing `YYYY-MM-DD` |
| `TestDropRun_SlugOverride` | `--slug custom-slug` | filename uses `custom-slug`, not a slugified title |
| `TestDropRun_DateOverride` | `--date 2020-01-01` | filename starts `2020-01-01-` |
| `TestDropRun_CollisionRefused` | run twice with identical title/date/slug, no `--force` | second call errors, first file untouched (compare bytes) |
| `TestDropRun_CollisionForced` | same, second call with `--force` | second call succeeds, file content reflects second call's body |
| `TestDropRun_EmptyBody` | `--body ""` (inline empty) | error containing `body is empty` |
| `TestDropRun_BodyFromFile` | `--body file:<tempfile>` | file body matches tempfile contents |
| `TestDropRun_BodyFromStdin` | default `--body -`, `os.Stdin` redirected via a piped tempfile (match whatever stdin-redirection idiom `write_test.go`'s stdin case already uses, if one exists — else construct via `os.Pipe`) | body matches piped content |
| `TestDropRun_TagsCommaAndRepeat` | `--tags a,b --tags c` | frontmatter `tags: [a, b, c]` |
| `TestDropRun_KBNameKeyQuotedWhenUnsafe` | `--kb-name "weird name"` (KB unresolved path) | frontmatter emits `"weird name": true` (quoted key), not bare `weird name: true` |
| `TestDropRun_WarnsInsideKB` | cwd == KB root itself | stderr contains "inside the KB"; file still written (non-fatal) |
| `TestDropRun_DryRun` | `--dry-run` | stdout contains the frontmatter+body; no file created on disk |
| `TestReadBodySource_StdinFileInline` | direct unit test of the extracted `readBodySource` (moved out of `write_test.go`'s implicit coverage, or duplicated as a focused test) | all three modes behave identically to the pre-refactor `WriteCmd.readBody` |

Also update `main_test.go`'s `TestRootCommandsAreGrouped` fixture list if it enumerates
command names explicitly (check first — if it iterates `CLI` struct fields via
reflection, as `commandIsReadOnly` does, no update is needed; only touch it if it hardcodes
a name list).

Skill-drift regression: add `TestSkillInstallCheckPassesAfterDropDocEdit` (or extend an
existing `skill_test.go` case) that runs the embedded `skills/scribe-kb/**` through
`readEmbeddedSkillFiles()` and confirms the file still parses as valid Markdown with the
`scriptorium: true`-style example blocks still present as fenced code (a regression guard
against a doc edit accidentally breaking the fence structure `scribe skill install
--check` diffs against). This is optional polish, not load-bearing — skip if
`skill_test.go` doesn't already have infrastructure for it and adding that infrastructure
would blow the size budget (see §7).

## 5. Risks & edge cases

- **Two different "root" concepts in one command.** The single biggest risk is
  implementer confusion between `root` (the resolved KB, used only for name/domain
  lookup) and `cwd` (where the file is actually written). Every write operation in
  `drop.go` must use `cwd`-derived paths; every validation lookup uses `root`-derived
  config. The plan's pseudocode in §3.3 keeps these as distinct local variables
  (`root` vs `cwd`) specifically to make this mistake harder to make by accident — do not
  collapse them.
- **`kbDir()`'s CWD-walk could accidentally "resolve" a KB that isn't the one the user
  wants** if `scribe drop` is run from inside some *other* KB's checkout while a
  *different* KB is the intended target. This is identical to the pre-existing
  multi-KB ambiguity `announceDefaultKB` (`config.go:887`) already surfaces for every
  other command — not a new risk this issue introduces, and already has an escape hatch
  (`-C`/`SCRIBE_KB`). No special handling needed beyond what `kbDir()` already does.
- **Domain free-form fallback could let a typo'd domain silently through** when no KB
  resolves. Mitigated by the mandatory stderr warning (§2.2 step 3) — the agent (and any
  human watching) sees it, and the eventual absorb-time LLM pass still has the domain
  value to reason about even if scribe itself couldn't validate it structurally.
- **`--force` silently overwriting a same-named drop file from a different, unrelated
  session.** Low risk in practice (filenames are `date-slug`, collision requires the same
  day + same slugified title), and `--force` is opt-in, not default. No further
  mitigation needed.
- **YAML key quoting edge case:** a `--kb-name` containing a literal `"` would break the
  `%q`-quoted key. `%q` (Go's `fmt` verb) already escapes embedded quotes correctly, so
  this is handled for free by using `fmt.Sprintf("%q", key)` rather than hand-rolled
  quoting.
- **Stdin body mode (`--body -`) when nothing is piped** will hang waiting for EOF if an
  agent invokes `scribe drop` with the default `--body` and no explicit stdin redirect.
  This mirrors `write.go`'s existing behavior exactly (same failure mode already exists
  for `scribe write`) — not a regression, but worth flagging in the doc edits (§3.5) so
  the skill explicitly recommends `--body file:<path>` for agent use, where a hang is
  much less likely than with a bare `-`. Add this as an explicit tip in the updated
  `DROP_FILES.md`.
- **Windows path separators:** not a concern — the whole codebase targets darwin/linux
  only per `Makefile`/`.goreleaser.yml` (no windows build target), consistent with every
  other `filepath.Join`-based command already in the package.

## 6. Interactions with other open issues

- **#26 (KB registry + KB-agnostic cron):** `scribe drop`'s KB-name resolution leans
  entirely on `kbDir()`'s existing user-config fallback (`~/.config/scribe/config.yaml`
  `kb_dir`), which is exactly the mechanism #26 is building out further (`kb.go`,
  `registry.go` already exist in this checkout — the registry itself, i.e. `registeredKBs()` /
  `registerKB()` / `KbCmd`, is **already implemented**, contrary to it still being listed
  as pending work). No blocking dependency: `scribe drop` only needs `kb_dir` (the
  *default*), not the full `kbs:` list, since a drop file always targets exactly one KB
  per invocation. If #26 later adds a `--kb <registered-name>` selector across the CLI, `scribe
  drop --kb-name` should probably be reconciled with it (rename or alias) — flag as a
  small follow-up, not a blocker.
- **#28 / #41 (mentioned as bundled in the same implementation phase per the roadmap):**
  no code overlap found in this research — different files entirely. Verify at
  implementation time that neither touches `main.go`'s `CLI` struct in a way that
  conflicts with the new `Drop` field ordering.
- **Any future "structural drop-file validation at absorb time" issue:** this plan
  deliberately does **not** add Go-side parsing of `action`/`rolling_target` on the
  consumer side (§1's "Conclusion" and §2.7) — that remains fully LLM-interpreted via
  `extract.md`/`extract-anthropic.md`/`extract-ollama.md`. If a future issue wants
  deterministic absorb-time enforcement (e.g., reject a drop whose `type:` isn't in
  `validTypes` before it ever reaches the LLM prompt), that's new scope in
  `sync_discover.go`/`sync_extract.go`, not something this issue should absorb — noted so
  the next planner doesn't assume this issue covers it.

## 7. Size estimate

**Size: M** (medium — one new command, one small refactor, four doc-only edits, one new
test file).

Rough LOC:
- `cmd/scribe/drop.go`: ~230 lines (command struct, `Run`, 4 helper methods, 2 package-level maps/regex).
- `cmd/scribe/write.go`: ~10 lines changed (extract `readBodySource`).
- `cmd/scribe/main.go`: 1 line added.
- `cmd/scribe/drop_test.go`: ~250-300 lines (20 test functions from §4, several sharing
  setup via a table or a small helper).
- Doc edits (`DROP_FILES.md`, `SKILL.md`, `claude-md-kb.md`, `codex-agents-md.md`):
  no Go LOC, but nontrivial prose — budget these as their own unit of work, not
  incidental.
- `README.md`: 0-15 lines, conditional on what's already there (§3.6).

Total new/changed Go: **~490-540 lines**. No new `go.mod` dependencies (everything used —
`regexp`, `time`, `path/filepath`, `os`, `errors`, `fmt`, `strings` — is already imported
elsewhere in the package).
