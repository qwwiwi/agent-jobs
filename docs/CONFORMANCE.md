# Conformance check against the reference (2026-10-18)

Base: `main` at `d747829`. Walked: [`REFERENCE.md`](REFERENCE.md) (English translation; the Russian original [`REFERENCE.ru.md`](REFERENCE.ru.md) wins where they differ) line by line against the code, with [`DEVIATIONS.md`](DEVIATIONS.md) rows #1–#16 as the only approved differences. Procedure: [`PLAN.md`](PLAN.md), «Conformance check against the reference after every wave».

**Update 2026-10-18:** Finding U1 is fixed (guard tool allowlist) and U2–U11 are listed as [DEVIATIONS #17–#26](DEVIATIONS.md); the table cells below point at those rows. Line numbers stay those of `d747829`.

Status values: **1:1**, **deviation #N** (listed in DEVIATIONS.md), **missing** (not implemented), **unlisted deviation** (differs from the reference and no DEVIATIONS.md row covers it). Paths are repository-relative; line numbers are at `d747829`.

## Table

### How a job is started (REFERENCE.md 13–46)

| Reference claim | Status | Where |
|---|---|---|
| «calls the MCP tool `job_start` with a title, a kind of work and a brief» | 1:1 (+ required model or complexity: deviation #6) | `internal/mcp/tools_jobs.go:156`, `internal/jobs/manager.go:222-230` |
| «returns an id at once … goes into a queue in SQLite» | 1:1 | `internal/jobs/manager.go:306` (`Status: store.StatusQueued`), `internal/store/store.go:176` |
| «Once a minute the daemon» | 1:1 | `internal/jobs/config.go:36` (`TickInterval = time.Minute`), `cmd/agent-jobs/main.go:1031` |
| «at most two jobs per agent» | deviation #27 (configurable `max_running`, default 2) | `internal/jobs/config.go` (`DefaultMaxRunningPerAgent = 2`, `CeilingMaxRunningPerAgent = 8`, config `max_running`), `internal/jobs/manager.go` (`launchQueued`) |
| Launch argv `claude -p --output-format stream-json --verbose --model M --max-turns N --max-budget-usd B --permission-mode bypassPermissions --settings … --setting-sources project --mcp-config … --strict-mcp-config --add-dir <jobdir> < prompt.md` | 1:1 flag by flag | `internal/jobs/manager.go:441-459`; stdin `internal/jobs/wrapper.go:406` |
| «`--no-session-persistence`» | deviation #15 (`--session-id`, `--resume`) | `internal/jobs/manager.go:441-459` |
| Second runner | deviation #5 (`codex exec`) | `internal/jobs/codex.go` |
| «started detached, in its own process group, and writes its exit code to a file itself» | 1:1 (+ scope: deviation #14; session sweep: deviation #9) | `internal/jobs/manager.go:565` (`Setsid: true`), `internal/jobs/wrapper.go:290` |
| «the new daemon finds the exit-code file on the next tick» | 1:1 | `internal/jobs/manager.go:877-882` (`readExitCode`), `:535` (pid adopted from `wrapper.json`) |
| Checklist review: «real bugs with file:line, the consequence and a minimal fix; verified findings kept apart from suspicions» | deviation (the reference row is kept, much is added; Finding U6 → #21) | `prompts/kind-review.md:3-12` |
| Checklist fix: «reproduce with a failing test, then the minimal change, tests, a local commit» | 1:1 | `prompts/kind-fix.md:3-4` |
| Checklist research: «only primary sources … verified kept apart from inferred» | 1:1 | `prompts/kind-research.md:3-4` |
| Checklist general: «state what counts as done, and prove it at the end» | 1:1 | `prompts/kind-general.md:3-4` |
| Defaults / ceilings 80/200 turns, $10/$50, 45/180 min | 1:1 | `internal/jobs/config.go:24-29` |
| «A request above a ceiling is rejected, not silently cut down» | 1:1 | `internal/jobs/manager.go:151` (`limits rejected (not clamped)`) |
| «concurrent per agent 2» (no ceiling) | 1:1 (a constant, not configurable) | `internal/jobs/config.go:30` |
| Job-done line `[reference:job-done] job <id> … succeeded · 39 turns · $1.76 · 28m` | deviation #7 (tag `[jobs:…]`); extra suffixes: `model <m> · effort <e>` (issue #40), containment #14, `WARNING: review job changed the worktree` (issue #39), survivors #9, `resumed (attempt N)` #15, `owner waits` (reference line 90), acceptance note (deviation, Finding U3 → #18) | `internal/jobs/finish.go:72-98` |
| «The report itself is not poured into the session; … `job_result`» | 1:1 | `internal/mcp/tools_jobs.go:256` |
| Model choice | deviation #6 (kind × complexity) | `internal/jobs/config.go:398-405, 444` |

### Where everything is kept (REFERENCE.md 48–65)

| Reference claim | Status | Where |
|---|---|---|
| «Each job has its own folder `jobs/<id>/` … status, cost and pid … in SQLite» | 1:1 | `internal/jobs/manager.go:1036` (`JobDir`), `internal/store/store.go:176` |
| «Statuses: queued, running, succeeded, failed, timeout, cancelled; only the first two can change» | deviation #15 (`stopped`; `stopped`/`timeout`/`failed` can be requeued by `job_resume`) | `internal/store/store.go:18-26, 911-930` |
| `prompt.md` | 1:1 | `internal/jobs/prepare.go:306` |
| `settings.json` «deny rules and the guard hook» | 1:1 (plus `disableAllHooks: false`) | `internal/jobs/prepare.go:182-200` |
| `mcp.json` «the job's MCP servers, and only those» | 1:1 | `internal/jobs/prepare.go:271-279, 300` |
| `work/` «a git worktree … or an empty directory» | 1:1 | `internal/jobs/prepare.go:317-331` |
| `base_commit` | 1:1 | `internal/jobs/prepare.go:353` |
| `stream.jsonl`, `stderr.log`, `exit_code`, `report.md` | 1:1 | `internal/jobs/wrapper.go:412`, `:236`, `:290`, `:278` |
| `diffstat.txt`, `commits.patch`, `uncommitted.patch` | 1:1 | `internal/jobs/finish.go:360-376` |
| `staged.patch`, `untracked.tar` | deviation #10 | `internal/jobs/finish.go:375, 386` |
| `attempts/<n>/`, `stream-resume-failed.jsonl` | deviation #15 (the `attempts` table is named; the directory is implied) | `internal/jobs/resume.go:37`, `internal/jobs/wrapper.go:358` |
| `acceptance.log`, `report.raw.md` | **unlisted deviation** (Findings U3, U4) | `internal/jobs/acceptance.go:43`, `internal/jobs/redact.go:483` |
| Report contract «four sections: Summary, Findings / Changes, Evidence, Not done / uncertain … the last section is mandatory» | four sections 1:1; added `## Progress` section is a deviation (Finding U5 → #20) | `prompts/report-contract.md:5-24`, `internal/mcp/tools_jobs.go:207-210` |

### How we clean up (REFERENCE.md 67–77)

| Reference claim | Status | Where |
|---|---|---|
| «30 minutes after the notification the daemon removes the worktree» | 1:1 | `internal/jobs/config.go:37` (`CleanupDelay`), `internal/jobs/finish.go:230` |
| diffstat, `git format-patch`, `git diff --binary` before removal | 1:1 | `internal/jobs/finish.go:360-376` |
| ref `refs/reference/jobs/<id>` | deviation #7 (`refs/agent-jobs/jobs/<id>`) | `internal/jobs/finish.go:393` |
| No patch when the repo config names a filter / diff driver (worktree kept) | deviation #9 | `internal/jobs/finish.go:435` |
| «Once a day `jobs prune` … more than 48 hours ago. The records stay … binaries, caches and builds go» | 1:1 | `internal/jobs/prune.go:27` (`PruneAge = 48 * time.Hour`), `:287-321`; daily run `internal/jobs/daily.go:84, 154` |
| Prune moves logs into a gzip archive instead of deleting them | deviation (Finding U7 → #22) | `internal/jobs/archive.go:19-23`, `internal/jobs/prune.go:74-76` |
| Prune removes the claude session files | deviation #15 | `internal/jobs/prune.go` |
| «Once a day a ghost check (`doctor procs`) … preview servers, Playwright browsers, wait loops, running rows with a dead pid. By default it only reports; it kills on a flag» | 1:1 (+ kill scope: deviation #13) | `internal/jobs/doctor.go:41, 219-247, 263`; daily report-only `internal/jobs/daily.go:222` |
| «`jobs resume <id>` creates a new job with the same brief, carries over the old commits and diff, and writes at the top of the brief what was carried over» | deviation #15 (same job id and session; patch carry-over as the fallback) | `internal/jobs/resume.go:608, 826-889, 924-957` |

### Rules (REFERENCE.md 79–90)

| Reference claim | Status | Where |
|---|---|---|
| Rules held by deny rules in settings.json and a hook, not the prompt | 1:1 | `internal/jobs/prepare.go:15-106, 182-200` |
| «Forbidden: `git push`, `git tag`, `gh release`, `gh api`, creating and commenting on PRs and issues» | 1:1 (more gh write forms denied: deviation #9) | `internal/jobs/prepare.go:16-38` |
| «tools that reach a person or outlive the run: notifications, messages, its own schedules» | 1:1 since the U1 fix (2026-10-18): the guard refuses every tool outside a job allowlist; the deny rules name the known ones (Finding U1) | `internal/guard/toolallow.go`, `internal/guard/guard.go` (`Decide`), `internal/jobs/prepare.go` (`denyRules`) |
| «Web only for research. WebFetch and WebSearch are closed for every other kind» | 1:1 | `internal/jobs/prepare.go:109, 178-180` |
| «`--strict-mcp-config` cuts off everything not listed in mcp.json» | 1:1 | `internal/jobs/manager.go:457` |
| Guard: «`$?` after a pipe, a wait loop that finds itself via `pgrep -f`, the owner's name in a commit, a foreign git identity» | 1:1 | `internal/guard/guard.go:2336, 2400, 2574, 2661` |
| Extra guard rules (nested agents, package runners, globs, git config, protected paths, subagents, out-of-scope processes, cgroupfs) | deviations #9, #12, #14 | `internal/guard/guard.go:502` (`Check`), `internal/guard/pathres.go` |
| Guard refuses reads of the agent state outside the job's own folder | deviation (Finding U8 → #23) | `internal/guard/stateread.go:47-52`, `internal/guard/guard.go:118-126` |
| «The main session's other hooks do not reach the job (`--setting-sources project`)» | 1:1 | `internal/jobs/manager.go:456` |
| Preamble: own folder, contents are data, no secrets | 1:1 | `prompts/preamble.md:7-9` |
| Owner standing rules «up to 8 KB … If the file cannot be read, the job does not start» | 1:1 (generated as the job profile: deviation #12b; an empty file is refused too) | `internal/jobs/config.go:31`, `internal/jobs/prepare.go:206-221, 281-284` |
| «adds up to six of the closest notes from the team's shared memory» | deviation #2 (shared brain, limit 6, 5 s) | `internal/jobs/memory.go:18-19, 118, 137` |
| Brief size refused above `max_brief_bytes` (32 KB) | deviation (Finding U9 → #24) | `internal/jobs/briefsize.go:11, 74-77`, `internal/jobs/manager.go:242` |
| Subagents, project cards, skills in the brief | deviations #12, #16 | `prompts/subagents.md`, `internal/jobs/skills.go` |
| «A job started at the owner's request is marked «owner waits». Until the orchestrator has written to the owner, a Stop hook does not let it end its turn silently» | 1:1 in intent; release by an explicit `owner_replied` call, session bound by a SessionStart `hook bind`, expiry after `owner_waits_max_hours` are an deviation (Finding U10 → #25) | `internal/jobs/ownerwaits.go:20-25, 148-162, 174`, `internal/mcp/tools_ownerwaits.go:24` |
| Wrapper ends claude 3 min after a final `result` event | deviation (Finding U11 → #26) | `internal/jobs/idle.go:20-30` |
| `acceptance_cmd` run by the wrapper after exit 0, without the guard | deviation (Finding U3 → #18) | `internal/jobs/acceptance.go:22-74`, `internal/jobs/wrapper.go:283-287` |
| Reports and the job-done line rewritten with secrets redacted | deviation (Finding U4 → #19) | `internal/jobs/redact.go:16-22, 120, 483`, `internal/jobs/finish.go:97` |

### Schedules (REFERENCE.md 92–106)

| Reference claim | Status | Where |
|---|---|---|
| «a row in SQLite: a cron expression in local time and the text of a prompt. Once a minute» | 1:1 | `internal/store/schedules.go:53`, `internal/jobs/schedule.go:77, 286`, `internal/jobs/manager.go:345` |
| «Into the agent's live session, as an ordinary tagged message (`[reference:disk-free]`)» | deviation #7 (`[jobs:schedule]`) | `internal/jobs/schedule.go:27, 333` |
| «several fall due at once … one message with the note «these are different instructions, do each one»» | 1:1 | `internal/jobs/schedule.go:30, 263-272` |
| «As a job, if the schedule has a kind set» | 1:1 | `internal/jobs/schedule.go:309-318` |
| «`schedule edit <id> --job-kind research`» | deviation (no `schedule edit`; the kind and job fields are fixed at creation; edits go through the extra tool `schedule_set`; Finding U2 → #17) | `internal/mcp/tools_schedules.go:33-38, 117-121`, `cmd/agent-jobs/main.go:174` |
| «A missed firing is lost, not queued» | 1:1 | `internal/jobs/schedule.go:18-19, 300-338`, `internal/store/schedules.go:238` |
| «writes a completion mark, and a separate «dead man's switch» compares the marks with the schedule» | 1:1 in idea; checks carry their own cron and grace, and the `check_*` tools are not in the reference MCP table: deviation (Finding U2 → #17) | `internal/jobs/checks.go:51, 215, 278, 620`, `internal/mcp/tools_checks.go:72-145` |
| «The schedule row itself never expires … edit it in place … re-read it … delete it only by the owner's decision» | 1:1 | `internal/store/schedules.go:199-218`, `internal/mcp/tools_schedules.go:133-134` |
| Heartbeat: «a cheap check in Go … then a small model decides» | deviation #1 (Sonnet decides, not Haiku); built, off by default (`heartbeat.enabled`) | `internal/jobs/heartbeat.go:36, 256, 489-491, 609, 771`, `internal/store/heartbeats.go`, `internal/mcp/tools_heartbeat.go` |

### MCP tools (REFERENCE.md 108–125)

| Reference claim | Status | Where |
|---|---|---|
| «one MCP server of the system, written in Go … separately for each agent, and each call sees only that agent's data» | 1:1 | `internal/mcp/server.go:191, 199`, `internal/jobs/manager.go:60` |
| Jobs: `job_start`, `job_status`, `job_result`, `job_list`, `job_cancel` | 1:1 | `internal/mcp/tools_jobs.go:156, 184, 256, 327, 366`, `internal/mcp/registry.go:35-40` |
| `job_resume` | deviation #15 | `internal/mcp/tools_resume.go:94` |
| `job_stop`, `job_amend` | deviation #15 | `internal/mcp/tools_resume.go:65, 124` |
| `job_stats` | deviation (Finding U2 → #17) | `internal/mcp/tools_stats.go:19`, `internal/mcp/registry.go:44` |
| `skill_list` | deviation #16 | `internal/mcp/tools_skills.go:22` |
| Memory `memory_search`, `memory_write`, `memory_already_exists`; job gets the first two | deviation #2 | `internal/mcp/tools_brain.go:65, 113, 164`, `internal/mcp/registry.go:85-86` |
| Tasks `task_create`, `task_list`, `task_update`, `task_get` | deviation #3 | `internal/mcp/tools_brain.go:206, 231, 256, 276` |
| Schedules `schedule_add`, `schedule_list`, `schedule_remove` | 1:1 | `internal/mcp/tools_schedules.go:81, 102, 133` |
| `schedule_set` | deviation (Finding U2 → #17) | `internal/mcp/tools_schedules.go:119`, `internal/mcp/registry.go:68` |
| Own goals `intention_add`, `intention_list`, `intention_set` | 1:1 | `internal/mcp/tools_intentions.go:42, 53, 63` |
| Team `send_message`, `inbox` | deviation #3 | `internal/mcp/tools_brain.go:295, 326` |
| Owner `notify_owner`, «reply in Telegram»; not for a job | deviation #4 (agent's own channel; job role allowed) | `internal/mcp/tools_notify.go:34`, `internal/mcp/registry.go:87`, `internal/mcp/server.go:117` |
| `owner_replied` | deviation (Finding U10 → #25) | `internal/mcp/tools_ownerwaits.go:24`, `internal/mcp/registry.go:77` |
| `check_add`, `check_set`, `check_list`, `check_remove`; `check_mark` also for a job | deviation (Finding U2 → #17) | `internal/mcp/tools_checks.go:72, 91, 107, 123, 136`, `internal/mcp/registry.go:72-75, 88` |
| System `system_doctor`, `system_logs`, `activity_report` | 1:1 | `internal/mcp/tools_system.go:53, 156, 204` |
| Files `trash_move`, `trash_restore` | 1:1 | `internal/mcp/tools_files.go:62, 94` |
| Code: «a code index as a graph (a separate server)», job may read and index | **missing** (PLAN Wave C, not started) | no code |
| «built as an allowlist … a test fails if a tool is in neither list» | 1:1 | `internal/mcp/registry.go:84-114, 162-180`; test `internal/mcp/mcp_test.go:141` (`TestEveryToolInExactlyOneList`, passes) |

### How to do it yourself (REFERENCE.md 127–136)

| Reference claim | Status | Where |
|---|---|---|
| 1. Queue and a per-minute loop «via launchd or cron» | 1:1 (systemd user service: deviation #11) | `internal/store/store.go:176`, `internal/jobs/manager.go:318`, `launchd/`, `deploy/systemd/agent-jobs@.service` |
| 2. One launch command, separate worktree, exit code to a file | 1:1 (#15 session id) | `internal/jobs/manager.go:441-459`, `internal/jobs/wrapper.go:290` |
| 3. Bans in settings | 1:1 | `internal/jobs/prepare.go:15-106` |
| 4. A fixed report | 1:1 (+ Progress, Finding U5 → #20) | `prompts/report-contract.md` |
| 5. Notification via tmux send-keys | 1:1 | `internal/jobs/finish.go:176-179` |
| 6. Cleanup from day one, keeping the patches | 1:1 (+ #10) | `internal/jobs/finish.go:322-399` |

## Findings

### Unlisted deviations

Each was checked with `grep -n -i` for its key terms in `docs/DEVIATIONS.md`; none returned a covering row (the only `progress` hit is row #3, the task board's states).

- **U1. Tools that reach other sessions or outlive the run are not denied.** The reference forbids «tools that reach a person or outlive the run: notifications, messages, its own schedules». The deny list has only `CronCreate`, `RemoteTrigger`, `PushNotification` (`internal/jobs/prepare.go:102-105`); `ScheduleWakeup`, `CronDelete` and `SendMessage` (messages to other local sessions) are not denied, and the guard passes every tool that is not Bash, a file tool or the subagent tool (`internal/guard/guard.go:138-140`: `if in.ToolName != "Bash" { return "", nil }`). This job's own tool list (2026-10-18) offered `ScheduleWakeup` directly and `SendMessage`, `CronDelete` as deferred tools; they were not called. `grep -rn -i schedulewakeup internal cmd` is empty. This is a gap in the implementation, not only a missing DEVIATIONS row. **Fixed (2026-10-18):** the guard now refuses every tool outside a job allowlist (`internal/guard/toolallow.go`, called first in `Decide`), so an unknown future tool is refused too; MCP tools pass only by their exact job-role names (fix loop 1: a prefix match admitted a server named `agent-jobs__other`); `Monitor` is refused because its command bypasses the Bash checks (allowing a checked `Monitor` is PLAN idea I8), `ScheduleWakeup` as job policy, both with a hint to wait in the foreground or a bounded Bash loop; the deny rules in `internal/jobs/prepare.go` name `ScheduleWakeup`, `CronDelete`, `CronList`, `SendMessage`, `Monitor`, `ListAgents`, `Workflow`, the worktree, question and Artifact tools as well.
- **U2. Extra MCP tools and changed schedule editing.** `schedule_set` (`internal/mcp/tools_schedules.go:119`), `check_add`/`check_set`/`check_list`/`check_remove` (`internal/mcp/tools_checks.go:72-123`), `check_mark` in the job role (`internal/mcp/registry.go:88`), `job_stats` (`internal/mcp/tools_stats.go:19`) are not in the reference table and not in DEVIATIONS.md (#16 names only CLI `stats --by skill --by-repo` and `GET /v1/skills`). The reference's `schedule edit <id> --job-kind research` cannot be done: no CLI edit, and `schedule_set` says «The job fields of a job schedule are fixed at creation» (`internal/mcp/tools_schedules.go:120-121`). Checks keep their own cron and grace instead of comparing marks with the schedule (`internal/jobs/checks.go:215`). **Listed (2026-10-18):** [DEVIATIONS #17](DEVIATIONS.md).
- **U3. `acceptance_cmd`.** A shell command run by the wrapper after exit 0, outside the guard, with `acceptance.log` and an acceptance note on the job-done line (`internal/jobs/acceptance.go:22-74`, `internal/jobs/wrapper.go:283-287`, `internal/jobs/finish.go:85`). Owner approval is recorded in DECISIONS.md (full mandate) but there is no DEVIATIONS row. **Listed (2026-10-18):** [DEVIATIONS #18](DEVIATIONS.md).
- **U4. Secret redaction.** `report.md` is rewritten in place, the raw copy kept only with `keep_raw_reports` (`report.raw.md`); the job-done line, `job_result` and the stats row are redacted (`internal/jobs/redact.go:16-22, 120, 483`, `internal/jobs/finish.go:97`). The reference says a report is the job's final message, stored as is. **Listed (2026-10-18):** [DEVIATIONS #19](DEVIATIONS.md).
- **U5. Report contract has a fifth section `## Progress`** (`prompts/report-contract.md:5, 20-24`; `job_result` returns `progress`, `internal/mcp/tools_jobs.go:207-209`). The reference fixes four sections. **Listed (2026-10-18):** [DEVIATIONS #20](DEVIATIONS.md).
- **U6. Review checklist extended.** Risky areas first, three questions Delivered / Design / Code, `### Blocking` / `### Non-blocking`, and a `Verdict:` first line (`prompts/kind-review.md:3-12`). The reference row is kept (line 8), the rest is added. **Listed (2026-10-18):** [DEVIATIONS #21](DEVIATIONS.md).
- **U7. Log archive in prune.** Logs are gzipped into `<archive>/<YYYY-MM>/<id>/` and expired after `log_archive_days` instead of removed (`internal/jobs/archive.go:19-23`, `internal/jobs/prune.go:74-76`). Owner approval is in DECISIONS.md 2026-10-17, no DEVIATIONS row. **Listed (2026-10-18):** [DEVIATIONS #22](DEVIATIONS.md).
- **U8. Guard refuses reads of the agent state dir outside the job's own folder** (`internal/guard/stateread.go:47-52`). #12 covers protected writes only. **Listed (2026-10-18):** [DEVIATIONS #23](DEVIATIONS.md).
- **U9. Brief size refusal.** `job_start` refuses a brief above `max_brief_bytes` (32 KB) and warns on many files or headings (`internal/jobs/briefsize.go:11-13, 74-77`, `internal/jobs/manager.go:242`). **Listed (2026-10-18):** [DEVIATIONS #24](DEVIATIONS.md).
- **U10. «Owner waits» release mechanism.** The reference releases the hold once the orchestrator «has written to the owner»; the code releases it on an explicit `owner_replied` tool call, binds the session through a SessionStart `agent-jobs hook bind`, and expires records after `owner_waits_max_hours` (6 h) (`internal/jobs/ownerwaits.go:20-25, 148-162, 174`, `internal/mcp/tools_ownerwaits.go:24`). **Listed (2026-10-18):** [DEVIATIONS #25](DEVIATIONS.md).
- **U11. Idle-after-result kill.** The wrapper ends claude 3 minutes after a final `result` event when the stream is silent and claude has no live descendants (`internal/jobs/idle.go:20-30`, config `idle_after_result_seconds`). **Listed (2026-10-18):** [DEVIATIONS #26](DEVIATIONS.md).
- Minor: control files `wrapper.json`, `wrapper.lock`, `admit.lock`, `worktree.json` and the `attempts/<n>/` directory are not named in the reference file table or in DEVIATIONS.md (they serve #9, #14, #15). **Named (2026-10-18)** in the job folder table of [`ARCHITECTURE.md`](ARCHITECTURE.md).

### Missing (planned, not deviations)

- **Code-graph MCP server** (REFERENCE.md 123): nothing built. PLAN Wave C.

Heartbeat (REFERENCE.md 106), listed here before, is built since (Wave D, `internal/jobs/heartbeat.go`, `internal/store/heartbeats.go`, off by default); its only difference from the reference is the model, deviation #1. See the Schedules table above.

### Rows that were «owner OK pending»

DEVIATIONS #4, #9, #10, #17, #23, #25 and #26 were approved by the owner on 2026-10-18 (DECISIONS.md, Telegram msg 64585). No listed row is pending now.

### PLAN.md / PLAN.ru.md status lines that did not match main (fixed in this commit)

- «In progress» listed PR #10 (cgroup containment), PR #9 (prune, doctor procs, system and file tools) and Wave E2 (stop, resume, amend). All three are merged and released in v0.3.0 (`CHANGELOG.md` section 0.3.0; merge `8cac5b4` «Merge main (PR #10 cgroup scope, PR #13 brain tools, PR #19 log archive and stats)», `cc37a21` «… PR #8/#9 …», release `99749ce`; code `internal/jobs/scope.go`, `cmd/agent-jobs/main.go:150-182`). Moved to «Done» together with #13 and #19; «In progress» now says nothing is open on main.
- E2 status said «see In progress» and the E7 stats note said «owner approved, see In progress»: both pointed at the stale section; now «see Done» / «see Wave C» with v0.3.0.
- Wave C «Log archive and per-job stats» had no status line; built in v0.3.0 (PR #19, `internal/jobs/archive.go`, CLI `archive`, `stats` at `cmd/agent-jobs/main.go:166, 170`).
- Wave D «API-credit fallback» said «Built switched off», but the code has only a `billing` column that is always `subscription` (`internal/store/store.go:167, 516`, `internal/jobs/manager.go:308`; no other match for `credit` or `billing` in `internal/`). Now «To be built switched off … Status: not built». Heartbeat, «Acceptance from a clean clone» and the code-graph index got explicit «not built» / «not started» status lines.
- PLAN.ru.md only: the Wave D section was still titled «Отложено» («each waits for a separate owner decision»), which DECISIONS.md 2026-10-17 «Full mandate» superseded and PLAN.md no longer says; the «Acceptance from a clean clone» item was missing; the «Autopilot items 23-27» line was duplicated. Aligned with PLAN.md.

No claim under «Done» was found that the code does not do. Every «built, unreleased» path named in the waves exists on main (`internal/jobs/{cron,schedule,checks,intentions,notifyowner,codex,ownerwaits,redact,briefsize,skills,skillrating}.go`, `internal/httpapi`, `internal/mcp/tools_skills.go`).
