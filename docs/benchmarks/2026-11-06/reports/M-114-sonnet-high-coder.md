[jobs:job-done] job 261106-131245-db79 BENCH M #114 sonnet-high succeeded · 2 turns · $0.42 · 1m · model sonnet · effort high · auto-continued (attempt 2; attempt 1 ended its turn without a report after 1 turns (cost $0.39)) · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)
attempt 1: new failed · 1 turns · $0.39 · 1739s · <agent-jobs-home>/jobs/261106-131245-db79/report.attempt-1.md
attempt 2: resume succeeded (current)

## Not done / uncertain
- `go test -race` could not run: `go: -race requires cgo; enable cgo by setting CGO_ENABLED=1`. The synchronous `io.Pipe` between the runner's copy and the reader is therefore not race-checked.
- The darwin kqueue/sysctl wait path from the issue's last line was not run on macOS. It is not in scope here.
- Closing the read end on the error path makes the runner record `io.ErrClosedPipe` as a copy error. It only affects `werr`, and `err` is already set on that path, so the result is unchanged. I did not add a test for it.
- I did not run `scripts/check.sh`. I ran `go test` and `go vet` directly.

## Progress
- --:-- Read `gitrun.go`, `gate.go` `materializeTree` and the existing timeout tests
- --:-- Created branch `bench/114-sonnet-high`, added the regression test and confirmed it fails
- --:-- Switched `materializeTree` stdout to an `io.Pipe` fed by the runner and closed the read end on the error path
- --:-- Confirmed the target test passes (count=5); `-race` is unavailable (no cgo)
- --:-- Ran the full `internal/jobs` tests (exit 0) and `go vet ./...` (exit 0)
- --:-- Committed locally as `60f4863`
- --:-- Checked `git status` (clean) and updated `PROGRESS.md`; wrote the final report

## Summary
Issue #114: `materializeTree` read `cat-file` stdout through its own `*os.File`, which the runner could not track. A descendant holding that pipe outlived the timeout. I reproduced this with a failing test, then fixed it by reading stdout through a runner-managed `io.Pipe`, so the group stays killable until the stream ends. The new test passes, the full `internal/jobs` package passes and `go vet ./...` is clean. Committed locally as `60f4863` on `bench/114-sonnet-high`; the working tree is clean, and nothing was pushed.

## Findings / Changes
- `internal/jobs/gate.go` (`materializeTree`, about lines 505-560): `cat-file`'s stdout is now an `io.Pipe` writer instead of an `os.Pipe` write end. `startGit` already treats a non-file writer as runner-fed, so the runner copies git's stdout through its own tracked pipe.
- The `p.copied` completion protocol now covers the stream. If a descendant holds stdout after git exits, git stays unreaped and its PID reserved. At the deadline `p.kill()` signals the group, or the runner does it after `gitWaitDelay`.
- Error path: the deferred function also closes the `io.Pipe` read end. Otherwise the runner's copy would block in `Write` on a stream nobody reads.
- `internal/jobs/git_timeout_test.go`: new `TestMaterializeTreeKillsDescendantAfterCatFileExit`.
- I did not touch `gitrun.go`.

## Evidence
- Before the fix, `go test ./internal/jobs -run TestMaterializeTreeKillsDescendant` failed: `cat-file's descendant 3275479 still runs after the deadline: the group was not killed`.
- The new test uses a fake `cat-file` that starts `sleep 60 &` with inherited stdout, records its PID and exits 0. Every other git call goes to the real git. `gitLongTimeout` is 1s and `gitWaitDelay` is 30s, so the kill must come from the deadline.
- It asserts that `materializeTree` returns `errGitTimeout` and that the child is gone.
- After the fix, with `-count=5`, the new test passed (exit 0).
- Full `go test ./internal/jobs -count=1 -timeout 40m`: exit 0, `ok ... 1009.112s`. This includes the existing `TestMaterializeTreeTimeoutAlwaysNamed` and `TestDaemonGitCallsTimeOut`.
- `go vet ./...`: exit 0. `gofmt -l internal/` printed nothing.
- I ran `go test` only for `internal/jobs`, the one package I touched.
