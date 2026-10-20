"""Carry cost of subagent hand-offs in a real main session, against the job alternative.

A subagent's result (the Agent tool_result, plus the <task-notification> of a background
subagent) stays in the parent's context and is re-read on every later parent call until the
next compaction. A job hands back one line, but its parent needs about four extra calls at the
real context size (controlled benchmark: 8.5 parent calls against 4.0 / 4.7).

Reads usage fields, tool ids and text lengths only; prints aggregates, never content.

Usage: carry_cost.py --projects DIR --project-filter SUBSTR --since YYYY-MM-DD
                     --bench-results FILE [--bench-projects-filter SUBSTR] [--json OUT]
"""
import argparse
import glob
import json
import re
import statistics
from collections import defaultdict
from pathlib import Path

CR, CW1H = 0.20, 8.0  # Opus 5.5 $/MTok: cache read, cache write 1h (benchmark price table)
JOB_LINE_TOKENS = 30  # the job-done line
EXTRA_PARENT_CALLS = 4  # benchmark: job parent 8.5 calls against fg 4.0 / bg 4.7


def read_lines(path):
    with open(path, errors="replace") as f:
        for i, line in enumerate(f):
            try:
                yield i, json.loads(line)
            except ValueError:
                continue


def ctx_of(u):
    return (u.get("input_tokens") or 0) + (u.get("cache_creation_input_tokens") or 0) + (u.get("cache_read_input_tokens") or 0)


def text_len(c):
    if isinstance(c, str):
        return len(c)
    if isinstance(c, list):
        n = 0
        for b in c:
            if isinstance(b, dict) and b.get("type") == "text":
                n += len(b.get("text") or "")
            elif isinstance(b, dict):
                n += len(json.dumps(b, ensure_ascii=False))
        return n
    return len(json.dumps(c, ensure_ascii=False)) if c is not None else 0


def scan(path):
    """Main-chain calls (line, usage), compactions, Agent uses and the fragments each hand-off put into the context."""
    calls, call_idx, compacts = [], {}, []
    uses = {}  # tool_use_id -> {line, bg, call_line}
    frags = defaultdict(list)  # tool_use_id -> [(line, chars, kind)]
    for ln, e in read_lines(path):
        if e.get("type") == "system" and e.get("subtype") == "compact_boundary" and not e.get("isSidechain"):
            compacts.append(ln)
            continue
        if e.get("isSidechain"):
            continue
        if e.get("type") == "attachment":
            a = e.get("attachment") or {}
            s = json.dumps(a, ensure_ascii=False)
            if "<task-notification>" in s:
                for m in re.finditer(r"<tool-use-id>(\S+?)</tool-use-id>", s):
                    if m.group(1) in uses:
                        frags[m.group(1)].append((ln, len(s), "notif"))
            continue
        m = e.get("message")
        if not isinstance(m, dict):
            continue
        if e.get("type") == "assistant" and "usage" in m:
            k = m.get("id") or e.get("requestId")
            if m.get("model") == "<synthetic>":
                continue  # local placeholder, not an API call
            if k not in call_idx:
                call_idx[k] = len(calls)
                calls.append([ln, m.get("model", "?"), dict(m["usage"])])
            else:
                old = calls[call_idx[k]][2]
                old["output_tokens"] = max(old.get("output_tokens") or 0, m["usage"].get("output_tokens") or 0)
            for b in m.get("content") if isinstance(m.get("content"), list) else []:
                if b.get("type") == "tool_use" and b.get("name") in ("Agent", "Task"):
                    inp = b.get("input") or {}
                    uses[b["id"]] = {"line": ln, "bg": bool(inp.get("run_in_background")), "call": call_idx[k]}
            continue
        c = m.get("content")
        if e.get("type") == "user":
            if isinstance(c, list):
                tr = [b for b in c if isinstance(b, dict) and b.get("type") == "tool_result"]
                for b in tr:
                    if b.get("tool_use_id") in uses:
                        n = text_len(b.get("content"))
                        frags[b["tool_use_id"]].append((ln, n, "tool_result"))
            s = c if isinstance(c, str) else json.dumps(c, ensure_ascii=False)
            if "<task-notification>" in s:
                for mt in re.finditer(r"<tool-use-id>(\S+?)</tool-use-id>", s):
                    if mt.group(1) in uses:
                        frags[mt.group(1)].append((ln, len(s), "notif"))
    return calls, compacts, uses, frags


def calibrate(files):
    """Chars per token from the real tokenizer: a task notification that opens a turn is the only new
    text besides a fixed-size hook context, so the context growth between the last call before it and
    the first call after it is linear in its length. Least squares over all such turns: slope = tokens
    per char, intercept = the hook context."""
    pts = []
    for p in files:
        prev, pend, ok = None, None, True
        for _, e in read_lines(p):
            if e.get("isSidechain"):
                continue
            t, m = e.get("type"), e.get("message") if isinstance(e.get("message"), dict) else None
            if t == "system" and e.get("subtype") == "compact_boundary":
                prev, pend = None, None
                continue
            if t == "assistant" and m and "usage" in m and m.get("model") != "<synthetic>":
                if prev and m.get("id") == prev[0]:
                    prev = (prev[0], prev[1], max(prev[2], m["usage"].get("output_tokens") or 0))
                    continue
                if pend and ok and prev:
                    pts.append((pend, ctx_of(m["usage"]) - ctx_of(prev[1]) - prev[2]))
                pend, ok = None, True
                prev = (m.get("id"), m["usage"], m["usage"].get("output_tokens") or 0)
                continue
            if t == "attachment" and pend is not None:
                ok = ok and (e.get("attachment") or {}).get("type") == "hook_additional_context"
            elif t == "user" and m:
                c = m.get("content")
                if isinstance(c, str) and c.startswith("<task-notification>") and pend is None and prev:
                    pend = len(c)
                elif pend is not None:
                    ok = False
    pts = [(x, y) for x, y in pts if y > 0]
    if len(pts) < 20:
        return 3.5, 0, len(pts)
    mx, my = statistics.mean(x for x, _ in pts), statistics.mean(y for _, y in pts)
    b = sum((x - mx) * (y - my) for x, y in pts) / sum((x - mx) ** 2 for x, _ in pts)
    return 1 / b, my - b * mx, len(pts)


def bench_followup(projects, filt):
    """Chars a job-mode benchmark parent took in after the job finished: the job-done line, then the
    tool results of its follow-up calls (job_result, diff check, cherry-pick, verification)."""
    out = []
    for p in sorted(glob.glob(f"{projects}/*{filt}job-t*/*.jsonl")):
        line_chars, n, resumed = 0, 0, False
        for _, e in read_lines(p):
            if e.get("isSidechain") or e.get("type") != "user":
                continue
            c = (e.get("message") or {}).get("content")
            if isinstance(c, str) and c.startswith("[jobs:job-done]"):
                resumed, line_chars = True, len(c)
            elif resumed and isinstance(c, list):
                n += sum(text_len(b.get("content")) for b in c if isinstance(b, dict) and b.get("type") == "tool_result")
        out.append((Path(p).parent.name.split("runs-")[-1], line_chars, n))
    return out


def pct(xs, q):
    if not xs:
        return 0
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(round(q * (len(xs) - 1))))]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--projects", required=True)
    ap.add_argument("--project-filter", required=True)
    ap.add_argument("--since", required=True)
    ap.add_argument("--bench-results", required=True)
    ap.add_argument("--bench-projects-filter", default="aj-bench-runs-")
    ap.add_argument("--json")
    a = ap.parse_args()

    bench = [json.loads(l) for l in open(a.bench_results)]
    by = defaultdict(list)
    for r in bench:
        by[r["arm"]].append(r)
    bcost = {k: statistics.mean(r["total"]["usd"] for r in v) for k, v in by.items()}
    bcalls = {k: statistics.mean(r["parent"]["calls"] for r in v) for k, v in by.items()}
    jp = by["job"]
    bctx = statistics.mean((r["parent"]["cache_read"] + r["parent"]["cache_creation"] + r["parent"]["input"]) / r["parent"]["calls"] for r in jp)

    fu = bench_followup(a.projects, a.bench_projects_filter)
    fu_tok = [(l + n) / 3.5 for _, l, n in fu]  # benchmark text is English: 3.5
    job_carry_line = statistics.median(fu_tok) if fu_tok else JOB_LINE_TOKENS

    files = [p for p in glob.glob(f"{a.projects}/{a.project_filter}/*.jsonl")]
    rows, sessions = [], 0
    models = defaultdict(int)
    for p in files:
        calls, compacts, uses, frags = scan(p)
        if not uses:
            continue
        sessions += 1
        for cl in calls:
            models[cl[1]] += 1
        for tid, u in uses.items():
            fr = frags.get(tid)
            if not fr:
                continue
            first_line = min(f[0] for f in fr)
            last_line = max(f[0] for f in fr)
            chars = sum(f[1] for f in fr)
            nxt_c = [c for c in compacts if c > last_line]
            end = nxt_c[0] if nxt_c else 10 ** 12
            later = [cl for cl in calls if last_line < cl[0] < end]
            pre_c = [c for c in compacts if c < u["line"]]
            rows.append({"session": Path(p).stem[:8], "tid": tid, "bg": u["bg"], "line": u["line"], "chars": chars,
                         "later": later, "ended_by_compact": bool(nxt_c), "after_compact": bool(pre_c),
                         "n_frag": len(fr)})
    # timestamps for date filter and per-day grouping
    ts_of = {}
    for p in files:
        want = {r["line"] for r in rows if r["session"] == Path(p).stem[:8]}
        if not want:
            continue
        for ln, e in read_lines(p):
            if ln in want:
                ts_of[(Path(p).stem[:8], ln)] = e.get("timestamp") or ""
    ratio, hook_tok, n_cal = calibrate(files)
    out_rows = []
    for r in rows:
        ts = ts_of.get((r["session"], r["line"]), "")
        if not ts or ts < a.since:
            continue
        rt = r["chars"] / ratio
        later = r["later"]
        pos = ctx_of(later[0][2]) if later else 0
        # each later call re-reads the result from cache, or re-writes it when the cache had expired
        sub_carry_usd, jl_usd, j30_usd, miss = 0.0, 0.0, 0.0, 0
        for cl in later:
            u = cl[2]
            hit = (u.get("cache_read_input_tokens") or 0) >= 0.9 * pos
            price = CR if hit else CW1H
            miss += 0 if hit else 1
            sub_carry_usd += rt * price / 1e6
            j30_usd += JOB_LINE_TOKENS * price / 1e6
            jl_usd += job_carry_line * price / 1e6
        sub_write = rt * CW1H / 1e6
        ctx_real = statistics.mean(ctx_of(cl[2]) for cl in later[:5]) if later else pos
        extra_usd = EXTRA_PARENT_CALLS * max(ctx_real - bctx, 0) * CR / 1e6  # benchmark already priced 4 calls at ~bctx
        arm = "bg" if r["bg"] else "fg"
        out_rows.append({
            "day": ts[:10], "session": r["session"], "arm": arm, "result_tokens": round(rt), "later_calls": len(later),
            "cache_miss_calls": miss, "carry_tokens": round(rt * len(later)), "ctx_at_handoff": pos, "ctx_real": round(ctx_real),
            "ended_by_compact": r["ended_by_compact"], "after_compact": r["after_compact"],
            "sub_carry_usd": sub_carry_usd + sub_write, "job_carry30_usd": j30_usd + JOB_LINE_TOKENS * CW1H / 1e6,
            "job_carry_real_usd": jl_usd + job_carry_line * CW1H / 1e6, "job_extra_usd": extra_usd,
            "sub_total": bcost[arm] + sub_carry_usd + sub_write,
            "job_total30": bcost["job"] + j30_usd + JOB_LINE_TOKENS * CW1H / 1e6 + extra_usd,
            "job_total_real": bcost["job"] + jl_usd + job_carry_line * CW1H / 1e6 + extra_usd,
        })

    R = out_rows
    def dist(key):
        xs = [r[key] for r in R]
        return {"median": pct(xs, .5), "p90": pct(xs, .9), "mean": statistics.mean(xs) if xs else 0, "sum": sum(xs)}
    days = defaultdict(lambda: defaultdict(float))
    for r in R:
        d = days[r["day"]]
        d["n"] += 1
        for k in ("carry_tokens", "sub_carry_usd", "job_carry30_usd", "job_carry_real_usd", "job_extra_usd", "sub_total", "job_total30", "job_total_real"):
            d[k] += r[k]
    # break-even: job cheaper when carry_tokens*CR > gap; gap per hand-off at the median real context
    ctx_med = pct([r["ctx_real"] for r in R], .5)
    gap = {arm: bcost["job"] - bcost[arm] + EXTRA_PARENT_CALLS * max(ctx_med - bctx, 0) * CR / 1e6 for arm in ("fg", "bg")}
    be_tok = {arm: g / (CR / 1e6) for arm, g in gap.items()}
    above30 = sum(1 for r in R if r["job_total30"] < r["sub_total"])
    above_real = sum(1 for r in R if r["job_total_real"] < r["sub_total"])
    res = {
        "sessions": sessions, "handoffs": len(R), "fg": sum(r["arm"] == "fg" for r in R), "bg": sum(r["arm"] == "bg" for r in R),
        "chars_per_token": round(ratio, 2), "calib_turns": n_cal, "calib_hook_tokens": round(hook_tok), "main_models": dict(models),
        "bench_cost": bcost, "bench_parent_calls": bcalls, "bench_parent_ctx_per_call": round(bctx),
        "bench_job_followup_tokens": [(k, round(l / 3.5), round(n / 3.5)) for k, l, n in fu], "job_carry_line_tokens": round(job_carry_line),
        "result_tokens": dist("result_tokens"), "later_calls": dist("later_calls"), "carry_tokens": dist("carry_tokens"),
        "ctx_real": dist("ctx_real"), "cache_miss_calls": dist("cache_miss_calls"),
        "sub_carry_usd": dist("sub_carry_usd"), "job_extra_usd": dist("job_extra_usd"),
        "job_carry30_usd": dist("job_carry30_usd"), "job_carry_real_usd": dist("job_carry_real_usd"),
        "sub_total": dist("sub_total"), "job_total30": dist("job_total30"), "job_total_real": dist("job_total_real"),
        "per_day": {k: dict(v) for k, v in sorted(days.items())},
        "break_even_carry_tokens_at_median_ctx": {k: round(v) for k, v in be_tok.items()}, "ctx_median": ctx_med,
        "share_job_cheaper_30": above30 / len(R) if R else 0, "share_job_cheaper_real": above_real / len(R) if R else 0,
        "ended_by_compact": sum(r["ended_by_compact"] for r in R), "censored_by_session_end": sum(not r["ended_by_compact"] for r in R),
        "after_a_compact": sum(r["after_compact"] for r in R),
        "carry_usd_ended_by_compact": sum(r["sub_carry_usd"] for r in R if r["ended_by_compact"]),
        "carry_usd_censored": sum(r["sub_carry_usd"] for r in R if not r["ended_by_compact"]),
    }
    if a.json:
        Path(a.json).write_text("\n".join(json.dumps({k: v for k, v in r.items()}) for r in R) + "\n")
    print(json.dumps(res, indent=1, default=str))


if __name__ == "__main__":
    main()
