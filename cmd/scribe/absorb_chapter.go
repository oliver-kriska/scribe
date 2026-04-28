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

// absorb_chapter.go implements Phase 3A.5 — chapter-aware pass-1
// absorb. When a raw article has a TOC sidecar with at least
// AbsorbConfig.ChapterThreshold chapters, pass-1 fans out across
// chapters in parallel: each chunk gets its own claude -p invocation
// with a chapter-scoped prompt template, and the per-chapter plans
// merge into a single plan.json that pass-2 consumes.
//
// Why fan out: a 73-page paper dumped into one absorb prompt
//   (a) forces haiku/sonnet to truncate context,
//   (b) silently drops mid-paper claims because the model's
//       attention is biased to the start and end,
//   (c) returns a flat 5-8 entity list that misses chapter-specific
//       detail.
// Iterating per chapter gives every section equal weight, lets us
// run chunks in parallel for wallclock parity, and produces 2-4×
// more (and more grounded) entities for downstream pass-2.
//
// Why not just enlarge the context window: token cost scales linearly
// with prompt size; chapter iteration cuts per-call tokens by 5-10×
// and the fan-out wallclock equals the longest-chapter time, not the
// sum of all chapters. Net cost is comparable, quality is higher.

// runPass1Whole is the legacy single-shot pass-1 path. Extracted as
// its own helper so the chaptered dispatcher can call it as a
// fallback (and so the dispatcher itself stays readable in
// absorbDenseTwoPass). Behavior is byte-identical to what landed
// pre-Phase-3A.5 — same prompt, same model, same tools, same
// timeout, same plan file shape. No callers changed.
func (s *SyncCmd) runPass1Whole(ctx context.Context, root, rawFile, planFile string, cfg AbsorbConfig) error {
	pass1Prompt, err := loadPrompt("absorb-pass1.md", map[string]string{
		"KB_DIR":    root,
		"RAW_FILE":  rawFile,
		"PLAN_FILE": planFile,
	})
	if err != nil {
		return fmt.Errorf("load pass1 prompt: %w", err)
	}
	pass1Tools := []string{"Read", "Write", "Glob", "Grep"}
	pass1Timeout := time.Duration(cfg.Pass1TimeoutMin) * time.Minute
	if _, err := runClaude(ctx, root, pass1Prompt, cfg.Pass1Model, pass1Tools, pass1Timeout); err != nil {
		return fmt.Errorf("pass1: %w", err)
	}
	return nil
}

// shouldAbsorbChaptered decides whether to take the chapter-aware
// path. Three signals must align:
//
//  1. AbsorbConfig.ChapterAware is true (default true; users opt out
//     via scribe.yaml `absorb.chapter_aware: false`).
//  2. A TOC sidecar exists for the raw article.
//  3. Sidecar has at least cfg.ChapterThreshold chapters AND the
//     chunker confirms it can produce that many chunks. The chunker
//     guard catches the edge case where the TOC has 30 entries but
//     only 2 resolved to body offsets (sidecar with too many
//     phantom titles).
//
// Returning false here means the caller falls through to the legacy
// single-shot pass-1 path, exactly the same behavior as Phase 3A.
func shouldAbsorbChaptered(rawFile string, cfg AbsorbConfig) (bool, []Chunk, string) {
	if cfg.ChapterAware == nil || !*cfg.ChapterAware {
		return false, nil, ""
	}
	threshold := cfg.ChapterThreshold
	if threshold <= 0 {
		threshold = 3
	}

	if !articleHasTOCSidecar(rawFile) {
		return false, nil, ""
	}
	strategy, chunks, err := ChunkArticle(rawFile)
	if err != nil {
		return false, nil, ""
	}
	if strategy != "toc" {
		return false, nil, ""
	}
	if len(chunks) < threshold {
		return false, nil, ""
	}
	return true, chunks, strategy
}

// runPass1Chaptered drives the parallel chapter-iteration of pass-1.
// Writes one chunk file per chapter into output/absorb-chunks/<rawname>/
// (so callers can inspect after the fact if absorb misbehaves), runs
// pass-1 against each chunk in parallel, and merges the per-chapter
// plans into a single absorbPlan. Returns the merged plan and the
// path the caller should pass to pass-2 (which is the merged plan
// file, not any of the per-chunk files).
//
// chunksDir is per-article so concurrent absorbs of different
// articles don't trample each other's chunk files.
//
// Parallelism: bounded by Pass2Parallel (we reuse the same knob —
// no point exposing yet another tuning surface; pass-1 chapters
// are similarly cheap-and-parallelizable).
func (s *SyncCmd) runPass1Chaptered(ctx context.Context, root, rawFile, rawName string, chunks []Chunk, cfg AbsorbConfig, planFile string) error {
	chunksDir := filepath.Join(root, "output", "absorb-chunks", strings.TrimSuffix(rawName, ".md"))
	if err := os.MkdirAll(chunksDir, 0o755); err != nil {
		return fmt.Errorf("mkdir chunks: %w", err)
	}
	plansDir := filepath.Join(root, "output", "absorb-plans", strings.TrimSuffix(rawName, ".md")+"-chapters")
	if err := os.MkdirAll(plansDir, 0o755); err != nil {
		return fmt.Errorf("mkdir per-chapter plans: %w", err)
	}

	pass1Tools := []string{"Read", "Write", "Glob", "Grep"}
	pass1Timeout := time.Duration(cfg.Pass1TimeoutMin) * time.Minute

	parallel := cfg.Pass2Parallel
	if parallel <= 0 {
		parallel = 3
	}
	if parallel > len(chunks) {
		parallel = len(chunks)
	}

	// Chapter file paths: <chunksDir>/00-<slug>.md, 01-, ... so
	// alphabetic sort matches chapter order on disk for easier
	// post-mortem inspection.
	runs := make([]chapterRun, len(chunks))
	for i, c := range chunks {
		stem := fmt.Sprintf("%02d-%s", i, slugifyForChunk(c.Title))
		runs[i] = chapterRun{
			index:    i,
			chunk:    c,
			chunkMD:  filepath.Join(chunksDir, stem+".md"),
			planJSON: filepath.Join(plansDir, stem+".json"),
		}
		// Persist the chunk so claude can Read it. Each chunk gets
		// the chapter title as its first line so the prompt's
		// "this is your input" framing reads naturally.
		body := "# " + c.Title + "\n\n" + c.Body
		if err := os.WriteFile(runs[i].chunkMD, []byte(body), 0o644); err != nil {
			return fmt.Errorf("write chunk %d: %w", i, err)
		}
	}

	// Source title for the chapter prompts — pull from the article
	// frontmatter once, reuse across all chapter runs.
	sourceTitle := readArticleTitle(rawFile)

	// Phase 3B: atomic-fact extraction (opt-in). Runs before pass-1
	// so each chunk's pass-1 prompt can include its facts as
	// grounding. Failures here fall through silently — pass-1 still
	// works without facts, just less grounded. nil mergedFacts means
	// "no facts available" and the {{FACTS}} placeholder renders empty.
	var mergedFacts *MergedFacts
	if shouldRunFactsPass(cfg) {
		mf, err := s.runFactsPass(ctx, root, rawFile, rawName, runs, cfg)
		if err != nil {
			if errors.Is(err, ErrRateLimit) {
				return err
			}
			logMsg("sync", "facts pass failed for %s (%v); proceeding with un-grounded pass-1", rawName, err)
		} else {
			mergedFacts = mf
		}
	}

	logMsg("sync", "pass1 chaptered: %d chapters, parallel=%d for %s", len(chunks), parallel, rawName)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallel)

	var rateLimitOnce gosync.Once
	var rateLimited bool

	for i := range runs {
		g.Go(func() error {
			if gctx.Err() != nil {
				return nil //nolint:nilerr // gctx canceled by another goroutine; quiet exit is intentional
			}
			r := runs[i]
			factsBlock := formatFactsForPrompt(mergedFacts.factsForChapter(r.index))
			prompt, err := loadPrompt("absorb-pass1-chapter.md", map[string]string{
				"KB_DIR":        root,
				"RAW_FILE":      rawFile,
				"CHUNK_FILE":    r.chunkMD,
				"CHAPTER_TITLE": r.chunk.Title,
				"SOURCE_TITLE":  sourceTitle,
				"PLAN_FILE":     r.planJSON,
				"FACTS":         factsBlock,
			})
			if err != nil {
				return fmt.Errorf("load chapter prompt %d: %w", r.index, err)
			}
			if _, err := runClaude(gctx, root, prompt, cfg.Pass1Model, pass1Tools, pass1Timeout); err != nil {
				if errors.Is(err, ErrRateLimit) {
					rateLimitOnce.Do(func() { rateLimited = true })
					return err
				}
				// Single-chapter failure shouldn't kill the whole
				// absorb — log and skip; merge will tolerate missing
				// chunk plans.
				logMsg("sync", "pass1 chapter %d (%s) failed: %v", r.index, fmtChapterTitle(r.chunk.Title), err)
				return nil
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		if rateLimited {
			return ErrRateLimit
		}
		return err
	}

	// Merge per-chapter plans into the single planFile pass-2 reads.
	merged, err := mergeChapterPlans(rawFile, runs)
	if err != nil {
		return fmt.Errorf("merge plans: %w", err)
	}
	mergedBytes, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal merged plan: %w", err)
	}
	if err := os.WriteFile(planFile, mergedBytes, 0o644); err != nil {
		return fmt.Errorf("write merged plan: %w", err)
	}
	logMsg("sync", "pass1 chaptered: merged %d entities from %d chapters into %s", len(merged.Entities), len(runs), filepath.Base(planFile))
	return nil
}

// chapterPlan is the per-chunk plan emitted by absorb-pass1-chapter.md.
// Same as absorbPlan plus the chapter title for diagnostics.
type chapterPlan struct {
	RawFile     string         `json:"raw_file"`
	SourceTitle string         `json:"source_title"`
	Chapter     string         `json:"chapter"`
	Domain      string         `json:"domain"`
	Entities    []absorbEntity `json:"entities"`
}

// chapterRun holds the per-chapter plumbing: chunk markdown path,
// per-chapter plan JSON path, and the original chunk metadata. Used
// internally by runPass1Chaptered to coordinate chunk emission,
// claude invocation, and merge.
type chapterRun struct {
	index    int
	chunk    Chunk
	chunkMD  string
	planJSON string
}

// mergeChapterPlans combines per-chapter plans into a single
// absorbPlan that pass-2 can consume unchanged. Dedup strategy: same
// label-after-trim merges — the second occurrence's key_claims are
// folded into the first, deduplicated by exact-string equality.
//
// We accumulate domain from the first non-empty chapter that
// supplies one. If chapters disagree on domain, the first wins —
// the assumption is that paper-level domain is stable across
// chapters; chapter-level disagreement is a model error and
// shouldn't override the article's own metadata.
//
// Missing or unparseable chapter plans get logged-and-skipped, not
// fatal: a long paper with one quirky chapter still yields a useful
// merged plan.
func mergeChapterPlans(rawFile string, runs []chapterRun) (absorbPlan, error) {
	merged := absorbPlan{RawFile: rawFile}
	byLabel := map[string]*absorbEntity{}
	order := []string{} // preserve first-seen order for stable pass-2 logs

	for _, r := range runs {
		data, err := os.ReadFile(r.planJSON)
		if err != nil {
			logMsg("sync", "skip chapter plan %d (no file): %v", r.index, err)
			continue
		}
		var cp chapterPlan
		if err := json.Unmarshal(data, &cp); err != nil {
			logMsg("sync", "skip chapter plan %d (parse error): %v", r.index, err)
			continue
		}
		if merged.SourceTitle == "" && cp.SourceTitle != "" {
			merged.SourceTitle = cp.SourceTitle
		}
		if merged.Domain == "" && cp.Domain != "" {
			merged.Domain = cp.Domain
		}
		for _, ent := range cp.Entities {
			label := strings.TrimSpace(ent.Label)
			if label == "" {
				continue
			}
			if existing, ok := byLabel[label]; ok {
				existing.KeyClaims = mergeStringSet(existing.KeyClaims, ent.KeyClaims)
				if existing.OneLine == "" && ent.OneLine != "" {
					existing.OneLine = ent.OneLine
				}
				continue
			}
			entCopy := ent
			byLabel[label] = &entCopy
			order = append(order, label)
		}
	}
	for _, label := range order {
		merged.Entities = append(merged.Entities, *byLabel[label])
	}
	if merged.Domain == "" {
		merged.Domain = "general"
	}
	return merged, nil
}

// mergeStringSet returns a deduplicated union of two string slices,
// preserving the order of `a` followed by new entries from `b`.
// Used to fold chapter-level key_claims without losing detail or
// allowing a flood of duplicates from chapters that re-mention the
// same numeric.
func mergeStringSet(a, b []string) []string {
	seen := make(map[string]struct{}, len(a))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		key := strings.TrimSpace(s)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	for _, s := range b {
		key := strings.TrimSpace(s)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	return out
}

// readArticleTitle pulls `title:` out of the YAML frontmatter. Best-
// effort string extraction without dragging in a yaml lib at this
// layer; absorb's other parse paths already use this loose match
// pattern. Empty result is acceptable — the prompt template just
// renders "{{SOURCE_TITLE}}" verbatim, which Claude tolerates.
func readArticleTitle(rawFile string) string {
	data, err := os.ReadFile(rawFile)
	if err != nil {
		return ""
	}
	s := string(data)
	if !strings.HasPrefix(s, "---") {
		return ""
	}
	end := strings.Index(s[3:], "\n---")
	if end < 0 {
		return ""
	}
	header := s[:end+4]
	for _, line := range strings.Split(header, "\n") {
		if strings.HasPrefix(line, "title:") {
			t := strings.TrimSpace(strings.TrimPrefix(line, "title:"))
			t = strings.Trim(t, `"'`)
			return t
		}
	}
	return ""
}

// slugifyForChunk produces a filesystem-safe slug suitable for a
// chunk filename. Mirrors slugifyForAssets (Phase 2A) but kept local
// so absorb_chapter.go has no compile-time dependency on the
// converter package code (in case we later split modules).
func slugifyForChunk(s string) string {
	var sb strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				sb.WriteRune('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(sb.String(), "-")
	if out == "" {
		out = "chunk"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}
