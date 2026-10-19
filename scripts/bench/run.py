"""Run one benchmark arm (fg | bg | job) on one task and record tokens, limits, time, acceptance.

Usage: run.py ARM TASK REP   (env BENCH = benchmark root, AGENT_JOBS_HOME = isolated agent-jobs state)
Writes BENCH/runs/<arm>-<task>-<rep>/result.json.
"""
import json
import os
import shutil
import subprocess
import sys
import time
import uuid
from pathlib import Path

BENCH = Path(os.environ["BENCH"])
AJ = BENCH / "bin" / "agent-jobs"
MODEL = "opus"
FIXTURE = {"t1": "t1-fix", "t2": "t2-leads", "t3": "t3-report"}
KIND = {"t1": "fix", "t2": "general", "t3": "general"}

TASKS = {
    "t1": "Fix the bug in the Python repo at {repo}: durations.parse_duration must follow its docstring exactly "
          "(test_minutes in test_durations.py fails today, and invalid input must raise ValueError as documented). "
          "Add tests for the documented error cases. Acceptance: `python3 -m unittest` passes in {repo}, the fix "
          "follows the whole docstring, and the change is committed on the repo's current branch.",
    "t2": "Run the daily DM lead sweep for the repo at {repo}: follow {repo}/RULES.md exactly (classify every thread "
          "in inbox/, update crm.csv, write drafts/ and sweep-report.md). Acceptance: every rule in RULES.md is met "
          "and the result is committed on the repo's current branch.",
    "t3": "Do the research report described in {repo}/TASK.md from the sources in {repo}/sources. Acceptance: "
          "report.md meets every requirement in TASK.md, every number is the exact value from the sources, and it is "
          "committed on the repo's current branch.",
}

ARM = {
    "fg": "Delegate the task to exactly one subagent with the Agent tool in the foreground (run_in_background false, "
          "model opus). Write it a complete brief. When it returns, verify the acceptance yourself, fix nothing "
          "yourself unless a check fails, then answer.",
    "bg": "Delegate the task to exactly one subagent with the Agent tool with run_in_background: true and model opus. "
          "Write it a complete brief. Then end your turn at once; you will be notified when it finishes. Then verify "
          "the acceptance yourself, fix nothing yourself unless a check fails, then answer.",
    "job": "Delegate the task as exactly one agent-jobs job: call the job_start tool (kind {kind}, model opus, repo {repo}, "
           "a complete brief written for a stranger who has no context, max_turns 80, max_budget_usd 10, "
           "timeout_minutes 30). Then end your turn at once with one short line; a [jobs:job-done] line will arrive "
           "later as a new message.",
}

JOB_TURN2 = ("{line}\n\nThe job finished. Read it with job_result (Not done / uncertain first), check its diff, bring "
             "its commit(s) onto the current branch of {repo} (the job ran in a git worktree of that same repository, "
             "so its commits are reachable: git cherry-pick them; leave the working tree clean), verify the "
             "acceptance yourself, fix nothing yourself unless a check fails, then answer.")

FRAME = ("Context from earlier in this session (unrelated to the new task; do not act on it):\n<earlier>\n{pad}\n"
         "</earlier>\n\nYou are the main session of an agent. A new task arrived. Do NOT do the task's work yourself: "
         "delegate it as instructed, then verify and report.\n\nHow to delegate: {arm}\n\nTask: {task}\n\nWhen you are "
         "done, end with a line `RESULT: pass` or `RESULT: fail` and at most three lines of summary.")


def claude(args: list[str], cwd: Path, out: Path) -> None:
    mcp = BENCH / "mcp.json"
    cmd = ["claude", "-p", *args, "--model", MODEL, "--output-format", "stream-json", "--verbose",
           "--setting-sources", "project", "--strict-mcp-config", "--mcp-config", str(mcp),
           "--dangerously-skip-permissions"]
    with out.open("a") as f:
        subprocess.run(cmd, cwd=cwd, stdout=f, stderr=subprocess.DEVNULL, timeout=3600, check=False)


def limits() -> dict:
    """Current 5h / 7d utilization from a minimal probe call (sonnet, one word)."""
    p = subprocess.run(["claude", "-p", "Reply ok.", "--model", "sonnet", "--output-format", "stream-json",
                        "--verbose", "--setting-sources", "project", "--strict-mcp-config",
                        "--no-session-persistence"], capture_output=True, text=True, cwd=BENCH, timeout=120)
    for line in p.stdout.splitlines():
        e = json.loads(line)
        if e.get("type") == "rate_limit_event":
            w = e["rate_limit_info"].get("unifiedWindows", {})
            return {"t": time.time(), "five_hour": w.get("five_hour", {}).get("utilization"),
                    "seven_day": w.get("seven_day", {}).get("utilization"),
                    "five_hour_resets": w.get("five_hour", {}).get("resetsAt")}
    return {"t": time.time()}


def aj(*a: str) -> str:
    return subprocess.run([str(AJ), *a], capture_output=True, text=True, check=False).stdout


def main() -> None:
    arm, task, rep = sys.argv[1], sys.argv[2], sys.argv[3]
    run = BENCH / "runs" / f"{arm}-{task}-{rep}"
    shutil.rmtree(run, ignore_errors=True)
    run.mkdir(parents=True)
    repo = run / "repo"
    shutil.copytree(BENCH / "fixtures" / FIXTURE[task], repo, symlinks=True)
    pad = (BENCH / "padding.txt").read_text()
    prompt = FRAME.format(pad=pad, arm=ARM[arm].format(kind=KIND[task], repo=repo), task=TASKS[task].format(repo=repo))
    sid = str(uuid.uuid4())
    res: dict = {"arm": arm, "task": task, "rep": rep, "session_id": sid, "limits_before": limits()}
    t0 = time.time()
    claude(["--session-id", sid, prompt], run, run / "parent.jsonl")
    if arm == "job":
        jobs = json.loads(aj("list", "--agent", "bench", "--json", "--limit", "1"))["jobs"]
        job = jobs[0] if jobs else None  # runs are sequential: the newest job is this run's
        res["job_id"] = job["id"] if job else None
        row = job or {}
        while job:
            row = [j for j in json.loads(aj("list", "--agent", "bench", "--json", "--limit", "5"))["jobs"]
                   if j["id"] == job["id"]][0]
            if row["status"] not in ("queued", "running"):
                break
            time.sleep(10)
        time.sleep(65)  # let the daemon tick deliver the job-done line, as in real use
        res["job_done_at"] = time.time()
        line = ""
        for ln in (BENCH / "daemon.out").read_text().splitlines():
            if job and job["id"] in ln and "[jobs:job-done]" in ln:
                line = ln[ln.index("[jobs:job-done]"):]
        if not line and job:
            line = f"[jobs:job-done] job {job['id']} {row['status']} · {row['turns']} turns · ${row['cost_usd']:.2f}"
        res["job_done_line"] = line
        claude(["--resume", sid, JOB_TURN2.format(line=line, repo=repo)], run, run / "parent.jsonl")
    res["wall_sec"] = time.time() - t0
    acc = subprocess.run([sys.executable, str(BENCH / "scripts" / "accept.py"), task, str(repo), str(BENCH / "hidden")],
                         capture_output=True, text=True)
    res["acceptance"] = json.loads(acc.stdout or '{"pass": false}')
    res["limits_after"] = limits()
    (run / "result.json").write_text(json.dumps(res, indent=1))
    print(json.dumps({k: res[k] for k in ("arm", "task", "rep", "wall_sec")}), res["acceptance"]["pass"])


if __name__ == "__main__":
    main()
