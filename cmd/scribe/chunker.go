package main

import (
	"regexp"
	"strings"
)

// Chunker splits a long markdown body into semantically meaningful
// chunks for chapter-aware absorb. Three strategies in priority order:
//
//  1. TOC sidecar (chunkByTOC) — when marker emitted a table_of_contents
//     and writeTOCSidecar resolved byte offsets. Best fidelity: the
//     chunks are real chapter boundaries from the original PDF outline.
//
//  2. Heading split (chunkByHeadings) — when no TOC is available but
//     the markdown carries `# ` / `## ` headings. Common path for
//     academic papers that didn't generate a clean PDF outline.
//
//  3. Fixed-token windows (chunkByTokens) — fallback for flat docs.
//     Approximates Claude's tokenization at 4 chars/token (close
//     enough for chunking decisions; never used for billing).
//
// Each strategy returns []Chunk with the same shape so the caller
// (Phase 3A.5 absorb-iteration) doesn't care which strategy fired.
// All strategies guarantee:
//   - chunks cover the entire body (concatenating .Body fields
//     reproduces the input modulo whitespace at boundaries)
//   - len(chunks) >= 1 even on empty input (returns [{Title: "(body)"}])
//   - chunks are at most maxBytes long, splitting again with the
//     fixed-token strategy if a chapter overflows
type Chunk struct {
	// Title is the heading or chapter that named this chunk. Empty
	// for fixed-token chunks. Used in absorb prompts so Claude knows
	// which section it's looking at.
	Title string
	// Level is the heading level (1 = h1, 2 = h2, ...). 0 for
	// TOC chapters where marker didn't classify, and for fixed-token
	// chunks.
	Level int
	// Body is the markdown content of this chunk, including its
	// own heading line where applicable (so absorb prompts get
	// real context, not headless snippets).
	Body string
	// SourcePages records the PDF page IDs this chunk spans, when
	// known (TOC chunker only). Empty for heading/token chunkers.
	SourcePages []int
}

// chunkOptions tunes the chunker. MaxBytes caps any single chunk
// before secondary splitting kicks in (the TOC chunker may produce
// a 40 KB chapter that absorb still can't swallow whole — we
// re-chunk that one with chunkByTokens). 24 KB is the empirical
// safe ceiling for a single haiku pass-1 prompt at current model
// limits, leaving room for the prompt template + tools.
type chunkOptions struct {
	MaxBytes int
}

func defaultChunkOptions() chunkOptions {
	return chunkOptions{MaxBytes: 24 * 1024}
}

// ChunkArticle is the canonical entry point. Inspects the article
// for a TOC sidecar, falls through to heading split, falls through
// to fixed-token. Returns the strategy name (for logging) along
// with the chunks so the caller can record which path fired.
//
// rawArticlePath: absolute path to the raw markdown file.
// Returns ("toc"|"headings"|"tokens"|"single", chunks, error).
// Error is reserved for IO failures; an empty body produces a single
// "(body)" chunk and no error.
func ChunkArticle(rawArticlePath string) (string, []Chunk, error) {
	body, err := readArticleBody(rawArticlePath)
	if err != nil {
		return "", nil, err
	}
	opts := defaultChunkOptions()
	if sc, sErr := readTOCSidecar(rawArticlePath); sErr == nil && sc != nil && len(sc.Chapters) > 0 {
		chunks := chunkByTOC(body, sc.Chapters, opts)
		if len(chunks) > 0 {
			return "toc", chunks, nil
		}
	}
	if chunks := chunkByHeadings(body, opts); len(chunks) > 1 {
		return "headings", chunks, nil
	}
	if len(body) <= opts.MaxBytes {
		return "single", []Chunk{{Title: "(body)", Body: body}}, nil
	}
	return "tokens", chunkByTokens(body, opts), nil
}

// chunkByTOC slices the body using the BodyOffset/BodyLength fields
// the sidecar writer computed. Chapters with zero offset (couldn't
// be located in the markdown) get skipped — the chunker downgrades
// to the next strategy when fewer than half the chapters resolved
// to ensure we don't ship a partial chunking.
//
// Per-chunk overflow: if a single chapter exceeds MaxBytes (rare on
// human-written docs, common on machine-generated reports), the
// chunk gets re-split with chunkByTokens and the resulting micro-
// chunks inherit the chapter title with " (part 1)" suffixes.
func chunkByTOC(body string, chapters []ChapterEntry, opts chunkOptions) []Chunk {
	if len(chapters) == 0 {
		return nil
	}
	resolved := 0
	for _, c := range chapters {
		if c.BodyOffset > 0 || (c.BodyOffset == 0 && resolved == 0) {
			resolved++
		}
	}
	if resolved < (len(chapters)+1)/2 {
		// Fewer than half the chapters located in the body —
		// the markdown's heading style probably diverged from
		// marker's outline. Better to fall through to the heading
		// chunker than splice on partial signal.
		return nil
	}

	out := make([]Chunk, 0, len(chapters))
	for i, c := range chapters {
		end := c.BodyOffset + c.BodyLength
		if end > len(body) || c.BodyLength == 0 {
			end = len(body)
		}
		if c.BodyOffset < 0 || c.BodyOffset >= len(body) {
			continue
		}
		section := body[c.BodyOffset:end]
		base := Chunk{
			Title:       c.Title,
			Level:       c.HeadingLvl,
			Body:        section,
			SourcePages: []int{c.PageID},
		}
		if len(section) <= opts.MaxBytes {
			out = append(out, base)
			continue
		}
		// Overflow: split this chapter with the token chunker, then
		// label the parts.
		parts := chunkByTokens(section, opts)
		for j, p := range parts {
			out = append(out, Chunk{
				Title:       fmtPart(c.Title, j+1, len(parts)),
				Level:       c.HeadingLvl,
				Body:        p.Body,
				SourcePages: []int{c.PageID},
			})
		}
		_ = i // unused but kept for clarity; the index doesn't change behavior
	}
	return out
}

// chunkByHeadings is the second-tier strategy: split on `^# ` and
// `^## ` markdown headings. Captures the heading line itself in the
// chunk body so absorb prompts retain context.
//
// Strategy: pre-walk the body to find every heading offset, then
// emit one chunk per heading-to-next-heading span. The pre-amble
// before the first heading gets its own chunk titled "(intro)".
//
// When the resulting chunks would still be too large (very long
// chapter under a single h1), the overflow gets split with
// chunkByTokens, mirroring chunkByTOC's behavior.
var headingRE = regexp.MustCompile(`(?m)^(#{1,3}) +(.+?)\s*$`)

func chunkByHeadings(body string, opts chunkOptions) []Chunk {
	matches := headingRE.FindAllStringSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return nil
	}

	type cut struct {
		start int
		end   int
		level int
		title string
	}
	cuts := make([]cut, 0, len(matches)+1)

	// Intro chunk before the first heading.
	if matches[0][0] > 0 {
		cuts = append(cuts, cut{
			start: 0,
			end:   matches[0][0],
			level: 0,
			title: "(intro)",
		})
	}

	for i, m := range matches {
		start := m[0]
		end := len(body)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		level := m[3] - m[2] // m[2..3] is the # group
		title := body[m[4]:m[5]]
		cuts = append(cuts, cut{
			start: start,
			end:   end,
			level: level,
			title: strings.TrimSpace(title),
		})
	}

	out := make([]Chunk, 0, len(cuts))
	for _, c := range cuts {
		section := body[c.start:c.end]
		if len(section) <= opts.MaxBytes {
			out = append(out, Chunk{
				Title: c.title,
				Level: c.level,
				Body:  section,
			})
			continue
		}
		parts := chunkByTokens(section, opts)
		for j, p := range parts {
			out = append(out, Chunk{
				Title: fmtPart(c.title, j+1, len(parts)),
				Level: c.level,
				Body:  p.Body,
			})
		}
	}
	return out
}

// chunkByTokens is the unconditional fallback. Approximates Claude
// tokens at 4 bytes per token (rough but workable for chunking
// decisions; we never use this for billing). Splits on paragraph
// boundaries when possible, falling back to mid-paragraph splits
// only when a single paragraph exceeds MaxBytes.
//
// Returns chunks titled "(part N/M)" so absorb logs read sensibly.
func chunkByTokens(body string, opts chunkOptions) []Chunk {
	if len(body) == 0 {
		return []Chunk{{Title: "(body)"}}
	}
	if len(body) <= opts.MaxBytes {
		return []Chunk{{Title: "(body)", Body: body}}
	}
	paras := strings.Split(body, "\n\n")
	var chunks []string
	var cur strings.Builder
	for _, p := range paras {
		// If current + p would overflow, flush cur first.
		if cur.Len() > 0 && cur.Len()+len(p)+2 > opts.MaxBytes {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
		// If a single paragraph is already too big, hard-split it on
		// byte boundaries. Keeps a paragraph together when possible
		// but doesn't refuse to chunk on monolithic input.
		if len(p) > opts.MaxBytes {
			if cur.Len() > 0 {
				chunks = append(chunks, cur.String())
				cur.Reset()
			}
			for start := 0; start < len(p); start += opts.MaxBytes {
				end := start + opts.MaxBytes
				if end > len(p) {
					end = len(p)
				}
				chunks = append(chunks, p[start:end])
			}
			continue
		}
		if cur.Len() > 0 {
			cur.WriteString("\n\n")
		}
		cur.WriteString(p)
	}
	if cur.Len() > 0 {
		chunks = append(chunks, cur.String())
	}
	out := make([]Chunk, len(chunks))
	for i, c := range chunks {
		out[i] = Chunk{
			Title: fmtPart("(body)", i+1, len(chunks)),
			Body:  c,
		}
	}
	return out
}

// fmtPart formats a part-of-N suffix for split chapter titles.
// "Chapter 1" + part 2 of 3 → "Chapter 1 (part 2/3)".
func fmtPart(title string, idx, total int) string {
	if total <= 1 {
		return title
	}
	if title == "" {
		return fmtChapterTitle("(part)") + " " + partTag(idx, total)
	}
	return title + " " + partTag(idx, total)
}

func partTag(idx, total int) string {
	return "(part " + itoa(idx) + "/" + itoa(total) + ")"
}

// itoa is a tiny int-to-string helper. We keep it local to avoid
// importing strconv just for this; the chunker is otherwise
// strconv-free.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
