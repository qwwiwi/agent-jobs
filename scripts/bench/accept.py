"""Objective acceptance checks for the three benchmark tasks.

Usage: accept.py TASK REPO HIDDEN_DIR -> prints JSON {"pass": bool, "checks": {...}}.
"""
import csv
import json
import re
import subprocess
import sys
from pathlib import Path

EMOJI = re.compile("[\U0001F300-\U0001FAFF☀-➿]")


def git_clean(repo: Path) -> bool:
    out = subprocess.run(["git", "-C", str(repo), "status", "--porcelain"], capture_output=True, text=True).stdout
    return out.strip() == ""


def t1(repo: Path, hidden: Path) -> dict:
    vis = subprocess.run([sys.executable, "-m", "unittest", "-q"], cwd=repo, capture_output=True, text=True)
    hid = subprocess.run([sys.executable, str(hidden / "t1_hidden_test.py"), str(repo)], capture_output=True, text=True)
    return {"visible_tests": vis.returncode == 0, "hidden_tests": hid.returncode == 0, "committed": git_clean(repo)}


def t2(repo: Path, hidden: Path) -> dict:
    exp = json.loads((hidden / "t2_expected.json").read_text())
    nxt = {"hot": "draft_reply", "warm": "draft_reply", "cold": "followup_d7", "ignore": "none"}
    rows = {r["chat_id"]: r for r in csv.DictReader((repo / "crm.csv").open())}
    stages_ok = all(rows.get(c, {}).get("stage") == s for c, s in exp.items())
    next_ok = all(rows.get(c, {}).get("next_action") == nxt[s] for c, s in exp.items())
    upd_ok = all(rows.get(c, {}).get("updated") == "2026-10-19" for c in exp)
    names = {c: rows.get(c, {}).get("name", "") for c in exp}
    want = {c for c, s in exp.items() if s in ("hot", "warm")}
    have = {p.stem for p in (repo / "drafts").glob("*.md")} if (repo / "drafts").is_dir() else set()
    drafts_ok = have == want
    quality = True
    for c in want & have:
        t = (repo / "drafts" / f"{c}.md").read_text()
        if len(t) > 600 or EMOJI.search(t) or names[c] not in t:
            quality = False
        if exp[c] == "hot" and "оплат" not in t.lower():
            quality = False
    return {"stages": stages_ok, "next_action": next_ok, "updated": upd_ok, "drafts_set": drafts_ok,
            "drafts_quality": quality, "report": (repo / "sweep-report.md").is_file(), "committed": git_clean(repo)}


def t3(repo: Path, hidden: Path) -> dict:
    p = repo / "report.md"
    if not p.is_file():
        return {"report": False}
    t = p.read_text()
    heads = re.findall(r"^## (.+?)\s*$", t, re.M)
    heads_ok = heads == ["Итог", "Цифры", "Риски", "Источники"]
    m = re.search(r"^## Цифры\s*$(.*?)^## ", t, re.M | re.S)
    nums = m.group(1) if m else ""
    rows = [ln for ln in nums.splitlines() if ln.strip().startswith("|") and not re.match(r"^\|\s*:?-", ln.strip())]
    vals = all(re.search(rx, nums) for rx in [r"\b80\b", r"\b50\b", r"\b30\b", r"\b48\b", r"\b2\b", r"\b3\b"])
    r = re.search(r"^## Риски\s*$(.*?)^## ", t, re.M | re.S)
    risks = len([ln for ln in (r.group(1) if r else "").splitlines() if re.match(r"^\s*([-*]|\d+\.|\|)", ln) and not re.match(r"^\|\s*:?-", ln.strip())])
    return {"report": True, "headings": heads_ok, "table_rows": len(rows) >= 7, "values": vals, "risks": risks >= 3, "committed": git_clean(repo)}


def main() -> None:
    task, repo, hidden = sys.argv[1], Path(sys.argv[2]), Path(sys.argv[3])
    checks = {"t1": t1, "t2": t2, "t3": t3}[task](repo, hidden)
    print(json.dumps({"pass": all(checks.values()), "checks": checks}))


if __name__ == "__main__":
    main()
