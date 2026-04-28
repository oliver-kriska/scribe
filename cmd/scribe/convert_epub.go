package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"strings"

	htmltomd "github.com/JohannesKaufmann/html-to-markdown/v2"
)

// convertEPUBTier0 walks an EPUB's spine and converts each chapter
// from XHTML to markdown via JohannesKaufmann/html-to-markdown.
//
// EPUB layout:
//
//	META-INF/container.xml         → points to the OPF file
//	<opf>                          → manifest (id → href) + spine (idref order)
//	<chapters>.xhtml               → the actual prose
//
// We emit chapters in spine order separated by a horizontal rule so
// the absorb prompt can detect chapter boundaries. Skip: TOC nav,
// images (Phase 2 sidecar), fonts/CSS, third-party reader metadata.
func convertEPUBTier0(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("open epub: %w", err)
	}

	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}

	containerFile, ok := files["META-INF/container.xml"]
	if !ok {
		return "", fmt.Errorf("epub missing META-INF/container.xml (not a valid EPUB?)")
	}
	opfPath, err := readOPFPath(containerFile)
	if err != nil {
		return "", err
	}
	opfFile, ok := files[opfPath]
	if !ok {
		return "", fmt.Errorf("epub OPF file %q not found in archive", opfPath)
	}

	manifest, spine, err := readOPF(opfFile)
	if err != nil {
		return "", err
	}

	opfDir := path.Dir(opfPath)
	var sb strings.Builder
	for i, idref := range spine {
		href, ok := manifest[idref]
		if !ok {
			continue
		}
		fullPath := path.Join(opfDir, href)
		f, ok := files[fullPath]
		if !ok {
			continue
		}
		md, err := chapterToMarkdown(f)
		if err != nil {
			// Soft-skip a broken chapter; never fail the whole book.
			continue
		}
		md = strings.TrimSpace(md)
		if md == "" {
			continue
		}
		if i > 0 && sb.Len() > 0 {
			sb.WriteString("\n\n---\n\n")
		}
		sb.WriteString(md)
	}

	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", fmt.Errorf("no extractable text in epub (image-only book?)")
	}
	return out, nil
}

// readOPFPath reads container.xml and returns the path of the OPF file
// (typically "OEBPS/content.opf" or "content.opf"). EPUB allows
// multiple rootfiles; we pick the first.
func readOPFPath(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", fmt.Errorf("open container.xml: %w", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return "", fmt.Errorf("read container.xml: %w", err)
	}
	type rootfile struct {
		FullPath string `xml:"full-path,attr"`
	}
	type container struct {
		Rootfiles struct {
			Rootfile []rootfile `xml:"rootfile"`
		} `xml:"rootfiles"`
	}
	var c container
	if err := xml.Unmarshal(data, &c); err != nil {
		return "", fmt.Errorf("parse container.xml: %w", err)
	}
	if len(c.Rootfiles.Rootfile) == 0 || c.Rootfiles.Rootfile[0].FullPath == "" {
		return "", fmt.Errorf("container.xml has no rootfile")
	}
	return c.Rootfiles.Rootfile[0].FullPath, nil
}

// readOPF parses the package OPF file and returns:
//   - manifest: id → href map
//   - spine:    list of idrefs in reading order
func readOPF(f *zip.File) (map[string]string, []string, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, nil, fmt.Errorf("open opf: %w", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, nil, fmt.Errorf("read opf: %w", err)
	}
	type item struct {
		ID   string `xml:"id,attr"`
		Href string `xml:"href,attr"`
	}
	type itemref struct {
		IDRef string `xml:"idref,attr"`
	}
	type opfPkg struct {
		Manifest struct {
			Items []item `xml:"item"`
		} `xml:"manifest"`
		Spine struct {
			Items []itemref `xml:"itemref"`
		} `xml:"spine"`
	}
	var p opfPkg
	if err := xml.Unmarshal(data, &p); err != nil {
		return nil, nil, fmt.Errorf("parse opf: %w", err)
	}
	manifest := make(map[string]string, len(p.Manifest.Items))
	for _, it := range p.Manifest.Items {
		manifest[it.ID] = it.Href
	}
	spine := make([]string, 0, len(p.Spine.Items))
	for _, ref := range p.Spine.Items {
		spine = append(spine, ref.IDRef)
	}
	return manifest, spine, nil
}

func chapterToMarkdown(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	md, err := htmltomd.ConvertReader(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	return string(md), nil
}
