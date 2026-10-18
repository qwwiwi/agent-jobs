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
| [#8](https://github.com/qwwiwi/agent-jobs/pull/8) | **Skill, subagents, job context** (moved out of Wave A). `agent-jobs` skill installed per agent by `scripts/install-linux.sh` (routing rule, model-or-complexity, how to read a result); jobs may use at most three subagents inside their own limits, guard-counted, only the input keys `description`, `prompt`, `subagent_type`, `model`; job profile (≤ 8 KB, `agent-jobs profile --from`) and project cards (≤ 4 KB, `agent-jobs card new\|list`) attached to the brief. |

## In progress

**[PR #10](https://github.com/qwwiwi/agent-jobs/pull/10): cgroup containment for jobs (hardening).** Session and ppid checks can be escaped with `setsid` plus reparenting; a cgroup cannot. On Linux:

1. The daemon launches each wrapper in its own transient systemd user scope, `systemd-run --user --scope --unit=agent-jobs-<agent>-<id>.scope` (config `cgroup_scope`: `auto` by default, `required`, `off`).
2. Control commands refuse when `/proc/self/cgroup` names an `agent-jobs-*` job scope. setsid, double-fork and reparenting all stay inside the cgroup.
3. Before `exit_code` the wrapper ends every other process of its scope's cgroup (pidfd signals, sub-cgroups included); cancel, timeout and every collection stop the whole unit (`systemctl --user stop`) before the row turns terminal.
4. Fallback when `systemd-run --user` is unavailable (macOS, containers, no user bus): the session sweep plus the marker, ancestry, session and store checks, documented as weaker.
5. Tests: a `setsid` + reparented orphan is refused and killed on the real launch path (and, as a control, missed by the fallback); cancel and timeout stop the scope; a `KillMode=process` daemon unit restart leaves the job scope running. They skip when no user scope can be created.

Also: the job's environment loses the user bus variables, and the guard rejects `systemd-run`, `systemctl --user`, `busctl`, `dbus-send`, `gdbus` and writes under `/sys/fs/cgroup` (the ways out of a scope).

**[PR #9](https://github.com/qwwiwi/agent-jobs/pull/9): prune, doctor procs, system and file tools.**

- `agent-jobs prune [--dry-run]`: folders of jobs finished more than 48 hours ago keep their records and small top-level text files; work dirs, caches, builds, binaries, large files and symlinks go.
- `agent-jobs doctor procs [--kill]`: processes that outlived their job or session and running rows with a dead pid; report only, `--kill` ends only processes proven to be a finished job's (DEVIATIONS #13).
- Orchestrator tools `system_doctor`, `system_logs`, `activity_report`, `trash_move`, `trash_restore` (none for jobs).

**Wave E2: stop, resume in the same session, amend** (owner approved and prioritised, see [DECISIONS.md](DECISIONS.md); DEVIATIONS #15). Built in v0.3.0.

- Every job gets a session id before launch (`--session-id`); `--no-session-persistence` is gone.
- `job_stop` / `agent-jobs stop`: a running or queued job ends `stopped`, keeping its worktree, uncommitted work and session.
- `job_resume` / `agent-jobs resume`: the next attempt of the same job id, same work dir, same session (`--resume`) with an «Orchestrator update»; limits default to what is left; a fresh session with the brief + a summary of the previous attempt when the session is gone or cannot be resumed; a cleaned worktree is restored from the ref and patches.
- `job_amend` / `agent-jobs amend`: append context to a queued job's brief, queue position kept.
- `prune` also removes the claude session files of the jobs it prunes.

## Wave A

- **Memory, tasks and team tools via the shared brain.** *Status: built in v0.3.0 ([PR #13](https://github.com/qwwiwi/agent-jobs/pull/13)); `internal/brain`, `internal/mcp/tools_brain.go`.* `memory_search`, `memory_write`, `memory_already_exists`; `task_create`, `task_list`, `task_update`, `task_get`; `send_message`, `inbox`. Same names as the reference, backed by the team's shared memory and task service (DEVIATIONS #2, #3). Jobs get `memory_search` and `memory_write` only.
- **Memory notes in the brief.** *Status: built in v0.3.0 ([PR #13](https://github.com/qwwiwi/agent-jobs/pull/13)); `internal/jobs/memory.go`.* The daemon adds up to six of the closest notes from shared memory to every brief, so a job does not rediscover what is already written down. If memory is unreachable, the job starts anyway and the brief says so.

## Wave B

- **Resume.** `job_resume` and `agent-jobs resume <id>`: a new job with the same brief, the old commits and diff carried over from the saved patches, and a note at the top of the brief saying what was carried and what was not. *Superseded by E2* (built): resume continues the same job id and session; the carry-over survives as its fallback (fresh session in the worktree restored from the ref and patches, with a summary of the previous attempt).
- **Schedules.** `schedule_add`, `schedule_list`, `schedule_remove`. A cron expression in local time plus a prompt, checked every minute, delivered one of two ways:
  - into the live session as a tagged message (several due at once arrive as one message: «these are different instructions, do each one»);
  - as a job, when the schedule has a kind; the session then only sees the job-done line.
  Schedules store their model and limits at creation. A missed firing is lost, not queued, so check windows overlap. Schedule rows never expire; they are edited in place and deleted only by the owner.
  *Status: built, unreleased; `internal/jobs/cron.go`, `internal/jobs/schedule.go`, `internal/store/schedules.go`, `internal/mcp/tools_schedules.go`.* Orchestrator tools `schedule_add`, `schedule_list`, `schedule_set` (cron, prompt, enabled; edited in place) and `schedule_remove` (only on the owner's instruction); `agent-jobs schedules [--json]` lists them. A 5-field cron (ranges, steps, lists, month and weekday names, macros; the classic day-of-month / day-of-week rule) in local time, own parser, no dependency; checked in every daemon tick. Prompt one line, ≤ 2000 characters, control and bidi characters refused, `[jobs:` defanged. Session delivery: one line `[jobs:schedule] …`, several due in the same minute as one numbered line; with a kind a job is queued with the model, runner and limits resolved and stored at creation (validated like `job_start`, codex rules included). Each firing is claimed in the store first (`last_fired_at`): never twice for a minute (restart-safe; a local wall-clock watermark: the repeated hour at the end of DST fires each wall-clock minute once), a missed minute or a failed delivery is lost, never replayed.
- **Dead-man's switch.** Checks that stay silent when all is well leave a completion mark; a watchdog compares the marks with the schedule.
- **Intentions.** *Status: built, unreleased; `internal/store/intentions.go`, `internal/jobs/intentions.go`, `internal/mcp/tools_intentions.go`.* `intention_add`, `intention_list`, `intention_set`: the agent's own goals, a table `intentions` in the agent's store (text one line, ≤ 500 characters; optional due date and priority; status `open` / `done` / `dropped`, edited in place, never deleted). Orchestrator only; `agent-jobs intentions [--status] [--json]` lists them.
- **`notify_owner`.** A message to the owner through the agent's existing channel (DEVIATIONS #4). *Status: built, unreleased; `internal/store/notifications.go`, `internal/jobs/notifyowner.go`, `internal/mcp/tools_notify.go`.* Orchestrator and job role (a job through the daemon's proxy); one line `[jobs:notify-owner] …` typed into the orchestrator session on the job-done tmux path, which relays it; agent-jobs calls no messaging API. Text one line, ≤ 300 characters, tags inside it defanged; urgency normal / high; per job ≤ 3 and ≤ 1 a minute, per agent ≤ 20 an hour; every call stored in `owner_notifications`, undelivered ones in `agent-jobs status` and `system_doctor`; CLI `agent-jobs notify-owner`.
- **Daily automation.** `prune` and `doctor procs` run once a day on their own. **Built, unreleased:** inside the daemon loop, once per local day at the first tick after `daily_hour` (default 4; `daily_off` disables); prune for real with the log archive, doctor procs report only (killing stays manual, `--kill`); the claimed day is stored (`daily` table), so a restart never repeats it and a missed day runs once, never caught up; each step time-boxed (10 min), in a goroutine that never overlaps itself or delays the ticks; the last run shows in `agent-jobs status` (no id) and `system_doctor`.
- **Codex runner.** *Status: built, unreleased; `internal/jobs/codex.go`.* A job can run on `codex exec` instead of `claude -p` (DEVIATIONS #5); without turn or budget caps there, so the timeout is the limit. Selected by a `gpt-*` model or `runner: codex`; read-only sandbox for review/research; write jobs refused unless `codex_write_allowed`; no guard inside codex (known limit); resume refused for now.

## Wave C

- **Code-graph index.** The reference's separate MCP server that indexes code as a graph; jobs can read and index. Research first: pick or build the indexer, then integrate.

**Log archive and per-job stats** (owner approved 2026-10-17, see [DECISIONS.md](DECISIONS.md); the stats part of E7 and Wave D observability).

- Prune moves a cleaned job's logs into `<archive>/<YYYY-MM>/<id>/` (gzip) instead of deleting them; `log_archive_dir`, `log_archive_days` (default 365, 0 = forever); `agent-jobs archive --dry-run`.
- Table `job_stats`, one row per finished job; `agent-jobs stats [--since] [--by kind|model|skill] [--json] [--backfill]` and the orchestrator tool `job_stats`.

## Wave D

Approved by the owner on 2026-10-17 as part of the full mandate (see [DECISIONS.md](DECISIONS.md)); built after Wave C:

- **Heartbeat.** The daemon decides whether to wake the agent for its own goals: a cheap Go check (time, daily limit, night), then a small model decides (DEVIATIONS #1).
- **«Owner waits» Stop hook.** A job started at the owner's request is marked; until the orchestrator has written to the owner, a Stop hook does not let the session end its turn silently.
- **API-credit fallback.** When the subscription's weekly window is nearly used up, jobs may run on API credit, only with explicit consent and a cap. Built switched off; turning it on (real money) needs its own owner «да».
- **Read-only HTTP API** for a jobs dashboard (DEVIATIONS #8). *Status: built, unreleased; `internal/httpapi`.* Off by default (`http_api.enabled`), loopback or unix socket only, bearer token `<state>/http-api.token` (`agent-jobs http-api token --rotate`) on every endpoint; GET only: `/v1/health`, `/v1/jobs`, `/v1/jobs/{id}` (`?report=1`, 64 KB cap), `/v1/stats`, `/v1/schedules`; no briefs, prompts, env, config or paths outside the agent state; server timeouts, 8 KB headers, 10 requests/s per client.
- **Acceptance from a clean clone.** The whole install and a test job are replayed from a fresh clone before a release.
- **Autopilot items 23-27:** risky-first review, a Progress section in reports, a job size check, secret redaction in reports and logs, an `acceptance_cmd` a job must pass.

## Wave E: borrowed ideas (planned; E2 approved and built)

Each item waits for a separate owner go (E2 has it, see [DECISIONS.md](DECISIONS.md)); it is not part of the current mandate (see [DECISIONS.md](DECISIONS.md)). Items that diverge from the reference have a «proposed, not approved» entry in [DEVIATIONS.md](DEVIATIONS.md#proposed-not-approved-wave-e). Sources are open-source projects from the 2026-10-17 market scan. The Codex runner and schedules, also seen in those projects, are already in Wave B; Wave E does not change them.

- **E1. Subscription limit: wait for the reset and resume.** Keep the last rate-limit event; hold new launches when the window is near its limit; on a usage-limit error wait for the reset + 60 s + jitter and resume the same session (needs E2). Why: subscription windows, not dollar budgets, stop overnight jobs; `--max-budget-usd` is a client-side estimate, not a subscription cap. Source: arbab. Related: Wave D API-credit fallback (E1 waits, the fallback pays). Priority: high. DEVIATIONS E1.
- **E2. True session resume.** Assign `--session-id` before launch and drop `--no-session-persistence`, so `job_resume` continues the same claude session instead of starting a fresh job from the saved patches. Why: a timed-out or killed job keeps its context, including turns that produced no commit. Source: agent-dispatch. Related: Wave B Resume (patch carry-over stays as the fallback). Priority: high. **Status: approved by the owner as a priority and built (stop, resume, amend; see In progress); DEVIATIONS #15.**
- **E3. Structured outcome.** Next to `report.md`, a machine-readable outcome (via `--json-schema`): status `done | partial | blocked`, changed files, checks with their exit codes, a not-verified list. Why: the orchestrator can triage without reading the report, and a job cannot claim a pass the outcome does not show. Source: arbab (fixed report schema), agent-dispatch (`STATUS: done|partial|blocked`). Related: the report contract (Done, PR #1); Wave D Progress section. Priority: high. DEVIATIONS E3.
- **E4. Verify by exit codes.** Check results are taken from tool exit codes in `stream.jsonl`, never from the model's summary; optionally a separate read-only verify job (no edit tools) runs the checks after a fix. Why: a report is a claim; exit codes are evidence. Source: arbab (verify / probe role). Related: Wave D `acceptance_cmd`. Priority: medium-high. DEVIATIONS E4.
- **E5. Claude Code native sandbox on top of the guard hook.** No network, no writes to `.git`, enforced by the OS rather than by command parsing (arbab: sandbox with `failIfUnavailable`, `dontAsk` plus an allowlist). Starts as a research spike on Linux (bubblewrap): what works under `claude -p` and what breaks (worktree git dirs, the job store, MCP). Why: the docs say the guard is not a sandbox (DEVIATIONS #9, #12); this would close part of that gap. Source: arbab. Related: Wave A cgroup containment, the guard. Priority: medium. DEVIATIONS E5 (only if adopted).
- **E6. `job_wait` / `job_peek`, an error taxonomy, flag-like input rejection.** `job_wait(timeout)` blocks until a job ends; `job_peek` returns a short progress tail; a failed job carries a cause (`budget`, `usage_limit`, `timeout`, `guard_refused`, `died`); MCP string inputs starting with `-` are refused. Why: a result within the same turn without Bash sleep loops; triage without opening logs; no CLI flag injection. Source: agent-dispatch, ai-cli-mcp (claude-async for exit causes). Related: PR #7 MCP job tools. Priority: low-medium. DEVIATIONS E6.
- **E7. Skills for jobs: a catalog and an explicit per-job selection.** Owner request, refined.
  - Today: `--setting-sources project` means user-level skills (`~/.claude/skills`, plugins) are not loaded; a job sees only the target repo's `.claude/skills` plus the profile and project cards. Skills tend to be scattered across hosts and agents, with many duplicates and plugin copies.
  - Catalog: one registry built by `agent-jobs skills index` from front matter in each `SKILL.md` (name, scope `common | project:<repo> | agent:<name> | kind:<code|review|research>`, path, size, has scripts, network/secrets risk, owner, version/sha, origin); duplicate names resolved by an explicit priority order; a lint step in `scripts/check.sh`.
  - Where skills live is up to you: keep them locally on the host, use a shared skills catalog repo across your projects, or use project skills in the repo's own `.claude/skills`. The index reads the roots you configure; agent-jobs ships no catalog and prescribes no source. Each entry records its origin (root, plugin name and version).
  - Discovery and selection: MCP tool `skill_list(scope?, kind?, repo?)`. Defaults (the `core` skills for the job kind plus the project card's skills) are attached automatically; `job_start(skills=[...])` only adds or excludes (`+name`, `-name`); an unknown name refuses the start. Deterministic matching by kind and project first, hard budget of 5-8 skills per job; semantic retrieval only if matching proves insufficient.
  - Pre-launch skill hook: a PreToolUse hook in the orchestrator session on `job_start`, installed by the agent-jobs skill installer. Without `skills_decision` it denies once with up to about 8 candidates (status and a one-line description each); the orchestrator re-calls with `skills=[...]` or `skills_decision='defaults'`. No model call in the hook (the classifier was rejected, see DECISIONS). One extra round trip per job; deprecated/draft names refused unless explicit; the decision is stored in job metadata.
  - Lifecycle: `status: core | active | draft | deprecated`, `owner`, `last_verified`. deprecated is never loaded; draft only on explicit request. Staleness: not used for 60 days or not verified for 90 days goes to a review list; a weekly digest to the orchestrator.
  - Stats: skills used (Skill tool calls in the job's session log) and the job outcome go to SQLite; `agent-jobs skills stats` shows uses, success rate, last used, and feeds the sort order of `skill_list`. Built ahead of the rest (owner approved, see In progress): the `job_stats` table records skills used per job and `agent-jobs stats --by skill` shows uses and success rate.
  - Delivery: copy only the selected skill directories (no symlinks, size cap, sha in job metadata) into the job's project `.claude/skills`; the report lists the skills used. Skills whose scripts reach the network or secrets are flagged; the guard and deny rules still apply; never the whole tree.
  - Native controls: `disable-model-invocation` for side-effect skills, `paths:` globs, `/skill-doctor` for context cost and never-invoked skills, `claude plugin eval` for trigger accuracy of 3-5 critical skills. Not adopted as a base: skills.sh, openskills, skillport (they widen distribution; the goal is narrowing); skillport's «metadata first» idea is borrowed.
  - Evidence, cited, to verify: arXiv 2604.04323 (from a market scan): with many weakly relevant skills the gain drops to near the no-skill baseline; skills refined to the task restore most of it (Terminal-Bench 2.0 57.7% → 65.5%).
  - Why: jobs get the right procedural knowledge without loading hundreds of skills. Source: owner request; market scan 2026-10-17. Related: Wave A `agent-jobs` skill and project cards. Priority: high. DEVIATIONS E7.

## Conformance check against the reference after every wave

Before a wave is merged, the reference is walked line by line: for each claim, is it implemented, where (`file:line`), and is it 1:1 or a listed deviation? A deviation that is not in [DEVIATIONS.md](DEVIATIONS.md) blocks the merge. The result table is published with the wave.
