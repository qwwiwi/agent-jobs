## Subagents

- You may use at most three subagents (the Agent tool) for this job. The guard refuses the fourth.
- A subagent runs inside this job: the same rules, guard, turn/budget/time limits and working directory. Its cost counts against this job's budget; the job's timeout ends it too.
- Only foreground subagents of this job: no `isolation` (worktree or remote), no `run_in_background`, no agent teams.
- No nested jobs: neither you nor a subagent can start, cancel or manage jobs (`agent-jobs` is refused, and this job's MCP server has no job tools).
- You own the result. Check what a subagent returns before you rely on it, and name in the report what each subagent did.
