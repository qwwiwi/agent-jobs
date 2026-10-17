# Roadmap

> Русская версия: [PLAN.ru.md](PLAN.ru.md)

Agent Jobs implements the [reference design](REFERENCE.md) 1:1; the only allowed differences are listed in [DEVIATIONS.md](DEVIATIONS.md), and scope decisions in [DECISIONS.md](DECISIONS.md). This page tracks what is built, what is in review and what comes next. Everything the reference describes is on this list; nothing below is shipped unless it is under «Done».

## Done

| PR | What |
|---|---|
| [#1](https://github.com/qwwiwi/agent-jobs/pull/1) | **Core.** SQLite queue, per-minute daemon, detached wrapper, launch command 1:1 with the reference, limits and ceilings, kind checklists, owner rules, report contract, one-line tmux notify, cleanup with patches and a ref, CLI. |
| [#2](https://github.com/qwwiwi/agent-jobs/pull/2) | **Linux port.** systemd user service `agent-jobs@<agent>` with `KillMode=process` (a daemon restart never kills jobs), idempotent installer, owner decisions recorded. |
| [#3](https://github.com/qwwiwi/agent-jobs/pull/3) | **Guard hardening.** gh flags before the subcommand, no `gh auth status`, file tools cannot write into `.git`. |
| [#5](https://github.com/qwwiwi/agent-jobs/pull/5) | **Publication prep.** Reference design published without personal names. |
| [#7](https://github.com/qwwiwi/agent-jobs/pull/7) | **MCP job tools** (supersedes [#4](https://github.com/qwwiwi/agent-jobs/pull/4)). One MCP server per agent in two roles (`orchestrator`, `job` as an allowlist), `job_start/status/result/list/cancel`, model or complexity required, tmux socket, nested-job prevention (marker, ancestry, session and store checks), session sweep before `exit_code`. |

## In progress

**[PR #PRNUM](https://github.com/qwwiwi/agent-jobs/pull/PRNUM): cgroup containment for jobs (hardening).** Session and ppid checks can be escaped with `setsid` plus reparenting; a cgroup cannot. On Linux:

1. The daemon launches each wrapper in its own transient systemd user scope, `systemd-run --user --scope --unit=agent-jobs-<agent>-<id>.scope` (config `cgroup_scope`: `auto` by default, `required`, `off`).
2. Control commands refuse when `/proc/self/cgroup` names an `agent-jobs-*` job scope. setsid, double-fork and reparenting all stay inside the cgroup.
3. Before `exit_code` the wrapper ends every other process of its scope's cgroup (pidfd signals, sub-cgroups included); cancel, timeout and every collection stop the whole unit (`systemctl --user stop`) before the row turns terminal.
4. Fallback when `systemd-run --user` is unavailable (macOS, containers, no user bus): the session sweep plus the marker, ancestry, session and store checks, documented as weaker.
5. Tests: a `setsid` + reparented orphan is refused and killed on the real launch path (and, as a control, missed by the fallback); cancel and timeout stop the scope; a `KillMode=process` daemon unit restart leaves the job scope running. They skip when no user scope can be created.

Also: the job's environment loses the user bus variables, and the guard rejects `systemd-run`, `systemctl --user`, `busctl`, `dbus-send`, `gdbus` and writes under `/sys/fs/cgroup` (the ways out of a scope).

## Wave A

- **`agent-jobs` skill.** A SKILL.md, installed per agent, that teaches the MCP tools and the routing rule: do it yourself (minutes), a subagent (the session needs the result now), or a job (long, background, overnight, must survive a restart).
- **Jobs may use subagents**, at most three per job, inside the job's own limits; no nested jobs.
- **`prune`.** Clean folders of jobs finished more than 48 hours ago; keep the records (report, patches, `base_commit`) and small top-level text files, drop binaries, caches and builds.
- **`doctor procs`.** Find processes that outlived their job or session: preview servers, browsers, wait loops, running rows with a dead pid. Reports by default, kills only on a flag.
- **System tools:** `system_doctor` (disk, processes, hooks), `system_logs`, `activity_report`.
- **Files:** `trash_move`, `trash_restore`: reversible deletion instead of `rm`.
- **Memory, tasks and team tools via the shared brain.** `memory_search`, `memory_write`, `memory_already_exists`; `task_create`, `task_list`, `task_update`, `task_get`; `send_message`, `inbox`. Same names as the reference, backed by the team's shared memory and task service (DEVIATIONS #2, #3). Jobs get `memory_search` and `memory_write` only.
- **Memory notes in the brief.** The daemon adds up to six of the closest notes from shared memory to every brief, so a job does not rediscover what is already written down. If memory is unreachable, the job starts anyway and the brief says so.
- **Job context.** A general job profile (≤ 8 KB) generated from the agent's own rules, plus short per-project cards (stack, check runner, what not to touch, known traps).

## Wave B

- **Resume.** `job_resume` and `agent-jobs resume <id>`: a new job with the same brief, the old commits and diff carried over from the saved patches, and a note at the top of the brief saying what was carried and what was not.
- **Schedules.** `schedule_add`, `schedule_list`, `schedule_remove`. A cron expression in local time plus a prompt, checked every minute, delivered one of two ways:
  - into the live session as a tagged message (several due at once arrive as one message: «these are different instructions, do each one»);
  - as a job, when the schedule has a kind; the session then only sees the job-done line.
  Schedules store their model and limits at creation. A missed firing is lost, not queued, so check windows overlap. Schedule rows never expire; they are edited in place and deleted only by the owner.
- **Dead-man's switch.** Checks that stay silent when all is well leave a completion mark; a watchdog compares the marks with the schedule.
- **Intentions.** `intention_add`, `intention_list`, `intention_set`: the agent's own goals.
- **`notify_owner`.** A message to the owner through the agent's existing channel (DEVIATIONS #4).
- **Daily automation.** `prune` and `doctor procs` run once a day on their own.
- **Codex runner.** A job can run on `codex exec` instead of `claude -p` (DEVIATIONS #5); without turn or budget caps there, so the timeout is the limit.

## Wave C

- **Code-graph index.** The reference's separate MCP server that indexes code as a graph; jobs can read and index. Research first: pick or build the indexer, then integrate.

## Deferred

Planned, but each waits for a separate owner decision:

- **Heartbeat.** The daemon decides whether to wake the agent for its own goals: a cheap Go check (time, daily limit, night), then a small model decides (DEVIATIONS #1).
- **«Owner waits» Stop hook.** A job started at the owner's request is marked; until the orchestrator has written to the owner, a Stop hook does not let the session end its turn silently.
- **API-credit fallback.** When the subscription's weekly window is nearly used up, jobs may run on API credit, only with explicit consent and a cap.
- **Read-only HTTP API** for a jobs dashboard (DEVIATIONS #8).

## Conformance check against the reference after every wave

Before a wave is merged, the reference is walked line by line: for each claim, is it implemented, where (`file:line`), and is it 1:1 or a listed deviation? A deviation that is not in [DEVIATIONS.md](DEVIATIONS.md) blocks the merge. The result table is published with the wave.
