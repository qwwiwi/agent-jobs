[jobs:job-done] job 261106-131245-8bfe BENCH S #146 opus-high succeeded · 68 turns · $1.83 · 40m · model opus · effort high · job dir 123.6 MB · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- **Owner decision needed:** the finer policy (allow sources only when every file is outside the denied repos and every `job:` report comes from a job on an allowed repo) is not built. `deny_repos` is a list of what to block, so even that policy would let through files or reports that sit outside any repository. The full refusal is the safe default until a policy is chosen.
- I did not test the milestone wording for planned sources: milestones currently reject planned sources outright (G02 disabled), so that path can't be reached yet.
- Commit message is in English, as this task asked; the general owner rule says Russian.
- The cause of the outside SIGTERM in the first full run is unknown; the package passed when run on its own.
- No subagents used. Two guard refusals (an inline python edit and a heredoc piped into `git commit`); I redid both with the Edit tool and a message file.

## Progress
- 15:51 UTC read the deny_repos check, `newJob` and the sources code
- --:-- wrote `TestOpenRouterRefusesSources`; it failed (3 jobs accepted, $3 reserved)
- --:-- added the refusal in `newJob`; updated the two existing tests that assumed sources were allowed
- 16:05 `go vet` passed; first jobs run hit go's default 10-minute timeout
- --:-- updated the MCP field description, GUIDE and CHANGELOG
- --:-- wrote the failing wrapper test; added the shared error and the launch-time check
- 16:07 focused tests passed; stopped the run that had compiled old code
- 16:08 committed `6f2413a` on `bench/146-opus-high`
- --:-- full `./...` run: 13 packages ok, jobs binary killed by outside SIGTERM; reran jobs on its own
- 16:30 `internal/jobs` ok (1147s); PROGRESS.md finalized

## Summary
Issue #146: `providers.openrouter.deny_repos` only checks the job's own repository. A file from a denied repository, or another job's report, could still be attached through `job_start sources` and sent to OpenRouter. I took the simpler option the issue offers: OpenRouter jobs now refuse sources until a real policy exists. The check runs in two places: when the job is created (`newJob`) and again when the wrapper launches it, which catches rows queued with sources before this change. The fix is committed locally as `6f2413a` on branch `bench/146-opus-high`. `go vet` and all tests pass, and nothing was pushed.

## Findings / Changes
- `internal/jobs/openrouter.go:395`: a new shared error `errOpenRouterSources` that explains why sources are refused.
- `internal/jobs/manager.go:508`: in `newJob`, right after the OpenRouter config check, a job with any `r.Sources` is refused. This happens before files are copied, before budget is reserved and before a row is written. If the sources come from a milestone task's plan, the message names the task and says how to drop them with `sources: []`.
- `internal/jobs/wrapper.go:873`: `wrapperOpenRouter` refuses to start a job whose row has `SourceFiles` or old-style `Sources` set.
- `internal/mcp/tools_jobs.go:222`: the description of the `job_start` `sources` field now says sources are refused for `openrouter:<slug>` models, so calling agents know before they try.
- `docs/GUIDE.md:160` and `CHANGELOG.md` (Unreleased / Fixed): describe the restriction.
- Tests:
  - New `TestOpenRouterRefusesSources` (`openrouter_test.go`): three cases are refused — a file inside a denied repo, a file outside any repo, and `job:<id>` pointing at another job's report. It also checks that nothing is reserved against the budget, that an OpenRouter job without sources still starts, and that a normal subscription job still gets its sources.
  - New `TestWrapperOpenRouterRefusesQueuedSources` (`openrouter_sources_test.go`): the launch-time refusal, for both `SourceFiles` and old-style `Sources`.
  - Rewrote `TestOpenRouterJobWithSourceFiles`: it used to assert that OpenRouter jobs keep their sources, which is the leak itself. It now asserts they are refused, with no job folder left and nothing reserved, both within the daily cap and past it.
  - `TestOpenRouterCapCollisionReservesOnce` (`jobid_test.go`): removed the sources from it. It still checks that a job-id collision reserves the budget only once; the claude and codex versions of the test still cover sources landing under the final id.

## Evidence
- Before the fix, `go test ./internal/jobs/ -run TestOpenRouterRefusesSources` exited 1: all three cases were accepted (`<nil>, want the sources refusal`) and `a refused job reserved $3.00`.
- Before the fix, `TestWrapperOpenRouterRefusesQueuedSources` exited 1: the job was refused only because the config was missing, not because of its sources.
- After the fix:
  - `go vet ./...` → exit 0 (run twice, the second time on the final code); `gofmt -l internal/` → no output.
  - Focused run `go test ./internal/jobs/ -run 'TestWrapperOpenRouter|TestOpenRouter|TestEnqueue|TestCodexCap' -count=1` → `ok … 33.187s`, exit 0.
  - Full `go test -count=1 -timeout 30m ./...`: 13 packages `ok`, including `internal/mcp`. The `internal/jobs` test binary in that run was stopped by an outside SIGTERM at 176s (`signal: terminated`), not by a failing test.
  - `go test -count=1 -timeout 30m ./internal/jobs/` run on its own → `ok … 1147.531s`, `jobs_exit=0`. Log: `<job dir>/jobs-test2.log`.
- An earlier jobs run with go's default 10-minute timeout timed out at 601s. That is known: `scripts/check.sh` says this package takes about 14 minutes, which is why I used `-timeout 30m`.
