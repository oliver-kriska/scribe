package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"time"

	"gopkg.in/yaml.v3"
)

// wiki_actions.go implements the Phase 4B JSON-action envelope. The
// motivation is documented in docs/local-model-support-plan.md: pass-2
// currently runs the model with Read/Write/Edit/Glob/Grep tools, which
// only Anthropic's claude -p harness supports cleanly. To localize
// pass-2 we flip the protocol — the model emits one JSON envelope
// describing the file mutations it wants, and Go applies them.
//
// Why this is the right shape:
//
//   - Local-model friendly. A 4–7B model that can produce one
//     well-formed JSON object handles this prompt; chaining 5–10 tool
//     calls in a row reliably is a bigger ask.
//   - Reviewable. Actions are auditable before they hit disk. Dry-run
//     mode falls out for free.
//   - Cheaper. No round-trip per tool call → fewer tokens, lower
//     wallclock.
//   - Safer. Path normalization, KB-rooting, and overwrite policies
//     live in Go where they're testable rather than in a prompt.
//
// Scope of this file: define the types, parse an envelope, apply the
// actions. The pass-2 prompt that produces these envelopes and the
// goroutine path that consumes them land in follow-up commits.

// WikiActionEnvelope is the top-level JSON shape pass-2 emits when
// running in `json` mode (Phase 4B). One envelope per pass-2
// invocation; each invocation focuses on a single entity but may
// produce multiple file actions (e.g. create the entity article AND
// update an index hint elsewhere).
//
// The `notes` field is freeform text the model can use to explain
// non-obvious decisions ("kept old confidence:medium because ..."). Go
// logs it but does not act on it.
//
// Phase 4C v2: `Meta` extends the envelope with side-channel writes
// that don't fit the wiki-dir sandbox (sessions log, rolling memory
// files, top-level log.md). The pass-2 path leaves Meta nil; session-
// mine, dream, and assess fill it. Envelopes without `meta` parse
// cleanly into V1 callers — backward-compatible by design.
type WikiActionEnvelope struct {
	// Version pins the schema. Defaults to 1 when omitted (legacy
	// pass-2 envelopes). Phase 4C bumps to 2 for envelopes that use
	// `Meta`. New consumers should always set version=2.
	Version int `json:"version,omitempty"`
	// Entity echoes the entity label from the pass-2 prompt. Used
	// for log breadcrumbs and to sanity-check that the model is on
	// task; mismatch is a soft warning, not a hard failure.
	Entity string `json:"entity,omitempty"`
	// Notes is freeform commentary from the model. Optional.
	Notes string `json:"notes,omitempty"`
	// Actions is the ordered list of file mutations to apply.
	// Order matters: a `create` must precede a later `append` to
	// the same path.
	Actions []WikiAction `json:"actions"`
	// Meta is the Phase 4C side-channel for writes that don't live
	// under wiki/. Empty for pass-2; session-mine fills it with
	// sessions_log_append + optional rolling_memory_append; dream
	// adds log_append. Each MetaAction op has its own allow-list
	// of writable paths inside validateMetaAction so the model
	// can never name an arbitrary file.
	Meta []MetaAction `json:"meta,omitempty"`
}

// MetaAction is a Phase 4C side-channel mutation. The op vocabulary
// is fixed (no free-form paths) so a model can never instruct scribe
// to overwrite an arbitrary file. Each op has a dedicated handler
// inside applyMetaActions; unknown ops surface as errors.
//
// Supported ops:
//
//   - log_append: append one line to the KB's root-level log.md (the
//     dream cycle's running log). Fields: line.
//   - sessions_log_append: register a processed session in
//     wiki/_sessions_log.json. Fields: session_id, timestamp.
//   - rolling_memory_append: append a paragraph to
//     <domain>/<target>.md where target ∈ {learnings, decisions-log}.
//     Fields: domain, target, content.
type MetaAction struct {
	Op string `json:"op"`
	// Line is the single line for log_append (no trailing newline
	// required; the executor adds one).
	Line string `json:"line,omitempty"`
	// SessionID + Timestamp for sessions_log_append. Timestamp can
	// be empty — the executor uses time.Now().UTC() in that case.
	SessionID string `json:"session_id,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	// Domain + Target for rolling_memory_append. Target is the
	// stem name (no .md), restricted to "learnings" or "decisions-log".
	Domain  string `json:"domain,omitempty"`
	Target  string `json:"target,omitempty"`
	Content string `json:"content,omitempty"`
}

// WikiAction is one file mutation. Op-specific fields are flat for
// JSON-schema simplicity (matches what local models produce reliably
// without nested discriminators):
//
//   - op="create": Path + Content. Refuses to overwrite unless the
//     caller sets ApplyOptions.AllowOverwrite.
//   - op="append": Path + Content. Content is written verbatim onto
//     the end of an existing file; caller-supplied leading newline
//     decides spacing.
//   - op="replace_section": Path + Heading + Content. The section
//     bounded by "## Heading" until the next h2 (or EOF) gets
//     replaced. Heading match is exact — typos surface as
//     "section not found" rather than silently creating a new section.
//   - op="update_frontmatter": Path + Frontmatter. Merged into the
//     existing YAML frontmatter; only the keys here are mutated, all
//     other keys are preserved.
//
// Unknown ops surface as errors during apply rather than silent skips.
type WikiAction struct {
	Op string `json:"op"`
	// Path is relative to the KB root and must resolve under one of
	// the wiki dirs (wiki, projects, research, ...). Absolute paths
	// or `..` traversals are rejected by validateActionPath.
	Path string `json:"path"`
	// Content is the file content for create / append / section
	// replace. Trailing newlines are preserved exactly — the
	// executor does not normalize.
	Content string `json:"content,omitempty"`
	// Heading is the h2 header for replace_section. Must match an
	// existing h2 line in the target file ("## " + Heading).
	Heading string `json:"heading,omitempty"`
	// Frontmatter is the merge map for update_frontmatter. Values
	// are encoded back into YAML when applied. Only the keys here
	// are mutated; all other frontmatter keys are preserved.
	Frontmatter map[string]any `json:"frontmatter,omitempty"`
}

// ApplyOptions controls how the executor processes an envelope. Flags
// here are the failure-mode knobs callers tune per pass:
//
//   - DryRun: parse + validate but skip all writes. Useful for
//     pre-flight inspection of model output.
//   - AllowOverwrite: when false, an `op=create` against an existing
//     file is an error. Pass-2 in json mode sets this true since
//     "create or update" is one logical operation from the model's
//     perspective.
type ApplyOptions struct {
	DryRun         bool
	AllowOverwrite bool
}

// ApplyResult summarizes what the executor did. Returned even on
// partial failures so callers can log "wrote 2 of 3 actions; failed on
// path X (reason Y)" instead of "error".
type ApplyResult struct {
	Applied []string // Paths actually written to disk
	Skipped []string // Paths skipped due to DryRun or no-op equivalence
	Errors  []string // Per-action failure messages, formatted for logs
}

// applyWikiActions processes an envelope's actions in order, applying
// file writes through the kb-root sandbox and recording per-action
// outcomes. The function returns an error only on catastrophic
// failure; per-action errors land in result.Errors so the caller can
// decide whether to roll back or accept the partial result.
//
// Phase 4C: after the WikiAction list, Meta actions execute in order.
// Meta failures land in result.Errors like wiki actions; a bad meta
// op does not prevent earlier wiki actions from sticking on disk.
func applyWikiActions(root string, env WikiActionEnvelope, opts ApplyOptions) (ApplyResult, error) {
	res := ApplyResult{}
	if root == "" {
		return res, fmt.Errorf("apply wiki actions: empty root")
	}
	for i, a := range env.Actions {
		abs, err := validateActionPath(root, a.Path)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("action[%d] %s %q: %v", i, a.Op, a.Path, err))
			continue
		}
		switch a.Op {
		case "create":
			if !opts.AllowOverwrite {
				if _, err := os.Stat(abs); err == nil {
					res.Errors = append(res.Errors, fmt.Sprintf("action[%d] create %q: file exists and AllowOverwrite=false", i, a.Path))
					continue
				}
			}
			if opts.DryRun {
				res.Skipped = append(res.Skipped, a.Path)
				continue
			}
			if err := writeFileAtomic(abs, []byte(a.Content), 0o644); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("action[%d] create %q: %v", i, a.Path, err))
				continue
			}
			res.Applied = append(res.Applied, a.Path)

		case "append":
			if opts.DryRun {
				res.Skipped = append(res.Skipped, a.Path)
				continue
			}
			if err := appendToFile(abs, a.Content); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("action[%d] append %q: %v", i, a.Path, err))
				continue
			}
			res.Applied = append(res.Applied, a.Path)

		case "replace_section":
			if a.Heading == "" {
				res.Errors = append(res.Errors, fmt.Sprintf("action[%d] replace_section %q: heading required", i, a.Path))
				continue
			}
			if opts.DryRun {
				res.Skipped = append(res.Skipped, a.Path)
				continue
			}
			if err := replaceSection(abs, a.Heading, a.Content); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("action[%d] replace_section %q: %v", i, a.Path, err))
				continue
			}
			res.Applied = append(res.Applied, a.Path)

		case "update_frontmatter":
			if len(a.Frontmatter) == 0 {
				res.Errors = append(res.Errors, fmt.Sprintf("action[%d] update_frontmatter %q: empty frontmatter map", i, a.Path))
				continue
			}
			if opts.DryRun {
				res.Skipped = append(res.Skipped, a.Path)
				continue
			}
			if err := updateFrontmatter(abs, a.Frontmatter); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("action[%d] update_frontmatter %q: %v", i, a.Path, err))
				continue
			}
			res.Applied = append(res.Applied, a.Path)

		default:
			res.Errors = append(res.Errors, fmt.Sprintf("action[%d] unknown op %q (path=%q)", i, a.Op, a.Path))
		}
	}
	for i, m := range env.Meta {
		if err := applyMetaAction(root, m, opts); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("meta[%d] %s: %v", i, m.Op, err))
			continue
		}
		res.Applied = append(res.Applied, metaActionLabel(m))
	}
	return res, nil
}

// allowedDomainsForRoot resolves the domain set the executor accepts
// for rolling_memory_append. We pull from scribe.yaml + universal
// domains so a model can't invent a new directory by mis-spelling a
// domain.
func allowedDomainsForRoot(root string) map[string]bool {
	out := map[string]bool{}
	cfg := loadConfig(root)
	if cfg != nil {
		for _, d := range cfg.AllDomains() {
			out[d] = true
		}
	} else {
		for _, d := range universalDomains {
			out[d] = true
		}
	}
	return out
}

// validRollingTargets is the historical closed set of rolling-memory
// targets the executor will write. Kept as a fallback when no scribe
// config is loaded (tests, dry-run from a non-KB cwd); runtime
// production uses allowedRollingTargetsForRoot to honor the user's
// `meta.rolling_targets:` list.
var validRollingTargets = map[string]bool{
	"learnings":     true,
	"decisions-log": true,
}

// allowedRollingTargetsForRoot returns the effective rolling-target
// allow-list for a KB. Loads the scribe config; falls back to the
// historical pair when the config is missing or contains no
// rolling_targets entries (defensive — applyMetaDefaults already
// fills the default).
func allowedRollingTargetsForRoot(root string) map[string]bool {
	cfg := loadConfig(root)
	if cfg != nil && len(cfg.Meta.RollingTargets) > 0 {
		out := make(map[string]bool, len(cfg.Meta.RollingTargets))
		for _, t := range cfg.Meta.RollingTargets {
			out[t] = true
		}
		return out
	}
	return validRollingTargets
}

// applyMetaAction routes one MetaAction to the appropriate handler.
// All paths are constructed inside the executor so a model can never
// name a free-form target. Returns an error per action; the caller
// records it without aborting the rest of the envelope.
func applyMetaAction(root string, m MetaAction, opts ApplyOptions) error {
	switch m.Op {
	case "log_append":
		return applyMetaLogAppend(root, m, opts)
	case "sessions_log_append":
		return applyMetaSessionsLogAppend(root, m, opts)
	case "rolling_memory_append":
		return applyMetaRollingAppend(root, m, opts)
	default:
		return fmt.Errorf("unknown meta op %q", m.Op)
	}
}

// metaActionLabel returns a short string suitable for logging which
// meta op landed. Avoids dumping the full Content field for rolling
// memory writes (which can be a multi-line paragraph).
func metaActionLabel(m MetaAction) string {
	switch m.Op {
	case "log_append":
		return "meta:log_append"
	case "sessions_log_append":
		return "meta:sessions_log_append:" + m.SessionID
	case "rolling_memory_append":
		return "meta:rolling_memory_append:" + m.Domain + "/" + m.Target
	default:
		return "meta:" + m.Op
	}
}

// applyMetaLogAppend appends one line to <root>/log.md, creating the
// file if absent. The dream cycle's Phase 5 wrap-up uses this; the
// session-mine path also adds a single line per processed session so
// the cron log has a chronological record of where time went.
//
// The model's input is one logical line; the executor strips embedded
// CRLFs and adds a trailing \n so subsequent appends start on a new
// line even if the model forgot.
func applyMetaLogAppend(root string, m MetaAction, opts ApplyOptions) error {
	line := strings.ReplaceAll(strings.TrimRight(m.Line, "\r\n"), "\n", " ")
	if line == "" {
		return fmt.Errorf("log_append: empty line")
	}
	if opts.DryRun {
		return nil
	}
	logPath := filepath.Join(root, "log.md")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log.md: %w", err)
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, line); err != nil {
		return fmt.Errorf("write log.md: %w", err)
	}
	return nil
}

// applyMetaSessionsLogAppend records one session ID under
// wiki/_sessions_log.json's `processed` map. The Phase 4C envelope
// replaces the previous "Claude writes the file" approach; centralizing
// this in Go gives us a per-file lock against concurrent session-mine
// goroutines stomping each other's JSON edits.
func applyMetaSessionsLogAppend(root string, m MetaAction, opts ApplyOptions) error {
	if m.SessionID == "" {
		return fmt.Errorf("sessions_log_append: session_id required")
	}
	if opts.DryRun {
		return nil
	}
	ts := m.Timestamp
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
	}
	path := filepath.Join(root, "wiki", "_sessions_log.json")
	sessionsLogMu.Lock()
	defer sessionsLogMu.Unlock()
	if !fileExists(path) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("mkdir wiki: %w", err)
		}
		if err := os.WriteFile(path, []byte(`{"processed": {}, "last_scan": null}`+"\n"), 0o644); err != nil {
			return fmt.Errorf("init sessions log: %w", err)
		}
	}
	return updateJSONFile(path, func(data map[string]any) {
		processed, _ := data["processed"].(map[string]any)
		if processed == nil {
			processed = map[string]any{}
		}
		processed[m.SessionID] = ts
		data["processed"] = processed
	})
}

// applyMetaRollingAppend appends one paragraph to a per-domain rolling
// memory file (projects/<domain>/<target>.md). Domain must be in the
// KB's allow-list and target must be in validRollingTargets. The
// executor creates the file with a minimal frontmatter when absent so
// the model never has to name the path.
//
// Concurrency: rolling-memory writes share rollingMemoryMu — multiple
// session-mine goroutines targeting the same file serialize through
// the lock.
func applyMetaRollingAppend(root string, m MetaAction, opts ApplyOptions) error {
	if m.Domain == "" {
		return fmt.Errorf("rolling_memory_append: domain required")
	}
	if m.Target == "" {
		return fmt.Errorf("rolling_memory_append: target required")
	}
	allowedTargets := allowedRollingTargetsForRoot(root)
	if !allowedTargets[m.Target] {
		keys := make([]string, 0, len(allowedTargets))
		for k := range allowedTargets {
			keys = append(keys, k)
		}
		return fmt.Errorf("rolling_memory_append: target %q not in {%s}", m.Target, strings.Join(keys, ", "))
	}
	if !allowedDomainsForRoot(root)[m.Domain] {
		return fmt.Errorf("rolling_memory_append: domain %q not in scribe.yaml domains", m.Domain)
	}
	content := strings.TrimRight(m.Content, "\n")
	if content == "" {
		return fmt.Errorf("rolling_memory_append: empty content")
	}
	if opts.DryRun {
		return nil
	}
	path := filepath.Join(root, "projects", m.Domain, m.Target+".md")
	rollingMemoryMu.Lock()
	defer rollingMemoryMu.Unlock()
	if !fileExists(path) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("mkdir project dir: %w", err)
		}
		fm := fmt.Sprintf("---\ntitle: \"%s — %s\"\ntype: article\ndomain: %s\nrolling: true\n---\n\n", m.Domain, m.Target, m.Domain)
		if err := os.WriteFile(path, []byte(fm), 0o644); err != nil {
			return fmt.Errorf("init rolling file: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open rolling file: %w", err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "\n%s\n", content); err != nil {
		return fmt.Errorf("write rolling file: %w", err)
	}
	return nil
}

var (
	sessionsLogMu   gosync.Mutex
	rollingMemoryMu gosync.Mutex
)

// validateActionPath rejects anything that would escape the KB root or
// land outside a known wiki directory. The path the model emits is
// always relative to root; an absolute path or `..` traversal is a
// red flag and gets refused.
//
// We don't follow symlinks: filepath.Clean handles `..` and the
// allowed-prefix check pins the result to a wiki dir. Symlinks
// pointing outside would still resolve transparently when the file
// gets written — that's fine because the user is the one who placed
// the symlink in their KB and has accepted that trust boundary.
func validateActionPath(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute paths refused (must be relative to KB root)")
	}
	cleaned := filepath.Clean(rel)
	// Reject explicit traversal. filepath.Clean turns "a/../b" into
	// "b" but leaves leading ".." alone — the only way to escape.
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path traverses outside KB root: %q", rel)
	}
	abs := filepath.Join(root, cleaned)
	// Walk the wiki dirs and accept the path if it's rooted in any.
	parts := strings.SplitN(cleaned, string(os.PathSeparator), 2)
	top := parts[0]
	for _, allowed := range wikiDirs {
		if top == allowed {
			return abs, nil
		}
	}
	return "", fmt.Errorf("path %q is outside known wiki dirs (%s)", rel, strings.Join(wikiDirs, ", "))
}

// writeFileAtomic writes content to path via tmp + rename so a
// partially-written file is never observed by readers. The parent
// dir is created on demand.
func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, mode); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// appendToFile appends content to the target. Missing file is an
// error — append assumes the article already exists. The model
// should have emitted `create` for new files.
func appendToFile(path, content string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("append target missing: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}

// replaceSection swaps the body of the H2 section "## <heading>" with
// the supplied content. The section is delimited by the heading line
// and either the next H2 line or EOF. Heading match is exact (no
// trailing-whitespace tolerance) so the model has to echo the
// existing heading verbatim, which is a feature: typos surface as
// "section not found" rather than silently creating new sections.
//
// The replacement preserves the heading line itself — the model
// supplies the body only. New content is normalized to end in exactly
// one newline so the next H2 doesn't end up glued to it.
func replaceSection(path, heading, body string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	target := "## " + heading
	startIdx := -1
	for i, l := range lines {
		if l == target {
			startIdx = i
			break
		}
	}
	if startIdx < 0 {
		return fmt.Errorf("section %q not found", heading)
	}
	endIdx := len(lines)
	for j := startIdx + 1; j < len(lines); j++ {
		if strings.HasPrefix(lines[j], "## ") {
			endIdx = j
			break
		}
	}
	normalized := strings.TrimRight(body, "\n") + "\n"
	rebuilt := make([]string, 0, len(lines))
	rebuilt = append(rebuilt, lines[:startIdx+1]...)
	// Empty line under heading for readability.
	rebuilt = append(rebuilt, "")
	rebuilt = append(rebuilt, strings.Split(strings.TrimRight(normalized, "\n"), "\n")...)
	rebuilt = append(rebuilt, "")
	rebuilt = append(rebuilt, lines[endIdx:]...)
	return writeFileAtomic(path, []byte(strings.Join(rebuilt, "\n")), 0o644)
}

// updateFrontmatter merges `updates` into the YAML frontmatter at the
// head of the file. Only keys present in `updates` are mutated. The
// rest of the file (frontmatter or body) is preserved byte-for-byte.
//
// Implementation note: we re-encode the whole frontmatter through
// yaml.Marshal because line-by-line key replacement risks corrupting
// multi-line YAML values (lists, block scalars). The cost is that key
// order is determined by the YAML library's iteration, not the
// original. Acceptable — frontmatter is read by tools, not humans
// scanning for ordering.
func updateFrontmatter(path string, updates map[string]any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	s := string(raw)
	if !strings.HasPrefix(s, "---") {
		return fmt.Errorf("no frontmatter delimiter in %s", filepath.Base(path))
	}
	end := strings.Index(s[3:], "\n---")
	if end < 0 {
		return fmt.Errorf("no closing frontmatter delimiter in %s", filepath.Base(path))
	}
	body := s[end+3+4:] // skip closing "\n---"
	// Tolerate the optional newline after the closing delimiter.
	body = strings.TrimPrefix(body, "\n")

	current, err := parseFrontmatterRaw(raw)
	if err != nil {
		return fmt.Errorf("parse frontmatter: %w", err)
	}
	for k, v := range updates {
		current[k] = v
	}
	encoded, err := marshalFrontmatter(current)
	if err != nil {
		return fmt.Errorf("encode frontmatter: %w", err)
	}
	rebuilt := "---\n" + encoded + "---\n" + body
	return writeFileAtomic(path, []byte(rebuilt), 0o644)
}

// marshalFrontmatter renders a frontmatter map back to YAML. Centralized
// so the test suite can swap encoders without chasing call sites.
func marshalFrontmatter(m map[string]any) (string, error) {
	out, err := yaml.Marshal(m)
	if err != nil {
		return "", err
	}
	s := string(out)
	// yaml.Marshal already emits a trailing newline, but be defensive
	// — our wrapper expects the closing --- to follow on the next line.
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s, nil
}

// parseEnvelope unmarshals one JSON envelope and validates the
// minimum shape pass-2 callers expect: at least one action, every
// action has an op + a path. Detailed per-action validation (heading
// required for replace_section, etc.) happens during apply so a bad
// action doesn't kill the whole envelope.
func parseEnvelope(jsonText string) (WikiActionEnvelope, error) {
	var env WikiActionEnvelope
	if err := json.Unmarshal([]byte(jsonText), &env); err != nil {
		return env, fmt.Errorf("unmarshal envelope: %w", err)
	}
	if len(env.Actions) == 0 {
		return env, fmt.Errorf("envelope has no actions")
	}
	for i, a := range env.Actions {
		if a.Op == "" {
			return env, fmt.Errorf("action[%d] missing op", i)
		}
		if a.Path == "" {
			return env, fmt.Errorf("action[%d] missing path", i)
		}
	}
	return env, nil
}

// parseEnvelopeV2 is the version-asserting parser for callers that
// depend on the Meta block (dream / assess / deep / session-mine).
// It accepts envelopes with version omitted (V1, no Meta — emits a
// debug log so the operator notices a model that hasn't picked up
// the new prompt) and rejects any explicitly-versioned envelope above
// the highest version this binary understands.
//
// The actual schema rules (actions list may be empty, every action
// needs op+path, etc.) are checked by parseEnvelopeAllowEmpty — V2
// uniquely allows empty actions because mining a session can produce
// zero wiki articles but still record sessions_log_append in Meta.
//
// callerLabel is only used in the warning log so the operator can
// find the misconfigured call site.
func parseEnvelopeV2(jsonText, callerLabel string) (WikiActionEnvelope, error) {
	env, err := parseEnvelopeAllowEmpty(jsonText)
	if err != nil {
		return env, err
	}
	const maxKnownVersion = 2
	switch {
	case env.Version == 0:
		// V1 shape — Meta block will be empty. Log so operators flipping
		// to envelope mode notice models still emitting V1.
		if len(env.Meta) > 0 {
			logMsg("envelope", "%s: V1 envelope with non-empty Meta — set version: 2 in the prompt to silence this", callerLabel)
		}
	case env.Version > maxKnownVersion:
		// Be lenient: log and continue. A future schema bump should not
		// brick today's binary on a forward-compatible envelope.
		logMsg("envelope", "%s: envelope version=%d exceeds max known %d — applying best-effort", callerLabel, env.Version, maxKnownVersion)
	}
	return env, nil
}
