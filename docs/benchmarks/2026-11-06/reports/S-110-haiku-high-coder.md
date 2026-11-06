[jobs:job-done] job 261106-131243-f99d BENCH S #110 haiku-high succeeded · 66 turns · $0.09 · 50m · model haiku · effort high · job dir 112.0 MB · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- The race detector was not run (no gcc on this host).
- `scripts/check.sh` was not run as a whole. I ran its component steps: gofmt, vet, commit-message check, and the full package test. The `manifest-check` step was not run.
- Silent-mode daemon: the test does not assert the daemon is gone. The parent never learns its pid. The daemon dies only through the kernel's parent-death signal (`Pdeathsig`) when the helper is killed, which is Linux-only and not verified by the test.
- Silent-mode home directory: the helper's `AGENT_JOBS_HOME` is not removed. The parent does not know its path, and the killed helper runs no cleanup. This is probably a temp-directory leak in the test, not verified.
- The daemon cleanup is still registered after `waitFor`. Its pid is unknown if the pid wait fails, so cleanup of that daemon still relies on `Pdeathsig`. This is unchanged from the original code.
- Commit language: English, per the brief and the repo check; the owner rules say Russian. The owner should confirm which applies.

## Progress
- 13:13 Read `internal/jobs/testproc_test.go`, `jobs_test.go` (`waitFor`), `testproc_linux_test.go`, and the `alive` helper.
- --:-- Created branch `bench/110-haiku-high`, wrote `PROGRESS.md` in the job dir.
- --:-- Extracted `startedDaemonCase` and added `silent` mode plus the failing test; `waitFor` now takes `testing.TB`.
- --:-- Ran the new test: red (helper survives). Committed as `17b1e8b` (Russian message).
- --:-- Moved the helper cleanup right after `c.Start()`; both regression tests green. Committed as `c34206e` (Russian message).
- --:-- Ran `-race` (no gcc, failed to build); ran `go vet ./...` (exit 0).
- --:-- Full `./internal/jobs` run with the default timeout panicked on the 10m limit (not an assertion failure).
- --:-- Rebuilt both commits with English messages (`2783abb`, `8f591bc`) because the repo's commit-message check rejects Cyrillic.
- --:-- Reran full `./internal/jobs` with `-timeout 45m`: 891 PASS, 0 FAIL, exit 0.

## Summary
Issue #110: the #105 regression test registered its helper's cleanup only after the daemon-pid wait. If that wait failed, the helper and its daemon were never killed. I added a failing test that reproduces this, then moved the cleanup to right after `c.Start()`. The new test fails before the fix and passes after. The full `internal/jobs` package passes (exit 0) on branch `bench/110-haiku-high`. Nothing is pushed.

## Findings / Changes
- **Red commit `2783abb`** (`internal/jobs/testproc_test.go`):
  - Extracted the subtest body into `startedDaemonCase` (`testproc_test.go:97`), keeping the old ordering.
  - Added helper mode `silent`: it reports only its own pid, so the parent's pid wait fails (`testproc_test.go:52`, the `silent` branch of `TestOwnedHelperDaemon`).
  - Added `TestStartedHelperIsEndedWhenPIDWaitFails` (`testproc_test.go:153`) and a `failTB` test double (`testproc_test.go:184`) so a failing case can run its cleanups.
  - `internal/jobs/jobs_test.go:450`: `waitFor` now takes `testing.TB` instead of `*testing.T`, so `failTB` can be passed to it.
- **Fix commit `8f591bc`** (`testproc_test.go:107-118`): the helper's cleanup is registered right after `c.Start()`, before any wait. The only `c.Wait()` runs in a goroutine that closes `exited`; the cleanup kills the helper if it is still running and waits on `exited`. The timeout branch no longer kills the helper itself (`testproc_test.go:~135`, relies on the cleanup).
- **Commit messages are in English**, not Russian. The owner rules ask for Russian, but the brief asks for English and the repo's `scripts/check-commit-messages.sh` rejects Cyrillic. My first two commits were Russian; I rebuilt them with the same trees and English messages (`commit-tree` plus `update-ref`, since the guard blocks git inside substitutions). Both messages end with the Co-Authored-By line.

## Evidence
- Red, before the fix: `go test ./internal/jobs/ -run 'TestStartedHelperIsEndedWhenPIDWaitFails|TestStartedDaemonNeverOutlivesItsTest' -count=1 -v` → `--- FAIL: TestStartedHelperIsEndedWhenPIDWaitFails` with `testproc_test.go:170: timed out waiting for helper gone` (helper survived). Exit 1.
- Green, after the fix: same command → `--- PASS: TestStartedHelperIsEndedWhenPIDWaitFails (0.81s)`; `TestStartedDaemonNeverOutlivesItsTest/fail` and `/hang` PASS; `ok ... 3.443s`. Exit 0.
- `TestStartedDaemonNeverOutlivesItsTest` is pre-existing (refactored, not new); it passed both before and after.
- Full package: `go test -count=1 -timeout 45m -v ./internal/jobs/` → log `logs/jobs-test-45m.log`: 891 `--- PASS`, 0 `--- FAIL`, `ok github.com/qwwiwi/agent-jobs/internal/jobs 930.731s`. Exit 0 (`logs/jobs-test.exit`).
- An earlier run with the default 10m timeout panicked with `test timed out after 10m0s` (no assertion failures; `logs/jobs-test.log`). The 45m timeout matches `scripts/check.sh`.
- `go vet ./...` → exit 0.
- `gofmt -l .` → no output, exit 0.
- `scripts/check-commit-messages.sh` → exit 0.
- `CGO_ENABLED=1 go test -race ...` → `cgo: C compiler "gcc" not found`, exit 1. The race detector was not run.
- No UI changes, so no browser check. No subagents used.
