# Issue #135: ui-check: orchestrator may run job-controlled code (job-dir copy + Playwright from cwd node_modules)

Found by an automated security review of the push of fix/job-finish-no-lost-work; the code came in with #130.

1. HIGH – prompts/orchestrator.md tells the orchestrator it may run `<job dir>/tools/ui-check.mjs`. That file sits in a directory the job can write, so a job (or content it read) can replace it and get code executed with the orchestrator's privileges.
2. MEDIUM – scripts/ui-check.mjs resolves Playwright via `createRequire(path.join(cwd, ...))` first, i.e. from node_modules under the job's worktree. Run by the orchestrator in a job worktree, a planted `node_modules/playwright` executes.

Fix:
- the orchestrator runs only the copy embedded in the binary (e.g. `agent-jobs ui-check` writing the embedded script to a private temp dir), never the job-dir copy; drop that sentence from orchestrator.md;
- add `--playwright <path>` / trusted global root resolution for trusted callers, no cwd lookup when run by the orchestrator;
- test: a planted tools/ui-check.mjs and a planted node_modules/playwright in the worktree are not executed by the trusted path.

---
## Task (benchmark run, model tag: opus-high)
Implement the fix for the GitHub issue above in this repository (Go). Work only on what the issue asks.
- Read the code the issue points at before changing anything.
- Make exactly one tool call per step.
- Add or update tests that prove the fix; run `go test` for every package you touched and `go vet ./...`; paste the real exit codes in Evidence.
- Commit on a new branch `bench/135-opus-high` with a clear English commit message. Do not push, do not open a PR, do not message anyone.
- If part of the issue needs an owner decision or is out of reach, do the rest and list it under "Not done / uncertain".
