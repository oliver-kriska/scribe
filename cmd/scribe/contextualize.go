package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxRawArticleBytesForContextualize caps how much of a raw article is
// fed to the LLM for contextualization. Most articles fit well under this;
// pathologically long ones get truncated — the paragraph we're producing
// only needs the first few thousand chars to identify source + topics.
const maxRawArticleBytesForContextualize = 20_000

// contextualizePaceDelay is the polite pause between successful
// contextualize calls. Anthropic's haiku quota is bursty and a tight
// loop hits 429 within ~15 calls on small accounts; 1.5s lets a single
// sync run drain a larger queue without the rate-limit early-break.
const contextualizePaceDelay = 1500 * time.Millisecond

// ContextualizeCmd inserts an LLM-generated "Retrieval context" paragraph
// between the frontmatter and body of each raw/articles/*.md file. This
// implements Anthropic's Contextual Retrieval pattern (Sep 2024) in a
// pragmatic, one-shot-per-document form: qmd indexes **/*.md, so the
// added paragraph is embedded naturally on the next qmd update+embed
// and lifts retrieval precision for semantic queries targeting the
// article's themes.
//
// Idempotent via wiki/_contextualized_log.json (raw) and
// wiki/_contextualized_wiki_log.json (wiki). Files carrying the marker
// line `<!-- scribe:retrieval-context -->` are also treated as done so
// a hand-edit can mark a file processed.
//
// Scope options: "raw" (default — contextualize raw/articles/*.md),
// "wiki" (contextualize curated wiki pages under wikiDirs), "all" (both).
// Wiki pages are usually shorter and already high-density, but giving
// them a retrieval-context paragraph still lifts qmd recall when the
// first heading doesn't spell out the subject (e.g. decision logs that
// refer to entities implicitly).
type ContextualizeCmd struct {
	Scope  string `help:"What to contextualize: raw | wiki | all." default:"raw" enum:"raw,wiki,all"`
	Limit  int    `help:"Process at most N articles (0 = no limit)." default:"0"`
	Model  string `help:"Model to use for context generation." default:"haiku"`
	DryRun bool   `help:"Show what would be processed without writing." short:"n"`
	Force  bool   `help:"Re-contextualize even if already done." short:"f"`
}

const retrievalContextMarker = "<!-- scribe:retrieval-context -->"

func (c *ContextualizeCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	if err := loadConfig(root).requireParseable(); err != nil {
		return err
	}
	switch c.Scope {
	case "raw":
		return contextualizeRawArticles(root, c.Limit, c.Model, c.DryRun, c.Force)
	case "wiki":
		return contextualizeWikiArticles(root, c.Limit, c.Model, c.DryRun, c.Force)
	case "all":
		if err := contextualizeRawArticles(root, c.Limit, c.Model, c.DryRun, c.Force); err != nil {
			return err
		}
		return contextualizeWikiArticles(root, c.Limit, c.Model, c.DryRun, c.Force)
	default:
		return fmt.Errorf("invalid --scope %q (expected raw|wiki|all)", c.Scope)
	}
}

// contextualizeRawArticles is the implementation shared by the standalone
// command and the sync pipeline.
func contextualizeRawArticles(root string, limit int, model string, dryRun, force bool) error {
	rawDir := filepath.Join(root, "raw", "articles")
	if !dirExists(rawDir) {
		return nil
	}

	logPath := filepath.Join(root, "wiki", "_contextualized_log.json")
	logMap := loadJSONMap(logPath)

	entries, err := os.ReadDir(rawDir)
	if err != nil {
		return fmt.Errorf("read raw/articles: %w", err)
	}

	processed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if limit > 0 && processed >= limit {
			break
		}

		path := filepath.Join(rawDir, e.Name())

		if !force {
			// The in-file marker is the source of truth: skip only if the
			// article currently carries it. The filename log is NOT
			// authoritative — a logged article whose content was later
			// rewritten (a re-collect that clobbered enrichment, or a stub
			// upgraded to its real fetched content) lost its marker, and
			// gating on the stale log entry would suppress re-enrichment
			// forever. contextualizeOne only logs after inserting the marker
			// and never logs on failure, so "logged but no marker" always
			// means the file was rewritten — never an intentional skip.
			if fileHasMarker(path, retrievalContextMarker) {
				logMap[e.Name()] = time.Now().UTC().Format(time.RFC3339)
				continue
			}
		}

		if dryRun {
			logMsg("contextualize", "would process %s", e.Name())
			processed++
			continue
		}

		if err := contextualizeOne(root, path, model); err != nil {
			if errors.Is(err, ErrRateLimit) {
				logMsg("contextualize", "rate limited — will resume next run")
				break
			}
			logMsg("contextualize", "failed %s: %v", e.Name(), err)
			continue
		}

		logMap[e.Name()] = time.Now().UTC().Format(time.RFC3339)
		if err := saveJSONMap(logPath, logMap); err != nil {
			logMsg("contextualize", "warn: could not persist log: %v", err)
		}
		processed++

		// Polite pacing — Anthropic's haiku quota is bursty and a tight
		// loop hits 429 within ~15 calls on small accounts. 1.5s between
		// successful calls keeps a single sync run flowing without the
		// rate-limit early-break that strands the rest of the queue.
		time.Sleep(contextualizePaceDelay)
	}

	// Flush final state even if nothing new processed (picks up marker-only
	// retrofits).
	if !dryRun {
		if err := saveJSONMap(logPath, logMap); err != nil {
			logMsg("contextualize", "warn: could not persist log: %v", err)
		}
	}
	if processed > 0 {
		verb := "contextualized"
		if dryRun {
			verb = "would contextualize"
		}
		logMsg("contextualize", "%s %d raw article(s)", verb, processed)
	}
	return nil
}

// contextualizeWikiArticles walks curated wiki dirs (decisions/, solutions/,
// patterns/, tools/, projects/, etc.) and inserts a retrieval-context
// paragraph into each article that does not yet carry the marker. Uses a
// separate log file from raw contextualization because the keyspace differs
// (wiki keys are relative-from-root paths; raw keys are basenames).
func contextualizeWikiArticles(root string, limit int, model string, dryRun, force bool) error {
	logPath := filepath.Join(root, "wiki", "_contextualized_wiki_log.json")
	logMap := loadJSONMap(logPath)

	var candidates []string
	_ = walkArticles(root, func(path string, _ []byte) error {
		candidates = append(candidates, path)
		return nil
	})

	processed := 0
	for _, path := range candidates {
		if limit > 0 && processed >= limit {
			break
		}
		relKey := relPath(root, path)

		if !force {
			// Marker is the source of truth — see contextualizeRawArticles.
			if fileHasMarker(path, retrievalContextMarker) {
				logMap[relKey] = time.Now().UTC().Format(time.RFC3339)
				continue
			}
		}

		if dryRun {
			logMsg("contextualize", "would process %s", relKey)
			processed++
			continue
		}

		if err := contextualizeOne(root, path, model); err != nil {
			if errors.Is(err, ErrRateLimit) {
				logMsg("contextualize", "rate limited — will resume next run")
				break
			}
			logMsg("contextualize", "failed %s: %v", relKey, err)
			continue
		}

		logMap[relKey] = time.Now().UTC().Format(time.RFC3339)
		if err := saveJSONMap(logPath, logMap); err != nil {
			logMsg("contextualize", "warn: could not persist log: %v", err)
		}
		processed++
		time.Sleep(contextualizePaceDelay)
	}

	if !dryRun {
		if err := saveJSONMap(logPath, logMap); err != nil {
			logMsg("contextualize", "warn: could not persist log: %v", err)
		}
	}
	if processed > 0 {
		verb := "contextualized"
		if dryRun {
			verb = "would contextualize"
		}
		logMsg("contextualize", "%s %d wiki article(s)", verb, processed)
	}
	return nil
}

// contextualizeOne generates a retrieval-context paragraph for one raw
// article and inserts it between the frontmatter and body. Uses the LLM
// provider configured under absorb.contextualize.provider (anthropic via
// claude -p, or ollama via local HTTP). Safe to call multiple times
// (second call is a no-op because the marker is present). Timeout comes
// from AbsorbConfig.Contextualize.TimeoutSec via scribe.yaml.
//
// Unlike the previous implementation, the raw article content is passed
// INLINE in the prompt rather than read via tool use — which means the
// local (ollama) provider works the same way as anthropic.
func contextualizeOne(root, rawPath, model string) error {
	cfg := loadConfig(root)
	stagingDir := filepath.Join(root, "output", "contextualize")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return fmt.Errorf("mkdir staging: %w", err)
	}
	base := filepath.Base(rawPath)
	ctxFile := filepath.Join(stagingDir, strings.TrimSuffix(base, ".md")+".txt")

	// Load raw article inline (truncated if pathologically long).
	articleBytes, err := os.ReadFile(rawPath)
	if err != nil {
		return fmt.Errorf("read raw: %w", err)
	}
	// Strip the YAML frontmatter before sending: the body is what we
	// summarize, and the frontmatter carries an ingest `captured:` date
	// that small models otherwise grab as the publication date — the
	// 2026-06-03 audit caught a study dated "June 2026" that was really
	// the capture date. The authoritative source + publication date go
	// through SOURCE_META instead, labeled and unambiguous.
	articleContent := stripFrontmatter(string(articleBytes))
	if len(articleContent) > maxRawArticleBytesForContextualize {
		articleContent = articleContent[:maxRawArticleBytesForContextualize] + "\n\n[…article truncated for contextualization]"
	}

	// Feed the known source into the prompt as an authoritative field so the
	// model attributes from fact, not guesswork. Small models otherwise
	// hallucinate the source (or copy the example paragraph's source) — see
	// .claude/research/2026-05-27-local-model-selection-m4-research.md. scribe
	// captures source_url/source_path + title in frontmatter at ingest time.
	sourceMeta := contextualizeSourceMeta(articleBytes)

	prompt, err := loadPrompt("contextualize.md", map[string]string{
		"ARTICLE_CONTENT": articleContent,
		"SOURCE_META":     sourceMeta,
	})
	if err != nil {
		return fmt.Errorf("load prompt: %w", err)
	}

	timeout := time.Duration(cfg.Absorb.Contextualize.TimeoutSec) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	provider := newLLMProvider(cfg.Absorb.Contextualize.Provider, model, cfg.Absorb.Contextualize.OllamaURL, root)
	text, err := provider.Generate(withOpLabel(ctx, "contextualize"), prompt)
	if err != nil {
		return fmt.Errorf("%s: %w", provider.Name(), err)
	}
	text = sanitizeContextParagraph(text)
	if text == "" {
		return fmt.Errorf("empty context paragraph")
	}
	// Reject degenerate output rather than splice it. Small models on
	// table/image-dense articles fall back to echoing the first line (the
	// 2026-06-03 audit caught a breadcrumb "AI Search, Data & Studies"
	// written as the whole summary). Returning an error means the caller
	// skips this file without recording the idempotency marker, so the
	// next run retries instead of persisting garbage into the index.
	if reason := degenerateContextReason(text, articleContent); reason != "" {
		return fmt.Errorf("degenerate context paragraph: %s", reason)
	}

	// Save paragraph to staging for debugging/inspection.
	_ = os.WriteFile(ctxFile, []byte(text+"\n"), 0o644)

	// Splice paragraph into the raw article between frontmatter and body.
	// We re-read from disk rather than reuse articleBytes because the prompt
	// may have been built off a truncated copy.
	rawBytes, err := os.ReadFile(rawPath)
	if err != nil {
		return fmt.Errorf("read raw: %w", err)
	}
	newContent, err := insertRetrievalContext(string(rawBytes), text)
	if err != nil {
		return err
	}
	if err := os.WriteFile(rawPath, []byte(newContent), 0o644); err != nil {
		return fmt.Errorf("write raw: %w", err)
	}

	// Leave the staging text file in place for debugging; the log json
	// is authoritative for idempotency.
	return nil
}

// insertRetrievalContext splices a retrieval-context block between the
// frontmatter and body of a markdown document. Returns the modified content.
// No-op if the marker is already present (caller should check, but we double-
// guard here).
func insertRetrievalContext(content, paragraph string) (string, error) {
	if strings.Contains(content, retrievalContextMarker) {
		return content, nil
	}

	if !strings.HasPrefix(content, "---") {
		// No frontmatter — prepend the block at the top.
		block := retrievalContextBlock(paragraph)
		return block + content, nil
	}

	end := strings.Index(content[3:], "\n---")
	if end < 0 {
		return "", fmt.Errorf("malformed frontmatter (no closing ---)")
	}
	// Absolute offset of the byte after the closing "---".
	absClosing := 3 + end + len("\n---")
	// Skip the newline that immediately follows, if any.
	if absClosing < len(content) && content[absClosing] == '\n' {
		absClosing++
	}
	before := content[:absClosing]
	body := content[absClosing:]
	return before + retrievalContextBlock(paragraph) + body, nil
}

func retrievalContextBlock(paragraph string) string {
	return fmt.Sprintf("\n%s\n> **Retrieval context (auto):** %s\n\n", retrievalContextMarker, paragraph)
}

// contextualizeSourceMeta renders an authoritative source hint from a raw
// article's frontmatter for the contextualize prompt's {{SOURCE_META}} slot.
// scribe records the origin at ingest (source_url for fetched pages,
// source_path for local files) plus the title; passing it verbatim stops
// small models inventing an attribution. Returns "" when no usable metadata
// exists, in which case the prompt instructs the model to infer from the body.
func contextualizeSourceMeta(raw []byte) string {
	fm, err := parseFrontmatterRaw(raw)
	if err != nil || fm == nil {
		return ""
	}
	str := func(k string) string {
		if s, ok := fm[k].(string); ok {
			return strings.TrimSpace(s)
		}
		return ""
	}
	var parts []string
	if title := str("title"); title != "" {
		parts = append(parts, fmt.Sprintf("Title: %q", title))
	}
	src := str("source_url")
	if src == "" {
		src = str("source_path")
	}
	if src != "" {
		parts = append(parts, "Source: "+src)
	}
	// Surface the publication date as authoritative so a date in the
	// paragraph (if the prompt allows one) is the real one, not the
	// ingest `captured:` date. stringFromAny handles both quoted strings
	// and yaml-parsed dates (time.Time).
	pub := stringFromAny(fm["published"])
	if pub == "" {
		pub = stringFromAny(fm["date"])
	}
	if pub != "" {
		parts = append(parts, "Published: "+pub)
	}
	if len(parts) == 0 {
		return ""
	}
	return "Known source (authoritative — use this for attribution and the only date you may cite, do not invent another): " +
		strings.Join(parts, " · ")
}

// sanitizeContextParagraph cleans common LLM output artifacts — wrapping
// quotes, markdown fences, "Here is the paragraph:" preambles, trailing
// newlines — so the block we splice into raw articles is actually a single
// clean paragraph.
func sanitizeContextParagraph(s string) string {
	s = strings.TrimSpace(s)
	// Drop markdown fences if the LLM wrapped output in ```.
	s = strings.TrimPrefix(s, "```markdown")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	// Drop common preamble patterns on the first line.
	for _, pre := range []string{
		"Here is the paragraph:",
		"Here is the retrieval context:",
		"Here's the paragraph:",
		"Retrieval context:",
		"Context:",
		"Paragraph:",
	} {
		if strings.HasPrefix(s, pre) {
			s = strings.TrimSpace(s[len(pre):])
			break
		}
	}
	// Collapse hard newlines inside the paragraph to spaces — the marker
	// block template uses a single line, and double-newlines would split the
	// blockquote when re-embedded.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	// Preserve paragraph breaks (double newlines) by stashing them.
	s = strings.ReplaceAll(s, "\n\n", "§§PARA§§")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "§§PARA§§", "\n\n")
	return strings.TrimSpace(s)
}

// degenerateContextReason returns a non-empty reason when a generated
// context paragraph is too poor to splice into the KB, or "" when it is
// acceptable. It catches the small-model failure modes the 2026-06-03
// audit found: the model echoing a heading/breadcrumb instead of
// summarizing (no prose), or returning a fragment far below the asked
// 60–120 words. The word floor is deliberately well under the target
// (25, not 60) so only genuine degeneracy is rejected, not a slightly
// terse-but-valid paragraph. body is the (frontmatter-stripped) article
// used for verbatim-echo detection.
func degenerateContextReason(text, body string) string {
	words := strings.Fields(text)
	if len(words) < 25 {
		return fmt.Sprintf("too short (%d words, want 60–120)", len(words))
	}
	if !strings.ContainsAny(text, ".!?") {
		return "no sentence punctuation (likely a heading/breadcrumb echo)"
	}
	norm := strings.TrimSpace(strings.ToLower(text))
	for _, line := range strings.Split(body, "\n") {
		l := strings.TrimSpace(strings.ToLower(line))
		if l != "" && l == norm {
			return "verbatim echo of a source line"
		}
	}
	return ""
}

// markerScanBytes bounds how much of a file fileHasMarker reads. The
// retrieval-context marker is spliced immediately after the frontmatter
// (insertRetrievalContext), so it always lives within the first few hundred
// bytes; a 16 KiB ceiling keeps the check cheap now that contextualize scans
// every article each pass (the in-file marker, not the log, is the authority).
const markerScanBytes = 16 << 10

// fileHasMarker reports whether the file at path contains marker. It scans
// only the first markerScanBytes, which is sufficient because the marker is
// always near the top (right after the frontmatter).
func fileHasMarker(path, marker string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, markerScanBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false
	}
	return strings.Contains(string(buf[:n]), marker)
}
