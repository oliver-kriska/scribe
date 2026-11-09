package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// eval_retrieval.go — `scribe eval retrieval`: measure how well the KB's
// search (qmd) finds the right article for a question.
//
// Two steps, deliberately separate:
//
//   - --generate N (LLM, run once): sample N articles and ask the model for
//     two queries per article: a "direct" one in the article's own words and
//     an "indirect" one that describes the situation without the title's
//     vocabulary. The indirect query is the hard case: the article holds
//     what you need, but you don't know what it's called. The fixture is
//     written to $KB/eval/retrieval-fixture.json and committed with the KB,
//     so later runs measure the same questions.
//   - default run (no LLM in scribe; qmd may use its local models): feed the
//     fixture to `qmd bench --json`, which scores four backends (bm25,
//     vector, hybrid, full = hybrid + rerank) on hit@5, hit@10 and MRR.
//     The result is stored under output/eval/, compared with the previous
//     run, and its headline numbers go into the run record.
//
// Ground truth is the source article only. A near-duplicate article that
// answers the question equally well counts as a miss. That is deliberate:
// duplicates split retrieval and are worth seeing in the numbers.

type EvalCmd struct {
	Retrieval EvalRetrievalCmd `cmd:"" help:"Benchmark KB search: generate a question set once, then score qmd's backends on it."`
}

type EvalRetrievalCmd struct {
	Generate int    `help:"Sample N articles and generate a new question set (LLM calls). 0 = run the existing set." default:"0"`
	Force    bool   `help:"With --generate: overwrite an existing question set."`
	Seed     int64  `help:"Sampling seed for --generate." default:"1"`
	Provider string `help:"LLM provider for --generate (default: absorb.contextualize.provider)."`
	Model    string `help:"LLM model for --generate (default: absorb.contextualize.model)."`
	Fixture  string `help:"Question set path, relative to the KB root." default:"eval/retrieval-fixture.json"`
	Misses   int    `help:"List up to N questions the full backend missed (MRR 0)." default:"10"`
}

// qmdBenchQuery / qmdBenchFixture mirror qmd's bench fixture format
// (dist/bench/types.d.ts in @tobilu/qmd 2.x).
type qmdBenchQuery struct {
	ID            string   `json:"id"`
	Query         string   `json:"query"`
	Type          string   `json:"type"`
	Description   string   `json:"description"`
	ExpectedFiles []string `json:"expected_files"`
	ExpectedInTop int      `json:"expected_in_top_k"`
}

type qmdBenchFixture struct {
	Description string          `json:"description"`
	Version     int             `json:"version"`
	Collection  string          `json:"collection,omitempty"`
	Queries     []qmdBenchQuery `json:"queries"`
	// scribe metadata (qmd ignores unknown keys).
	Generated string `json:"generated,omitempty"`
	Generator string `json:"generator,omitempty"`
}

type qmdBenchBackend struct {
	PrecisionAtK float64  `json:"precision_at_k"`
	Recall       float64  `json:"recall"`
	MRR          float64  `json:"mrr"`
	LatencyMS    float64  `json:"latency_ms"`
	TopFiles     []string `json:"top_files"`
}

type qmdBenchResult struct {
	Timestamp string `json:"timestamp"`
	Results   []struct {
		ID       string                     `json:"id"`
		Query    string                     `json:"query"`
		Type     string                     `json:"type"`
		Backends map[string]qmdBenchBackend `json:"backends"`
	} `json:"results"`
}

// evalCell is one backend × query-type aggregate.
type evalCell struct {
	N         int     `json:"n"`
	Hit5      float64 `json:"hit5"`
	Hit10     float64 `json:"hit10"`
	MRR       float64 `json:"mrr"`
	LatencyMS float64 `json:"latency_p50_ms"`
}

// evalReport is what scribe stores per run.
type evalReport struct {
	Timestamp  string                         `json:"timestamp"`
	Fixture    string                         `json:"fixture"`
	Collection string                         `json:"collection"`
	Queries    int                            `json:"queries"`
	Skipped    int                            `json:"skipped_missing_articles"`
	Cells      map[string]map[string]evalCell `json:"cells"` // backend → type ("all", "exact", "semantic")
	Raw        json.RawMessage                `json:"raw"`
}

// Query types used in the fixture. qmd's type vocabulary is fixed; these
// two map onto ours: exact = direct, semantic = indirect.
const (
	evalTypeDirect   = "exact"
	evalTypeIndirect = "semantic"
)

var evalBackends = []string{"bm25", "vector", "hybrid", "full"}

func (c *EvalRetrievalCmd) Run() error {
	root, err := kbDir()
	if err != nil {
		return err
	}
	cfg := loadConfig(root)
	fixturePath := filepath.Join(root, filepath.FromSlash(c.Fixture))

	if c.Generate > 0 {
		if fileExists(fixturePath) && !c.Force {
			return fmt.Errorf("%s exists — a stable question set is the point; pass --force to replace it", c.Fixture)
		}
		provider, model := c.Provider, c.Model
		if provider == "" {
			provider = cfg.Absorb.Contextualize.Provider
		}
		if model == "" {
			model = cfg.Absorb.Contextualize.Model
		}
		gen := newLLMProvider(provider, model, cfg.Absorb.Contextualize.OllamaURL, root)
		fx, stats, err := generateRetrievalFixture(context.Background(), root, gen, c.Generate, c.Seed)
		if err != nil {
			return err
		}
		if err := writeJSONFile(fixturePath, fx); err != nil {
			return err
		}
		fmt.Printf("eval: wrote %d questions from %d articles to %s (%s; indirect dropped for title-word leaks: %d, failed articles: %d)\n",
			len(fx.Queries), stats.articles, c.Fixture, gen.Name(), stats.leaked, stats.failed)
		mergeRunStats(map[string]any{"eval_generated": len(fx.Queries)})
		return nil
	}

	if !fileExists(fixturePath) {
		return fmt.Errorf("no question set at %s — create one with `scribe eval retrieval --generate 40`", c.Fixture)
	}
	coll, ok := qmdCollectionFor(root)
	if !ok {
		return errors.New("eval: no qmd collection covers this KB (run `scribe init` or `qmd collection add`)")
	}
	report, err := runRetrievalEval(root, fixturePath, coll.Name)
	if err != nil {
		return err
	}
	report.Fixture = c.Fixture

	outDir := filepath.Join(root, "output", "eval")
	prev, _ := latestEvalReport(outDir)
	name := "retrieval-" + time.Now().UTC().Format("20060102-150405") + ".json"
	if err := writeJSONFile(filepath.Join(outDir, name), report); err != nil {
		return err
	}

	fmt.Print(formatEvalReport(report, prev))
	if c.Misses > 0 {
		fmt.Print(formatEvalMisses(report, c.Misses))
	}
	mergeRunStats(evalRunStatsFields(report))
	return nil
}

// --- generation ---

type evalGenStats struct{ articles, leaked, failed int }

type evalCandidate struct {
	rel, title, body string
}

// sampleEvalArticles picks n articles spread across top-level dirs, seeded
// for reproducibility. Superseded records, stubs, and very short pages are
// skipped: a question needs real content to point at.
func sampleEvalArticles(root string, n int, seed int64) []evalCandidate {
	byDir := map[string][]evalCandidate{}
	_ = walkArticles(root, func(path string, content []byte) error {
		fm, err := parseFrontmatter(content)
		if err != nil || fm == nil || fm.Title == "" || isSuperseded(fm) {
			return nil //nolint:nilerr // unparseable frontmatter: skip, keep sampling
		}
		body := bodyAfterFrontmatter(string(content))
		if len(body) < 600 {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		dir, _, _ := strings.Cut(rel, "/")
		byDir[dir] = append(byDir[dir], evalCandidate{rel: rel, title: fm.Title, body: body})
		return nil
	})
	rng := rand.New(rand.NewSource(seed))
	dirs := make([]string, 0, len(byDir))
	for d, list := range byDir {
		sort.Slice(list, func(i, j int) bool { return list[i].rel < list[j].rel })
		rng.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	// Round-robin across dirs so one huge directory can't dominate.
	var out []evalCandidate
	for i := 0; len(out) < n; i++ {
		added := false
		for _, d := range dirs {
			if i < len(byDir[d]) && len(out) < n {
				out = append(out, byDir[d][i])
				added = true
			}
		}
		if !added {
			break
		}
	}
	return out
}

// evalArticleMaxBytes bounds the article text inlined into the prompt.
const evalArticleMaxBytes = 8000

var evalQuestionSchema = jsonSchemaSpec{
	Name: "eval_questions",
	Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"direct":   map[string]any{"type": "string", "minLength": 10},
			"indirect": map[string]any{"type": "string", "minLength": 10},
		},
		"required":             []string{"direct", "indirect"},
		"additionalProperties": false,
	},
}

func generateRetrievalFixture(ctx context.Context, root string, gen llmProviderGenerator, n int, seed int64) (qmdBenchFixture, evalGenStats, error) {
	var stats evalGenStats
	fx := qmdBenchFixture{
		Description: "scribe retrieval eval: direct (exact) and indirect (semantic) questions per sampled article",
		Version:     1,
		Generated:   time.Now().UTC().Format(time.RFC3339),
		Generator:   gen.Name(),
	}
	cands := sampleEvalArticles(root, n, seed)
	if len(cands) == 0 {
		return fx, stats, errors.New("eval: no eligible articles to sample")
	}
	for i, cand := range cands {
		titleWords := evalTitleWords(cand.title)
		prompt, err := loadPrompt("eval-retrieval-questions.md", map[string]string{
			"TITLE":           cand.title,
			"TITLE_WORDS":     strings.Join(titleWords, ", "),
			"ARTICLE_CONTENT": truncateBytes(cand.body, evalArticleMaxBytes),
		})
		if err != nil {
			return fx, stats, err
		}
		raw, err := generateWithSchema(withOpLabel(ctx, "eval"), gen, prompt, evalQuestionSchema)
		if err != nil {
			logMsg("eval", "%s: %v", cand.rel, err)
			stats.failed++
			continue
		}
		var q struct{ Direct, Indirect string }
		if err := json.Unmarshal([]byte(extractJSONObject(raw)), &q); err != nil || strings.TrimSpace(q.Direct) == "" {
			logMsg("eval", "%s: unparseable reply", cand.rel)
			stats.failed++
			continue
		}
		stats.articles++
		id := fmt.Sprintf("q%03d", i+1)
		fx.Queries = append(fx.Queries, qmdBenchQuery{
			ID: id + "-direct", Query: strings.TrimSpace(q.Direct), Type: evalTypeDirect,
			Description: cand.title, ExpectedFiles: []string{cand.rel}, ExpectedInTop: 5,
		})
		ind := strings.TrimSpace(q.Indirect)
		if ind == "" || evalLeaksTitle(ind, titleWords) {
			stats.leaked++
			continue
		}
		fx.Queries = append(fx.Queries, qmdBenchQuery{
			ID: id + "-indirect", Query: ind, Type: evalTypeIndirect,
			Description: cand.title, ExpectedFiles: []string{cand.rel}, ExpectedInTop: 5,
		})
	}
	if len(fx.Queries) == 0 {
		return fx, stats, errors.New("eval: the model produced no usable questions")
	}
	return fx, stats, nil
}

// extractJSONObject trims prose or code fences around the first {...}.
func extractJSONObject(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return s
	}
	return s[start : end+1]
}

var evalStopwords = map[string]bool{
	"with": true, "from": true, "that": true, "this": true, "into": true, "your": true,
	"what": true, "when": true, "where": true, "which": true, "about": true, "over": true,
	"than": true, "then": true, "they": true, "them": true, "have": true, "does": true,
	"using": true, "used": true, "versus": true, "between": true, "after": true, "before": true,
	"without": true, "through": true, "their": true, "there": true, "these": true, "those": true,
}

// evalTitleWords returns the distinctive title words (≥4 letters, not a
// stopword), lowercased, in title order.
func evalTitleWords(title string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range evalTokens(title) {
		if len(w) < 4 || evalStopwords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

func evalTokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
}

// evalLeaksTitle reports whether q reuses a distinctive title word. A
// shared 5-letter prefix counts too, which catches simple inflections
// (cache/caching, retrieve/retrieval) without a stemmer.
func evalLeaksTitle(q string, titleWords []string) bool {
	for _, w := range evalTokens(q) {
		for _, tw := range titleWords {
			if w == tw || (len(w) >= 5 && len(tw) >= 5 && w[:5] == tw[:5]) {
				return true
			}
		}
	}
	return false
}

// --- running ---

func runRetrievalEval(root, fixturePath, collection string) (evalReport, error) {
	report := evalReport{Timestamp: time.Now().UTC().Format(time.RFC3339), Collection: collection}
	var fx qmdBenchFixture
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		return report, err
	}
	if err := json.Unmarshal(data, &fx); err != nil {
		return report, fmt.Errorf("parse %s: %w", fixturePath, err)
	}
	// Articles get renamed, merged and deleted. A question whose answer is
	// gone can't be scored fairly, so drop it and say how many.
	kept := fx.Queries[:0]
	for _, q := range fx.Queries {
		if len(q.ExpectedFiles) > 0 && fileExists(filepath.Join(root, filepath.FromSlash(q.ExpectedFiles[0]))) {
			kept = append(kept, q)
		} else {
			report.Skipped++
		}
	}
	fx.Queries = kept
	fx.Collection = collection
	if len(fx.Queries) == 0 {
		return report, errors.New("eval: no question in the set points at an existing article")
	}
	report.Queries = len(fx.Queries)

	tmp, err := os.CreateTemp("", "scribe-eval-*.json")
	if err != nil {
		return report, err
	}
	defer os.Remove(tmp.Name())
	if err := json.NewEncoder(tmp).Encode(fx); err != nil {
		tmp.Close()
		return report, err
	}
	tmp.Close()

	raw, err := runQMDBenchFn(root, tmp.Name(), collection)
	if err != nil {
		return report, err
	}
	res, err := parseQMDBenchOutput(raw, fx.Queries)
	if err != nil {
		return report, err
	}
	// Store the normalized result, never the raw table: Raw must be JSON.
	if raw, err = json.Marshal(res); err != nil {
		return report, err
	}
	report.Raw = raw
	report.Cells = aggregateEval(res)
	return report, nil
}

// parseQMDBenchOutput accepts both shapes `qmd bench` can print. qmd
// 2.1.0 never emits JSON: its CLI stores --json as opts.format="json",
// but the bench case reads opts.json, so the flag is silently ignored and
// the human table is printed. The table has every metric scribe scores,
// but not top_files, so the misses list can't name the wrong top hit in
// that case. The query type is taken from the fixture by ID.
func parseQMDBenchOutput(raw []byte, queries []qmdBenchQuery) (qmdBenchResult, error) {
	var res qmdBenchResult
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		if err := json.Unmarshal(trimmed, &res); err != nil {
			return res, fmt.Errorf("parse qmd bench JSON: %w", err)
		}
		return res, nil
	}
	byID := make(map[string]qmdBenchQuery, len(queries))
	for _, q := range queries {
		byID[q.ID] = q
	}
	index := map[string]int{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		// "<id>  <backend>  <P@k> <Recall> <MRR> <F1> <N>ms"
		f := strings.Fields(line)
		if len(f) != 7 || !strings.HasSuffix(f[6], "ms") {
			continue
		}
		q, ok := byID[f[0]]
		if !ok {
			continue // header, separator or summary row
		}
		nums := make([]float64, 5)
		bad := false
		for i, s := range []string{f[2], f[3], f[4], f[5], strings.TrimSuffix(f[6], "ms")} {
			if _, err := fmt.Sscanf(s, "%g", &nums[i]); err != nil {
				bad = true
				break
			}
		}
		if bad {
			continue
		}
		i, seen := index[q.ID]
		if !seen {
			i = len(res.Results)
			index[q.ID] = i
			res.Results = append(res.Results, struct {
				ID       string                     `json:"id"`
				Query    string                     `json:"query"`
				Type     string                     `json:"type"`
				Backends map[string]qmdBenchBackend `json:"backends"`
			}{ID: q.ID, Query: q.Query, Type: q.Type, Backends: map[string]qmdBenchBackend{}})
		}
		res.Results[i].Backends[f[1]] = qmdBenchBackend{PrecisionAtK: nums[0], Recall: nums[1], MRR: nums[2], LatencyMS: nums[4]}
	}
	if len(res.Results) == 0 {
		return res, fmt.Errorf("parse qmd bench output: neither JSON nor a result table (%d bytes)", len(raw))
	}
	return res, nil
}

// runQMDBenchFn is a seam so tests can score a scripted bench result
// without a qmd install; production code never reassigns it.
var runQMDBenchFn = runQMDBench

// runQMDBench runs `qmd bench --json` keeping stdout separate: qmd writes
// warnings (e.g. "N documents need embedding") to stderr, which would
// corrupt the JSON if mixed in the way runQMD's CombinedOutput does.
func runQMDBench(root, fixture, collection string) ([]byte, error) {
	bin := resolveQMDBinary(root)
	cmd := exec.Command(bin, "bench", fixture, "--json", "-c", collection) //nolint:noctx // CLI top level
	cmd.Dir = root
	cmd.Env = qmdEnv(bin, os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("qmd bench: %w: %s", err, truncateBytes(strings.TrimSpace(stderr.String()), 500))
	}
	out := stdout.Bytes()
	if i := bytes.IndexByte(out, '{'); i > 0 {
		out = out[i:]
	}
	return out, nil
}

func aggregateEval(res qmdBenchResult) map[string]map[string]evalCell {
	type acc struct {
		n                int
		hit5, hit10, mrr float64
		lat              []float64
	}
	accs := map[string]map[string]*acc{}
	add := func(backend, typ string, b qmdBenchBackend) {
		if accs[backend] == nil {
			accs[backend] = map[string]*acc{}
		}
		a := accs[backend][typ]
		if a == nil {
			a = &acc{}
			accs[backend][typ] = a
		}
		a.n++
		a.hit5 += b.PrecisionAtK // one expected file, k=5 → precision@k is hit@5
		a.hit10 += b.Recall      // qmd fetches 10 results → recall is hit@10
		a.mrr += b.MRR
		a.lat = append(a.lat, b.LatencyMS)
	}
	for _, r := range res.Results {
		for backend, b := range r.Backends {
			add(backend, r.Type, b)
			add(backend, "all", b)
		}
	}
	out := map[string]map[string]evalCell{}
	for backend, byType := range accs {
		out[backend] = map[string]evalCell{}
		for typ, a := range byType {
			sort.Float64s(a.lat)
			n := float64(a.n)
			out[backend][typ] = evalCell{N: a.n, Hit5: a.hit5 / n, Hit10: a.hit10 / n, MRR: a.mrr / n, LatencyMS: a.lat[len(a.lat)/2]}
		}
	}
	return out
}

// --- reporting ---

func latestEvalReport(dir string) (*evalReport, error) {
	matches, _ := filepath.Glob(filepath.Join(dir, "retrieval-*.json"))
	if len(matches) == 0 {
		return nil, os.ErrNotExist
	}
	sort.Strings(matches)
	data, err := os.ReadFile(matches[len(matches)-1])
	if err != nil {
		return nil, err
	}
	var r evalReport
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func formatEvalReport(r evalReport, prev *evalReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "retrieval eval — %d questions on collection %q", r.Queries, r.Collection)
	if r.Skipped > 0 {
		fmt.Fprintf(&b, " (%d skipped: article no longer exists)", r.Skipped)
	}
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "%-8s %-9s %5s %7s %7s %7s %9s\n", "backend", "type", "n", "hit@5", "hit@10", "MRR", "p50 ms")
	for _, backend := range evalBackends {
		for _, typ := range []string{evalTypeDirect, evalTypeIndirect, "all"} {
			cell, ok := r.Cells[backend][typ]
			if !ok {
				continue
			}
			label := map[string]string{evalTypeDirect: "direct", evalTypeIndirect: "indirect", "all": "all"}[typ]
			line := fmt.Sprintf("%-8s %-9s %5d %7.2f %7.2f %7.2f %9.0f", backend, label, cell.N, cell.Hit5, cell.Hit10, cell.MRR, cell.LatencyMS)
			if prev != nil {
				if p, ok := prev.Cells[backend][typ]; ok {
					line += fmt.Sprintf("   ΔMRR %+.2f", cell.MRR-p.MRR)
				}
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	if prev != nil {
		fmt.Fprintf(&b, "\n(Δ vs %s; deltas compare like with like only if the question set is unchanged)\n", prev.Timestamp)
	}
	return b.String()
}

// formatEvalMisses lists the questions the full pipeline answered with
// the wrong article, with what it returned first. Duplicate pages show up
// here as "expected A, got A-variant".
func formatEvalMisses(r evalReport, limit int) string {
	var res qmdBenchResult
	if json.Unmarshal(r.Raw, &res) != nil {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, q := range res.Results {
		full, ok := q.Backends["full"]
		if !ok || full.MRR > 0 {
			continue
		}
		if n == 0 {
			b.WriteString("\nmissed by full (not in top 10):\n")
		}
		got := "(not reported: qmd printed its table, not JSON)"
		if len(full.TopFiles) > 0 {
			got = full.TopFiles[0]
		}
		fmt.Fprintf(&b, "  %s  %q\n      → top hit: %s\n", q.ID, truncateBytes(q.Query, 100), got)
		n++
		if n >= limit {
			break
		}
	}
	return b.String()
}

func evalRunStatsFields(r evalReport) map[string]any {
	out := map[string]any{"eval_queries": r.Queries, "eval_skipped": r.Skipped}
	for _, backend := range evalBackends {
		for typ, short := range map[string]string{"all": "all", evalTypeDirect: "direct", evalTypeIndirect: "indirect"} {
			if c, ok := r.Cells[backend][typ]; ok {
				out[fmt.Sprintf("eval_%s_%s_mrr", backend, short)] = round3(c.MRR)
				out[fmt.Sprintf("eval_%s_%s_hit5", backend, short)] = round3(c.Hit5)
			}
		}
	}
	return out
}

func round3(f float64) float64 { return float64(int(f*1000+0.5)) / 1000 }

func writeJSONFile(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
