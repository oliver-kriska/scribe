package main

import (
	"bytes"
	"fmt"
	"strings"

	htmltomd "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/ledongthuc/pdf"
)

// convertPDFTier0 extracts plain text from a PDF using ledongthuc/pdf
// (pure Go, BSD-3). Quality caveats:
//   - text-only; no OCR (scanned PDFs return empty/garbage)
//   - no table reconstruction (multi-column may interleave)
//   - no equation handling
//
// For digital-text PDFs (most blogs, simple papers, exports from word
// processors) the output is good enough for absorb. For anything more
// complex, the user should install marker-pdf and tier 1 takes over.
//
// Output is a single markdown string with one blank line between pages.
// We don't try to reconstruct headings — there's no reliable signal in
// raw PDF text streams. Absorb's density classifier treats it as prose,
// which is the right default for tier 0.
func convertPDFTier0(data []byte) (string, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("open pdf: %w", err)
	}

	var sb strings.Builder
	totalPages := r.NumPage()
	for pageIdx := 1; pageIdx <= totalPages; pageIdx++ {
		p := r.Page(pageIdx)
		if p.V.IsNull() {
			continue
		}
		text, err := p.GetPlainText(nil)
		if err != nil {
			// Soft-skip unreadable pages. A single bad page should
			// never prevent ingestion of the rest of the document.
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(text)
	}

	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", fmt.Errorf("no extractable text (likely a scanned PDF — install marker-pdf for OCR)")
	}
	return out, nil
}

// pdfPageCount returns the number of pages in a PDF, or 0 on any
// error. Used by smart-routing — a "0" result means "we can't tell,
// fall through to marker" rather than "trust me, it's empty".
func pdfPageCount(data []byte) int {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 0
	}
	return r.NumPage()
}

// convertHTMLTier0 converts HTML to markdown using JohannesKaufmann's
// production-grade library (MIT, 3.6k stars). This replaces the existing
// stripHTML regex chain in absorb.go for the explicit-conversion path.
//
// We keep stripHTML intact for the trafilatura-fetched URL flow because
// trafilatura already produces clean prose. This converter is for
// raw .html files dropped into the inbox where structure (lists,
// blockquotes, code blocks) matters.
func convertHTMLTier0(data []byte) (string, error) {
	md, err := htmltomd.ConvertReader(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("html-to-markdown: %w", err)
	}
	return strings.TrimSpace(string(md)), nil
}
