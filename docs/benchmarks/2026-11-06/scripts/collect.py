"""Collect bench results: cost/turns/time per job + sol verdict per branch."""
import re
import subprocess
import sys
from pathlib import Path

B = Path("<bench-dir>")


def sh(*a: str) -> str:
    return subprocess.run(a, capture_output=True, text=True).stdout


def status(jid: str) -> dict[str, str]:
    out = sh("agent-jobs", "status", "--agent", "<agent>", jid)
    d = {"status": "", "usage": ""}
    for line in out.splitlines():
        k, _, v = line.partition(":")
        if k.strip() in d:
            d[k.strip()] = v.strip()
    return d


def verdict(rid: str) -> str:
    if not rid or rid == "no-review":
        return "no-review"
    st = status(rid)["status"]
    if st != "succeeded":
        return f"review-{st}"
    m = re.search(r"BENCH-VERDICT:[^\n]*", sh("agent-jobs", "result", "--agent", "<agent>", rid))
    return m.group(0).replace("BENCH-VERDICT: ", "") if m else "no-verdict-line"


rev: dict[str, str] = {}
for f in ("reviews.tsv", "reviews-extra.tsv"):
    p = B / f
    if p.exists():
        for line in p.read_text().splitlines():
            c = line.split("\t")
            if len(c) >= 3 and (c[0] not in rev or c[2] != "no-review"):
                rev[c[0]] = c[2]
rows = []
for line in (B / "manifest.tsv").read_text().splitlines():
    lvl, n, tag, jid = line.split("\t")
    s = status(jid)
    rows.append((lvl, n, tag, s["status"], s["usage"], verdict(rev.get(jid, ""))))
out = ["| level | issue | model | job | usage | sol verdict |", "|---|---|---|---|---|---|"]
out += [f"| {r[0]} | #{r[1]} | {r[2]} | {r[3]} | {r[4]} | {r[5]} |" for r in rows]
(B / "results.md").write_text("\n".join(out) + "\n")
print("\n".join(out))
pending = sum(1 for r in rows if r[5].startswith("review-q") or r[5].startswith("review-r"))
print(f"pending reviews: {pending}", file=sys.stderr)
