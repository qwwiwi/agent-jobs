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

## In progress

**[PR #7](https://github.com/qwwiwi/agent-jobs/pull/7): MCP job tools** (supersedes [#4](https://github.com/qwwiwi/agent-jobs/pull/4)).

- One Go MCP server per agent with two roles: `orchestrator` (main session) and `job`. The job role is an allowlist; the server itself refuses hidden tools, and a test fails if a tool is in neither list.
- `job_start`, `job_status`, `job_result`, `job_list`, `job_cancel` on top of the existing manager.
- A job's `mcp.json` points at the job-role server.
- The caller must give a model or a complexity: no classifier, no guessing.
- tmux socket support for the job-done line.
- Guard: a job cannot start or control other jobs through the CLI (environment marker, process-ancestry and session checks).
- Session sweep: after claude exits, the wrapper ends every leftover process of its session before it writes `exit_code`; signals go through pidfds, and survivors fail the job («processes survived the job»).

**[PR #8](https://github.com/qwwiwi/agent-jobs/pull/8): `agent-jobs` skill, subagents in jobs, job context** (moved out of Wave A).

- **`agent-jobs` skill.** `skills/agent-jobs/SKILL.md`, installed per agent by `scripts/install-linux.sh` (`--skills-dir`, default `${CLAUDE_CONFIG_DIR:-~/.claude}/skills`; idempotent; removed on uninstall only while unchanged). Teaches the MCP tools, the mandatory model-or-complexity, the routing rule (yourself: minutes; subagent: the session needs the result now; job: long, background, overnight, must survive a restart) and how to read a result («Not done / uncertain» first, check the diff before anything leaves the machine).
- **Jobs may use subagents**, at most three per job, inside the job's own limits; no nested jobs. The brief says so; the subagent tool is not denied; the guard counts Agent/Task calls per job (`<state>/subagents/<id>.log`, locked), refuses the fourth; only the input keys `description`, `prompt`, `subagent_type`, `model` pass (exact, case-sensitive; no `isolation`, `run_in_background`, team).
- **Job context.** The job profile is the reference's ≤ 8 KB owner-rules file, generated with `agent-jobs profile --from <file>...` from the sections of the agent's own rules marked `<!-- agent-jobs:profile -->`; larger than 8 KB is refused. Project cards (`<state>/cards/<name>.md`, ≤ 4 KB, `agent-jobs card new|list`) are matched by repository path or remote and attached to the brief; a broken card refuses the start. Example: `cards/example-agent-jobs.md`.

## Wave A

- **Cgroup containment for jobs (hardening).** Session and ppid checks can be escaped with `setsid` plus reparenting; a cgroup cannot. On Linux:
  1. The daemon launches each wrapper in its own transient systemd user scope, `systemd-run --user --scope --unit=agent-jobs-<agent>-<id> …` (or the same call over D-Bus).
  2. Control commands refuse when `/proc/self/cgroup` names an `agent-jobs-*` job scope. setsid, double-fork and reparenting all stay inside the cgroup.
  3. On wrapper exit, cancel or timeout the whole scope is stopped (`systemctl --user stop` / kill the unit) before `exit_code` is written, so no descendant survives.
  4. Fallback when `systemd-run --user` is unavailable (macOS, containers, no user bus): today's sid sweep plus the marker, ancestry, session and store checks. That path is documented as weaker.
  5. Needs: a reachable user bus for the daemon (linger is already on), a launch-path test that a `setsid` + reparented orphan is still refused and still killed, and a check that `KillMode=process` daemon restarts leave job scopes alone.
- **`prune`.** Clean folders of jobs finished more than 48 hours ago; keep the records (report, patches, `base_commit`) and small top-level text files, drop binaries, caches and builds.
- **`doctor procs`.** Find processes that outlived their job or session: preview servers, browsers, wait loops, running rows with a dead pid. Reports by default, kills only on a flag.
- **System tools:** `system_doctor` (disk, processes, hooks), `system_logs`, `activity_report`.
- **Files:** `trash_move`, `trash_restore`: reversible deletion instead of `rm`.
- **Memory, tasks and team tools via the shared brain.** `memory_search`, `memory_write`, `memory_already_exists`; `task_create`, `task_list`, `task_update`, `task_get`; `send_message`, `inbox`. Same names as the reference, backed by the team's shared memory and task service (DEVIATIONS #2, #3). Jobs get `memory_search` and `memory_write` only.
- **Memory notes in the brief.** The daemon adds up to six of the closest notes from shared memory to every brief, so a job does not rediscover what is already written down. If memory is unreachable, the job starts anyway and the brief says so.

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

## Wave D

Approved by the owner on 2026-10-17 as part of the full mandate (see [DECISIONS.md](DECISIONS.md)); built after Wave C:

- **Heartbeat.** The daemon decides whether to wake the agent for its own goals: a cheap Go check (time, daily limit, night), then a small model decides (DEVIATIONS #1).
- **«Owner waits» Stop hook.** A job started at the owner's request is marked; until the orchestrator has written to the owner, a Stop hook does not let the session end its turn silently.
- **API-credit fallback.** When the subscription's weekly window is nearly used up, jobs may run on API credit, only with explicit consent and a cap. Built switched off; turning it on (real money) needs its own owner «да».
- **Read-only HTTP API** for a jobs dashboard (DEVIATIONS #8).
- **Acceptance from a clean clone.** The whole install and a test job are replayed from a fresh clone before a release.
- **Autopilot items 23-27:** risky-first review, a Progress section in reports, a job size check, secret redaction in reports and logs, an `acceptance_cmd` a job must pass.

## Conformance check against the reference after every wave

Before a wave is merged, the reference is walked line by line: for each claim, is it implemented, where (`file:line`), and is it 1:1 or a listed deviation? A deviation that is not in [DEVIATIONS.md](DEVIATIONS.md) blocks the merge. The result table is published with the wave.
