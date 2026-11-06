[jobs:job-done] job 261106-131244-773e BENCH S #145 haiku-high succeeded · 51 turns · $0.06 · 31m · model haiku · effort high · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- Commit message is in English because the brief asked for it. The owner rules in the brief say commit messages are in Russian. This conflicts, and I followed the task text. Flagging it so the orchestrator can decide.
- No `Requirements:` trailer: the brief lists no requirement IDs.
- No UI changes, so no browser check was needed.
- The mutation check for the `//` cases is coarse: all 7 table rows fail when the Clean check is removed, so it does not prove the `//` rows fail on their own.

## Progress
- --:-- Read `internal/jobs/resume_skills_test.go` and `internal/mcp/source_files_test.go`
- --:-- Located `prepareResume`, `checkedSkillLinks`, `openAbsoluteSource` in source
- --:-- Added 3 `//` rows to the mcp source table; test passed
- --:-- Added `TestResumeQueuedLegacySkillsLaunchRefused`; first run failed on missing `worktree.json`
- --:-- Added `markWorktree` to the new test; passed
- --:-- Mutation checks for legacy refusal and Clean check; both caught, both reverted
- --:-- Full `./internal/mcp` run exit 0; first `./internal/jobs` run hit 10m default timeout
- --:-- `go vet ./...` exit 0; gofmt clean
- --:-- Committed `d5488e8` on `bench/145-haiku-high`
- --:-- `./internal/jobs` rerun with 45m timeout: exit 0

## Summary
Issue #145 asked for missing regression tests only: a launch-time refusal test for a queued legacy job (skills delivered, no manifest), and `//` cases for non-canonical source paths. Both are added and pass on the unchanged code. Commit `d5488e8` is on local branch `bench/145-haiku-high`, not pushed. No production code changed.

## Findings / Changes
- `internal/jobs/resume_skills_test.go:339` (new `TestResumeQueuedLegacySkillsLaunchRefused`, subtests `unchanged` and `edited`). It admits a resume with the manifest intact, claims the job, clears the manifest with `SetSkillsDelivered`, then calls `prepareResume` and expects `no verifiable baseline`. It also calls `markWorktree` because the launch verifies `worktree.json`, which the shared fixture does not write.
- `internal/mcp/source_files_test.go:74-76`: three new rows in `TestJobStartRejectsNonCanonicalSources`: `dir//report.md`, `dir/linked//report.md`, `origin//`.
- Commit: `d5488e8 Add launch refusal and double-slash source tests (#145)`, touching 2 files, +48 lines, against base `8f6e8f9`.

## Evidence
- Reproduction: both new tests pass on the unchanged code, as the issue predicted. To show they catch regressions, I ran mutation checks and reverted each one with `git checkout`:
  - Removed the legacy refusal in `checkedSkillLinks` (`skill_manifest.go`): `TestResumeQueuedLegacySkillsLaunchRefused` failed in both subtests. The message was a different guard ("differs from the job's base commit"), so the new test catches the mutant, but the legacy check is not the only gate in that path.
  - Dropped `filepath.Clean(path) != path` in `openAbsoluteSource` (`source_files.go`): all 7 subtests of `TestJobStartRejectsNonCanonicalSources` failed, including the 3 new ones. This checks that the table catches the regression, but it does not isolate the `//` rows.
- `go test -count=1 ./internal/mcp -run TestJobStartRejectsNonCanonicalSources -v`: 7/7 subtests PASS, exit 0.
- `go test -count=1 ./internal/jobs -run TestResumeQueuedLegacySkillsLaunchRefused -v`: both subtests PASS, exit 0 (after adding `markWorktree`; the first run failed on the missing `worktree.json`).
- `go test -count=1 ./internal/mcp`: `ok`, exit 0 (110.6s).
- `go test -count=1 -timeout 45m ./internal/jobs`: `ok`, exit 0 (1051s). The first run used the default 10m timeout and hit `panic: test timed out` with no test failures; the rerun was clean (`test-jobs.log`, `JOBS_EXIT=0`).
- `go vet ./...`: exit 0 (`vet.log`).
- `gofmt -l` on both files: no output, exit 0.
- `git status` after commit: clean. `git diff 8f6e8f9 --stat`: only the 2 test files.
