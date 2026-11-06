"""Build per-level detail-table prompts from results.md."""
import re
from pathlib import Path

BASE = Path(__file__).resolve().parent
RESULTS = BASE.parent / "results.md"

DESC = {
    "#110": "Тест может оставить зависший помощник, если ожидание PID упало",
    "#145": "Закрыть пробелы в тестах после ревью #124 и #125",
    "#146": "OpenRouter: файловые источники обходят запрет deny_repos",
    "#154": "Отчёт теряется, когда фоновое уведомление добавляет лишнее событие",
    "#155": "Гард: оставшиеся обходы rsync (find +, python -c, $VAR)",
    "#114": "Git-раннер: потомки cat-file переживают таймаут",
    "#134": "Раннер гейтов: проверка HEAD, маркер выхода, kill группы, чистое окружение",
    "#119": "Вехи: считать реальные зависимости и занимать все слоты джоб",
    "#135": "ui-check: оркестратор может запустить код джобы",
}
LEVELS = {"S": ("Простые задачи", ["#110", "#145", "#146"]),
          "M": ("Средние задачи", ["#154", "#155", "#114"]),
          "L": ("Большие задачи", ["#134", "#119", "#135"])}
ORDER = ["opus-high", "sonnet-high", "sonnet-medium", "glm", "haiku-high"]
NAMES = {"opus-high": "Opus high", "sonnet-high": "Sonnet high", "sonnet-medium": "Sonnet medium",
         "glm": "GLM 5.3", "haiku-high": "Haiku high"}
STYLE = ("A clean, minimalist, professional data table infographic in a dark theme (near-black "
         "background #0E0F12, soft gold accent #D4A84B, white and light-grey text, thin divider lines). "
         "Style of a top-tier ML benchmark report: flat, crisp sans-serif typography, aligned columns, "
         "no 3D, no icons, no emoji. All text in Russian exactly as written. Every number and word in "
         "the table must be rendered exactly as given, no extra rows.")


def parse() -> dict[tuple[str, str, str], list[str]]:
    """Return {(level, issue, model): cells} from results.md, last row wins."""
    rows: dict[tuple[str, str, str], list[str]] = {}
    for line in RESULTS.read_text().splitlines():
        cells = [c.strip() for c in line.strip("|").split("|")]
        if len(cells) == 6 and cells[0] in LEVELS:
            rows[(cells[0], cells[1], cells[2])] = cells
    return rows


def row_text(cells: list[str]) -> str:
    """Format one table row for the prompt."""
    _, _, model, status, usage, verdict = cells
    cost = re.search(r"\$([0-9.]+)", usage)
    secs = re.search(r"(\d+)s", usage)
    minutes = f"{round(int(secs.group(1)) / 60)} мин" if secs else "—"
    if "no-review" in verdict:
        return f"{NAMES[model]} | таймаут, ветки нет | — | — | {minutes} | джоба не уложилась в 60 мин"
    v = dict(kv.split("=") for kv in verdict.split())
    res = {"yes": "решено", "partly": "частично", "no": "не решено"}[v["issue_fixed"]]
    note = "джоба упала, коммит есть" if status == "failed" else ""
    if secs and int(secs.group(1)) < 120:
        minutes, note = "—", "1-я попытка без отчёта, дозапуск"
    found = f"H {v['high']} · M {v['medium']} · L {v['low']}"
    price = f"${cost.group(1)}" if cost else "—"
    return f"{NAMES[model]} | {res} | {found} | {price} | {minutes} | {note}"


def main() -> None:
    """Write p4-S.txt, p5-M.txt, p6-L.txt."""
    rows = parse()
    for idx, (lvl, (title, issues)) in enumerate(LEVELS.items(), start=4):
        parts = [STYLE, "", f"Title: «{title}: каждая джоба»",
                 "Subtitle: «Вердикт Codex gpt-6.1-sol · находки ревью H = High, M = Medium, L = Low · "
                 "цена: Claude – эквивалент API, GLM – реальные деньги»", "",
                 "Table columns: Модель | Результат | Находки | Цена | Время | Примечание",
                 "Color the Результат cell: «решено» gold text, «частично» light grey, "
                 "«таймаут, ветки нет» muted red. Each issue is a group with a gold group header row "
                 "showing the issue number and its description.", ""]
        for issue in issues:
            parts.append(f"Group header: «{issue} – {DESC[issue]}»")
            for model in ORDER:
                cells = rows.get((lvl, issue, model))
                if cells:
                    parts.append("  " + row_text(cells))
            parts.append("")
        parts.append("Footer small grey: «Один прогон на клетку · ревью читало код, тесты не запускало · "
                     "время завышено: шло 5 джоб параллельно»")
        (BASE / f"p{idx}-{lvl}.txt").write_text("\n".join(parts) + "\n")


if __name__ == "__main__":
    main()
