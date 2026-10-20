"""Aggregate Claude Code transcript usage per day, per session category and per model (prints totals only).

Usage: usage_by_day.py PROJECTS_DIR MAIN_FILTER
  PROJECTS_DIR  ~/.claude/projects of the agent's user
  MAIN_FILTER   substring of the project folder of the agent's main session
Categories: job (agent-jobs job work dirs), main (MAIN_FILTER), other. Chains: chain (main thread) or
side (subagents). Streamed messages repeat one usage per content block: the largest value per field is kept.
"""
import glob
import json
import sys
from collections import defaultdict

F = ["input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "output_tokens"]


def main() -> None:
    root, main_filter = sys.argv[1], sys.argv[2]
    msgs: dict = {}
    for f in glob.glob(root + "/**/*.jsonl", recursive=True):
        d = f[len(root):].split("/")[1]
        cat = "job" if ("agent-jobs-" in d and "-jobs-" in d) else ("main" if main_filter in d else "other")
        sub = "/subagents/" in f
        for line in open(f, errors="replace"):
            if '"usage"' not in line:
                continue
            try:
                e = json.loads(line)
            except ValueError:
                continue
            m = e.get("message")
            if not isinstance(m, dict) or "usage" not in m:
                continue
            k = (m.get("id"), e.get("requestId"))
            u = m["usage"]
            if k in msgs:
                old = msgs[k][4]
                for x in F:
                    old[x] = max(old.get(x) or 0, u.get(x) or 0)
                continue
            ch = "side" if (e.get("isSidechain") or sub) else "chain"
            msgs[k] = ((e.get("timestamp") or "")[:10], cat, ch, m.get("model", "?"), {x: u.get(x) or 0 for x in F})
    agg = defaultdict(lambda: [0, 0, 0, 0, 0])
    for day, cat, ch, model, u in msgs.values():
        a = agg[(day, cat, ch, model)]
        for i, x in enumerate(F):
            a[i] += u[x]
        a[4] += 1
    print(json.dumps([dict(day=k[0], cat=k[1], chain=k[2], model=k[3], input=v[0], cache_creation=v[1],
                           cache_read=v[2], output=v[3], calls=v[4]) for k, v in sorted(agg.items())]))


if __name__ == "__main__":
    main()
