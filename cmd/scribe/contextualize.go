package main

import (
	"context"
	"errors"
	"fmt"
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
			if _, ok := logMap[e.Name()]; ok {
				continue
			}
			// Also skip if the file already has the marker — lets us retrofit
			// hand-edited articles.
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
			if _, ok := logMap[relKey]; ok {
				continue
			}
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
	articleContent := string(articleBytes)
	if len(articleContent) > maxRawArticleBytesForContextualize {
		articleContent = articleContent[:maxRawArticleBytesForContextualize] + "\n\n[…article truncated for contextualization]"
	}

	prompt, err := loadPrompt("contextualize.md", map[string]string{
		"ARTICLE_CONTENT": articleContent,
	})
	if err != nil {
		return fmt.Errorf("load prompt: %w", err)
	}

	timeout := time.Duration(cfg.Absorb.Contextualize.TimeoutSec) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	provider := newLLMProvider(cfg.Absorb.Contextualize.Provider, model, cfg.Absorb.Contextualize.OllamaURL, root)
	text, err := provider.Generate(ctx, prompt)
	if err != nil {
		return fmt.Errorf("%s: %w", provider.Name(), err)
	}
	text = sanitizeContextParagraph(text)
	if text == "" {
		return fmt.Errorf("empty context paragraph")
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

// fileHasMarker returns true if a file contains the given marker.
func fileHasMarker(path, marker string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), marker)
}
