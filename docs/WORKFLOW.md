# Working as an orchestrator

> Русская версия: [WORKFLOW.ru.md](WORKFLOW.ru.md)

This page is about how an agent works with agent-jobs day to day. The [README](../README.md) explains what jobs are and how they run; [`prompts/orchestrator.md`](../prompts/orchestrator.md) lists the main session's duties, quoted from the [reference design](REFERENCE.md). Below, «shipped» means on `main` now; anything marked **planned** names its item in [PLAN.md](PLAN.md).

## The rule

The main session is the orchestrator. It plans, writes briefs, reads results, checks diffs, decides, merges, deploys and reports to the owner. It does not write code or reviews itself: those go to jobs.

The point is where the context lives. The main session is long-lived and its context is large, so every extra step it takes is paid for again on every following call. A job does the work in its own process with its own context, and the session pays for one job-done line and one read of the report.

## Routing: yourself, a subagent or a job

| Work | Where it goes |
|---|---|
| Takes minutes, or you already have the answer | Do it yourself |
| Takes minutes and the result is needed inside this turn | A subagent |
| Everything long: coding, fix loops, reviews, research, overnight work | A job |
| Not sure | A job |

A job is started with `job_start` (MCP) or `agent-jobs start` (CLI). Give it:

- `title`: short, shown in the job-done line;
- `kind`: `fix`, `review`, `research` or `general`. The kind adds its checklist to the brief, and only `research` gets web tools;
- `model` **or** `complexity` (`light` / `normal` / `hard`). One of them is required; with neither the job is rejected. There is no default and no classifier: you decide;
- `repo` (absolute path) and `base` (the ref to start from, default `HEAD`). The job works in its own worktree detached at that commit. Omit `repo` for an empty work dir;
- `brief`: written for a stranger. A job has none of your conversation; state the goal, the boundaries, what counts as done and what not to touch;
- limits only when the defaults (80 turns / $10 / 45 min) do not fit; above the ceilings (200 / $50 / 180 min) the call is rejected, not clamped.

**Cost note.** Even an empty subagent loads a full context: we measured about 100K tokens for a subagent that only answered «ok». Its result then lands in the parent and is re-read on every following call. A job runs outside the session; the parent gets one line.

## Concurrency

At most two jobs run per agent at a time; the rest wait in the queue and start on a later daemon tick (once a minute). Several coders can work on the same repository at once: every job gets its own git worktree, so they do not see each other's changes. Give parallel jobs work that will merge cleanly, or run them one after another.

## The pipeline

A typical change goes through these steps. Each step says who does it.

| # | Step | Who | Notes |
|---|---|---|---|
| 1 | Research | `research` job | Primary sources only; verified kept apart from inferred |
| 2 | Plan and plan review | Orchestrator writes the plan; a `review` job reviews it | For non-trivial work |
| 3 | Owner approval | Owner | The plan, not every step of it |
| 4 | Coding | `fix` or `general` job | Commits locally in its worktree |
| 5 | Deterministic checks and a short audit | Orchestrator | `git diff --stat <base_commit>` (after cleanup: `diffstat.txt`), the repository's own checks (here `scripts/check.sh`), `doctor procs` (**planned**, PLAN Wave A); then a short read of the diff |
| 6 | Review | One or two `review` jobs | On a different model than the coder |
| 7 | Fix loops | `fix` jobs, at most two | Each fed with the review findings; after two, stop and rethink |
| 8 | Push, PR, merge | Orchestrator | Bound to the reviewed head SHA: what was reviewed is what ships |
| 9 | Deploy and live check | Orchestrator | Through the project's own deploy script, then check the live system |
| 10 | Report | Orchestrator | To the owner: result, evidence, what is left |

A job commits locally and never pushes, by design: the deny rules and the guard block `git push`, tags, releases and PR or issue writes in a job. Step 8 is always the orchestrator's. To take a job's work, use its worktree while it exists, or after cleanup the ref `refs/agent-jobs/jobs/<id>` and the saved patches (see the README, «Cleanup and patches»).

## Review policy

- Review is never skipped.
- One reviewer for ordinary changes. Two reviewers, on different models, for money, auth, production databases and infrastructure.
- The reviewer runs on a different model than the coder.
- If a reviewer is out of quota, switch to another model and say so in the report.
- A review that did not run is never reported as passed.

## Reading a result

When a job finishes, the session gets one line:

```
[jobs:job-done] job 261015-124136-70d3 review auth module succeeded · 3 turns · $0.25 · 0m
```

Read the report with `job_result` (or `agent-jobs result --agent <agent> <id>`):

1. Read «Not done / uncertain» first. It is where the job names what it skipped or could not verify.
2. Then read the diff against `base_commit`, not just the report. The report is a claim, not a result.
3. Only then merge, deploy or report onward.

`succeeded` means the job ended with exit code 0 and a report. It does not mean the work is correct.

The worktree is removed 30 minutes after the job-done line. Reading later is fine: the work is kept as patches and a ref.

## Known limits

- **The guard is a hook, not a sandbox.** It blocks the direct spellings of outward actions. A script a job writes and then runs is not inspected.
- **Jobs run as the same unix user** as the orchestrator, with that user's credentials. Run agents as a user whose credentials you are comfortable a job having.
- **Testing agent-jobs inside a job.** Tests of agent-jobs itself that spawn its CLI refuse to run inside a job (`refused: running under the job wrapper`), because a job never starts or manages jobs. Run this repository's full `scripts/check.sh` outside a job, from the orchestrator or by hand.
- **Planned, not shipped yet** (PLAN Wave A): the `agent-jobs` skill that teaches these tools and the routing rule per agent; the documented cap of three subagents per job; `prune` and `doctor procs`; cgroup containment of jobs.

## Optional hook: a reminder before a subagent

To nudge the main session towards jobs, add a PreToolUse hook on the `Agent` and `Task` tools. It never blocks; it only adds a reminder to the model's context. The script ships as [`examples/hooks/prefer-jobs-reminder.sh`](../examples/hooks/prefer-jobs-reminder.sh): it reads and discards the hook input, prints

```json
{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"Reminder (agent-jobs): ..."}}
```

and exits 0.

Add it to the main session's user settings (`~/.claude/settings.json`), with the path to your clone:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Agent|Task",
        "hooks": [
          {
            "type": "command",
            "command": "bash /path/to/agent-jobs/examples/hooks/prefer-jobs-reminder.sh"
          }
        ]
      }
    ]
  }
}
```

Jobs do not load user settings (they run with `--setting-sources project` and their own `settings.json`), so the reminder stays in the main session.
