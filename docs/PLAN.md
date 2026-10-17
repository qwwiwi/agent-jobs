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
| [#8](https://github.com/qwwiwi/agent-jobs/pull/8) | **Skill, subagents, job context** (moved out of Wave A). `agent-jobs` skill installed per agent by `scripts/install-linux.sh` (routing rule, model-or-complexity, how to read a result); jobs may use at most three subagents inside their own limits, guard-counted, only the input keys `description`, `prompt`, `subagent_type`, `model`; job profile (≤ 8 KB, `agent-jobs profile --from`) and project cards (≤ 4 KB, `agent-jobs card new\|list`) attached to the brief. |

## In progress

**[PR #7](https://github.com/qwwiwi/agent-jobs/pull/7): MCP job tools** (supersedes [#4](https://github.com/qwwiwi/agent-jobs/pull/4)).

- One Go MCP server per agent with two roles: `orchestrator` (main session) and `job`. The job role is an allowlist; the server itself refuses hidden tools, and a test fails if a tool is in neither list.
- `job_start`, `job_status`, `job_result`, `job_list`, `job_cancel` on top of the existing manager.
- A job's `mcp.json` points at the job-role server.
- The caller must give a model or a complexity: no classifier, no guessing.
- tmux socket support for the job-done line.
- Guard: a job cannot start or control other jobs through the CLI (environment marker, process-ancestry and session checks).
- Session sweep: after claude exits, the wrapper ends every leftover process of its session before it writes `exit_code`; signals go through pidfds, and survivors fail the job («processes survived the job»).

**[PR #9](https://github.com/qwwiwi/agent-jobs/pull/9): prune, doctor procs, system and file tools.**

- `agent-jobs prune [--dry-run]`: folders of jobs finished more than 48 hours ago keep their records and small top-level text files; work dirs, caches, builds, binaries, large files and symlinks go.
- `agent-jobs doctor procs [--kill]`: processes that outlived their job or session and running rows with a dead pid; report only, `--kill` ends only processes proven to be a finished job's (DEVIATIONS #13).
- Orchestrator tools `system_doctor`, `system_logs`, `activity_report`, `trash_move`, `trash_restore` (none for jobs).

## Wave A

- **Cgroup containment for jobs (hardening).** Session and ppid checks can be escaped with `setsid` plus reparenting; a cgroup cannot. On Linux:
  1. The daemon launches each wrapper in its own transient systemd user scope, `systemd-run --user --scope --unit=agent-jobs-<agent>-<id> …` (or the same call over D-Bus).
  2. Control commands refuse when `/proc/self/cgroup` names an `agent-jobs-*` job scope. setsid, double-fork and reparenting all stay inside the cgroup.
  3. On wrapper exit, cancel or timeout the whole scope is stopped (`systemctl --user stop` / kill the unit) before `exit_code` is written, so no descendant survives.
  4. Fallback when `systemd-run --user` is unavailable (macOS, containers, no user bus): today's sid sweep plus the marker, ancestry, session and store checks. That path is documented as weaker.
  5. Needs: a reachable user bus for the daemon (linger is already on), a launch-path test that a `setsid` + reparented orphan is still refused and still killed, and a check that `KillMode=process` daemon restarts leave job scopes alone.
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

## Wave E: borrowed ideas (planned, not approved)

Each item waits for a separate owner go; it is not part of the current mandate (see [DECISIONS.md](DECISIONS.md)). Items that diverge from the reference have a «proposed, not approved» entry in [DEVIATIONS.md](DEVIATIONS.md#proposed-not-approved-wave-e). Sources are open-source projects from the 2026-10-17 market scan. The Codex runner and schedules, also seen in those projects, are already in Wave B; Wave E does not change them.

- **E1. Subscription limit: wait for the reset and resume.** Keep the last rate-limit event; hold new launches when the window is near its limit; on a usage-limit error wait for the reset + 60 s + jitter and resume the same session (needs E2). Why: subscription windows, not dollar budgets, stop overnight jobs; `--max-budget-usd` is a client-side estimate, not a subscription cap. Source: arbab. Related: Wave D API-credit fallback (E1 waits, the fallback pays). Priority: high. DEVIATIONS E1.
- **E2. True session resume.** Assign `--session-id` before launch and drop `--no-session-persistence`, so `job_resume` continues the same claude session instead of starting a fresh job from the saved patches. Why: a timed-out or killed job keeps its context, including turns that produced no commit. Source: agent-dispatch. Related: Wave B Resume (patch carry-over stays as the fallback). Priority: high. DEVIATIONS E2.
- **E3. Structured outcome.** Next to `report.md`, a machine-readable outcome (via `--json-schema`): status `done | partial | blocked`, changed files, checks with their exit codes, a not-verified list. Why: the orchestrator can triage without reading the report, and a job cannot claim a pass the outcome does not show. Source: arbab (fixed report schema), agent-dispatch (`STATUS: done|partial|blocked`). Related: the report contract (Done, PR #1); Wave D Progress section. Priority: high. DEVIATIONS E3.
- **E4. Verify by exit codes.** Check results are taken from tool exit codes in `stream.jsonl`, never from the model's summary; optionally a separate read-only verify job (no edit tools) runs the checks after a fix. Why: a report is a claim; exit codes are evidence. Source: arbab (verify / probe role). Related: Wave D `acceptance_cmd`. Priority: medium-high. DEVIATIONS E4.
- **E5. Claude Code native sandbox on top of the guard hook.** No network, no writes to `.git`, enforced by the OS rather than by command parsing (arbab: sandbox with `failIfUnavailable`, `dontAsk` plus an allowlist). Starts as a research spike on Linux (bubblewrap): what works under `claude -p` and what breaks (worktree git dirs, the job store, MCP). Why: the docs say the guard is not a sandbox (DEVIATIONS #9, #12); this would close part of that gap. Source: arbab. Related: Wave A cgroup containment, the guard. Priority: medium. DEVIATIONS E5 (only if adopted).
- **E6. `job_wait` / `job_peek`, an error taxonomy, flag-like input rejection.** `job_wait(timeout)` blocks until a job ends; `job_peek` returns a short progress tail; a failed job carries a cause (`budget`, `usage_limit`, `timeout`, `guard_refused`, `died`); MCP string inputs starting with `-` are refused. Why: a result within the same turn without Bash sleep loops; triage without opening logs; no CLI flag injection. Source: agent-dispatch, ai-cli-mcp (claude-async for exit causes). Related: PR #7 MCP job tools. Priority: low-medium. DEVIATIONS E6.
- **E7. Skills for jobs: a catalog and an explicit per-job selection.** Owner request, refined.
  - Today: `--setting-sources project` means user-level skills (`~/.claude/skills`, plugins) are not loaded; a job sees only the target repo's `.claude/skills` plus the profile and project cards. Skills tend to be scattered across hosts and agents, with many duplicates and plugin copies.
  - Catalog: one registry built by `agent-jobs skills index` from front matter in each `SKILL.md` (name, scope `common | project:<repo> | agent:<name> | kind:<code|review|research>`, path, size, has scripts, network/secrets risk, owner, version/sha, origin); duplicate names resolved by an explicit priority order; a lint step in `scripts/check.sh`.
  - Storage: a single git repo of skills is the source of truth (a private repo set in config, not part of this project: `common/`, `agents/<name>/`, `projects/<repo>/`), pulled by each host; the index reads only from it plus repo-local `.claude/skills`; ad-hoc copies are not indexed. Same format on every host.
  - Sources rule (owner decision): NEW skills from public registries or marketplaces are never downloaded without the owner's permission. Everything already installed on the hosts may be used: own workspaces and repos, and installed plugins (referenced in the catalog by plugin name and version, not copied). The index refuses entries with no recorded origin.
  - Discovery and selection: MCP tool `skill_list(scope?, kind?, repo?)`. Defaults (the `core` skills for the job kind plus the project card's skills) are attached automatically; `job_start(skills=[...])` only adds or excludes (`+name`, `-name`); an unknown name refuses the start. Deterministic matching by kind and project first, hard budget of 5-8 skills per job; semantic retrieval only if matching proves insufficient.
  - Pre-launch skill hook: a PreToolUse hook in the orchestrator session on `job_start`, installed by the agent-jobs skill installer. Without `skills_decision` it denies once with up to about 8 candidates (status and a one-line description each); the orchestrator re-calls with `skills=[...]` or `skills_decision='defaults'`. No model call in the hook (the classifier was rejected, see DECISIONS). One extra round trip per job; deprecated/draft names refused unless explicit; the decision is stored in job metadata.
  - Lifecycle: `status: core | active | draft | deprecated`, `owner`, `last_verified`. deprecated is never loaded; draft only on explicit request. Staleness: not used for 60 days or not verified for 90 days goes to a review list; a weekly digest to the orchestrator.
  - Stats: skills used (Skill tool calls in the job's session log) and the job outcome go to SQLite; `agent-jobs skills stats` shows uses, success rate, last used, and feeds the sort order of `skill_list`.
  - Delivery: copy only the selected skill directories (no symlinks, size cap, sha in job metadata) into the job's project `.claude/skills`; the report lists the skills used. Skills whose scripts reach the network or secrets are flagged; the guard and deny rules still apply; never the whole tree.
  - Native controls: `disable-model-invocation` for side-effect skills, `paths:` globs, `/skill-doctor` for context cost and never-invoked skills, `claude plugin eval` for trigger accuracy of 3-5 critical skills. Not adopted as a base: skills.sh, openskills, skillport (they widen distribution; the goal is narrowing); skillport's «metadata first» idea is borrowed.
  - Evidence, cited, to verify: arXiv 2604.04323 (from the coordinator's scan): with many weakly relevant skills the gain drops to near the no-skill baseline; skills refined to the task restore most of it (Terminal-Bench 2.0 57.7% → 65.5%).
  - Why: jobs get the right procedural knowledge without loading hundreds of skills. Source: owner request; coordinator's market scan 2026-10-17. Related: Wave A `agent-jobs` skill and project cards. Priority: high. DEVIATIONS E7.

## Conformance check against the reference after every wave

Before a wave is merged, the reference is walked line by line: for each claim, is it implemented, where (`file:line`), and is it 1:1 or a listed deviation? A deviation that is not in [DEVIATIONS.md](DEVIATIONS.md) blocks the merge. The result table is published with the wave.
