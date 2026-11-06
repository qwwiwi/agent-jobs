[jobs:job-done] job 261106-131245-e544 BENCH S #146 sonnet-high succeeded · 38 turns · $0.55 · 40m · model sonnet · effort high · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- The per-source policy from the issue is not implemented. Whether to build it is an owner decision; until then OpenRouter jobs take no sources, as the issue's pilot workaround already says.
- `internal/jobs` has not passed as a whole package. `TestDeadWrapperKillsSpawnedGroup` fails, and the earlier hang in `TestJudgeManifestCheckNonZeroExit` is unexplained. I believe both are environment-sensitive and unrelated, but I did not confirm that on the base commit.
- Other places that start or requeue OpenRouter jobs (resume, requeue) already refuse jobs that carry sources (`resume.go:769`, `:822`, `:907`), so I added no change there. I did not test those paths for OpenRouter specifically.
- The owner rules say commit messages in Russian, but the brief asked for English. I followed the brief.
- No UI changes, no new dependencies, no placeholders.

## Progress
- 15:5x UTC Read `openrouter.go`, `manager.go`, `source_files.go` and the existing OpenRouter tests
- 15:5x UTC Created branch `bench/146-sonnet-high` and wrote the failing test `TestOpenRouterRefusesSources` (reproduced)
- 16:0x UTC Added the sources refusal in `newJob` and updated the two tests that assumed OpenRouter plus sources
- 16:0x UTC Ran `go vet` and the targeted tests (pass)
- 16:08 UTC Committed `e45252e`, with the message in English as the brief asked
- 16:30 UTC Full `internal/jobs` run: one unrelated failure (`TestDeadWrapperKillsSpawnedGroup`); wrote the report

## Summary
Issue #146: a file source from a denied repo, or a `job:` report about one, could reach an OpenRouter job, because `deny_repos` only checks the job's own repository. I took the issue's simpler option: `Enqueue` now refuses `sources` on any OpenRouter job. The fix is committed locally as `e45252e` on `bench/146-sonnet-high`. Targeted tests and `go vet` pass. The full `internal/jobs` run fails on one test that has nothing to do with sources, and two earlier full runs hung or were killed (see below).

## Findings / Changes
- `internal/jobs/manager.go` (in `newJob`, right after `checkOpenRouterJob`): if `len(r.Sources) > 0` for an OpenRouter model, it returns an error: "sources are not allowed on an openrouter job: providers.openrouter.deny_repos cannot check a source file's repository or a job report's; start the job without sources".
  - The check runs before any job row, job folder or budget reservation exists, so a refused job leaves nothing behind.
  - It also covers milestone-planned sources, since those arrive through `r.Sources`.
- I did not implement the per-source policy (map each path to its git repo, check `job:` ids against allowed repos). `deny_repos` matches repo names and globs, and resolving a source file's repo through symlinks and non-repo paths is a design question for the owner.
- Two existing tests asserted the old behavior, so I changed them:
  - `TestOpenRouterCapCollisionReservesOnce` (`jobid_test.go`) no longer passes sources. It still checks that an id redraw reserves the budget once. The sources-under-final-id case is still covered by `TestEnqueueSourcesLandUnderFinalID`.
  - `TestOpenRouterJobWithSourceFiles` (`openrouter_sources_test.go`) now expects the refusal, and still checks the store round trip and the daily-cap refusal for a job without sources.
- New test `TestOpenRouterRefusesSources` (`openrouter_test.go`).

## Evidence
- Before the fix, `TestOpenRouterRefusesSources` failed:
  - `file in a denied repo: want a sources refusal naming openrouter, got <nil>`. The file from a repo named in `deny_repos` was accepted.
  - `job report: ... got sources "job:...": report.md: open ...: no such file`. It was only refused because the other job had no report yet; there was no policy check.
  - `a refused job reserved $10.00`.
- After the fix: `go test ./internal/jobs -run 'TestOpenRouter|Source|Enqueue'` gives `ok`, exit 0. It covers refusal for a denied-repo file and a `job:` source, no reserved spend, the same file still accepted on a claude job, and an OpenRouter job without sources still accepted.
- `go vet ./...` exit 0. `gofmt -l internal` prints nothing.
- Full `go test ./...`, first attempt:
  - Packages root, `cmd/agent-jobs`, `examples/manifest-check`, `brain`, `casefold`, `checklist`, `guard` and `httpapi` printed `ok`.
  - `internal/jobs` hit the 10-minute timeout in `TestJudgeManifestCheckNonZeroExit`.
  - That test passed when run on its own.
  - I did not run the packages after `internal/jobs` in this attempt.
- Full `go test ./internal/jobs -timeout 25m` on the final commit: exit 1 after 1149s. The only `--- FAIL` is `TestDeadWrapperKillsSpawnedGroup`: `deadwrapper_test.go:58: the job's group died with its wrapper; nothing to test`.
  - It exercises process-group and signal behavior, not sources.
  - I did not run it on the base commit, so I can't say whether it already fails there.
  - An earlier full run of the package ended with `signal: terminated` at 184s, probably killed by the harness.
