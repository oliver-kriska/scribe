package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// lint_content_dupes.go is the structural (no-LLM) content-duplication
// detector behind `scribe lint --duplicates`. It implements the two-tier
// design recorded in the KB (research/scribe-auto-ingestion-redesign.md +
// projects/phd-knowledge/learnings.md): "exact dedup first (free tokens);
// near-duplicate merging requires judgment."
//
//   - Tier 1 — EXACT: articles whose normalized body hashes identically are
//     verbatim copies (the same content re-ingested under another filename).
//   - Tier 2 — NEAR: token-overlap between bodies. The overlap coefficient
//     |A∩B|/min(|A|,|B|) catches the fragment case — a short session-mined
//     stub whose vocabulary is largely contained in a longer canonical
//     article (the empirical scriptorium failure: a cache-economics analysis
//     split across wiki/, research/, and a self-named wiki/scriptorium/ dir).
//
// Both tiers are REPORT-ONLY — they write candidates to wiki/_duplicates.md
// for human review (and the LLM `--contradictions`/`--resolve` pass). Merging
// content is a judgment call; this never deletes a page. Pairwise comparison
// is bounded by an inverted index over discriminative (rare) tokens so the
// pass stays cheap on large KBs.

// dupDefaultOverlap is the containment threshold for the fragment branch.
// Tuned against scriptorium for precision over recall — the report is for
// human review, so false positives cost more than a missed weak pair.
const dupDefaultOverlap = 0.70

// dupJaccardNearIdentical flags two articles as near-identical on symmetric
// overlap alone, independent of size.
const dupJaccardNearIdentical = 0.55

// dupJaccardFragmentFloor is the minimum symmetric overlap a contained
// fragment must still clear, so a small note doesn't match a larger article
// purely on shared common words.
const dupJaccardFragmentFloor = 0.30

// dupReportCap bounds how many near-dup pairs are written (highest-scoring
// first); the total is always reported.
const dupReportCap = 150

// dupMaxDocFreq caps how common a token may be to act as a candidate-pair
// generator. Tokens appearing in more than this many articles are too generic
// to discriminate (and would explode the candidate set), so they're indexed
// for the overlap math but not used to seed pairs.
const dupMaxDocFreq = 40

// dupMinTokens skips articles too small to judge — a handful of tokens
// overlaps too easily by chance.
const dupMinTokens = 40

// dupMaxSizeRatio bounds the containment branch. A short doc whose vocabulary
// is "contained" in one 20× its size is almost always a small note whose
// common words happen to appear in a large aggregation, not a real fragment.
const dupMaxSizeRatio = 4.0

// contentDoc is one article reduced to the signals the detector needs.
type contentDoc struct {
	rel    string
	hash   string          // sha256 of normalized body (exact tier)
	tokens map[string]bool // unique significant tokens (near tier)
}

// dupPair is a flagged near-duplicate candidate.
type dupPair struct {
	A, B    string
	Overlap float64 // |A∩B| / min(|A|,|B|)
	Jaccard float64 // |A∩B| / |A∪B|
}

// runDuplicatesCheck scans wiki articles for exact and near-duplicate content
// and writes findings to wiki/_duplicates.md (and stdout). threshold overrides
// dupDefaultOverlap when > 0. Report-only; never mutates articles.
func runDuplicatesCheck(threshold float64, outputMD string, dryRun bool) error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	if threshold <= 0 {
		threshold = dupDefaultOverlap
	}

	docs := collectContentDocs(root)
	if len(docs) < 2 {
		fmt.Printf("only %d article(s) in scope — need at least 2\n", len(docs))
		return nil
	}

	exact := findExactContentDuplicates(docs)
	near := findNearDuplicates(docs, threshold)

	logMsg("duplicates", "scanned %d article(s): %d exact group(s), %d near-dup pair(s)",
		len(docs), len(exact), len(near))

	if dryRun {
		fmt.Printf("would write %d exact group(s) + %d near-dup pair(s) to wiki/_duplicates.md\n", len(exact), len(near))
		return nil
	}

	body := renderDuplicatesReport(exact, near, threshold)
	outPath := outputMD
	if outPath == "" {
		outPath = filepath.Join(root, "wiki", "_duplicates.md")
	}
	if err := os.WriteFile(outPath, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	fmt.Printf("%d exact group(s), %d near-dup pair(s) → %s\n", len(exact), len(near), relPath(root, outPath))
	if len(exact) > 0 || len(near) > 0 {
		fmt.Println("review wiki/_duplicates.md, then run `scribe lint --contradictions` on a cluster or merge by hand")
	}
	return nil
}

// collectContentDocs reduces every wiki article to its dedup signals,
// skipping aggregation files (rolling memory, archives) — those deliberately
// accumulate many unrelated topics, so they're not duplicate candidates and
// would otherwise match every small note that shares a word with them.
func collectContentDocs(root string) []contentDoc {
	var docs []contentDoc
	_ = walkArticles(root, func(path string, content []byte) error {
		if isAggregationArticle(relPath(root, path), content) {
			return nil
		}
		body := stripFrontmatterBody(content)
		norm := normalizeForDedup(body)
		if strings.TrimSpace(norm) == "" {
			return nil
		}
		sum := sha256.Sum256([]byte(norm))
		docs = append(docs, contentDoc{
			rel:    relPath(root, path),
			hash:   hex.EncodeToString(sum[:]),
			tokens: tokenSet(norm),
		})
		return nil
	})
	sort.Slice(docs, func(i, j int) bool { return docs[i].rel < docs[j].rel })
	return docs
}

// isAggregationArticle reports whether an article is a rolling/aggregation
// file (learnings, decisions-log, yearly archives) that collects many topics
// and so must be excluded from duplicate detection.
func isAggregationArticle(rel string, content []byte) bool {
	if fm, err := parseFrontmatter(content); err == nil && fm != nil && fm.Rolling {
		return true
	}
	base := filepath.Base(rel)
	if strings.Contains(base, "-archive-") {
		return true
	}
	switch base {
	case "learnings.md", "decisions-log.md", "log.md", "decisions-archive.md":
		return true
	}
	return false
}

// findExactContentDuplicates groups articles by normalized-body hash and
// returns the groups with more than one member (each group is a set of
// verbatim-content copies under different paths).
func findExactContentDuplicates(docs []contentDoc) [][]string {
	byHash := map[string][]string{}
	for _, d := range docs {
		byHash[d.hash] = append(byHash[d.hash], d.rel)
	}
	var groups [][]string
	for _, rels := range byHash {
		if len(rels) > 1 {
			sort.Strings(rels)
			groups = append(groups, rels)
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i][0] < groups[j][0] })
	return groups
}

// findNearDuplicates flags pairs whose overlap coefficient meets the
// threshold. Candidate pairs are generated from an inverted index over
// discriminative tokens (those in ≤ dupMaxDocFreq articles) so the pass never
// pays the full O(n²) cost on a large KB. Exact-hash twins are skipped here —
// they're already reported by the exact tier.
func findNearDuplicates(docs []contentDoc, threshold float64) []dupPair {
	// Inverted index: token → doc indices, limited to discriminative tokens.
	postings := map[string][]int{}
	for i, d := range docs {
		for tok := range d.tokens {
			postings[tok] = append(postings[tok], i)
		}
	}

	candidates := map[[2]int]bool{}
	for _, idxs := range postings {
		if len(idxs) < 2 || len(idxs) > dupMaxDocFreq {
			continue
		}
		for a := range idxs {
			for b := a + 1; b < len(idxs); b++ {
				candidates[[2]int{idxs[a], idxs[b]}] = true
			}
		}
	}

	var pairs []dupPair
	for pair := range candidates {
		da, db := docs[pair[0]], docs[pair[1]]
		if da.hash == db.hash {
			continue // verbatim copy — covered by the exact tier
		}
		la, lb := len(da.tokens), len(db.tokens)
		if la < dupMinTokens || lb < dupMinTokens {
			continue // too small to judge
		}
		inter := intersectionSize(da.tokens, db.tokens)
		if inter == 0 {
			continue
		}
		minLen := min(la, lb)
		overlap := float64(inter) / float64(minLen)
		jaccard := float64(inter) / float64(la+lb-inter)
		ratio := float64(max(la, lb)) / float64(minLen)

		// Flag when EITHER the two are symmetrically near-identical (jaccard),
		// OR one is largely contained in the other with comparable size and a
		// real shared core (the fragment/stub case). The size cap + jaccard
		// floor are what keep a small note from matching a larger article on
		// shared common words alone.
		nearIdentical := jaccard >= dupJaccardNearIdentical
		fragment := overlap >= threshold && ratio <= dupMaxSizeRatio && jaccard >= dupJaccardFragmentFloor
		if !nearIdentical && !fragment {
			continue
		}
		pairs = append(pairs, dupPair{
			A: da.rel, B: db.rel,
			Overlap: overlap,
			Jaccard: jaccard,
		})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Overlap != pairs[j].Overlap {
			return pairs[i].Overlap > pairs[j].Overlap
		}
		return pairs[i].A < pairs[j].A
	})
	return pairs
}

// renderDuplicatesReport builds the wiki/_duplicates.md body.
func renderDuplicatesReport(exact [][]string, near []dupPair, threshold float64) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Duplicate candidates — scanned %s\n\n", time.Now().UTC().Format(time.RFC3339))
	sb.WriteString("Generated by `scribe lint --duplicates` (structural, no LLM). Review by hand; ")
	sb.WriteString("merge fragments into the canonical article and mark the rest `status: superseded`, ")
	sb.WriteString("or feed a cluster to `scribe lint --contradictions`. Nothing here is auto-deleted.\n\n")
	sb.WriteString("> Note: byte-for-byte identical pages are removed automatically by `scribe lint --fix`. ")
	sb.WriteString("The exact groups below share a normalized body but differ in frontmatter or formatting, ")
	sb.WriteString("so a human still picks which copy is canonical.\n\n")

	sb.WriteString("## Exact content duplicates (verbatim body)\n\n")
	if len(exact) == 0 {
		sb.WriteString("_none_\n\n")
	} else {
		for _, g := range exact {
			fmt.Fprintf(&sb, "- [ ] %s\n", strings.Join(g, " | "))
		}
		sb.WriteString("\n")
	}

	fmt.Fprintf(&sb, "## Near-duplicates (%d total; containment ≥ %.2f or jaccard ≥ %.2f)\n\n",
		len(near), threshold, dupJaccardNearIdentical)
	if len(near) == 0 {
		sb.WriteString("_none_\n")
	} else {
		shown := near
		if len(shown) > dupReportCap {
			shown = shown[:dupReportCap]
		}
		for _, p := range shown {
			fmt.Fprintf(&sb, "- [ ] [%s | %s] overlap=%.2f jaccard=%.2f\n", p.A, p.B, p.Overlap, p.Jaccard)
		}
		if len(near) > len(shown) {
			fmt.Fprintf(&sb, "\n_… %d more below the top %d — tighten with `--threshold` or merge the above first._\n", len(near)-len(shown), dupReportCap)
		}
	}
	return sb.String()
}

// --- text normalization helpers ---

// stripFrontmatterBody returns the article body with a leading YAML
// frontmatter block (--- … ---) removed. Content with no frontmatter is
// returned unchanged.
func stripFrontmatterBody(content []byte) string {
	s := string(content)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return s
	}
	if i := strings.Index(s[4:], "\n---"); i >= 0 {
		rest := s[4+i+len("\n---"):]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			return rest[nl+1:]
		}
		return ""
	}
	return s
}

// normalizeForDedup lowercases, drops scribe/markdown machinery (HTML
// comments, code fences, link/heading punctuation), and collapses whitespace
// so cosmetic differences don't defeat the exact-hash tier.
func normalizeForDedup(body string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "<!--") || t == "```" || strings.HasPrefix(t, "```") {
			continue // scribe markers / fence lines carry no content signal
		}
		b.WriteString(t)
		b.WriteByte(' ')
	}
	return strings.ToLower(strings.Join(strings.Fields(b.String()), " "))
}

// tokenSet returns the set of significant tokens in normalized text: word
// runs of length ≥ 4 that aren't stopwords. Length-4 floor drops most
// markdown/glue noise without an exhaustive stopword list.
func tokenSet(norm string) map[string]bool {
	out := map[string]bool{}
	for _, tok := range strings.FieldsFunc(norm, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		if len(tok) < 4 || dedupStopwords[tok] {
			continue
		}
		out[tok] = true
	}
	return out
}

// intersectionSize counts shared keys, iterating the smaller map.
func intersectionSize(a, b map[string]bool) int {
	if len(b) < len(a) {
		a, b = b, a
	}
	n := 0
	for k := range a {
		if b[k] {
			n++
		}
	}
	return n
}

// dedupStopwords is a small high-frequency English/markdown set. Kept short on
// purpose — the length-4 floor already removes most glue, and the inverted
// index ignores over-common tokens anyway.
var dedupStopwords = map[string]bool{
	"this": true, "that": true, "with": true, "from": true, "have": true,
	"will": true, "when": true, "then": true, "than": true, "your": true,
	"they": true, "them": true, "what": true, "which": true, "into": true,
	"over": true, "more": true, "most": true, "some": true, "such": true,
	"also": true, "only": true, "very": true, "much": true, "each": true,
	"about": true, "would": true, "could": true, "should": true, "their": true,
	"there": true, "these": true, "those": true, "because": true, "while": true,
}
