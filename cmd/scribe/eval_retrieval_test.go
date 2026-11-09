package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func evalArticle(title string, extra string) string {
	return "---\ntitle: \"" + title + "\"\ntype: pattern\ndomain: general\n" + extra + "---\n\n" +
		strings.Repeat("Body text about "+title+" with enough content to be sampled. ", 20) + "\n"
}

func TestEvalTitleWordsAndLeaks(t *testing.T) {
	words := evalTitleWords("Token Rate Limiter for the Caching Layer")
	if got := strings.Join(words, ","); got != "token,rate,limiter,caching,layer" {
		t.Fatalf("title words = %q", got)
	}
	cases := []struct {
		q    string
		leak bool
	}{
		{"how do I stop bursts of API calls from getting throttled", false},
		{"why does my rate keep dropping", true},  // exact word
		{"tokens run out under heavy load", true}, // inflection via shared 5-letter prefix
		{"requests pile up and the upstream starts rejecting", false},
		// Known limit, no stemmer: "cache" vs "caching" differ at letter 5.
		{"cache misses spike when traffic doubles", false},
	}
	for _, c := range cases {
		got := evalLeaksTitle(c.q, words)
		if got != c.leak {
			t.Errorf("%q: leak=%v, want %v", c.q, got, c.leak)
		}
	}
}

func TestSampleEvalArticles(t *testing.T) {
	root := t.TempDir()
	for i := range 6 {
		writeTestArticle(t, root, fmt.Sprintf("patterns/p%d.md", i), evalArticle(fmt.Sprintf("Pattern %d", i), ""))
	}
	writeTestArticle(t, root, "decisions/d1.md", evalArticle("Decision One", ""))
	writeTestArticle(t, root, "decisions/old.md", evalArticle("Old Decision", "status: superseded\n"))
	writeTestArticle(t, root, "tools/stub.md", "---\ntitle: \"Stub\"\ntype: tool\n---\n\nshort\n")
	writeTestArticle(t, root, "raw/articles/r.md", evalArticle("Raw Capture", ""))

	got := sampleEvalArticles(root, 3, 1)
	if len(got) != 3 {
		t.Fatalf("sampled %d, want 3", len(got))
	}
	rels := make([]string, 0, len(got))
	for _, c := range got {
		rels = append(rels, c.rel)
		if strings.HasPrefix(c.rel, "raw/") || c.rel == "decisions/old.md" || c.rel == "tools/stub.md" {
			t.Errorf("ineligible article sampled: %s", c.rel)
		}
	}
	if rels[0] != "decisions/d1.md" {
		t.Errorf("round-robin should take the only decisions article first, got %v", rels)
	}
	again := sampleEvalArticles(root, 3, 1)
	for i := range got {
		if again[i].rel != got[i].rel {
			t.Fatalf("same seed must give the same sample: %v vs %v", got, again)
		}
	}
}

func TestGenerateRetrievalFixture(t *testing.T) {
	root := t.TempDir()
	writeTestArticle(t, root, "patterns/limiter.md", evalArticle("Token Rate Limiter", ""))
	writeTestArticle(t, root, "solutions/wal.md", evalArticle("SQLite WAL Contention", ""))

	stub := &stubLLM{Rules: []*stubRule{
		{MatchPrompt: "Title: Token Rate Limiter", Reply: `{"direct":"how does the token rate limiter work","indirect":"how do I stop bursts of calls getting rejected upstream"}`},
		// indirect leaks a title word → dropped, direct kept
		{MatchPrompt: "Title: SQLite WAL Contention", Reply: "```json\n{\"direct\":\"sqlite wal contention with cron jobs\",\"indirect\":\"database locked errors when sqlite runs from two processes\"}\n```"},
	}}
	fx, stats, err := generateRetrievalFixture(context.Background(), root, stub, 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	if stats.articles != 2 || stats.leaked != 1 || stats.failed != 0 {
		t.Errorf("stats = %+v, want articles=2 leaked=1 failed=0", stats)
	}
	if len(fx.Queries) != 3 {
		t.Fatalf("queries = %d, want 3: %+v", len(fx.Queries), fx.Queries)
	}
	for _, q := range fx.Queries {
		if q.ExpectedInTop != 5 || len(q.ExpectedFiles) != 1 {
			t.Errorf("bad query shape: %+v", q)
		}
		if q.Type != evalTypeDirect && q.Type != evalTypeIndirect {
			t.Errorf("type %q not in qmd's vocabulary mapping", q.Type)
		}
	}
	for _, c := range stub.calls {
		if !strings.Contains(c.Prompt, "token, rate, limiter") && !strings.Contains(c.Prompt, "sqlite, contention") {
			t.Errorf("prompt does not list the forbidden title words:\n%s", c.Prompt)
		}
		if c.Op != "eval" {
			t.Errorf("op label = %q, want eval (cost ledger attribution)", c.Op)
		}
	}
}

func TestRunRetrievalEvalSkipsMissingAndAggregates(t *testing.T) {
	root := t.TempDir()
	writeTestArticle(t, root, "patterns/a.md", evalArticle("A", ""))
	fx := qmdBenchFixture{Version: 1, Queries: []qmdBenchQuery{
		{ID: "q1-direct", Query: "a", Type: evalTypeDirect, ExpectedFiles: []string{"patterns/a.md"}, ExpectedInTop: 5},
		{ID: "q1-indirect", Query: "b", Type: evalTypeIndirect, ExpectedFiles: []string{"patterns/a.md"}, ExpectedInTop: 5},
		{ID: "q2-direct", Query: "c", Type: evalTypeDirect, ExpectedFiles: []string{"patterns/gone.md"}, ExpectedInTop: 5},
	}}
	fixturePath := root + "/eval/retrieval-fixture.json"
	if err := writeJSONFile(fixturePath, fx); err != nil {
		t.Fatal(err)
	}

	var sentQueries int
	orig := runQMDBenchFn
	t.Cleanup(func() { runQMDBenchFn = orig })
	runQMDBenchFn = func(_, fixture, collection string) ([]byte, error) {
		data, _ := os.ReadFile(fixture)
		var sent qmdBenchFixture
		_ = json.Unmarshal(data, &sent)
		sentQueries = len(sent.Queries)
		if sent.Collection != "kb" || collection != "kb" {
			t.Errorf("collection not passed through: %q / %q", sent.Collection, collection)
		}
		return []byte(`{"timestamp":"t","results":[
		 {"id":"q1-direct","query":"a","type":"exact","backends":{
		   "bm25":{"precision_at_k":1,"recall":1,"mrr":1,"latency_ms":5,"top_files":["patterns/a.md"]},
		   "full":{"precision_at_k":1,"recall":1,"mrr":0.5,"latency_ms":900,"top_files":["x.md","patterns/a.md"]}}},
		 {"id":"q1-indirect","query":"b","type":"semantic","backends":{
		   "bm25":{"precision_at_k":0,"recall":0,"mrr":0,"latency_ms":7,"top_files":["x.md"]},
		   "full":{"precision_at_k":0,"recall":0,"mrr":0,"latency_ms":1100,"top_files":["dup.md"]}}}]}`), nil
	}

	r, err := runRetrievalEval(root, fixturePath, "kb")
	if err != nil {
		t.Fatal(err)
	}
	if r.Skipped != 1 || r.Queries != 2 || sentQueries != 2 {
		t.Errorf("skipped=%d queries=%d sent=%d, want 1/2/2", r.Skipped, r.Queries, sentQueries)
	}
	if c := r.Cells["full"]["all"]; c.N != 2 || c.MRR != 0.25 || c.Hit5 != 0.5 {
		t.Errorf("full/all = %+v, want n=2 mrr=0.25 hit5=0.5", c)
	}
	if c := r.Cells["bm25"][evalTypeIndirect]; c.MRR != 0 || c.N != 1 {
		t.Errorf("bm25/indirect = %+v", c)
	}

	out := formatEvalReport(r, &r)
	if !strings.Contains(out, "full     indirect") || !strings.Contains(out, "ΔMRR +0.00") {
		t.Errorf("report missing rows or deltas:\n%s", out)
	}
	if m := formatEvalMisses(r, 5); !strings.Contains(m, "q1-indirect") || !strings.Contains(m, "dup.md") {
		t.Errorf("misses should name the question and the wrong top hit:\n%s", m)
	}
	f := evalRunStatsFields(r)
	if f["eval_full_all_mrr"] != 0.25 || f["eval_queries"] != 2 {
		t.Errorf("run stats = %v", f)
	}
}
