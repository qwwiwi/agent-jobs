"""Per-task token, time and main-session overhead for real subagent runs and agent-jobs jobs.

Reads Claude Code transcripts (usage fields and tool-call metadata only, no content is printed) and,
optionally, an agent-jobs state dir. Prints one JSON object: {"subagents": [...], "jobs": [...]}.

Usage: realworld.py --projects DIR [--project-filter SUBSTR] [--since YYYY-MM-DD] [--jobs-state DIR --jobs-meta FILE]
"""
import argparse
import glob
import json
import re
from collections import defaultdict
from pathlib import Path

PRICES = {  # $/MTok: input, cache write 5m, cache write 1h, cache read, output
    "claude-opus-5-5": (4, 5, 8, 0.20, 20), "claude-fable-5-1": (10, 12.5, 20, 0.25, 50),
    "claude-fable-5": (10, 12.5, 20, 1.0, 50), "claude-sonnet-5-5": (2, 2.5, 4, 0.20, 10),
    "claude-sonnet-5": (2, 2.5, 4, 0.20, 10), "claude-opus-4-8": (5, 6.25, 10, 0.5, 25),
    "claude-opus-4-7": (5, 6.25, 10, 0.5, 25), "claude-haiku-5-5": (0.1, 0.125, 0.2, 0.01, 0.5),
}
FIELDS = ("input", "cache_creation", "cache_read", "output")


def usage_cost(model: str, u: dict) -> float:
    p = PRICES.get(model)
    if not p:
        return 0.0
    cc = u.get("cache_creation_input_tokens") or 0
    split = u.get("cache_creation") or {}
    w1h = split.get("ephemeral_1h_input_tokens")
    w5m = split.get("ephemeral_5m_input_tokens")
    if w1h is None and w5m is None:
        w1h, w5m = cc, 0  # Claude Code writes 1h cache entries by default
    return ((u.get("input_tokens") or 0) * p[0] + (w5m or 0) * p[1] + (w1h or 0) * p[2]
            + (u.get("cache_read_input_tokens") or 0) * p[3] + (u.get("output_tokens") or 0) * p[4]) / 1e6


def add(acc: dict, model: str, u: dict) -> None:
    acc["input"] += u.get("input_tokens") or 0
    acc["cache_creation"] += u.get("cache_creation_input_tokens") or 0
    acc["cache_read"] += u.get("cache_read_input_tokens") or 0
    acc["output"] += u.get("output_tokens") or 0
    acc["usd"] += usage_cost(model, u)
    acc["calls"] += 1


def new_acc() -> dict:
    return {"input": 0, "cache_creation": 0, "cache_read": 0, "output": 0, "usd": 0.0, "calls": 0}


def merge(seen: dict, key, model: str, u: dict) -> None:
    """Streamed messages repeat one usage per content block; keep the largest value of each field."""
    if key not in seen:
        seen[key] = (model, dict(u))
        return
    old = seen[key][1]
    for f in ("input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "output_tokens"):
        old[f] = max(old.get(f) or 0, u.get(f) or 0)


def kind_of(text: str) -> str:
    t = text.lower()
    if re.search(r"review|ревью|audit|аудит|critic|verify|провер", t):
        return "review"
    if re.search(r"research|исслед|ресёрч|ресерч|search|sweep|explore|find|collect|scan|read|digest|metric|stats|анализ|сбор", t):
        return "research"
    if re.search(r"fix|implement|build|code|pr\b|refactor|write|patch|deploy|migrat|test|чин|фикс|сдела|пиш", t):
        return "code"
    return "other"


def read_lines(path: str):
    with open(path, errors="replace") as f:
        for line in f:
            try:
                yield json.loads(line)
            except ValueError:
                continue


def scan_main(path: str, since: str) -> dict:
    """Main-chain calls, compactions, Agent / job tool calls and result sizes of one session file."""
    calls, compacts, uses, results = [], [], {}, {}
    seen = {}
    last_ts = ""
    for e in read_lines(path):
        ts = e.get("timestamp") or last_ts
        last_ts = ts
        if e.get("type") == "system" and e.get("subtype") == "compact_boundary":
            compacts.append(ts)
        if e.get("type") == "queue-operation" and "<task-notification>" in str(e.get("content", "")):
            c = str(e["content"])
            mt = re.search(r"<tool-use-id>(\S+?)</tool-use-id>", c)
            if mt and mt.group(1) in uses:
                prev = results.get(mt.group(1), {"chars": 0})
                results[mt.group(1)] = {"ts": ts, "chars": prev["chars"] + len(c), "notified": True}
        if e.get("isSidechain"):
            continue
        m = e.get("message")
        if not isinstance(m, dict):
            continue
        if e.get("type") == "assistant" and "usage" in m:
            k = (m.get("id"), e.get("requestId"))
            if k not in seen:
                seen[k] = len(calls)
                calls.append((ts, m.get("model", "?"), dict(m["usage"])))
            else:
                old = calls[seen[k]][2]
                old["output_tokens"] = max(old.get("output_tokens") or 0, m["usage"].get("output_tokens") or 0)
        for b in m.get("content") if isinstance(m.get("content"), list) else []:
            if b.get("type") == "tool_use" and (b.get("name") in ("Agent", "Task") or "agent-jobs__job_" in b.get("name", "")):
                inp = b.get("input") or {}
                uses[b["id"]] = {"name": b["name"], "ts": ts, "bg": bool(inp.get("run_in_background")),
                                 "brief_chars": len(json.dumps(inp, ensure_ascii=False)),
                                 "kind_hint": kind_of(str(inp.get("description", "")) + " " + str(inp.get("title", "")) + " " + str(inp.get("kind", "")))}
            if b.get("type") == "tool_result" and b.get("tool_use_id") in uses:
                c = b.get("content")
                n = len(c) if isinstance(c, str) else len(json.dumps(c, ensure_ascii=False))
                if b["tool_use_id"] not in results:
                    results[b["tool_use_id"]] = {"ts": ts, "chars": n}
    return {"calls": [c for c in calls if c[0] >= since], "all_calls": calls, "compacts": sorted(compacts),
            "uses": uses, "results": results}


def overhead(main: dict, tool_id: str) -> dict:
    """Main-session cost a delegated task added: its brief, its result re-read until the next compaction."""
    u, r = main["uses"].get(tool_id), main["results"].get(tool_id)
    out = {"brief_tokens": 0, "result_tokens": 0, "reread_calls": 0, "overhead_tokens_cache_read": 0,
           "overhead_tokens_write": 0, "busy_sec": 0.0}
    if not u:
        return out
    out["brief_tokens"] = u["brief_chars"] // 4
    if not r:
        return out
    rt = r["chars"] // 4
    nxt = [c for c in main["compacts"] if c > r["ts"]]
    end = nxt[0] if nxt else "9999"
    n = sum(1 for c in main["all_calls"] if r["ts"] < c[0] < end)
    out.update(result_tokens=rt, reread_calls=n, overhead_tokens_cache_read=rt * max(n - 1, 0),
               overhead_tokens_write=rt + out["brief_tokens"])
    if not u["bg"] and u["name"] in ("Agent", "Task") and r["ts"] and u["ts"]:
        from datetime import datetime
        f = lambda s: datetime.fromisoformat(s.replace("Z", "+00:00"))
        out["busy_sec"] = (f(r["ts"]) - f(u["ts"])).total_seconds()
    return out


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--projects", required=True)
    ap.add_argument("--project-filter", default="")
    ap.add_argument("--since", default="2026-10-12")
    ap.add_argument("--jobs-state")
    ap.add_argument("--jobs-meta")
    a = ap.parse_args()
    mains = {}
    for f in glob.glob(f"{a.projects}/*{a.project_filter}*/*.jsonl"):
        mains[Path(f).stem] = scan_main(f, a.since)
    # subagents: aggregate nested spawns into the top-level one
    subs = {}
    for f in glob.glob(f"{a.projects}/*{a.project_filter}*/*/subagents/*.jsonl"):
        meta_p = Path(f).with_suffix(".meta.json")
        meta = json.loads(meta_p.read_text()) if meta_p.exists() else {}
        sid = Path(f).parent.parent.name
        acc, first, last, seen = new_acc(), None, None, {}
        models = defaultdict(int)
        for e in read_lines(f):
            ts = e.get("timestamp")
            if ts:
                first = first or ts
                last = ts
            m = e.get("message")
            if e.get("type") == "assistant" and isinstance(m, dict) and "usage" in m:
                merge(seen, (m.get("id"), e.get("requestId")), m.get("model", "?"), m["usage"])
        for (model, u) in seen.values():
            add(acc, model, u)
            models[model] += 1
        aid = Path(f).stem.replace("agent-", "")
        subs[aid] = {"session": sid, "meta": meta, "acc": acc, "first": first, "last": last, "models": dict(models)}
    def root(aid: str) -> str:
        seen = set()
        while subs.get(aid, {}).get("meta", {}).get("parentAgentId") in subs and aid not in seen:
            seen.add(aid)
            aid = subs[aid]["meta"]["parentAgentId"]
        return aid
    tasks = {}
    for aid, s in subs.items():
        r = root(aid)
        t = tasks.setdefault(r, {"children": 0, "acc": new_acc()})
        for k in t["acc"]:
            t["acc"][k] += s["acc"][k]
        if r != aid:
            t["children"] += 1
    from datetime import datetime
    f = lambda s: datetime.fromisoformat(s.replace("Z", "+00:00"))
    out_subs = []
    for r, t in tasks.items():
        s = subs[r]
        if not s["first"] or s["first"] < a.since:
            continue
        meta = s["meta"]
        main = mains.get(s["session"], {"uses": {}, "results": {}, "compacts": [], "all_calls": []})
        tid = meta.get("toolUseId", "")
        hint = main["uses"].get(tid, {}).get("kind_hint") or kind_of(meta.get("description", "") + " " + meta.get("agentType", ""))
        out_subs.append({"day": s["first"][:10], "shape": meta.get("requestShape", "?"), "model": meta.get("model", "?"),
                         "agent_type": meta.get("agentType", "?"), "kind": hint, "nested": t["children"],
                         "wall_sec": (f(s["last"]) - f(s["first"])).total_seconds(), **t["acc"],
                         "main_overhead": overhead(main, tid)})
    out_jobs = []
    if a.jobs_state and a.jobs_meta:
        meta = {j["id"]: j for j in json.loads(Path(a.jobs_meta).read_text())}
        jt = {}  # job id -> tool ids of job_result calls in the main session
        for m in mains.values():
            for tid, u in m["uses"].items():
                jt.setdefault(u["name"], []).append((m, tid))
        for d in sorted(glob.glob(f"{a.jobs_state}/jobs/*/")):
            jid = Path(d).name
            st = Path(d) / "stream.jsonl"
            if jid not in meta or not st.exists():
                continue
            # modelUsage in a result event is cumulative for its claude process; a new process (attempt)
            # starts with a system init event, so keep the last result of each process and sum them
            acc, segs = new_acc(), [None]
            for e in read_lines(str(st)):
                if e.get("type") == "system" and e.get("subtype") == "init" and segs[-1] is not None:
                    segs.append(None)
                if e.get("type") == "result":
                    segs[-1] = e
            for e in segs:
                if e is None:
                    continue
                for model, mu in (e.get("modelUsage") or {}).items():
                    acc["input"] += mu.get("inputTokens") or 0
                    acc["cache_creation"] += mu.get("cacheCreationInputTokens") or 0
                    acc["cache_read"] += mu.get("cacheReadInputTokens") or 0
                    acc["output"] += mu.get("outputTokens") or 0
                    acc["usd"] += mu.get("costUSD") or 0
                acc["calls"] += e.get("num_turns") or 0
            j = meta[jid]
            out_jobs.append({"day": jid[:6], "kind": j["kind"], "model": j["model"], "status": j["status"],
                             "wall_sec": j["duration_sec"], "reported_usd": j["cost_usd"], **acc})
        # main-session overhead of jobs: job_start briefs and job_result reads, averaged per job
        ov = new_acc() | {"brief_tokens": 0, "result_tokens": 0, "overhead_tokens_cache_read": 0, "overhead_tokens_write": 0, "n_start": 0, "n_result": 0}
        for name, lst in jt.items():
            for m, tid in lst:
                o = overhead(m, tid)
                if name.endswith("job_start"):
                    ov["n_start"] += 1
                if name.endswith("job_result"):
                    ov["n_result"] += 1
                for k in ("brief_tokens", "result_tokens", "overhead_tokens_cache_read", "overhead_tokens_write"):
                    ov[k] += o[k]
        out_jobs.append({"main_overhead_total": {k: v for k, v in ov.items() if k not in ("usd", "calls", "input", "output", "cache_read", "cache_creation")}})
    print(json.dumps({"subagents": out_subs, "jobs": out_jobs}))


if __name__ == "__main__":
    main()
