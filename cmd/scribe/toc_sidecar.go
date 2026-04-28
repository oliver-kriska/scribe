package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// TOCSidecar is the schema we write to raw/articles/<slug>.toc.json
// when a long PDF gets ingested through marker. Its job is to give
// the absorb chunker (Phase 3A.5) deterministic semantic boundaries:
// "split this 73-page paper here, here, and here." qmd indexes the
// full markdown; the wiki absorb pass produces summaries; this
// sidecar tells those steps where the chapter boundaries are.
//
// Compatibility: the sidecar is purely additive. Articles without
// one fall back to the existing absorb behavior. Old articles get
// no sidecar and behave as today. Newly ingested long PDFs gain it.
type TOCSidecar struct {
	Version     int            `json:"version"`
	SourcePDF   string         `json:"source_pdf,omitempty"` // basename of original PDF
	GeneratedAt string         `json:"generated_at"`
	Pages       int            `json:"pages,omitempty"`
	Chapters    []ChapterEntry `json:"chapters"`
}

// tocSidecarVersion bumps when the schema changes incompatibly. The
// chunker treats unknown versions as "no sidecar" rather than risk
// a wrong chunk on a schema it doesn't understand.
const tocSidecarVersion = 1

// writeTOCSidecar persists the marker-derived chapter list next to
// the raw article when the document is long enough to benefit. Skips
// quietly when:
//   - stats is nil (HTML/text source, or marker didn't run)
//   - chapters list is empty (flat doc with no detected outline)
//   - the article doesn't exist on disk (caller wrote nothing)
//
// computeBodyOffsets walks the markdown body to attach byte offsets
// to each chapter heading, so the chunker can splice the article
// without re-parsing the markdown later. Best-effort — when a
// chapter title isn't found in the body (marker outline references
// a heading that didn't survive markdown conversion), the offset
// stays zero and the chunker falls through to the next strategy.
func writeTOCSidecar(rawArticlePath, sourceBasename string, stats *MarkerStats) error {
	if stats == nil || len(stats.Chapters) == 0 {
		return nil
	}
	body, err := readArticleBody(rawArticlePath)
	if err != nil {
		// Caller hasn't written the article yet, or read failed —
		// either way, don't block ingestion on a sidecar miss.
		return nil //nolint:nilerr // intentional: sidecar is best-effort
	}

	chapters := append([]ChapterEntry(nil), stats.Chapters...)
	computeBodyOffsets(body, chapters)

	sidecarPath := tocSidecarPath(rawArticlePath)
	payload := TOCSidecar{
		Version:     tocSidecarVersion,
		SourcePDF:   sourceBasename,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Pages:       stats.Pages,
		Chapters:    chapters,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal toc sidecar: %w", err)
	}
	if err := os.WriteFile(sidecarPath, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", sidecarPath, err)
	}
	return nil
}

// tocSidecarPath returns the canonical sidecar path for an article.
// raw/articles/2026-04-28-foo.md → raw/articles/2026-04-28-foo.toc.json
// Kept as a helper so tests + future readers don't have to repeat
// the suffix-swap logic.
func tocSidecarPath(rawArticlePath string) string {
	stem := strings.TrimSuffix(rawArticlePath, ".md")
	return stem + ".toc.json"
}

// readTOCSidecar loads a sidecar by article path. Returns nil + nil
// when no sidecar exists (the common case for short docs and pre-
// Phase-3A articles). Returns nil on schema-version mismatch — see
// tocSidecarVersion comment.
func readTOCSidecar(rawArticlePath string) (*TOCSidecar, error) {
	data, err := os.ReadFile(tocSidecarPath(rawArticlePath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sc TOCSidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		return nil, fmt.Errorf("parse toc sidecar: %w", err)
	}
	if sc.Version != tocSidecarVersion {
		return nil, nil
	}
	return &sc, nil
}

// readArticleBody returns the markdown body of a raw article, with
// the YAML frontmatter stripped. The chunker operates on body bytes,
// not the frontmatter — splicing the latter would corrupt the article
// when the chunks get reassembled or referenced.
func readArticleBody(rawArticlePath string) (string, error) {
	data, err := os.ReadFile(rawArticlePath)
	if err != nil {
		return "", err
	}
	return stripFrontmatter(string(data)), nil
}

// computeBodyOffsets walks the markdown body and locates each chapter
// title to assign BodyOffset (and BodyLength = next-offset - this-offset
// for all but the last chapter; the last gets the remaining bytes).
// In-place mutation: callers pass a slice they own.
//
// Search strategy is deliberately simple: case-sensitive substring
// match on the title, advancing the cursor so we don't re-find an
// earlier chapter when titles repeat. If a title can't be located
// (marker outline references something the markdown doesn't carry
// verbatim — e.g. heading numbering differs), the offset stays zero
// and the chunker treats that chapter as "skip — fall through to
// the heading or token chunker for this doc."
func computeBodyOffsets(body string, chapters []ChapterEntry) {
	cursor := 0
	for i := range chapters {
		title := chapters[i].Title
		if title == "" {
			continue
		}
		idx := indexFromCursor(body, title, cursor)
		if idx < 0 {
			continue
		}
		chapters[i].BodyOffset = idx
		// Length filled in on the second pass below — easier than
		// peeking ahead to find the next match while the loop runs.
		cursor = idx + len(title)
	}
	// Second pass: BodyLength is the gap to the next chapter that
	// successfully resolved its offset. Last chapter eats the rest.
	for i := range chapters {
		if chapters[i].BodyOffset == 0 && i > 0 {
			continue
		}
		next := -1
		for j := i + 1; j < len(chapters); j++ {
			if chapters[j].BodyOffset > chapters[i].BodyOffset {
				next = chapters[j].BodyOffset
				break
			}
		}
		if next < 0 {
			chapters[i].BodyLength = len(body) - chapters[i].BodyOffset
		} else {
			chapters[i].BodyLength = next - chapters[i].BodyOffset
		}
	}
}

// indexFromCursor is strings.Index restricted to s[from:]. Returns
// the absolute index in s on hit, -1 on miss.
func indexFromCursor(s, sub string, from int) int {
	if from >= len(s) {
		return -1
	}
	rel := strings.Index(s[from:], sub)
	if rel < 0 {
		return -1
	}
	return from + rel
}

// articleHasTOCSidecar is a cheap existence probe used by the absorb
// dispatcher to decide between chapter-aware and legacy paths
// without paying the JSON parse cost. Wraps the same path computation
// as readTOCSidecar so the two stay in sync.
func articleHasTOCSidecar(rawArticlePath string) bool {
	_, err := os.Stat(tocSidecarPath(rawArticlePath))
	return err == nil
}

// fmtChapterTitle renders a chapter title for log lines. Truncated
// to keep logs readable; the full title is in the sidecar JSON.
func fmtChapterTitle(t string) string {
	t = strings.TrimSpace(t)
	if len(t) > 60 {
		return t[:57] + "..."
	}
	return t
}

// removeTOCSidecar is the inverse of writeTOCSidecar — used during
// quarantine cleanup so a failed re-ingest doesn't leave a stale
// sidecar referencing a deleted article. Best-effort; never errors
// the caller because a missing sidecar is the desired state.
func removeTOCSidecar(rawArticlePath string) {
	_ = os.Remove(tocSidecarPath(rawArticlePath))
}
