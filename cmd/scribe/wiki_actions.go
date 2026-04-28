package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
type WikiActionEnvelope struct {
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
	return res, nil
}

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
