[jobs:job-done] job 261106-131245-54d9 BENCH S #146 haiku-high succeeded · 52 turns · $0.07 · 34m · model haiku · effort high · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- Policy-based option not built. The issue's first option (per-source checks against `deny_repos`, and `job:` sources only from allowed repos) needs an owner decision. Refusing all sources is the simpler option the issue allows.
- Jobs already queued before this fix that carry `SourceFiles` on an OpenRouter provider are not re-checked at launch. `setupOpenRouter` checks only the model and repo. I did not inspect the live job DB, so I cannot say whether any exist.
- Milestone tasks with planned sources on an OpenRouter model: the error says "start the job without sources", but the milestone path may need `milestone_set sources: []` instead. Not verified.
- Commit message language: the brief asked for English, but the owner rules say commit messages are in Russian. I followed the owner rules; the repo's recent commits are also in Russian.
- Full `go test ./...` was not run again in one pass after the GUIDE.md edit. The `internal/jobs` run and the `cmd/agent-jobs` and `internal/mcp` reruns were, but the other packages were only run before that edit (they touch no code that changed since).
- No subagents were used.

## Progress
- 16:10 UTC checked worktree, created branch bench/146-haiku-high, read deny_repos and sources code
- --:-- wrote failing tests in openrouter_sources_test.go; confirmed they fail before the fix
- --:-- added checkOpenRouterSources and its call in newJob; updated jobid_test.go collision test
- 16:13 wrote PROGRESS.md
- 16:15 full go test ./... (internal/jobs timed out under load); go vet ./... exit 0
- --:-- isolated TestWrapperSweepsSessionBeforeExit: pass
- --:-- added GUIDE.md refusal line
- --:-- committed 55663de (message via file)
- 16:41 internal/jobs full run with 45m timeout: exit 0
- --:-- cmd/agent-jobs and internal/mcp rerun: exit 0

## Summary
Issue #146: `job_start` with `sources` on an OpenRouter job could attach a file from a `deny_repos` repository, or another job's report, and send it to a third-party provider. I took the simpler option from the issue: OpenRouter jobs now refuse any `sources` before anything is copied. Other jobs are unchanged. Commit `55663de` is on local branch `bench/146-haiku-high`, not pushed. The new tests failed before the change and pass after it.

## Findings / Changes
- `internal/jobs/openrouter.go`: new `checkOpenRouterSources(sources)`. It returns an error when `len(sources) > 0`.
- `internal/jobs/manager.go:508`: `newJob` calls it inside the `if openRouter {` block, after `checkOpenRouterJob`. That covers `job_start`, milestone tasks and the file-source copy path, since all of them pass through `newJob`.
- `internal/jobs/openrouter_sources_test.go`:
  - `TestOpenRouterJobWithSourceFiles` is replaced by `TestOpenRouterCappedJobLeavesNoFolder`. It has no sources and checks the daily cap.
  - New `TestOpenRouterJobRefusesSourceFiles`. It covers a file in a denied repo, a file outside it, and a `job:abc123` origin. It also checks that a job without sources is accepted and that a `sonnet` job with a file source is still accepted.
- `internal/jobs/jobid_test.go`: `TestOpenRouterCapCollisionReservesOnce` no longer passes `Sources`. The budget-reservation check it makes is unchanged. The sources-under-final-id check is covered by `TestEnqueueSourcesLandUnderFinalID`, which uses `sonnet`.
- `docs/GUIDE.md:160`: the refusal list now includes `sources`.
- Commit `55663de` (message in Russian, per owner rules, with the Haiku co-author trailer).

## Evidence
Before the fix, `go test ./internal/jobs -run 'TestOpenRouterJobRefusesSourceFiles|TestOpenRouterCappedJobLeavesNoFolder' -count=1` exited 1:
```
--- FAIL: TestOpenRouterJobRefusesSourceFiles
    openrouter_sources_test.go:56: job report: sources "job:abc123": unknown job or unavailable job record: job not found
    openrouter_sources_test.go:56: denied repo file: <nil>
    openrouter_sources_test.go:56: file outside: <nil>
```
So the denied-repo file and the outside file were accepted, and the job report was only refused for not existing. `TestOpenRouterCappedJobLeavesNoFolder` also failed on my first draft, but only because its assertion was wrong (a job without sources makes no folder). After correcting the assertion it passes both before and after the fix. It is a regression guard, not a reproduction.

After the fix:
- `go test ./internal/jobs -run 'TestOpenRouter|TestEnqueue|Collision' -count=1` → `ok`.
- `go vet ./...` → exit 0.
- `go test ./...` (first full run, before the GUIDE.md edit) → every package `ok` except `internal/jobs`, which hit the 10m default timeout in `TestWrapperSweepsSessionBeforeExit` under machine load.
- `TestWrapperSweepsSessionBeforeExit` alone → `PASS` (12.42s), exit 0.
- `go test ./internal/jobs -count=1 -timeout 45m` → `ok ... 1023.978s`, exit 0.
- `go test ./cmd/agent-jobs ./internal/mcp -count=1` (rerun after the GUIDE.md edit, since those tests read the doc) → both `ok`, exit 0.

No UI files changed, so no browser check applies.
