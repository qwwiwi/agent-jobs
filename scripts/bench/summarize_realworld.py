"""Summarize realworld.py output per task kind: medians of limit-weighted cost, tokens by class, time, overhead."""
import json
import statistics as st
import sys
from collections import defaultdict

REMAP = {"Explore": "research", "code-reviewer": "review", "codex:codex-rescue": "review", "Plan": "review"}


def med(xs):
    return round(st.median(xs), 3) if xs else None


def summarize(rows, overhead_key=True):
    g = defaultdict(list)
    for r in rows:
        k = REMAP.get(r.get("agent_type"), r["kind"])
        k = {"fix": "code", "general": "code"}.get(k, k)
        g[k].append(r)
        g["all"].append(r)
    out = {}
    for k, rs in g.items():
        o = {"n": len(rs), "usd_med": med([r["usd"] for r in rs]), "usd_sum": round(sum(r["usd"] for r in rs), 1),
             "wall_med_min": med([r["wall_sec"] / 60 for r in rs]),
             "tok_med": {f: med([r[f] for r in rs]) for f in ("input", "cache_creation", "cache_read", "output")},
             "calls_med": med([r["calls"] for r in rs])}
        if overhead_key and rs and "main_overhead" in rs[0]:
            o["ovh_result_tok_med"] = med([r["main_overhead"]["result_tokens"] for r in rs])
            o["ovh_reread_tok_med"] = med([r["main_overhead"]["overhead_tokens_cache_read"] for r in rs])
            o["ovh_write_tok_med"] = med([r["main_overhead"]["overhead_tokens_write"] for r in rs])
        out[k] = o
    return out


if __name__ == "__main__":
    d = json.load(open(sys.argv[1]))
    print(json.dumps({"subagents": summarize(d["subagents"]),
                      "jobs": summarize([j for j in d["jobs"] if "kind" in j], False),
                      "jobs_overhead": [j for j in d["jobs"] if "main_overhead_total" in j]}, indent=1, ensure_ascii=False))
