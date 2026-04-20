package main

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// AbsorbCmd is the one-shot "I have a local file, pull it into the KB"
// entry point. Accepts any text-ish format, cleans it up, drops it into
// raw/articles/ with proper frontmatter, and runs contextualize + absorb
// synchronously. Designed for manual use — e.g. the user saves a PDF as
// text, exports a note from another app, or has a chat log they want to
// absorb.
//
// Supported formats:
//   - .md / .markdown — pass through
//   - .txt            — pass through (treated as prose)
//   - .html / .htm    — tag-stripped + entities decoded
//   - anything else   — treated as .txt
//
// The file name is NOT used as the destination filename because raw/articles/
// follows the YYYY-MM-DD-slug.md convention. The title (derived from --title,
// the first H1, or the <title> tag) feeds the slug.
type AbsorbCmd struct {
	File            string   `arg:"" help:"Path to a local file to absorb (md, txt, html, etc)."`
	Title           string   `help:"Override the article title."`
	Tag             []string `help:"Tag to add to frontmatter (repeatable)." short:"t"`
	Domain          string   `help:"Domain tag." default:"general"`
	NoContextualize bool     `help:"Skip the contextualize step before absorb."`
	Model           string   `help:"Override absorb model (default: sync.default_model)."`
	DryRun          bool     `help:"Show what would happen without writing." short:"n"`
}

func (c *AbsorbCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}

	absPath, err := filepath.Abs(c.File)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("stat file: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", absPath)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	ext := strings.ToLower(filepath.Ext(absPath))
	title, body := normalizeForAbsorb(ext, string(data), c.Title)
	if title == "" {
		// Fall back to filename stem.
		title = strings.TrimSuffix(filepath.Base(absPath), ext)
		title = strings.ReplaceAll(title, "-", " ")
		title = strings.ReplaceAll(title, "_", " ")
	}

	// Use file:// URL as source so absorb-log and frontmatter are honest
	// about provenance. The path is elided to basename so the resulting
	// frontmatter doesn't leak the absorbing user's home directory into
	// a KB that may be publicly shared.
	sourceURL := "file:///" + filepath.Base(absPath)

	rawPath, content := buildRawArticle(root, sourceURL, title, body, "local", c.Domain, c.Tag)

	if c.DryRun {
		fmt.Printf("[dry-run] would write: %s\n", rawPath)
		fmt.Printf("  title:  %s\n", title)
		fmt.Printf("  source: %s\n", sourceURL)
		fmt.Printf("  bytes:  %d\n", len(content))
		if !c.NoContextualize {
			fmt.Println("  next:   contextualize + absorb")
		} else {
			fmt.Println("  next:   absorb")
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(rawPath), 0o755); err != nil {
		return fmt.Errorf("mkdir raw dir: %w", err)
	}
	if err := os.WriteFile(rawPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write raw article: %w", err)
	}
	logMsg("absorb", "wrote %s", relPath(root, rawPath))

	// Mirror the ingest --absorb pipeline: contextualize (optional) + absorb.
	cfg := loadConfig(root)
	if !c.NoContextualize {
		cx := cfg.Absorb.Contextualize
		if cx.Enabled != nil && *cx.Enabled {
			logMsg("absorb", "contextualizing %s", filepath.Base(rawPath))
			if err := contextualizeOne(root, rawPath, cx.Model); err != nil {
				logMsg("absorb", "contextualize failed (continuing): %v", err)
			} else {
				markContextualized(root, filepath.Base(rawPath))
			}
		}
	}

	model := c.Model
	if model == "" {
		model = cfg.DefaultModel
	}
	if model == "" {
		model = "sonnet"
	}
	sc := &SyncCmd{Model: model}
	density := readRawDensity(rawPath)
	logMsg("absorb", "absorbing %s (density=%s)", filepath.Base(rawPath), density)

	if density == "dense" {
		err = sc.absorbDenseTwoPass(root, rawPath, filepath.Base(rawPath))
	} else {
		err = sc.absorbSinglePass(root, rawPath)
	}
	if err != nil {
		return fmt.Errorf("absorb: %w", err)
	}

	absorbLogPath := filepath.Join(root, "wiki", "_absorb_log.json")
	absorbLog := loadJSONMap(absorbLogPath)
	absorbLog[filepath.Base(rawPath)] = time.Now().UTC().Format(time.RFC3339)
	if err := saveJSONMap(absorbLogPath, absorbLog); err != nil {
		logMsg("absorb", "warn: could not update _absorb_log.json: %v", err)
	}

	logMsg("absorb", "done: %s absorbed", filepath.Base(rawPath))
	return nil
}

// normalizeForAbsorb converts raw file bytes into a (title, body) pair
// suitable for feeding buildRawArticle. Tag stripping for HTML is
// intentionally simple — trafilatura is URL-oriented and we don't want a
// Python dep for local files. The output feeds the LLM, which tolerates
// lightly-formatted text.
func normalizeForAbsorb(ext, raw, overrideTitle string) (title, body string) {
	switch ext {
	case ".md", ".markdown":
		title = firstMarkdownHeading(raw)
		body = raw
	case ".html", ".htm":
		title = firstHTMLTitle(raw)
		body = stripHTML(raw)
	default:
		// txt and unknowns: treat as prose, try first line as title.
		title = firstNonEmptyLine(raw)
		if len(title) > 120 {
			title = title[:120]
		}
		body = raw
	}

	if overrideTitle != "" {
		title = overrideTitle
	}
	return title, body
}

var htmlTitleRE = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

func firstHTMLTitle(s string) string {
	m := htmlTitleRE.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(m[1]))
}

func firstNonEmptyLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

var (
	htmlScriptRE = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	htmlStyleRE  = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	htmlTagRE    = regexp.MustCompile(`(?s)<[^>]+>`)
	htmlWSRE     = regexp.MustCompile(`[ \t]+`)
	htmlBlankRE  = regexp.MustCompile(`\n{3,}`)
)

// stripHTML removes script/style blocks and every remaining tag, decodes
// entities, and normalizes whitespace. Good enough to feed an LLM; not a
// replacement for trafilatura/readability.
func stripHTML(s string) string {
	s = htmlScriptRE.ReplaceAllString(s, "")
	s = htmlStyleRE.ReplaceAllString(s, "")
	s = htmlTagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	// Normalize whitespace without collapsing paragraph breaks.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = htmlWSRE.ReplaceAllString(s, " ")
	s = htmlBlankRE.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
