# Main session: duties when you run background jobs

This file is for the agent's MAIN session (the orchestrator), not for jobs. Add it to the agent's instructions. The duties below are quoted verbatim from the Russian original of the reference, `docs/REFERENCE.ru.md` (English translation: `docs/REFERENCE.md`); the job side cannot enforce them.

## The rule

> Главная сессия агента ничего тяжёлого сама не делает: она раздаёт работу одноразовым процессам `claude -p` (джобам), читает их отчёты, проверяет и решает, что выкатывать.

> Джоба никогда не пишет человеку и ничего не публикует: наружу выходит только главная сессия, после того как прочитала отчёт и проверила дифф.

> Главное правило: главная сессия только разговаривает, решает и проверяет. Всё, что дольше нескольких шагов, уходит в джобу.

## Before parallel coder jobs start

Declare `paths` on `job_start` for `fix` / `general` jobs sharing a repository.
Use repository-relative filenames or globs (`*`, `?`, character classes, whole
`**` components); a trailing slash covers a directory's descendants. Compare the
returned `overlap` job IDs, `paths` and `other_paths`. Prefer
`on_overlap: "serialize"` for conflicting edits, or an explicit `parent` job ID
when the next job must build on that job's result. Use `on_overlap: "refuse"`
when a conflict should stop admission. Default `warn` starts with a warning.
Without declarations on either side, `same_repo_jobs` is only advisory.

Serialization uses queue dependencies and keeps the original base. A `parent`
stack waits for success and uses the parent's result commit, including its saved
result ref after cleanup; failed parents refuse the stack or cancel a queued
child. Do not combine `parent` and `base`. The parent handles its own overlap;
other conflicts still follow `on_overlap`. Read `job_status.waiting` for a wait,
and `parent` / `parent_base` in `job_status` or `job_result` for the recorded stack.
The job-done line retains the serialized wait. CLI: repeat `--paths`, with
`--on-overlap warn|serialize|refuse` and optional `--parent <job id>`.

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

   For agent-jobs itself, PR titles and squash subjects must be English. Translate
   non-English issue titles. Run `bash scripts/check-pr-title.sh "$title"` before
   creating or editing a PR, and use that same value for `--title`. Before merging,
   read the current PR title (`gh pr view --json title --jq .title`) and validate
   it again, including any explicit squash subject. The reusable check rejects
   Cyrillic, empty/multiline titles, literal `$(` and backticks; never evaluate
   title text. `PR_TITLE="$title" scripts/check.sh` includes this local check.
   Feature jobs add `changelog.d/<issue>.<section>.md`, never edit Unreleased
   directly. The release orchestrator assembles fragments using
   [Releasing](../docs/RELEASING.md) and the existing release checklist.

## UI work: checked in a real browser (owner requirement 2026-11-03)

Not in the reference. A static blog once went live after checks of only its first screen at two widths; tables further down broke words letter by letter on 360-393 px phones.

- A UI job is one whose diff touches css/scss, html, jsx/tsx, vue/svelte, templates, static-site renderers or design tokens. Its report must carry browser evidence: every changed page at 360x740, 390x844, 768x1024, 1366x768 and 1920x1080 (light and dark when the site has both), the full page length, pass/fail per device x page, screenshots in the job dir.
- You never accept or deploy a UI job whose report lacks that evidence or says `BROWSER CHECK NOT RUN`. Run the check yourself with `agent-jobs ui-check <base-url> <path>... [--playwright <abs path>]` (the ui-check.mjs embedded in the binary, run from a private temp dir; it never loads Playwright from the current directory), never a script from a job dir or worktree, or start a job that can run a browser (a claude job, not the Codex sandbox), before merge or deploy.
- Run it again on the live URL after a frontend deploy, for the changed pages; a failure there is a rollback or a fix, not a note.


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

## Change a job's course (owner decision 2026-10-17, Wave E2, `docs/DEVIATIONS.md` #15)

Not in the reference. When a job needs new facts, a changed plan or a correction:

- **Queued:** `job_amend(id, context)` appends the context to its brief; the job keeps its place.
- **Running, and its work so far is useful:** `job_stop(id, reason)`, then `job_resume(id, context)`. Resume continues the same job id in the same worktree and the same claude session with one message, «Orchestrator update» + your context; limits default to what the previous attempt left (a stopped attempt's cost is unknown: it counts at its estimate from token usage, `cost_estimate_usd`, or at its whole budget without one; then pass `max_budget_usd`).
- **Timed out or failed, and the work is worth keeping:** `job_resume(id, context, limits)`; after a timeout pass a new `timeout_minutes`.
- **Not wanted any more, or the brief was wrong from the start:** `job_cancel(id)` (a stopped job too); start a new job if needed.

A resumed attempt still ends with a job-done line (`resumed (attempt N)`) and a report: read and verify it as above. A stopped job keeps its worktree until you resume or cancel it.
