# Issue #135: ui-check: orchestrator may run job-controlled code (job-dir copy + Playwright from cwd node_modules)

Found by an automated security review of the push of fix/job-finish-no-lost-work; the code came in with #130.

1. HIGH – prompts/orchestrator.md tells the orchestrator it may run `<job dir>/tools/ui-check.mjs`. That file sits in a directory the job can write, so a job (or content it read) can replace it and get code executed with the orchestrator's privileges.
2. MEDIUM – scripts/ui-check.mjs resolves Playwright via `createRequire(path.join(cwd, ...))` first, i.e. from node_modules under the job's worktree. Run by the orchestrator in a job worktree, a planted `node_modules/playwright` executes.

Fix:
- the orchestrator runs only the copy embedded in the binary (e.g. `agent-jobs ui-check` writing the embedded script to a private temp dir), never the job-dir copy; drop that sentence from orchestrator.md;
- add `--playwright <path>` / trusted global root resolution for trusted callers, no cwd lookup when run by the orchestrator;
- test: a planted tools/ui-check.mjs and a planted node_modules/playwright in the worktree are not executed by the trusted path.

---
## Review task (benchmark)
Review the change on branch `bench/135-sonnet-medium` (diff `8f6e8f9e5969b205fa9e400b66d0728ba39602cd..HEAD` in this worktree) as the fix for the issue above.
Judge correctness, whether the issue is fully fixed, regressions, test quality. Do not modify files.
End the report with exactly one line in this form, counts of findings by severity:
BENCH-VERDICT: critical=<n> high=<n> medium=<n> low=<n> issue_fixed=<yes|partly|no>
