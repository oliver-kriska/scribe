package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// convertDOCXTier0 extracts text + minimal structure from a .docx file.
// DOCX is a zip archive; the prose lives at word/document.xml. We walk
// paragraphs and tables, emitting markdown for the elements that
// matter most for absorb (headings, paragraphs, lists, basic emphasis,
// tables). Things deliberately out of scope:
//
//   - embedded images (Phase 2 image sidecar)
//   - comments / track-changes
//   - footnotes / endnotes
//   - exact list numbering (we use "-" / "1." regardless of w:numId)
//   - styled spans beyond bold/italic
//
// For complex DOCX, marker tier 1 is the right answer. This converter
// is the no-Python fallback.
func convertDOCXTier0(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("open docx: %w", err)
	}

	var docXML []byte
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			rc, err := f.Open()
			if err != nil {
				return "", fmt.Errorf("open word/document.xml: %w", err)
			}
			docXML, err = io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return "", fmt.Errorf("read word/document.xml: %w", err)
			}
			break
		}
	}
	if len(docXML) == 0 {
		return "", fmt.Errorf("docx missing word/document.xml (not a Word doc?)")
	}

	return renderDOCX(docXML)
}

// docxPara is the partial OOXML structure we use to decode a single
// paragraph during the token walk. Other DOCX nodes (styles.xml,
// settings.xml, comments, images) are ignored — we only care about
// content nodes the absorb pipeline can use.
type docxPara struct {
	PPr  docxPPr   `xml:"pPr"`
	Runs []docxRun `xml:"r"`
}

type docxPPr struct {
	PStyle docxStyleRef `xml:"pStyle"`
	NumPr  docxNumPr    `xml:"numPr"`
}

type docxStyleRef struct {
	Val string `xml:"val,attr"`
}

type docxNumPr struct {
	NumID docxStyleRef `xml:"numId"`
	ILvl  docxStyleRef `xml:"ilvl"`
}

type docxRun struct {
	RPr  docxRPr   `xml:"rPr"`
	Text string    `xml:"t"`
	Tab  *struct{} `xml:"tab"`
	Br   *struct{} `xml:"br"`
}

type docxRPr struct {
	Bold   *struct{} `xml:"b"`
	Italic *struct{} `xml:"i"`
}

// renderDOCX is split out so tests can feed pre-extracted document.xml
// without re-zipping.
func renderDOCX(docXML []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(docXML))
	var sb strings.Builder

	// We walk the token stream rather than unmarshal-into-struct so we
	// can preserve document order between paragraphs and tables — a
	// struct with []docxBlock interleaves them but xml.Unmarshal
	// flattens runs/cells in a way that loses the row→paragraph nesting
	// we need for tables. The token walk is more verbose but
	// deterministic.
	depth := 0
	inTable := false
	var tableRows [][]string
	var currentRow []string
	var cellSB strings.Builder
	cellDepth := 0

	flushPara := func(p *docxPara) {
		text := renderRuns(p.Runs)
		if strings.TrimSpace(text) == "" {
			if cellDepth == 0 {
				sb.WriteString("\n")
			}
			return
		}
		style := strings.ToLower(p.PPr.PStyle.Val)
		listLevel := strings.TrimSpace(p.PPr.NumPr.NumID.Val)
		switch {
		case strings.HasPrefix(style, "heading1") || style == "title":
			fmt.Fprintf(&sb, "\n# %s\n\n", text)
		case strings.HasPrefix(style, "heading2"):
			fmt.Fprintf(&sb, "\n## %s\n\n", text)
		case strings.HasPrefix(style, "heading3"):
			fmt.Fprintf(&sb, "\n### %s\n\n", text)
		case strings.HasPrefix(style, "heading4"):
			fmt.Fprintf(&sb, "\n#### %s\n\n", text)
		case listLevel != "":
			fmt.Fprintf(&sb, "- %s\n", text)
		default:
			if cellDepth > 0 {
				if cellSB.Len() > 0 {
					cellSB.WriteString(" ")
				}
				cellSB.WriteString(text)
				return
			}
			sb.WriteString(text)
			sb.WriteString("\n\n")
		}
	}

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("xml decode: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch t.Name.Local {
			case "p":
				var p docxPara
				if err := dec.DecodeElement(&p, &t); err != nil {
					continue
				}
				flushPara(&p)
				depth--
			case "tbl":
				inTable = true
				tableRows = nil
			case "tr":
				if inTable {
					currentRow = nil
				}
			case "tc":
				if inTable {
					cellDepth++
					cellSB.Reset()
				}
			}
		case xml.EndElement:
			depth--
			switch t.Name.Local {
			case "tc":
				if inTable && cellDepth > 0 {
					currentRow = append(currentRow, strings.TrimSpace(cellSB.String()))
					cellSB.Reset()
					cellDepth--
				}
			case "tr":
				if inTable && len(currentRow) > 0 {
					tableRows = append(tableRows, currentRow)
				}
			case "tbl":
				if inTable {
					sb.WriteString(renderTable(tableRows))
					sb.WriteString("\n\n")
					inTable = false
					tableRows = nil
				}
			}
		}
	}

	out := strings.TrimSpace(sb.String())
	// Collapse 3+ blank lines to 2 — the heading/paragraph branches
	// each emit their own \n boundaries which can stack up.
	out = collapseBlankLines(out)
	if out == "" {
		return "", fmt.Errorf("no extractable text in docx")
	}
	return out, nil
}

// renderRuns concatenates a paragraph's runs, applying minimal markdown
// emphasis. Whitespace from <w:tab> and <w:br> is preserved.
func renderRuns(runs []docxRun) string {
	var sb strings.Builder
	for _, r := range runs {
		text := r.Text
		if r.Tab != nil {
			text = "\t" + text
		}
		if r.Br != nil {
			text += "  \n"
		}
		if strings.TrimSpace(text) == "" {
			sb.WriteString(text)
			continue
		}
		bold := r.RPr.Bold != nil
		italic := r.RPr.Italic != nil
		switch {
		case bold && italic:
			sb.WriteString("***" + text + "***")
		case bold:
			sb.WriteString("**" + text + "**")
		case italic:
			sb.WriteString("*" + text + "*")
		default:
			sb.WriteString(text)
		}
	}
	return sb.String()
}

// renderTable emits a GFM table when there are at least two rows.
// Single-row "tables" (sometimes used as layout primitives) collapse
// to a paragraph to avoid rendering noise.
func renderTable(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	if len(rows) == 1 {
		return strings.Join(rows[0], " ")
	}
	var sb strings.Builder
	header := rows[0]
	cols := len(header)
	for i := 1; i < len(rows); i++ {
		if len(rows[i]) > cols {
			cols = len(rows[i])
		}
	}
	pad := func(row []string) {
		for len(row) < cols {
			row = append(row, "")
		}
		sb.WriteString("| ")
		for i, cell := range row[:cols] {
			cell = strings.ReplaceAll(cell, "|", "\\|")
			sb.WriteString(cell)
			if i < cols-1 {
				sb.WriteString(" | ")
			}
		}
		sb.WriteString(" |\n")
	}
	pad(header)
	sb.WriteString("|")
	for i := 0; i < cols; i++ {
		sb.WriteString(" --- |")
	}
	sb.WriteString("\n")
	for i := 1; i < len(rows); i++ {
		pad(rows[i])
	}
	return sb.String()
}

func collapseBlankLines(s string) string {
	for strings.Contains(s, "\n\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n\n", "\n\n\n")
	}
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return s
}
