#!/usr/bin/env python3
"""Benchmark absorb pass-2 candidates on scribe's REAL two-pass writer prompt.

Usage:
    SCRIBE_KB=~/Projects/mykb ./scripts/bench-absorb-pass2.py [N] [ENTITY_IDX]

Env:
    SCRIBE_KB      (required) KB root — supplies the raw article + persisted plan
    BENCH_SLUG     raw/articles/<slug>.md + output/absorb-plans/<slug>.json
    BENCH_MODELS   comma-separated Together model ids
    SCRIBE_REPO    scribe checkout (default: this script's parent repo)

Reads the Together key from ~/.config/scribe/config.yaml (`llm_api_key`).
Requires pyyaml; a dev-only tool, not a dependency of the binary.

Committed on purpose: the 2026-07-14 benchmark's scripts lived in a scratchpad
and were lost, so the 2026-08-10 re-check had to rebuild them from scratch.

Why this differs from the 2026-07-14 benchmark: that one used the single-pass
`absorb-ollama.md` prompt, and its own postmortem names this as the reason it
missed the empty-`{}` escape. The 2026-08-10 bodyless-envelope escape is the same
class of bug one level down. So this harness:

  * renders the real `absorb-pass2-json.md` with a real multi-entity plan,
  * uses json_schema strict + max_tokens=16384 (what ships),
  * scores BODY-POPULATED rate, not just parse/schema validity,
  * runs N high enough to sample a stochastic ~20-40% tail.

Scoring per call:
  parse_ok        response is a JSON object
  actions>=1      envelope has at least one action  (the July metric)
  bodies_ok       EVERY action has a non-empty body after frontmatter  (the new one)
  richness        related-links / tags / sections / body lines, as July measured
"""
import json, os, pathlib, re, statistics, sys, time, urllib.error, urllib.request

import yaml

KEY = (yaml.safe_load((pathlib.Path.home() / ".config/scribe/config.yaml").read_text()) or {})["llm_api_key"]
URL = "https://api.together.xyz/v1/chat/completions"
REPO = pathlib.Path(os.environ.get("SCRIBE_REPO", pathlib.Path(__file__).resolve().parent.parent))
KB = pathlib.Path(os.environ["SCRIBE_KB"])

N = int(sys.argv[1]) if len(sys.argv) > 1 else 20
MODELS = (os.environ.get("BENCH_MODELS")
          or "MiniMaxAI/MiniMax-M3,deepseek-ai/DeepSeek-V4-Flash-0731").split(",")

# The article that exposed the bug, with the REAL persisted pass-1 plan — not a
# hand-written stand-in. The first N=20 run used a 3-entity approximation and
# scored MiniMax 20/20, failing to reproduce the production failure at all; the
# plan payload is the main remaining difference from what sync actually sends.
SLUG = os.environ.get("BENCH_SLUG",
    "research-elixir-live-claude-engineer-2026-08-10-archive-plugin-source")
RAW = KB / "raw/articles" / f"{SLUG}.md"
PLAN = json.loads((KB / "output/absorb-plans" / f"{SLUG}.json").read_text())
# Entity index from ENT_IDX (default 4 = "Archive Release Bookkeeping Overhead",
# one that failed every corrective retry in production).
_i = int(sys.argv[2]) if len(sys.argv) > 2 else 4
_e = PLAN["entities"][_i]
ENTITY = {
    "label": _e["label"], "type": _e["type"], "one_line": _e.get("one_line", ""),
    "claims": " | ".join(_e.get("key_claims") or []) or "(none flagged)",
}


def render():
    tpl = (REPO / "cmd/scribe/prompts/absorb-pass2-json.md").read_text()
    body = RAW.read_text()
    vals = {
        "DOMAIN": PLAN.get("domain","general"), "ENTITY_KEY_CLAIMS": ENTITY["claims"],
        "ENTITY_LABEL": ENTITY["label"], "ENTITY_ONE_LINE": ENTITY["one_line"],
        "ENTITY_TYPE": ENTITY["type"], "FACTS": "", "PLAN_JSON": json.dumps(PLAN, indent=2),
        "RAW_BODY": body, "RAW_FILE": str(RAW), "TODAY": "2026-08-10",
    }
    for k, v in vals.items():
        tpl = tpl.replace("{{" + k + "}}", v)
    return tpl


SCHEMA = {"name": "WikiActionEnvelope", "strict": True, "schema": {
    "type": "object",
    "properties": {
        "entity": {"type": "string"}, "notes": {"type": "string"},
        "actions": {"type": "array", "minItems": 1, "items": {
            "type": "object",
            "properties": {"op": {"type": "string"}, "path": {"type": "string"},
                           "content": {"type": "string"}, "heading": {"type": "string"}},
            "required": ["op", "path"]}}},
    "required": ["entity", "actions"]}}


def body_after_fm(c):
    if c.startswith("---"):
        i = c.find("\n---", 3)
        if i >= 0:
            rest = c[i + 4:]
            nl = rest.find("\n")
            return rest[nl + 1:].strip() if nl >= 0 else ""
    return c.strip()


def call(model, prompt):
    payload = {"model": model, "messages": [{"role": "user", "content": prompt}],
               "max_tokens": 16384, "temperature": 0.3,
               "response_format": {"type": "json_schema", "json_schema": SCHEMA}}
    req = urllib.request.Request(URL, data=json.dumps(payload).encode(),
                                 headers={"Authorization": f"Bearer {KEY}",
                                          "Content-Type": "application/json",
                                          "User-Agent": "curl/8.4.0"})
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=420) as r:
            d = json.loads(r.read())
    except Exception as e:
        return {"err": f"{type(e).__name__}", "s": time.time() - t0}
    ch = (d.get("choices") or [{}])[0]
    txt = (ch.get("message") or {}).get("content") or ""
    rec = {"s": time.time() - t0, "finish": ch.get("finish_reason"),
           "out": (d.get("usage") or {}).get("completion_tokens"),
           "in": (d.get("usage") or {}).get("prompt_tokens")}
    try:
        env = json.loads(txt)
    except Exception:
        rec["parse_ok"] = False
        return rec
    rec["parse_ok"] = True
    acts = env.get("actions") or []
    rec["n_actions"] = len(acts)
    bodies = [body_after_fm(a.get("content", "") or "") for a in acts]
    rec["bodies_ok"] = bool(acts) and all(b for b in bodies)
    rec["body_chars"] = sum(len(b) for b in bodies)
    joined = "\n".join(a.get("content", "") or "" for a in acts)
    rec["related"] = len(re.findall(r"\[\[[^\]]+\]\]", joined))
    rec["sections"] = len(re.findall(r"^##\s", joined, re.M))
    return rec


prompt = render()
print(f"prompt chars={len(prompt)}  N={N} per model\n")
for m in MODELS:
    rows = [call(m, prompt) for _ in range(N)]
    ok = [r for r in rows if not r.get("err")]
    errs = len(rows) - len(ok)
    parse = sum(1 for r in ok if r.get("parse_ok"))
    acts = sum(1 for r in ok if r.get("n_actions", 0) >= 1)
    bods = sum(1 for r in ok if r.get("bodies_ok"))
    trunc = sum(1 for r in ok if r.get("finish") == "length")
    lat = sorted(r["s"] for r in ok) or [0]
    def med(k):
        v = [r[k] for r in ok if r.get(k) is not None]
        return statistics.median(v) if v else 0
    print(f"=== {m}")
    print(f"  transport errors : {errs}/{N}")
    print(f"  parse_ok         : {parse}/{len(ok)}")
    print(f"  actions>=1       : {acts}/{len(ok)}   (the July metric)")
    print(f"  BODIES POPULATED : {bods}/{len(ok)}   <-- the 2026-08-10 metric")
    print(f"  finish=length    : {trunc}/{len(ok)}")
    print(f"  latency p50/p90  : {lat[len(lat)//2]:.1f}s / {lat[int(len(lat)*0.9)-1]:.1f}s")
    print(f"  median body chars: {med('body_chars'):.0f}   related-links: {med('related'):.0f}   sections: {med('sections'):.0f}")
    print(f"  median tokens    : in {med('in'):.0f} / out {med('out'):.0f}\n")
