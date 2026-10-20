"""Turn measure.py JSON lines into the README benchmark table (mean per task and arm) and per-arm totals.

Columns: 5h limit points (mean per run), non-cache-read tokens, cache-read tokens, $ equivalent, then the
raw classes, acceptance, wall minutes, parent-busy minutes.

Usage: bench_table.py measurements.jsonl [ru|en]
"""
import json
import statistics as st
import sys
from collections import defaultdict

ARM = {"en": {"fg": "Foreground subagent", "bg": "Background subagent", "job": "Job"},
       "ru": {"fg": "Субагент на переднем плане", "bg": "Фоновый субагент", "job": "Джоба"}}
TASK = {"en": {"t1": "bug fix (Python, hidden tests)", "t2": "DM lead sweep (12 threads, CRM, drafts)",
               "t3": "research report from docs (RU)"},
        "ru": {"t1": "фикс бага (Python, скрытые тесты)", "t2": "разбор лидов в личке (12 диалогов, CRM, черновики)",
               "t3": "отчёт-исследование по документации"}}


def fmt(n: float) -> str:
    if n < 1e3:
        return f"{n:.0f}"
    return f"{n/1e3:,.1f}k" if n < 1e6 else f"{n/1e6:,.2f}M"


def main() -> None:
    rows = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
    lang = sys.argv[2] if len(sys.argv) > 2 else "en"
    g = defaultdict(list)
    for r in rows:
        g[(r["task"], r["arm"])].append(r)
    yes, no = ("да", "нет") if lang == "ru" else ("yes", "no")
    for task in ("t1", "t2", "t3"):
        for arm in ("fg", "bg", "job"):
            rs = g.get((task, arm), [])
            if not rs:
                continue
            m = lambda f: st.mean(f(r) for r in rs)
            t = lambda k: m(lambda r: r["total"][k])
            passed = sum(r["pass"] for r in rs)
            fh = sum(r["five_hour_delta"] for r in rs) / len(rs) * 100
            note = (f"parent {m(lambda r: r['parent']['usd']):.2f} $ + worker {m(lambda r: r['worker']['usd']):.2f} $; n={len(rs)}"
                    if lang == "en" else
                    f"родитель {m(lambda r: r['parent']['usd']):.2f} $ + исполнитель {m(lambda r: r['worker']['usd']):.2f} $; n={len(rs)}")
            nonread = t("input") + t("cache_creation") + t("output")
            print(f"| {ARM[lang][arm]} | {TASK[lang][task]} | {fh:.1f} | {fmt(nonread)} | {fmt(t('cache_read'))} | "
                  f"{t('usd'):.2f} | {fmt(t('input'))} | {fmt(t('cache_creation'))} | {fmt(t('output'))} | "
                  f"{fmt(nonread + t('cache_read'))} | {passed}/{len(rs)} | {m(lambda r: r['wall_min']):.1f} | "
                  f"{m(lambda r: r['parent_busy_min']):.1f} | {note} |")
    print()
    for arm in ("fg", "bg", "job"):
        rs = [r for r in rows if r["arm"] == arm]
        if not rs:
            continue
        print(arm, "runs", len(rs), "pass", sum(r["pass"] for r in rs), "usd", round(sum(r["total"]["usd"] for r in rs), 2),
              "parent usd", round(sum(r["parent"]["usd"] for r in rs), 2), "5h pts", round(sum(r["five_hour_delta"] for r in rs) * 100, 1),
              "7d pts", round(sum(r["seven_day_delta"] for r in rs) * 100, 1), "wall", round(sum(r["wall_min"] for r in rs), 1),
              "busy", round(sum(r["parent_busy_min"] for r in rs), 1),
              "nonread tok", round(sum(r["total"]["input"] + r["total"]["cache_creation"] + r["total"]["output"] for r in rs) / 1e3),
              "read tok", round(sum(r["total"]["cache_read"] for r in rs) / 1e3))


if __name__ == "__main__":
    main()
