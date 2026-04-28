package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// absorb_facts.go implements Phase 3B atomic-fact extraction. The
// pass runs after chunking and before pass-1, only when:
//   - cfg.Absorb.AtomicFacts is true (opt-in until validated)
//   - the article qualified for chaptered absorb (TOC sidecar +
//     ChapterThreshold met). Atomic facts on a one-shot pass-1 is
//     a separate problem — too much context for one prompt.
//
// What facts give us
//
//   1. Pass-1 grounding: each chapter's pass-1 prompt gets the
//      chapter's atomic facts as evidence. The model proposes
//      entities anchored in actual claims rather than re-reading
//      the chunk and inventing summaries.
//   2. Queryable artifact at output/facts/<slug>.json. Future
//      consumers (Phase 4 contradiction detector, dream cycle's
//      contradiction resolution) need facts at uniform granularity.
//   3. Verbatim citations. Each fact carries an `anchor` substring
//      from the chunk so downstream readers can locate the source.
//
// What this pass deliberately does NOT do
//
//   - Wire facts into pass-2 (entity-writing) prompts. Pass-2 still
//     consumes the merged plan's `key_claims`. Phase 3B.5 wires
//     facts into pass-2 once Phase 3B validates per-document.
//   - De-duplicate cross-chapter facts. Each chapter's facts stand
//     on their own; a follow-up dedup pass can use embedding
//     similarity (Phase 3C).
//   - Score confidence. The Phase 4 dream cycle does that against
//     the wiki article, not the raw fact list.

// AtomicFact is one row from the facts pass output.
type AtomicFact struct {
	// ID is unique within a chapter (f1, f2, ...). The merge step
	// prefixes with the chapter index so the merged file's IDs are
	// globally unique — c00-f1, c00-f2, c01-f1, ... — without
	// requiring the model to track cross-chapter state.
	ID string `json:"id"`
	// Type is one of definition | claim | numeric | decision | citation.
	// The model picks; downstream consumers can branch on type to
	// (e.g.) prioritize numeric facts in pass-2's key_claims pool.
	Type string `json:"type"`
	// Claim is the single-sentence atomic statement.
	Claim string `json:"claim"`
	// Anchor is a verbatim substring of the chunk that locates the
	// fact for human readers. 4-12 words is the sweet spot per the
	// prompt; we don't enforce that in code.
	Anchor string `json:"anchor"`
	// ChapterTitle is filled in during merge; the per-chunk JSON
	// doesn't carry it (it's in the chapterFacts envelope).
	ChapterTitle string `json:"chapter,omitempty"`
}

// chapterFacts is the JSON envelope absorb-facts.md emits per chunk.
type chapterFacts struct {
	RawFile     string       `json:"raw_file"`
	SourceTitle string       `json:"source_title"`
	Chapter     string       `json:"chapter"`
	Facts       []AtomicFact `json:"facts"`
}

// MergedFacts is what gets written to output/facts/<slug>.json. One
// flat list, ID-prefixed by chapter index, plus chapter metadata so
// readers can group by chapter without re-walking the source.
type MergedFacts struct {
	Version     int                 `json:"version"`
	RawFile     string              `json:"raw_file"`
	SourceTitle string              `json:"source_title"`
	GeneratedAt string              `json:"generated_at"`
	Chapters    []factsChapterEntry `json:"chapters"`
	Facts       []AtomicFact        `json:"facts"`
}

// factsChapterEntry indexes one chapter inside the merged file: the
// title and the inclusive ID range that belongs to it. Readers
// looking for "facts from chapter 3" use this to slice without
// re-parsing every fact.
type factsChapterEntry struct {
	Index   int    `json:"index"`
	Title   string `json:"title"`
	IDStart string `json:"id_start"`
	IDEnd   string `json:"id_end"`
	Count   int    `json:"count"`
}

const factsSchemaVersion = 1

// shouldRunFactsPass mirrors shouldAbsorbChaptered's gate plus the
// AtomicFacts opt-in. Returns false on any disqualifier so the
// caller's branchless code falls through to plain chaptered absorb.
func shouldRunFactsPass(cfg AbsorbConfig) bool {
	if cfg.AtomicFacts == nil || !*cfg.AtomicFacts {
		return false
	}
	if cfg.ChapterAware == nil || !*cfg.ChapterAware {
		// Facts only run inside the chaptered path; no chapters,
		// no facts. (Whole-article facts would just re-implement
		// pass-1 with extra steps.)
		return false
	}
	return true
}

// runFactsPass executes the atomic-fact extraction phase against the
// already-prepared chunk files. It assumes runPass1Chaptered has
// already written the chunk markdown files and we have a chapterRun
// list pointing at them. We do NOT re-emit chunks — the runs slice
// is the contract.
//
// Output paths:
//   - per-chunk JSON: output/absorb-facts/<rawname>/00-<slug>.json
//   - merged JSON:    output/facts/<slug>.json
//
// The merged file is the consumer-facing artifact (stable path,
// queryable from outside scribe). Per-chunk files stick around for
// post-mortem inspection — same disposable-but-debuggable pattern
// as Phase 3A.5's chapter chunks.
//
// Failure tolerance: a chunk that fails facts extraction (timeout,
// bad JSON output, etc.) gets logged and skipped. The merged file
// still gets written with whichever chunks succeeded — partial
// grounding is better than no grounding.
func (s *SyncCmd) runFactsPass(ctx context.Context, root, rawFile, rawName string, runs []chapterRun, cfg AbsorbConfig) (*MergedFacts, error) {
	if len(runs) == 0 {
		return nil, nil
	}

	factsDir := filepath.Join(root, "output", "absorb-facts", strings.TrimSuffix(rawName, ".md"))
	if err := os.MkdirAll(factsDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir facts dir: %w", err)
	}

	tools := []string{"Read", "Write", "Glob", "Grep"}
	timeout := time.Duration(cfg.FactsTimeoutMin) * time.Minute
	model := cfg.FactsModel
	if model == "" {
		model = cfg.Pass1Model
	}

	parallel := cfg.Pass2Parallel
	if parallel <= 0 {
		parallel = 3
	}
	if parallel > len(runs) {
		parallel = len(runs)
	}

	sourceTitle := readArticleTitle(rawFile)
	logMsg("sync", "facts pass: %d chapters, parallel=%d for %s", len(runs), parallel, rawName)

	// Build per-chunk facts output paths up front so the goroutines
	// can write deterministic filenames matching their chunk indexes.
	factsPaths := make([]string, len(runs))
	for i, r := range runs {
		stem := fmt.Sprintf("%02d-%s", i, slugifyForChunk(r.chunk.Title))
		factsPaths[i] = filepath.Join(factsDir, stem+".json")
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallel)

	var rateLimitOnce gosync.Once
	var rateLimited bool

	for i := range runs {
		g.Go(func() error {
			if gctx.Err() != nil {
				return nil //nolint:nilerr // sibling goroutine canceled gctx; quiet exit
			}
			r := runs[i]
			prompt, err := loadPrompt("absorb-facts.md", map[string]string{
				"KB_DIR":        root,
				"RAW_FILE":      rawFile,
				"CHUNK_FILE":    r.chunkMD,
				"CHAPTER_TITLE": r.chunk.Title,
				"SOURCE_TITLE":  sourceTitle,
				"FACTS_FILE":    factsPaths[i],
			})
			if err != nil {
				return fmt.Errorf("load facts prompt %d: %w", r.index, err)
			}
			if _, err := runClaude(withOpLabel(gctx, "absorb-facts"), root, prompt, model, tools, timeout); err != nil {
				if errors.Is(err, ErrRateLimit) {
					rateLimitOnce.Do(func() { rateLimited = true })
					return err
				}
				logMsg("sync", "facts chapter %d (%s) failed: %v", r.index, fmtChapterTitle(r.chunk.Title), err)
				return nil
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		if rateLimited {
			return nil, ErrRateLimit
		}
		return nil, err
	}

	merged := mergeFacts(rawFile, sourceTitle, runs, factsPaths)

	// Write merged file under output/facts/. Stable consumer-facing
	// path so future commands can `cat output/facts/<slug>.json`
	// without knowing the per-run absorb plumbing.
	publicDir := filepath.Join(root, "output", "facts")
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir public facts dir: %w", err)
	}
	mergedPath := filepath.Join(publicDir, strings.TrimSuffix(rawName, ".md")+".json")
	mergedBytes, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal merged facts: %w", err)
	}
	if err := os.WriteFile(mergedPath, mergedBytes, 0o644); err != nil {
		return nil, fmt.Errorf("write merged facts: %w", err)
	}
	logMsg("sync", "facts pass: merged %d facts from %d chapters → %s", len(merged.Facts), len(merged.Chapters), filepath.Base(mergedPath))
	return merged, nil
}

// mergeFacts combines per-chunk facts JSON into a single MergedFacts
// envelope. ID prefixing: each fact's `id` is rewritten to
// "c<chapter-index>-<original-id>" so the merged file has globally
// unique IDs without making the model coordinate across chunks.
//
// Missing or unparseable per-chunk files are skipped quietly — the
// surrounding pass already logged the upstream failure. The chapter
// gets a zero-count entry in the index so consumers can still see
// "chapter 3 was attempted but produced no facts."
func mergeFacts(rawFile, sourceTitle string, runs []chapterRun, factsPaths []string) *MergedFacts {
	merged := &MergedFacts{
		Version:     factsSchemaVersion,
		RawFile:     rawFile,
		SourceTitle: sourceTitle,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}
	for i, r := range runs {
		entry := factsChapterEntry{Index: r.index, Title: r.chunk.Title}
		path := factsPaths[i]
		data, err := os.ReadFile(path)
		if err != nil {
			merged.Chapters = append(merged.Chapters, entry)
			continue
		}
		var cf chapterFacts
		if err := json.Unmarshal(data, &cf); err != nil {
			merged.Chapters = append(merged.Chapters, entry)
			continue
		}
		if len(cf.Facts) == 0 {
			merged.Chapters = append(merged.Chapters, entry)
			continue
		}
		startIdx := len(merged.Facts)
		for _, f := range cf.Facts {
			f.ID = fmt.Sprintf("c%02d-%s", r.index, f.ID)
			f.ChapterTitle = r.chunk.Title
			merged.Facts = append(merged.Facts, f)
		}
		entry.IDStart = merged.Facts[startIdx].ID
		entry.IDEnd = merged.Facts[len(merged.Facts)-1].ID
		entry.Count = len(merged.Facts) - startIdx
		merged.Chapters = append(merged.Chapters, entry)
	}
	return merged
}

// loadMergedFacts reads a previously-written facts file from
// output/facts/<rawname>.json. Returns nil (no error) when the file
// is absent — the caller is expected to treat "no facts file" as
// equivalent to "facts pass disabled" and proceed un-grounded.
//
// Version mismatches also return nil: a future schema bump
// shouldn't crash an absorb run that happens to find an old facts
// file on disk. The next facts pass will overwrite it.
func loadMergedFacts(root, rawName string) (*MergedFacts, error) {
	path := filepath.Join(root, "output", "facts", strings.TrimSuffix(rawName, ".md")+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read facts: %w", err)
	}
	var m MergedFacts
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse facts: %w", err)
	}
	if m.Version != factsSchemaVersion {
		return nil, nil
	}
	return &m, nil
}

// factsForChapter returns the facts for one chapter, or nil if no
// facts exist for that chapter. Used by pass-1 chaptered to inject
// per-chapter grounding into each chunk's prompt.
func (m *MergedFacts) factsForChapter(chapterIndex int) []AtomicFact {
	if m == nil {
		return nil
	}
	prefix := fmt.Sprintf("c%02d-", chapterIndex)
	var out []AtomicFact
	for _, f := range m.Facts {
		if strings.HasPrefix(f.ID, prefix) {
			out = append(out, f)
		}
	}
	return out
}

// formatFactsForPrompt renders a fact list as a compact text block
// for embedding in a prompt template. Format:
//
//	[id, type] claim "anchor"
//
// One line per fact. Empty input returns "" so callers can splat
// the result into a {{FACTS}} placeholder unconditionally.
func formatFactsForPrompt(facts []AtomicFact) string {
	if len(facts) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, f := range facts {
		sb.WriteString("- [")
		sb.WriteString(f.ID)
		sb.WriteString(", ")
		sb.WriteString(f.Type)
		sb.WriteString("] ")
		sb.WriteString(f.Claim)
		if f.Anchor != "" {
			sb.WriteString(" (\"")
			sb.WriteString(f.Anchor)
			sb.WriteString("\")")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
