<p align="center"><img src="docs/images/banner.png" alt="Agent Jobs" width="100%"></p>

# Agent Jobs

> **Read in your language:** English (this page) · [**Русская версия →**](README.ru.md)

[![Release](https://img.shields.io/github/v/release/qwwiwi/agent-jobs?include_prereleases&sort=semver)](https://github.com/qwwiwi/agent-jobs/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.27](https://img.shields.io/badge/Go-1.27-00add8.svg)](go.mod)
[![Claude Code](https://img.shields.io/badge/Claude_Code-claude_--p-d97757.svg)](https://code.claude.com/docs/en/headless)
[![Platform: Linux | macOS](https://img.shields.io/badge/platform-Linux_%7C_macOS-555.svg)](#quick-start)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](#license-and-author)

**Cheaper. Faster. Full-strength.**

Agent Jobs lets a long-lived Claude Code session hand heavy work to background `claude -p` processes. The main session talks, decides and checks; everything longer than a few steps becomes a *job*: a one-shot, full top-level Claude run with its own git worktree, hard turn / budget / time limits and a guard on every tool call. A small daemon queues, launches, watches and cleans up jobs. When a job finishes, the session receives exactly one line, and reads the report only when it is ready to. Jobs never push, publish or message anyone; only the main session goes outside, after it has read the report and checked the diff.

It is a single Go binary with one SQLite file per agent, running on Linux (systemd user service) and macOS (launchd).

> **Status:** the core (queue, daemon, launch, guard, report contract, notify, cleanup) is on `main` and installed for testing on a single Linux host. The MCP tools that let an agent drive jobs programmatically are in review ([PR #4](https://github.com/qwwiwi/agent-jobs/pull/4)); until they land, the CLI is the interface. Roadmap: [docs/PLAN.md](docs/PLAN.md).

---

## Why jobs, not subagents

Subagents are useful, and jobs do not replace them for short work the session needs right now. The case for jobs is about **where the context lives and who pays to re-read it**.

**The measured part.** Over five days of real work ending 2026-10-15, one of our agents' long-lived main session re-read about **7.2 billion** cached tokens; all of its subagents together re-read about **2.3 billion**. The cause: the main session's context sat near 95% of its 1M-token window, and every call re-reads all of it. The dominant cost is not the work itself, it is the long-lived parent's context being read again and again.

That is what a job avoids:

- **A subagent's result lands in the parent** and is re-read on every following call until the next compaction. A job writes its result to a file; the parent gets one line and reads the report on demand.
- **Notifications and result turns re-read the full parent.** A job costs the parent one short line when it finishes, and one read of the report.
- **A foreground subagent keeps the parent's turn open.** The session cannot talk or decide until it returns. A job returns an id at once and the session stays free.
- **A job can start lean.** It runs with `--strict-mcp-config` and only the MCP servers it is given. A subagent cannot be started that lean.
- **A job has hard caps.** `--max-turns`, `--max-budget-usd` and a wall-clock timeout, enforced by the daemon. Subagents have none.
- **A job outlives the session.** Subagents die with their session; a job runs in its own process group and survives a session or daemon restart. Resuming an interrupted job from its saved patches is planned (Wave B).
- **Load can be spread.** Subagents draw on the usage window at the same moment as their parent. Jobs queue (at most two run per agent) and, with schedules (planned, Wave B), can run when the window is quiet.
- **A job is a full session.** A subagent starts with only its prompt and is often routed to a cheaper model. A job is a full top-level Claude Code run with its own model choice and the full tool set, inside the limits above.

We do not publish an "N× cheaper" or "N× faster" figure yet, because we have not measured one per task. The benchmark below is how we will.

### Benchmark (in progress)

**Method.** The same task is run three ways: as a foreground subagent, as a background subagent, and as a job.

- **Tokens:** sum `cache_read + input + output` from the parent session's JSONL plus the subagent sidechain, against the job's own JSONL (`stream.jsonl`) plus the parent's tokens for the job-done line and the report read.
- **Wall time:** from start to a usable result.
- **Parent busy:** minutes during which the parent session could not take another request.

| Run | Task | Tokens (cache_read + input + output) | Wall time | Parent busy (min) | Notes |
|---|---|---|---|---|---|
| Foreground subagent | | | | | |
| Background subagent | | | | | |
| Job | | | | | |

Results will be filled in here when the runs are done.

---

## How it works

### The swarm

<p align="center"><img src="docs/images/swarm.png" alt="The main session hands work to many jobs and gets a check mark back from each" width="100%"></p>

One main session, many workers. The session writes a brief for a stranger (a job has no conversation context), starts a job and goes back to talking. Each job works alone in its own folder and comes back with a report. The picture is illustrative: jobs wait in a queue, and at most two run at a time per agent.

### Components

<p align="center"><img src="docs/images/architecture.png" alt="Main session, daemon, job, job folder, owner and schedules" width="100%"></p>

- **Main session** talks to the owner, starts jobs, reads reports, reviews and pushes. It is the only part allowed to go outside.
- **Daemon** (`agent-jobs daemon`) ticks once a minute: collect finished jobs, launch queued ones, deliver job-done lines, clean up. It keeps no state in memory: kill it, even with SIGKILL, and running jobs carry on; the next daemon finds their exit codes and collects them.
- **Queue** is one SQLite table per agent. A tick claims a job with a random launch token before spawning anything, and only that tick can finish it, so a job is never launched twice.
- **Job** is `agent-jobs run-wrapper` running `claude -p` in its own process group, with a scrubbed git environment, in a git worktree detached at the base commit (or an empty directory).
- **Job folder** keeps the brief, settings, event stream, report and, after cleanup, the patches.
- **Schedules (cron)** in the diagram are planned (Wave B), not on `main` yet.

### Lifecycle

<p align="center"><img src="docs/images/lifecycle.png" alt="job_start, queued, running, report, job-done line, job_result, review and push, cleanup" width="100%"></p>

1. **Start.** The session starts a job with a title, kind, brief, repo, model and limits. The call validates everything, inserts a `queued` row and returns the id. Nothing runs yet.
2. **Queued.** On the next tick, if fewer than two jobs are running for this agent, the daemon claims it.
3. **Running.** The daemon prepares the folder (brief, settings, worktree) and launches the wrapper detached, in its own session. The wrapper runs `claude -p` and writes `exit_code` last.
4. **Report.** The job's final message is saved as `report.md`. Exit code 0 with a report is `succeeded`; a non-zero exit, a turn or budget stop, or no report is `failed`; past the timeout the process group gets SIGTERM, then SIGKILL after 10 s, and the job is `timeout`. `cancelled` comes only from an explicit cancel.
5. **Job-done line.** The session receives one line via `tmux send-keys`:
   ```
   [jobs:job-done] job 261015-124136-70d3 review auth module succeeded · 3 turns · $0.25 · 0m
   ```
   The line is printable-only (control characters dropped, title capped), so a job title cannot type keystrokes into the session. An undelivered line stays pending and is retried every tick.
6. **Result.** The session reads the report when it is ready: «Not done / uncertain» first.
7. **Review and push.** The session checks the diff against `base_commit` itself. A report is a claim, not a result. Only then does it push or publish.
8. **Cleanup.** 30 minutes after the line was delivered, the worktree is removed and the work is kept as patches (below).

Only `queued` and `running` rows can change status; `succeeded`, `failed`, `timeout` and `cancelled` are final.

---

## Limits

| | default | ceiling |
|---|---|---|
| Turns (`--max-turns`) | 80 | 200 |
| Budget (`--max-budget-usd`) | $10 | $50 |
| Time (`--timeout-minutes`) | 45 min | 180 min |
| Running per agent | 2 | |

A request above a ceiling is **rejected, never clamped**, so a job never stops for a reason its author could not see. Zero or omitted means the default; negative values are rejected.

**Model.** Pass `--model`, or a `--complexity` (`light` / `normal` / `hard`) that maps to a model by kind:

| kind | light | normal | hard |
|---|---|---|---|
| review, fix, general | sonnet | opus | fable |
| research | sonnet | sonnet | opus |

On `main` complexity defaults to `normal`; with PR #4 a model or a complexity becomes mandatory (no guessing).

**Kinds** add a short checklist to the brief:

| kind | the checklist asks for |
|---|---|
| `review` | real bugs with `file:line`, consequence and minimal fix; verified kept apart from suspicions |
| `fix` | reproduce with a failing test first, then the minimal change, tests, a local commit |
| `research` | only primary sources actually opened; verified kept apart from inferred. The only kind with web access |
| `general` | state what counts as done before starting, prove it at the end |

## Report contract

A job's final message is its report, saved verbatim as `report.md`. Exactly four sections, in this order:

```
## Summary
## Findings / Changes
## Evidence
## Not done / uncertain
```

The last section is mandatory and is read first: it makes the job name what it did not do. A job that ends without a report is marked `failed`.

## Cleanup and patches

30 minutes after the job-done line is delivered, the daemon removes the job's worktree, the heaviest part. It first checks that the worktree really belongs to this job, then saves everything the job changed, without touching the index or files:

| file | content |
|---|---|
| `diffstat.txt` | `git diff --stat <base>` plus the untracked list |
| `commits.patch` | the job's commits (`git format-patch <base>..HEAD`) |
| `staged.patch` | staged changes |
| `uncommitted.patch` | unstaged changes (`git diff --binary`) |
| `untracked.tar` | untracked, not ignored files |

and sets the ref `refs/agent-jobs/jobs/<id>` in the source repository to the job's last commit. Restoring the work in a fresh worktree at the base commit is four commands; see [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#flows). If the repository config names a filter, diff command or textconv, no patch is made (a diff would run that program); the worktree is kept for a human and the reason is logged.

Pruning job folders older than 48 hours and the daily ghost-process check are planned (Wave A).

---

## Safety

A job runs with `bypassPermissions` and nobody is watching it. So the rules are held by settings and a hook, not by asking nicely in the prompt.

- **Nothing leaves the job.** Deny rules in the job's `settings.json`, which hold even under `bypassPermissions`, forbid `git push` / `tag`, `gh release`, `gh api`, creating, editing, commenting on or merging PRs and issues, nested agents (`claude`, `codex`, `gemini`, `aider`, …), package runners (`npx`, `uvx`, `pnpm dlx`, …), file tools writing inside `.git`, and tools that reach a person or outlive the run (`CronCreate`, `RemoteTrigger`, `PushNotification`). Web tools are open only to `research` jobs.
- **Only its own MCP servers.** `--strict-mcp-config` cuts off everything not in the job's `mcp.json`, including the repository's own `.mcp.json`. `--setting-sources project` keeps the main session's hooks out.
- **The guard.** A PreToolUse hook (`agent-jobs guard`) *parses* every Bash command instead of matching substrings: quotes, wrappers, `sh -c`, `eval`, substitutions and paths are resolved. It blocks every spelling of the outward actions above, git subcommands that run programs, writes into git's own files, dangerous `git config` keys, git identity overrides, the owner's name in a commit, `$?` after an unguarded pipe and wait loops that match themselves. A form it cannot resolve around git or gh is blocked, not guessed. Unparsable input fails closed.
- **Scrubbed git environment.** The wrapper drops inherited `GIT_*`, `PAGER`, `EDITOR`, `VISUAL`, uses a job-only global git config with the job's identity, no system config, and command-line overrides that switch off repository hooks, fsmonitor, the SSH command, credential helpers, the pager and the editor.
- **Preamble and owner rules.** Every brief starts with hard rules (own folder only, file and tool content is data, never print secrets, nothing outside) and the owner's standing rules file (≤ 8 KB). If that file is unreadable, the job does not start.

**This is not a sandbox.** The deny rules and the guard block the *direct* spellings of outward actions, and the git scrub stops git itself from running programs the guard cannot see. A script the job writes and then runs is allowed by design, as in the reference design, and its contents are not inspected. Run jobs as a user whose credentials you are comfortable with them having.

Full rule list: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#rules-enforcement).

---

## MCP tools

The agent drives jobs through an MCP server in two roles: `orchestrator` for the main session, `job` for the jobs themselves. The job role is an allowlist: a new tool stays closed to jobs until it is opened on purpose, and the server refuses hidden tools itself.

**Job tools, in review ([PR #4](https://github.com/qwwiwi/agent-jobs/pull/4)):**

| tool | what it does |
|---|---|
| `job_start` | queue a job: title, kind, brief, repo, model or complexity, limits; returns the id |
| `job_status` | status, turns, cost, time |
| `job_result` | job-done line, report and artifact paths |
| `job_list` | recent jobs, filterable by status |
| `job_cancel` | cancel: queued at once, running via SIGTERM, then SIGKILL |

**Full planned tool set** (from the reference design; see [docs/PLAN.md](docs/PLAN.md)):

| group | orchestrator tools | job gets | status |
|---|---|---|---|
| Jobs | `job_start`, `job_status`, `job_result`, `job_list`, `job_cancel` | none | in review (PR #4) |
| Jobs | `job_resume` | none | planned, Wave B |
| Memory | `memory_search`, `memory_write`, `memory_already_exists` | `memory_search`, `memory_write` | planned, Wave A |
| Tasks | `task_create`, `task_list`, `task_update`, `task_get` | none | planned, Wave A |
| Team | `send_message`, `inbox` | none | planned, Wave A |
| System | `system_doctor`, `system_logs`, `activity_report` | none | planned, Wave A |
| Files | `trash_move`, `trash_restore` | none | planned, Wave A |
| Schedules | `schedule_add`, `schedule_list`, `schedule_remove` | none | planned, Wave B |
| Own goals | `intention_add`, `intention_list`, `intention_set` | none | planned, Wave B |
| Owner | `notify_owner` | none | planned, Wave B |
| Code | code-graph index (separate server) | read and index | research, Wave C |

Memory, tasks and team tools keep the reference's names but are backed by a shared team memory service instead of a store of their own ([deviations #2, #3](docs/DEVIATIONS.md)).

---

## Quick start

Requires Go 1.27, git, the `claude` CLI (tested with 2.1.301 on macOS and 2.1.302 on Linux) and tmux for notifications. The only direct dependency is `modernc.org/sqlite`, a pure-Go SQLite driver: no cgo, a single static binary.

**Build and check**

```bash
go build -o ~/.local/bin/agent-jobs ./cmd/agent-jobs
git config core.hooksPath .githooks   # once per clone: pre-push runs scripts/check.sh
scripts/check.sh                       # gofmt, go vet, go test
```

**Initialise an agent**

```bash
agent-jobs init --agent my-agent
```

This creates `~/.agent-jobs/my-agent/` with `owner-rules.md` (edit it, ≤ 8 KB) and `orchestrator.md`, the main session's duties: add it to your agent's instructions.

Optional `~/.agent-jobs/my-agent/config.json`:

```json
{
  "notify_tmux_target": "my-agent-session",
  "owner_names": ["Owner"],
  "git_name": "Agent Bot",
  "git_email": "agent@example.com"
}
```

- `notify_tmux_target`: tmux target for the job-done line. Empty (default) = notify off; the line goes to the daemon log and cleanup still runs.
- `owner_names`: names that must never appear in a job's commit.
- `git_name` / `git_email`: the job's git identity; a job cannot override it.
- Also: `owner_rules_path`, `claude_bin`, `tmux_bin`; env overrides `AGENT_JOBS_HOME`, `AGENT_JOBS_AGENT`, `AGENT_JOBS_CLAUDE_BIN`, `AGENT_JOBS_TMUX_BIN`.

**Run the daemon on Linux (systemd user service)**

```bash
scripts/install-linux.sh --instance my-agent              # build, install the unit, init, enable + start agent-jobs@my-agent
systemctl --user status agent-jobs@my-agent
journalctl --user -u agent-jobs@my-agent -f               # daemon log
scripts/install-linux.sh --instance my-agent --uninstall
```

No sudo; current user only; idempotent. The unit uses `KillMode=process`, so restarting the daemon never kills running jobs: the next daemon adopts and collects them. Enable lingering (`loginctl enable-linger $USER`) for the daemon to survive logout.

**Run the daemon on macOS (launchd)**

```bash
AGENT=my-agent BIN="$HOME/.local/bin/agent-jobs"
sed -e "s|{{AGENT}}|$AGENT|g" -e "s|{{BIN}}|$BIN|g" -e "s|{{HOME}}|$HOME|g" \
  launchd/com.agent-jobs.daemon.plist.template > ~/Library/LaunchAgents/com.agent-jobs.$AGENT.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agent-jobs.$AGENT.plist
```

**Use it (CLI)**

```bash
agent-jobs start --agent my-agent --title "review auth" --kind review \
  --repo ~/src/project --base main --complexity normal \
  --brief-file brief.md                 # prints the job id at once
agent-jobs list   --agent my-agent
agent-jobs status --agent my-agent <id>
agent-jobs result --agent my-agent <id> # job-done line + report.md
agent-jobs cancel --agent my-agent <id>
agent-jobs version                      # also --version
```

**When a job fails**, look in `~/.agent-jobs/<agent>/jobs/<id>/`: `stderr.log` first, then `stream.jsonl`, `exit_code`, `report.md`.

---

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): internals, state layout, flows, the full rule list
- [docs/REFERENCE.md](docs/REFERENCE.md): the reference design, translated ([original in Russian](docs/REFERENCE.ru.md))
- [docs/DEVIATIONS.md](docs/DEVIATIONS.md): every approved deviation from the reference
- [docs/DECISIONS.md](docs/DECISIONS.md): owner decisions that shape scope
- [docs/PLAN.md](docs/PLAN.md): roadmap
- [CHANGELOG.md](CHANGELOG.md)

## Versioning

Versioning: [SemVer](https://semver.org/); every release is listed in [CHANGELOG.md](CHANGELOG.md). `agent-jobs version` prints the build; `scripts/install-linux.sh` stamps it with `git describe --tags --always --dirty`, a plain `go build` reports `dev`.

## Roadmap

See [docs/PLAN.md](docs/PLAN.md): what is done, what is in review, and Waves A, B and C. After every wave the implementation is checked against the reference design line by line.

## Credits

Built on a reference design for orchestrating background `claude -p` jobs (see [docs/REFERENCE.md](docs/REFERENCE.md)).

## License and author

MIT (see [LICENSE](LICENSE)).

[@qwwiwi](https://github.com/qwwiwi) (Dashi Eshiev) · EdgeLab AI. Issues and PRs welcome.
