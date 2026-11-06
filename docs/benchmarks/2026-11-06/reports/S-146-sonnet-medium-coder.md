[jobs:job-done] job 261106-131245-b0f4 BENCH S #146 sonnet-medium succeeded · 20 turns · $0.32 · 14m · model sonnet · effort medium · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- The full `go test ./internal/jobs` did not finish: it timed out at 600s with a goroutine dump. I did not find which test hung, so I cannot say whether my change causes it. I did not run the unmodified base to compare.
- The check covers file sources in a denied repo and `job:` sources of a job on a denied repo. It does not follow a report's own provenance: a `job:` report from a job with no repo but built from sources of a denied repo is still allowed. A full fix would need provenance tracking, and that is the owner's call.
- A file source is matched through the git toplevel only. A plain directory named in an absolute `deny_repos` glob, if it is not a git repository, is not matched.

## Progress
- --:-- Read `deny_repos` handling and the sources code in `openrouter.go`, `manager.go` and `source_files.go`
- --:-- Added `checkOpenRouterSources` and `sourceFileRepos`, and wired the call into Enqueue
- --:-- Wrote `TestOpenRouterSourcesRespectDenyRepos`; it failed without the manager.go call and passed with it
- --:-- Ran targeted tests (ok) and `go vet` (exit 0)
- --:-- Committed `f6eefa6` on `bench/146-sonnet-medium`
- --:-- Full package run timed out at 600s; reran the related test groups, which passed

## Summary
Issue #146: `providers.openrouter.deny_repos` only checked the job's own repository, so `sources` could carry denied-repo files or reports to OpenRouter. I added a check at enqueue for OpenRouter jobs. It refuses a file source inside a denied repository and a `job:` source whose job ran on a denied repository. The new test fails before the fix and passes after. The full `internal/jobs` package run did not finish (see Not done / uncertain).

## Findings / Changes
Commit `f6eefa6` on branch `bench/146-sonnet-medium`.
- `internal/jobs/openrouter.go`: new `Manager.checkOpenRouterSources`. For each source it takes the repository roots to test:
  - A `job:<id>` source uses that job's `RepoPath`.
  - A file source uses `sourceFileRepos`: the git toplevel and main worktree of the file's directory, with symlinks resolved. A file outside any git repository gives no roots, so it is allowed.
  - Each root goes through the existing `deniedRepo` matcher, so name and absolute-glob entries work as they do for the job's own repo.
- `internal/jobs/manager.go`: Enqueue calls the check for OpenRouter jobs with sources, right after the existing repo deny check.
- `internal/jobs/openrouter_sources_test.go`: new `TestOpenRouterSourcesRespectDenyRepos`.

## Evidence
- Before the fix (manager.go call stashed): `TestOpenRouterSourcesRespectDenyRepos` fails with `file in a denied repo: <nil>`.
- After the fix: the test passes. It covers four cases: a file in a denied repo is refused; the same file on a claude job is accepted; a file outside the repo is accepted; `job:priv` (a job on the denied repo) is refused.
- `go test ./internal/jobs -run 'OpenRouter|Source|Enqueue|Milestone'`: ok, exit 0, including the existing `TestOpenRouterJobWithSourceFiles`.
- `go vet ./...`: exit 0.
- Full `go test ./internal/jobs`: exit 1. The run hit the 600s go test timeout and panicked.
