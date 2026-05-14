package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Phase 6A v2 — LLM relation classifier.
//
// This pass walks every wiki article that has a non-empty `related:`
// list, batches the wikilinks per source article, and asks the LLM to
// classify each into the closed set of typed relations defined in
// relations.go. High-confidence classifications move from `related:`
// into the typed field; low-confidence and `null` results stay in
// `related:` as before.
//
// Two modes:
//   - default — fully automatic, writes everything the classifier
//     returns above the configured confidence threshold.
//   - --assisted — prompts the user to confirm each candidate before
//     writing.
//
// Audit trail:
//   - wiki/_relations_migration_<ts>.jsonl — one JSONL record per
//     change. `scribe relations migrate-revert <ts>` replays this
//     log in reverse to undo a run.
//   - wiki/_relations_classifier/<source-rel>.json — sidecar caching
//     the classifier's verdict so re-runs don't re-bill the same edges.
//
// Per-article opt-out: `relations_locked: true` in frontmatter skips
// the article entirely.

// classifierFn is the seam tests use to substitute the LLM. The
// production binding is callLLMForRelationsMigrate.
var classifierFn = callLLMForRelationsMigrate

// ClassifierResult is one element of the LLM's JSON array reply,
// after parsing and bounds-checking.
type ClassifierResult struct {
	Target     string  `json:"target"`
	Kind       *string `json:"kind"` // nil = leave in related:
	Confidence string  `json:"confidence"`
	Reasoning  string  `json:"reasoning"`
}

// MigrationLogEntry is one append-only record per change. Sufficient
// to reconstruct what was written and to revert it.
type MigrationLogEntry struct {
	TS         string `json:"ts"`
	Model      string `json:"model"`
	Source     string `json:"source"`     // article rel path
	Target     string `json:"target"`     // bare title
	FromField  string `json:"from_field"` // always "related" in v2
	ToField    string `json:"to_field"`   // typed kind
	Confidence string `json:"confidence"`
	Reasoning  string `json:"reasoning"`
	Reverse    string `json:"reverse,omitempty"` // populated when we also wrote the inverse on target
}

// classifierSidecar lives at wiki/_relations_classifier/<source>.json.
// Records the classifier's verdict on every (source, target) pair so a
// rerun (or `--assisted` after a `--dry-run`) can short-circuit
// already-classified edges instead of re-billing the LLM.
type classifierSidecar struct {
	Source     string             `json:"source"`
	UpdatedAt  string             `json:"updated_at"`
	Classifier string             `json:"classifier"`
	Results    []ClassifierResult `json:"results"`
}

// ---- CLI ----

// RelationsMigrateCmd is a subcommand of RelationsCmd.
type RelationsMigrateCmd struct {
	DryRun    bool   `help:"Show what would change without writing." name:"dry-run"`
	Assisted  bool   `help:"Prompt for confirmation on every classification."`
	Limit     int    `help:"Cap on articles processed per run (cost control)." default:"0"`
	Model     string `help:"Claude model to use." default:"haiku"`
	MinConf   string `help:"Minimum confidence to write ('low' | 'medium' | 'high')." name:"min-confidence" default:"medium" enum:"low,medium,high"`
	Force     bool   `help:"Re-classify articles whose sidecar is already up to date."`
	NoReverse bool   `help:"Skip auto-injecting inverse edges on the target." name:"no-reverse"`
}

func (m *RelationsMigrateCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	logPath := filepath.Join(root, "wiki", fmt.Sprintf("_relations_migration_%s.jsonl", ts))

	candidates, err := collectMigrationCandidates(root, m.Force)
	if err != nil {
		return err
	}
	if m.Limit > 0 && len(candidates) > m.Limit {
		candidates = candidates[:m.Limit]
	}
	if len(candidates) == 0 {
		fmt.Println("(nothing to migrate — every article with related: is either locked or already classified)")
		return nil
	}

	logMsg("relations-migrate", "starting: articles=%d model=%s dry_run=%v assisted=%v",
		len(candidates), m.Model, m.DryRun, m.Assisted)

	confThreshold := confidenceRank(m.MinConf)
	totalEdges := 0
	totalApplied := 0

	for _, c := range candidates {
		fmt.Printf("\n=== %s (%s) ===\n", c.SourceRel, c.SourceType)
		results, used, err := classifyOrLoadSidecar(root, c, m.Model, m.Force)
		if err != nil {
			logMsg("relations-migrate", "error classifying %s: %v", c.SourceRel, err)
			continue
		}
		_ = used
		// Persist sidecar regardless — even null results have audit value.
		if err := writeClassifierSidecar(root, c.SourceRel, results, m.Model); err != nil {
			logMsg("relations-migrate", "warn: sidecar write failed for %s: %v", c.SourceRel, err)
		}

		for _, r := range results {
			totalEdges++
			if r.Kind == nil || *r.Kind == "" {
				fmt.Printf("  - [[%s]] → keep in related: (no clear typed kind)\n", r.Target)
				continue
			}
			kind := RelationKind(*r.Kind)
			if !kindAllowedForType(kind, c.SourceType) {
				fmt.Printf("  - [[%s]] → REJECTED %s (not allowed on type %q)\n", r.Target, *r.Kind, c.SourceType)
				continue
			}
			if confidenceRank(r.Confidence) < confThreshold {
				fmt.Printf("  - [[%s]] → SKIP %s (confidence=%s below %s)\n",
					r.Target, *r.Kind, r.Confidence, m.MinConf)
				continue
			}

			fmt.Printf("  + [[%s]] → %s (confidence=%s) — %s\n", r.Target, *r.Kind, r.Confidence, r.Reasoning)

			if m.Assisted && !confirmEdge(c.SourceRel, r) {
				fmt.Printf("    [skipped by user]\n")
				continue
			}
			if m.DryRun {
				continue
			}

			// Apply the edit.
			sourcePath := filepath.Join(root, c.SourceRel)
			if err := addTypedEdgeToFrontmatter(sourcePath, kind, r.Target); err != nil {
				logMsg("relations-migrate", "warn: add edge failed: %v", err)
				continue
			}
			if err := removeFromRelated(sourcePath, r.Target); err != nil {
				logMsg("relations-migrate", "warn: remove from related failed: %v", err)
			}

			entry := MigrationLogEntry{
				TS:         time.Now().UTC().Format(time.RFC3339),
				Model:      m.Model,
				Source:     c.SourceRel,
				Target:     r.Target,
				FromField:  "related",
				ToField:    string(kind),
				Confidence: r.Confidence,
				Reasoning:  r.Reasoning,
			}

			// Optional reverse edge. Only when the target exists and the
			// kind has a defined inverse in the closed set.
			if !m.NoReverse {
				if inv, ok := inverseOf(kind); ok {
					if targetPath, found := findArticleByTitle(root, r.Target); found {
						if err := addTypedEdgeToFrontmatter(targetPath, inv, c.SourceTitle); err == nil {
							rel, _ := filepath.Rel(root, targetPath)
							entry.Reverse = filepath.ToSlash(rel) + ":" + string(inv)
						}
					}
				}
			}

			if err := appendMigrationLog(logPath, entry); err != nil {
				logMsg("relations-migrate", "warn: append log failed: %v", err)
			}
			totalApplied++
		}
	}

	logMsg("relations-migrate", "done: articles=%d edges=%d applied=%d log=%s",
		len(candidates), totalEdges, totalApplied, filepath.Base(logPath))
	runStats = map[string]any{
		"migrate_articles": len(candidates),
		"migrate_edges":    totalEdges,
		"migrate_applied":  totalApplied,
		"migrate_log":      filepath.Base(logPath),
	}
	return nil
}

// RelationsMigrateRevertCmd undoes a single migration run by replaying
// its audit log in reverse.
type RelationsMigrateRevertCmd struct {
	Log string `arg:"" help:"Migration log file (basename or full path)."`
}

func (r *RelationsMigrateRevertCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	logPath := r.Log
	if !filepath.IsAbs(logPath) && !strings.Contains(logPath, "/") {
		logPath = filepath.Join(root, "wiki", logPath)
	}
	entries, err := readMigrationLog(logPath)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("log %s contains no entries", logPath)
	}
	// Reverse order so the "last write wins" property holds during revert.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		sourcePath := filepath.Join(root, e.Source)
		// Remove the typed edge we wrote.
		if err := removeTypedEdgeFromFrontmatter(sourcePath, RelationKind(e.ToField), e.Target); err != nil {
			logMsg("relations-revert", "warn: remove typed edge: %v", err)
		}
		// Put the wikilink back into related:.
		if err := addTypedEdgeToFrontmatter(sourcePath, RelationKind("related"), e.Target); err != nil {
			logMsg("relations-revert", "warn: restore related: %v", err)
		}
		// Undo the reverse edge if we wrote one.
		if e.Reverse != "" {
			parts := strings.SplitN(e.Reverse, ":", 2)
			if len(parts) == 2 {
				targetPath := filepath.Join(root, parts[0])
				if err := removeTypedEdgeFromFrontmatter(targetPath, RelationKind(parts[1]), titleFromSourcePath(root, e.Source)); err != nil {
					logMsg("relations-revert", "warn: remove reverse: %v", err)
				}
			}
		}
	}
	logMsg("relations-revert", "reverted %d entries from %s", len(entries), filepath.Base(logPath))
	return nil
}

// ---- candidate collection ----

type migrationCandidate struct {
	SourceRel   string
	SourcePath  string
	SourceTitle string
	SourceType  string
	SourceBody  string // first ~600 words
	Targets     []string
}

// collectMigrationCandidates walks every wiki article and returns
// those that (a) have at least one `related:` entry, (b) are not
// `relations_locked: true`, and (c) have either no sidecar yet or
// the caller passed --force.
func collectMigrationCandidates(root string, force bool) ([]migrationCandidate, error) {
	var out []migrationCandidate
	err := walkArticles(root, func(path string, content []byte) error {
		fm, err := parseFrontmatter(content)
		if err != nil || fm == nil {
			return nil //nolint:nilerr // skip unparseable frontmatter
		}
		if fm.RelationsLocked {
			return nil
		}
		targets := extractWikilinkList(fm.Related)
		if len(targets) == 0 {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if !force && hasFreshSidecar(root, rel, targets) {
			return nil
		}
		out = append(out, migrationCandidate{
			SourceRel:   rel,
			SourcePath:  path,
			SourceTitle: fm.Title,
			SourceType:  fm.Type,
			SourceBody:  bodyExcerpt(content, 600),
			Targets:     targets,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourceRel < out[j].SourceRel })
	return out, nil
}

// extractWikilinkList accepts the YAML-typed value of a related-style
// field and returns the bare titles. Tolerates string, []string, and
// []any shapes.
func extractWikilinkList(v any) []string {
	if v == nil {
		return nil
	}
	var raw []string
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok {
				raw = append(raw, s)
			}
		}
	case []string:
		raw = append(raw, x...)
	case string:
		raw = append(raw, x)
	}
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, r := range raw {
		t := strings.TrimSpace(r)
		t = strings.TrimPrefix(t, "[[")
		t = strings.TrimSuffix(t, "]]")
		if i := strings.Index(t, "|"); i >= 0 {
			t = t[:i]
		}
		t = strings.TrimSpace(t)
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// bodyExcerpt returns the first n words of the article body (post-frontmatter).
func bodyExcerpt(content []byte, n int) string {
	body := stripFrontmatter(string(content))
	fields := strings.Fields(body)
	if len(fields) > n {
		fields = fields[:n]
	}
	return strings.Join(fields, " ")
}

// ---- sidecar ----

func classifierSidecarDir(root string) string {
	return filepath.Join(root, "wiki", "_relations_classifier")
}

func classifierSidecarPath(root, sourceRel string) string {
	flat := strings.ReplaceAll(sourceRel, "/", "__")
	flat = strings.TrimSuffix(flat, ".md") + ".json"
	return filepath.Join(classifierSidecarDir(root), flat)
}

// hasFreshSidecar returns true when a sidecar already exists and
// covers every target the caller is about to classify. Checks
// presence only, not content equality — partial coverage triggers a
// re-classification.
func hasFreshSidecar(root, sourceRel string, targets []string) bool {
	path := classifierSidecarPath(root, sourceRel)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var sc classifierSidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		return false
	}
	covered := map[string]bool{}
	for _, r := range sc.Results {
		covered[r.Target] = true
	}
	for _, t := range targets {
		if !covered[t] {
			return false
		}
	}
	return true
}

func writeClassifierSidecar(root, sourceRel string, results []ClassifierResult, model string) error {
	if err := os.MkdirAll(classifierSidecarDir(root), 0o755); err != nil {
		return err
	}
	sc := classifierSidecar{
		Source:     sourceRel,
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
		Classifier: model,
		Results:    results,
	}
	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(classifierSidecarPath(root, sourceRel), data, 0o644)
}

// classifyOrLoadSidecar returns the classifier's verdict on a source
// article. Reads from the sidecar when available and not forcing;
// otherwise calls the LLM. Returns (results, fromSidecar, err).
func classifyOrLoadSidecar(root string, c migrationCandidate, model string, force bool) ([]ClassifierResult, bool, error) {
	if !force {
		if data, err := os.ReadFile(classifierSidecarPath(root, c.SourceRel)); err == nil {
			var sc classifierSidecar
			if err := json.Unmarshal(data, &sc); err == nil {
				covered := map[string]ClassifierResult{}
				for _, r := range sc.Results {
					covered[r.Target] = r
				}
				ok := true
				out := make([]ClassifierResult, 0, len(c.Targets))
				for _, t := range c.Targets {
					r, found := covered[t]
					if !found {
						ok = false
						break
					}
					out = append(out, r)
				}
				if ok {
					return out, true, nil
				}
			}
		}
	}
	results, err := classifierFn(c, model)
	if err != nil {
		return nil, false, err
	}
	return results, false, nil
}

// ---- LLM call ----

// callLLMForRelationsMigrate dispatches the classifier prompt through
// llmProviderGenerator instead of `claude -p`. Phase 4A.4 port — the
// underlying prompt is pure text-in / JSON-out (no tool use), so any
// provider that can produce one JSON document handles it. The provider
// + model + ollama URL come from the per-op RelationsConfig (or, when
// that's empty, the top-level LLMConfig — see config.go).
func callLLMForRelationsMigrate(c migrationCandidate, model string) ([]ClassifierResult, error) {
	tpl, err := promptFS.ReadFile("prompts/relations-migrate.md")
	if err != nil {
		return nil, fmt.Errorf("load prompt: %w", err)
	}
	allowed := allowedRelationsByType[c.SourceType]
	allowedNames := make([]string, len(allowed))
	for i, k := range allowed {
		allowedNames[i] = string(k)
	}

	// Each candidate is a numbered block with title only — body
	// excerpt for the target requires reading every related article,
	// which would multiply token cost. Title alone forces the
	// classifier to lean on the *source's* description of the link,
	// which is exactly the signal we want.
	var cb strings.Builder
	for i, t := range c.Targets {
		fmt.Fprintf(&cb, "%d. [[%s]]\n", i+1, t)
	}

	prompt := string(tpl)
	prompt = strings.ReplaceAll(prompt, "{{SOURCE_TITLE}}", c.SourceTitle)
	prompt = strings.ReplaceAll(prompt, "{{SOURCE_TYPE}}", c.SourceType)
	prompt = strings.ReplaceAll(prompt, "{{ALLOWED_KINDS}}", strings.Join(allowedNames, ", "))
	prompt = strings.ReplaceAll(prompt, "{{SOURCE_BODY}}", c.SourceBody)
	prompt = strings.ReplaceAll(prompt, "{{CANDIDATES}}", cb.String())

	root := mustKBRoot()
	provider, providerModel, ollamaURL := relationsProviderModel(root, model)
	ctx, cancel := context.WithTimeout(withOpLabel(context.Background(), "relations-migrate"), 90*time.Second)
	defer cancel()
	gen := newLLMProvider(provider, providerModel, ollamaURL, root)
	out, err := generateMaybeJSON(ctx, gen, prompt)
	if err != nil {
		return nil, err
	}
	jsonText := extractJSONResult(out)
	var results []ClassifierResult
	if err := json.Unmarshal([]byte(jsonText), &results); err != nil {
		return nil, fmt.Errorf("classifier produced invalid JSON: %w (got %q)", err, truncate(jsonText, 200))
	}
	// Normalize: drop empty kinds, lowercase confidence, trim reasoning.
	for i := range results {
		results[i].Confidence = strings.ToLower(strings.TrimSpace(results[i].Confidence))
		results[i].Reasoning = strings.TrimSpace(results[i].Reasoning)
		if results[i].Kind != nil {
			k := strings.TrimSpace(*results[i].Kind)
			if k == "" || k == "null" {
				results[i].Kind = nil
			} else {
				results[i].Kind = &k
			}
		}
	}
	return results, nil
}

// extractJSONResult pulls the JSON array from claude's text output.
// The `claude -p --output-format json` envelope wraps the model's
// reply in a "result" string field; grab that, then strip any prose
// before/after the bracket pair.
func extractJSONResult(out string) string {
	// Try envelope first.
	var env struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &env); err == nil && env.Result != "" {
		out = env.Result
	}
	start := strings.Index(out, "[")
	end := strings.LastIndex(out, "]")
	if start < 0 || end <= start {
		return "[]"
	}
	return out[start : end+1]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---- helpers ----

func confidenceRank(conf string) int {
	switch strings.ToLower(strings.TrimSpace(conf)) {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

func kindAllowedForType(k RelationKind, articleType string) bool {
	return slices.Contains(allowedRelationsByType[articleType], k)
}

// removeFromRelated drops a wikilink target from the untyped `related:`
// list. RelationKind is just a typed string so we can reuse the
// existing remover with the literal "related" key.
func removeFromRelated(path, target string) error {
	return removeTypedEdgeFromFrontmatter(path, RelationKind("related"), target)
}

// findArticleByTitle scans wiki dirs for a file whose frontmatter
// title matches `title`. Returns ("", false) when not found.
func findArticleByTitle(root, title string) (string, bool) {
	var found string
	_ = walkArticles(root, func(path string, content []byte) error {
		fm, err := parseFrontmatter(content)
		if err != nil {
			return nil //nolint:nilerr
		}
		if fm.Title == title {
			found = path
			return fmt.Errorf("__found__") // sentinel break
		}
		return nil
	})
	if found != "" {
		return found, true
	}
	return "", false
}

// titleFromSourcePath reads the title field from a known source rel
// path. Used by revert to pass the right wikilink for inverse cleanup.
func titleFromSourcePath(root, sourceRel string) string {
	data, err := os.ReadFile(filepath.Join(root, sourceRel))
	if err != nil {
		return ""
	}
	fm, err := parseFrontmatter(data)
	if err != nil || fm == nil {
		return ""
	}
	return fm.Title
}

// confirmEdge prompts the user to accept/reject one classification.
// Returns true on yes (default), false on no. Non-tty falls through
// to true so non-interactive `--assisted` runs match auto behavior.
func confirmEdge(source string, r ClassifierResult) bool {
	if !isTerminal() {
		return true
	}
	prompt := fmt.Sprintf("    apply [[%s]] → %s on %s? [Y/n] ", r.Target, *r.Kind, source)
	fmt.Print(prompt)
	var line string
	if _, err := fmt.Scanln(&line); err != nil {
		// Empty line / EOF treats as accept.
		return true
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line != "n" && line != "no"
}

// isTerminal returns true when stdin is attached to a TTY.
func isTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// mustKBRoot is a panicking version of kbDir for use deep inside
// classifierFn. We've already confirmed KB resolution before reaching
// the LLM call site (the migrate Run uses kbDir up top).
func mustKBRoot() string {
	root, err := kbDir()
	if err != nil {
		panic(err)
	}
	return root
}

// ---- migration log ----

func appendMigrationLog(path string, e MigrationLogEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func readMigrationLog(path string) ([]MigrationLogEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []MigrationLogEntry
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e MigrationLogEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}
