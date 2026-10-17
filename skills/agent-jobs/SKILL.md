---
name: agent-jobs
description: Run long work as background agent-jobs (one-shot `claude -p` jobs with their own worktree, limits and report) through the agent-jobs MCP tools job_start, job_status, job_result, job_list, job_cancel. Use when deciding whether to do a task yourself, hand it to a subagent, or start a job; when starting a job (a model or a complexity is mandatory); when a "[jobs:job-done]" line arrives; and when reading or verifying a job's report.
---

# agent-jobs: background jobs for this agent

A job is a separate one-shot `claude -p` process with its own git worktree, a turn cap, a USD budget, a wall-clock timeout and a fixed report. It runs in the background, survives a restart of this session and of the daemon, and costs this session one line when it ends. A job never writes to a person and never publishes anything: only this session goes outside, after it has read the report and checked the diff.

## Route the work first

| the work | do this |
|---|---|
| a few minutes, a few steps | do it yourself |
| this session needs the result now, before it can answer or decide | a subagent |
| long, background, overnight, or it must survive a restart | a job |

The main session only talks, decides and checks. Anything longer than a few steps goes into a job. When unsure between a subagent and a job: if you would otherwise sit and wait more than a few minutes, it is a job.

## Start a job: `job_start`

| field | rules |
|---|---|
| `title` | short, says what is being done |
| `kind` | `review` (bugs with file:line), `fix` (failing test first, minimal change, local commit), `research` (primary sources only; the only kind with web tools), `general` (state what done means, prove it) |
| `brief` | written for a stranger: the job has no conversation context. Goal, boundaries, what counts as done, what not to do, where to look |
| `model` **or** `complexity` | mandatory, one of them. `complexity` is `light`, `normal` or `hard` and picks the model by kind. With neither, job_start is rejected: there is no default and no classifier |
| `repo`, `base` | optional: the job gets a git worktree of `repo` at `base` (default HEAD). Without a repo it works in an empty directory |
| `max_turns`, `max_budget_usd`, `timeout_minutes` | optional. Defaults 80 / $10 / 45 min; ceilings 200 / $50 / 180. Above a ceiling the call is rejected, never cut down |

job_start returns the id at once; the daemon starts the job within a minute, at most two at a time per agent. Do not poll it in a loop: wait for the job-done line.

What the job gets on its own, so the brief need not repeat it: the hard rules, the owner's rules (the job profile), the project card of the repository if one exists, the kind checklist and the report contract.

Inside a job:

- **Subagents:** at most three per job, inside the job's own limits (budget, timeout, rules, guard), foreground only. The guard refuses a fourth. Plan the brief so that this is enough.
- **No nested jobs:** a job cannot start, cancel or manage jobs, and neither can its subagents. Split work into several jobs from this session instead.
- **Nothing outside:** no push, no tags, no PR or issue writes, no messages. The job commits locally.

## Other tools

| tool | use |
|---|---|
| `job_status` | status, turns, cost, time of one job |
| `job_list` | recent jobs, optionally by status |
| `job_cancel` | cancel a queued job at once, a running one with SIGTERM then SIGKILL |
| `job_result` | the job-done line, the report and the paths of the saved patches |

## When a job is done

You receive one line: `[jobs:job-done] job <id> … succeeded · 39 turns · $1.76 · 28m`. The report is not pushed into the session.

1. Read it with `job_result` when you are ready. Read the section **«Not done / uncertain» first**: it names what the job did not do or could not verify.
2. The report is a claim, not a result. Check the diff yourself against `base_commit`: in the worktree while it exists, afterwards `commits.patch`, `staged.patch`, `uncommitted.patch`, `untracked.tar`, `diffstat.txt` and the ref `refs/agent-jobs/jobs/<id>`. Run the checks yourself.
3. Only then does anything leave the machine: push, PR, deploy, a message to a person. That is this session's job, never the job's.
4. A failed job without a report: look at `stderr.log`, then `stream.jsonl` in the job's directory.

The daemon removes the worktree 30 minutes after the job-done line. Read within that window; later the work is still in the patches and the ref.

## Job context

- **Job profile.** The owner's rules pasted into every brief, at most 8 KB. Generated from the agent's own rules: mark the sections that must hold in every job with `<!-- agent-jobs:profile -->` … `<!-- /agent-jobs:profile -->`, then run `agent-jobs profile --agent <agent> --from <file>...`. A job does not start when the profile is missing, empty or larger than 8 KB.
- **Project cards.** Short notes per repository (stack, check runner, what not to touch, known traps), attached to the brief of every job whose repository matches. Create one with `agent-jobs card new --agent <agent> --repo <path>`, fill in the four sections (at most 4 KB), and see which cards a repository gets with `agent-jobs card list --agent <agent> --repo <path>`.
