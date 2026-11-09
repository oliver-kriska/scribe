//go:build benchtogether

package main

// Hosted-model benchmark over scribe's real code paths: prompts rendered by
// loadPrompt, the shipped json_schema + degrade cascade, extractJSON and the
// production parsers. Behind a build tag, so normal builds, tests and lint
// never see it. It produced the README §Hosted-model benchmark table:
//
//	go test -tags 'sqlite_fts5 benchtogether' -run TestBenchTogether -count=1 ./cmd/scribe/
//
// Env:
//
//	BENCH_TASK        preflight | pass2 | session
//	BENCH_MODELS      comma-separated Together model ids
//	BENCH_ROOT        scratch KB root holding a copy of scribe.yaml; the cost
//	                  ledger rows land here, never in the real KB
//	BENCH_SRC_KB      real KB for pass2 inputs (raw/articles + output/absorb-plans)
//	BENCH_PASS2_ITEMS slug:entityIdx,... (pass2)
//	BENCH_SESSIONS    session ids (session); BENCH_CCRIDER_DB the ccrider DB
//	BENCH_REPS        calls per item (default 1)
//	BENCH_OUT         jsonl of per-call metrics; BENCH_RAW_DIR keeps each
//	                  envelope's pages for a blind quality judge
//	TOGETHER_API_KEY  the TestMain scrubs HOME, so export the key
//
// Structural metrics cannot rank models on their own (2026-11-09: most scored
// 100% valid); judge the saved pages blind, with labels shuffled per item.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type benchRec struct {
	Model     string  `json:"model"`
	Task      string  `json:"task"`
	Item      string  `json:"item"`
	Rep       int     `json:"rep"`
	Secs      float64 `json:"secs"`
	Err       string  `json:"err,omitempty"`
	Actions   int     `json:"actions"`
	Bodyless  bool    `json:"bodyless"`
	BodyChars int     `json:"body_chars"`
	FMValid   int     `json:"fm_valid"`
	BadPaths  int     `json:"bad_paths"`
	Related   int     `json:"related"`
	Links     int     `json:"links"`
	Sections  int     `json:"sections"`
	MetaOK    bool    `json:"meta_ok"`
	Raw       string  `json:"raw,omitempty"`
	Full      string  `json:"-"`
}

var benchMu sync.Mutex

func benchWrite(t *testing.T, r benchRec) {
	benchMu.Lock()
	defer benchMu.Unlock()
	f, err := os.OpenFile(os.Getenv("BENCH_OUT"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	_, _ = f.Write(append(b, '\n'))
	if dir := os.Getenv("BENCH_RAW_DIR"); dir != "" && r.Full != "" {
		name := strings.NewReplacer("/", "_", ":", "_").Replace(r.Task + "__" + r.Model + "__" + r.Item + "__" + strconv.Itoa(r.Rep) + ".txt")
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, name), []byte(r.Full), 0o644)
	}
	t.Logf("%-45s %-8s %-28s rep%d %5.1fs act=%d body=%d err=%.80s", r.Model, r.Task, r.Item, r.Rep, r.Secs, r.Actions, r.BodyChars, r.Err)
}

var (
	benchWikilinkRE = regexp.MustCompile(`\[\[[^\]]+\]\]`)
	benchSectionRE  = regexp.MustCompile(`(?m)^## `)
)

// scoreEnvelope fills the structural metrics for one parsed envelope.
func scoreEnvelope(root string, env WikiActionEnvelope, r *benchRec) {
	r.Actions = len(env.Actions)
	var full strings.Builder
	for _, a := range env.Actions {
		full.WriteString("=== " + a.Op + " " + a.Path + "\n" + a.Content + "\n\n")
	}
	r.Full = full.String()
	r.Bodyless = len(env.Actions) > 0 && envelopeAllBodyless(env)
	for _, a := range env.Actions {
		if _, err := validateActionPath(root, a.Path); err != nil {
			r.BadPaths++
		}
		content := a.Content
		if fm, err := parseFrontmatter([]byte(content)); err == nil && fm != nil && fm.Title != "" && fm.Type != "" {
			r.FMValid++
			r.Related += len(toStringSlice(fm.Related))
		}
		body := content
		if sp, err := frontmatterSpan(content); err == nil && sp.BodyStart > 0 {
			body = content[sp.BodyStart:]
		}
		r.BodyChars += len(strings.TrimSpace(body))
		r.Links += len(benchWikilinkRE.FindAllString(body, -1))
		r.Sections += len(benchSectionRE.FindAllString(body, -1))
	}
}

func benchEnv(t *testing.T) (models []string, reps int, root string) {
	for _, m := range strings.Split(os.Getenv("BENCH_MODELS"), ",") {
		if m = strings.TrimSpace(m); m != "" {
			models = append(models, m)
		}
	}
	reps, _ = strconv.Atoi(os.Getenv("BENCH_REPS"))
	if reps < 1 {
		reps = 1
	}
	root = os.Getenv("BENCH_ROOT")
	if root == "" || len(models) == 0 || os.Getenv("BENCH_OUT") == "" {
		t.Skip("BENCH_ROOT, BENCH_MODELS, BENCH_OUT required")
	}
	os.Setenv("SCRIBE_KB", root)
	return models, reps, root
}

type benchJob struct {
	item string
	rep  int
	run  func(ctx context.Context, p *openaiCompatProvider, r *benchRec)
}

// runBench runs every job for every model: models in parallel, at most
// perModel calls in flight per model.
func runBench(t *testing.T, models []string, root, task string, jobs []benchJob, perModel int) {
	var wg sync.WaitGroup
	for _, m := range models {
		wg.Add(1)
		go func(model string) {
			defer wg.Done()
			p := newOpenAICompatProvider("together", model, root)
			sem := make(chan struct{}, perModel)
			var inner sync.WaitGroup
			for _, j := range jobs {
				inner.Add(1)
				sem <- struct{}{}
				go func(j benchJob) {
					defer inner.Done()
					defer func() { <-sem }()
					r := benchRec{Model: model, Task: task, Item: j.item, Rep: j.rep}
					start := time.Now()
					j.run(context.Background(), p, &r)
					r.Secs = time.Since(start).Seconds()
					benchWrite(t, r)
				}(j)
			}
			inner.Wait()
		}(m)
	}
	wg.Wait()
}

func TestBenchTogether(t *testing.T) {
	models, reps, root := benchEnv(t)
	switch task := os.Getenv("BENCH_TASK"); task {
	case "preflight":
		schema := jsonSchemaSpec{Name: "Probe", Schema: map[string]any{
			"type": "object", "properties": map[string]any{
				"answer": map[string]any{"type": "string"},
				"items":  map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}},
			}, "required": []any{"answer", "items"},
		}}
		jobs := []benchJob{{item: "probe", run: func(ctx context.Context, p *openaiCompatProvider, r *benchRec) {
			cctx, cancel := context.WithTimeout(withOpLabel(ctx, "bench-preflight"), 3*time.Minute)
			defer cancel()
			out, err := p.GenerateJSONSchema(cctx, "Name three primary colors. Reply as JSON with an `answer` sentence and an `items` list.", schema)
			if err != nil {
				r.Err = err.Error()
				return
			}
			r.Raw = truncateBytes(out, 300)
			var v map[string]any
			if js, ok := extractJSON(out); ok && json.Unmarshal([]byte(js), &v) == nil {
				if items, _ := v["items"].([]any); len(items) > 0 {
					r.MetaOK = true
				}
			}
		}}}
		runBench(t, models, root, task, jobs, 1)
	case "pass2":
		runPass2Bench(t, models, reps, root)
	case "session":
		runSessionBench(t, models, reps, root)
	default:
		t.Skip("BENCH_TASK must be preflight|pass2|session")
	}
}

func runPass2Bench(t *testing.T, models []string, reps int, root string) {
	src := os.Getenv("BENCH_SRC_KB")
	var jobs []benchJob
	for _, spec := range strings.Split(os.Getenv("BENCH_PASS2_ITEMS"), ",") {
		// slug:entityIdx
		slug, idxS, _ := strings.Cut(strings.TrimSpace(spec), ":")
		idx, _ := strconv.Atoi(idxS)
		planData, err := os.ReadFile(filepath.Join(src, "output", "absorb-plans", slug+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var plan absorbPlan
		if err := json.Unmarshal(planData, &plan); err != nil {
			t.Fatal(err)
		}
		rawPath := filepath.Join(src, "raw", "articles", slug+".md")
		rawBody, err := os.ReadFile(rawPath)
		if err != nil {
			t.Fatal(err)
		}
		ent := plan.Entities[idx]
		claims := strings.Join(ent.KeyClaims, " | ")
		if claims == "" {
			claims = "(none flagged)"
		}
		domain := plan.Domain
		if domain == "" {
			domain = "general"
		}
		prompt, err := loadPrompt("absorb-pass2-json.md", map[string]string{
			"KB_DIR": root, "RAW_FILE": "raw/articles/" + slug + ".md", "PLAN_FILE": "output/absorb-plans/" + slug + ".json",
			"ENTITY_LABEL": ent.Label, "ENTITY_TYPE": ent.Type, "ENTITY_ONE_LINE": ent.OneLine,
			"ENTITY_KEY_CLAIMS": claims, "DOMAIN": domain, "FACTS": "",
			"RAW_BODY": string(rawBody), "PLAN_JSON": string(planData), "TODAY": "2026-11-09",
		})
		if err != nil {
			t.Fatal(err)
		}
		item := shortItem(slug) + ":" + idxS
		for rep := 1; rep <= reps; rep++ {
			jobs = append(jobs, benchJob{item: item, rep: rep, run: func(ctx context.Context, p *openaiCompatProvider, r *benchRec) {
				env, raw, err := runPass2JSONOnce(ctx, p, prompt, 6*time.Minute)
				if err != nil {
					r.Err = err.Error()
					r.Raw = truncateBytes(raw, 400)
					return
				}
				scoreEnvelope(root, env, r)
				if r.Bodyless {
					r.Raw = truncateBytes(raw, 400)
				}
			}})
		}
	}
	runBench(t, models, root, "pass2", jobs, 4)
}

func shortItem(slug string) string {
	parts := strings.Split(slug, "-")
	if len(parts) > 5 {
		parts = parts[len(parts)-4:]
	}
	return strings.Join(parts, "-")
}

func runSessionBench(t *testing.T, models []string, reps int, root string) {
	db := os.Getenv("BENCH_CCRIDER_DB")
	ids := strings.Split(os.Getenv("BENCH_SESSIONS"), ",")
	sort.Strings(ids)
	var jobs []benchJob
	for _, id := range ids {
		id = strings.TrimSpace(id)
		turns, err := fetchSessionTranscript(db, id)
		if err != nil {
			t.Logf("skip %s: %v", id, err)
			continue
		}
		transcript := renderTranscriptForPrompt(turns, 24000)
		if transcript == "" {
			t.Logf("skip %s: empty transcript", id)
			continue
		}
		project := lookupSessionProjectPath(db, id)
		related := formatRelatedSessions(queryRelatedSessions(db, id, 7, 10))
		for rep := 1; rep <= reps; rep++ {
			jobs = append(jobs, benchJob{item: id[:8], rep: rep, run: func(ctx context.Context, p *openaiCompatProvider, r *benchRec) {
				env, err := runSessionEnvelopeOnce(ctx, p, "bench-session", "session-mine", project, id, related, transcript, nil, 6*time.Minute, "")
				if err != nil {
					r.Err = err.Error()
					return
				}
				r.MetaOK = len(env.Meta) > 0
				scoreEnvelope(root, env, r)
			}})
		}
	}
	runBench(t, models, root, "session", jobs, 4)
}
