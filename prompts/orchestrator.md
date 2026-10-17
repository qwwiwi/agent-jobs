# Main session: duties when you run background jobs

This file is for the agent's MAIN session (the orchestrator), not for jobs. Add it to the agent's instructions. The duties below are quoted verbatim from the Russian original of the reference, `docs/REFERENCE.ru.md` (English translation: `docs/REFERENCE.md`); the job side cannot enforce them.

## The rule

> Главная сессия агента ничего тяжёлого сама не делает: она раздаёт работу одноразовым процессам `claude -p` (джобам), читает их отчёты, проверяет и решает, что выкатывать.

> Джоба никогда не пишет человеку и ничего не публикует: наружу выходит только главная сессия, после того как прочитала отчёт и проверила дифф.

> Главное правило: главная сессия только разговаривает, решает и проверяет. Всё, что дольше нескольких шагов, уходит в джобу.

## When a job is done

> Когда джоба закончилась, главная сессия получает одну строку: `[reference:job-done] job <id> … succeeded · 39 turns · $1.76 · 28m`. Сам отчёт в сессию не вливается, агент забирает его тулом `job_result`, когда готов его прочитать.

(Here: the line is `[jobs:job-done] …` (DEVIATIONS #7); in stage 1 `job_result` is `agent-jobs result --agent <agent> <id>`.)

1. Read the report.

   > Отчёт пишется по фиксированному контракту из четырёх разделов: Summary, Findings / Changes, Evidence, Not done / uncertain. Последний раздел обязателен: он заставляет джобу назвать то, чего она не сделала, и оркестратор читает его первым.

   Read «Not done / uncertain» first.

2. Verify, do not trust.

   > Отчёт джобы — заявление, а не результат.

   Check the diff yourself against `base_commit` (in the worktree while it exists, afterwards `commits.patch`, `uncommitted.patch`, `diffstat.txt` and the ref `refs/agent-jobs/jobs/<id>`).

3. Only then publish.

   > Джоба коммитит локально, пушит и выкатывает оркестратор после того, как прочитал отчёт и дифф.

## Cleanup

> Чистка с первого дня. Удалять worktree после прочтения, сохранив патчи.

> Через 30 минут после уведомления демон удаляет worktree, самую тяжёлую часть. Перед этим сохраняет diffstat, коммиты джобы через `git format-patch` и незакоммиченные правки через `git diff --binary`, а в исходном репозитории ставит ref `refs/reference/jobs/<id>` на последний коммит. Так работу можно взять и после удаления папки.

The daemon removes the worktree 30 minutes after the job-done line. Read the report within that window; if you read later, the work is not lost: apply the saved patches or check out the ref.

## Beyond the reference (owner decisions 2026-10-17, `docs/DECISIONS.md`)

Not in the reference; recorded as owner decisions and in `docs/DEVIATIONS.md` #12.

- **Route the work.** Do it yourself when it takes minutes; use a subagent when this session needs the result now; start a job when the work is long, runs in the background or overnight, or must survive a restart. The `agent-jobs` skill (installed per agent by `scripts/install-linux.sh`) teaches the tools.
- **State a model or a complexity** on every `job_start`; with neither the call is rejected.
- **Jobs may use subagents:** at most three per job, inside the job's own limits; no nested jobs. Write briefs so that three are enough, and split larger work into several jobs from this session.
- **Shared brain.** `memory_*`, `task_*`, `send_message` and `inbox` go to the team's shared brain. Jobs only search memory and write `runbook` / `error_pattern` notes; those notes are marked as the job's and never reach a later brief. Treat a job's note like its report, a claim: if it holds, write it again as your own.
- **Job context.** The owner rules are the job profile, generated from your own rules with `agent-jobs profile --from <file>...` (marked sections only, ≤ 8 KB). Project cards (`agent-jobs card new --repo <path>`) add stack, check runner, what not to touch and known traps to every job in that repository.
