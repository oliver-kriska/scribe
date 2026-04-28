package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Convert is the central dispatch for non-text source files. It picks the
// best available tier for a given extension and returns the file's body as
// markdown plus a suggested title. The caller is responsible for writing
// the result into raw/articles/ via buildRawArticle.
//
// Tier preference: marker (tier 1) → tier 0 Go-native → fail with an
// actionable hint. Phase 1A ships .pdf and .html through tier 0; .docx/
// .epub remain unsupported in tier 0 until Phase 1B (zip+xml parsers) and
// route exclusively through marker today. PPTX/XLSX have no tier 0 path.
//
// Errors carry a hint flag so the caller can surface "install marker-pdf
// to handle this format" cleanly to the user.
type ConvertResult struct {
	Title    string
	Markdown string
	Tier     string // "tier0" or "marker", recorded so the caller can log/test.
}

// ErrConvertUnsupported signals that no tier handles this extension on the
// current system. The wrapped error explains why and what to install.
type ErrConvertUnsupported struct {
	Ext    string
	Reason string
}

func (e *ErrConvertUnsupported) Error() string {
	return fmt.Sprintf("convert: %s not supported (%s)", e.Ext, e.Reason)
}

// markerTierAvailable reports whether marker_single is on PATH. Result is
// not cached because users may install/uninstall during a long-running
// daemon; the lookup is microseconds.
func markerTierAvailable() bool {
	_, err := exec.LookPath("marker_single")
	return err == nil
}

// convertFile is the entry point. ext is lowercase including the leading
// dot (".pdf"). data is the raw file bytes. titleHint is an optional
// override the caller can pass (typically from frontmatter or filename).
func convertFile(path, ext string, data []byte, titleHint string) (*ConvertResult, error) {
	ext = strings.ToLower(ext)

	// Plain-text formats short-circuit straight back — they don't need
	// a converter at all. Caller will normalize via the existing
	// normalizeForAbsorb path. We return nil to signal "not my job".
	switch ext {
	case ".md", ".markdown", ".txt", "":
		return nil, nil
	}

	// HTML always goes through tier 0: JohannesKaufmann/html-to-markdown
	// is purpose-built for HTML, beats marker on structural fidelity,
	// and avoids spinning up marker's 3 GB model load for what is
	// fundamentally a tag-walk problem.
	if ext == ".html" || ext == ".htm" {
		md, err := convertHTMLTier0(data)
		if err != nil {
			return nil, fmt.Errorf("tier0 html: %w", err)
		}
		return &ConvertResult{
			Title:    pickTitle(titleHint, md, path),
			Markdown: md,
			Tier:     "tier0",
		}, nil
	}

	// Tier 1 (marker) is the right tool for document formats: PDF and
	// the Office family. We prefer it when available because of its
	// benchmark lead on tables/equations/layout. Tier 0 only handles
	// PDF (text-only) when marker is absent.
	//
	// Smart routing short-circuits trivial PDFs to tier 0 even when
	// marker is installed — marker cold-loads ~3 GB on every
	// invocation, which is wasted work for a 2-page receipt. Driven
	// by ingest.smart_routing in scribe.yaml.
	//
	// If marker is missing and the user has opted into auto-install
	// (SCRIBE_AUTO_INSTALL_TOOLS=1), lazy-bootstrap. The bootstrap
	// downloads ~3 GB the first time, so we don't fire it unannounced.
	if shouldRouteSmallPDFToTier0(ext, data) {
		md, err := convertPDFTier0(data)
		if err == nil {
			return &ConvertResult{
				Title:    pickTitle(titleHint, md, path),
				Markdown: md,
				Tier:     "tier0-smart",
			}, nil
		}
		// Fall through on tier 0 failure (e.g., scanned PDF) so
		// marker still gets a chance.
	}
	if !markerTierAvailable() {
		_ = lazyBootstrapMarker() // best-effort; ignore error, fall through to tier 0
	}
	if markerTierAvailable() {
		md, err := convertWithMarker(path, ext)
		if err != nil {
			return nil, fmt.Errorf("marker conversion: %w", err)
		}
		return &ConvertResult{
			Title:    pickTitle(titleHint, md, path),
			Markdown: md,
			Tier:     "marker",
		}, nil
	}

	// Tier 0 fallback. Phase 1B adds Go-native DOCX and EPUB.
	switch ext {
	case ".pdf":
		md, err := convertPDFTier0(data)
		if err != nil {
			return nil, fmt.Errorf("tier0 pdf: %w", err)
		}
		return &ConvertResult{
			Title:    pickTitle(titleHint, md, path),
			Markdown: md,
			Tier:     "tier0",
		}, nil
	case ".docx":
		md, err := convertDOCXTier0(data)
		if err != nil {
			return nil, fmt.Errorf("tier0 docx: %w", err)
		}
		return &ConvertResult{
			Title:    pickTitle(titleHint, md, path),
			Markdown: md,
			Tier:     "tier0",
		}, nil
	case ".epub":
		md, err := convertEPUBTier0(data)
		if err != nil {
			return nil, fmt.Errorf("tier0 epub: %w", err)
		}
		return &ConvertResult{
			Title:    pickTitle(titleHint, md, path),
			Markdown: md,
			Tier:     "tier0",
		}, nil
	case ".pptx", ".xlsx":
		return nil, &ErrConvertUnsupported{
			Ext:    ext,
			Reason: "marker_single not on PATH; install marker-pdf (`pipx install marker-pdf`) — tier 0 has no native PPTX/XLSX support",
		}
	default:
		return nil, &ErrConvertUnsupported{
			Ext:    ext,
			Reason: "no converter registered for this extension",
		}
	}
}

// shouldRouteSmallPDFToTier0 returns true when a PDF is small enough
// that marker's cold-load cost outweighs its quality advantage. Reads
// thresholds from scribe.yaml ingest.smart_routing; falls back to
// "no" when no KB is resolvable (tests, edge cases) so the dispatcher
// keeps its existing behavior.
//
// Page-count probe is delegated to ledongthuc/pdf — the same library
// tier 0 PDF conversion uses, so we're not pulling in extra deps.
// We don't wrap it in a top-level pageCount() helper because the
// only reader of page counts is this routing decision.
func shouldRouteSmallPDFToTier0(ext string, data []byte) bool {
	if ext != ".pdf" {
		return false
	}
	cfg := loadIngestConfigBestEffort()
	if cfg == nil || cfg.SmartRouting.Enabled == nil || !*cfg.SmartRouting.Enabled {
		return false
	}
	if cfg.SmartRouting.MaxPDFBytes > 0 && int64(len(data)) > cfg.SmartRouting.MaxPDFBytes {
		return false
	}
	if cfg.SmartRouting.MaxPDFPages > 0 {
		pages := pdfPageCount(data)
		if pages == 0 || pages > cfg.SmartRouting.MaxPDFPages {
			return false
		}
	}
	return true
}

// loadIngestConfigBestEffort returns the resolved IngestConfig for
// the current KB, or nil when called outside one. Never panics.
func loadIngestConfigBestEffort() *IngestConfig {
	root, err := kbDir()
	if err != nil {
		return nil
	}
	cfg := loadConfig(root)
	if cfg == nil {
		return nil
	}
	return &cfg.Ingest
}

// pickTitle returns the first non-empty option among (override hint,
// first real `# heading` in the markdown, filename stem). We require
// a real markdown heading (`^# `) here rather than firstMarkdownHeading's
// "first non-empty line" fallback because the latter would mask the
// filename path entirely — and a clean filename ("some-paper-2026.pdf"
// → "some paper 2026") is usually a better article title than a random
// first-line snippet from a PDF text dump.
func pickTitle(hint, md, path string) string {
	if t := strings.TrimSpace(hint); t != "" {
		return t
	}
	if m := h1RE.FindStringSubmatch(md); len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	stem = strings.ReplaceAll(stem, "-", " ")
	stem = strings.ReplaceAll(stem, "_", " ")
	return strings.TrimSpace(stem)
}
