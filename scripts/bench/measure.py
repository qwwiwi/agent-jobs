"""Token, cost, time and limit numbers for benchmark runs (reads transcripts and stream files, prints JSON lines).

Usage: measure.py RUN_DIR... (env BENCH, AGENT_JOBS_HOME). Parent = main chain of the parent session;
worker = the subagent's sidechain files, or the job's own session (its transcript plus its subagents).
"""
import glob
import json
import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
from realworld import FIELDS, merge, new_acc, add, read_lines  # noqa: E402

PROJECTS = Path.home() / ".claude" / "projects"


def slug(p: Path) -> str:
    return "".join(c if c.isalnum() else "-" for c in str(p))


def session_usage(path: Path, main_only: bool) -> dict:
    seen: dict = {}
    for e in read_lines(str(path)):
        m = e.get("message")
        if e.get("type") != "assistant" or not isinstance(m, dict) or "usage" not in m:
            continue
        if main_only and e.get("isSidechain"):
            continue
        merge(seen, (m.get("id"), e.get("requestId")), m.get("model", "?"), m["usage"])
    acc = new_acc()
    for model, u in seen.values():
        add(acc, model, u)
    return acc


def plus(a: dict, b: dict) -> dict:
    return {k: a[k] + b[k] for k in a}


def cli_totals(stream: Path) -> tuple[float, float]:
    """(CLI total_cost_usd, sum of turn durations in seconds) from a stream-json file.

    total_cost_usd in a result event is cumulative for the session, also across --resume, so the
    largest one is the session total.
    """
    usd, busy = 0.0, 0.0
    for e in read_lines(str(stream)):
        if e.get("type") == "result":
            usd = max(usd, e.get("total_cost_usd") or 0.0)
            busy += (e.get("duration_ms") or 0) / 1000
    return usd, busy


def main() -> None:
    home = Path(os.environ["AGENT_JOBS_HOME"]) / "bench"
    for rd in sys.argv[1:]:
        run = Path(rd).resolve()
        r = json.loads((run / "result.json").read_text())
        pdir = PROJECTS / slug(run)
        parent = session_usage(pdir / f"{r['session_id']}.jsonl", main_only=True)
        worker = new_acc()
        for f in glob.glob(str(pdir / r["session_id"] / "subagents" / "*.jsonl")):
            worker = plus(worker, session_usage(Path(f), main_only=False))
        cli_usd, busy = cli_totals(run / "parent.jsonl")
        if r.get("job_id"):
            jdir = home / "jobs" / r["job_id"]
            wdir = PROJECTS / slug(jdir / "work")
            for f in glob.glob(str(wdir / "*.jsonl")) + glob.glob(str(wdir / "*" / "subagents" / "*.jsonl")):
                worker = plus(worker, session_usage(Path(f), main_only=False))
            j_usd, _ = cli_totals(jdir / "stream.jsonl")
            cli_usd += j_usd
        lb, la = r["limits_before"], r["limits_after"]
        out = {"arm": r["arm"], "task": r["task"], "rep": r["rep"], "pass": r["acceptance"]["pass"],
               "wall_min": round(r["wall_sec"] / 60, 2), "parent_busy_min": round(busy / 60, 2),
               "parent": parent, "worker": worker, "total": plus(parent, worker), "cli_usd": round(cli_usd, 4),
               "five_hour_delta": round((la.get("five_hour") or 0) - (lb.get("five_hour") or 0), 3),
               "seven_day_delta": round((la.get("seven_day") or 0) - (lb.get("seven_day") or 0), 3),
               "five_hour_before": lb.get("five_hour"), "five_hour_after": la.get("five_hour"),
               "window_reset_inside": (lb.get("five_hour_resets") != la.get("five_hour_resets"))}
        print(json.dumps(out))


if __name__ == "__main__":
    main()
