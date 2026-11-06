[jobs:job-done] job 261106-131245-37cf BENCH M #114 sonnet-medium succeeded · 21 turns · $0.31 · 33m · model sonnet · effort medium · job dir 140.7 MB · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- The darwin wait path (kqueue/sysctl) is still not run on macOS; the issue lists it as open and it is out of reach on this Linux host.
- The new test uses `processGone`, which skips on platforms other than Linux and darwin.
- The first run of the new test, before the fix, did not check that `materializeTree` returned `errGitTimeout`. It failed earlier, on the surviving child.

## Progress
- --:-- Read gitrun.go and gate.go, created branch `bench/114-sonnet-medium`
- --:-- Added the regression test and confirmed it fails
- --:-- Replaced the `os.Pipe` with an `io.Pipe` in `materializeTree`; found a hang in `TestMaterializeTree` and fixed it by closing the reader before `wait`
- --:-- Ran targeted tests, vet, and the full `internal/jobs` suite (all passed)
- --:-- Committed `fbcdb87`

## Summary
Issue #114: `materializeTree` passed cat-file's stdout as an `*os.File`, so the runner's output tracking was bypassed. A child holding that pipe could outlive the timeout. I wrote a failing regression test first, then fixed `materializeTree`. The new test and the full `internal/jobs` package now pass, and `go vet ./...` is clean. Commit `fbcdb87` is on branch `bench/114-sonnet-medium`.

## Findings / Changes
- `internal/jobs/gate.go` (`materializeTree`): stdout is now an `io.Pipe` instead of an `os.Pipe`, so `startGit` copies it itself.
  - The copy takes part in `p.copied`. While a descendant holds the pipe, the reaper waits and keeps the PID reservation. At the deadline `p.kill()` or the wait-delay then kills the group.
  - A goroutine closes the pipe writer after `p.copied`, so the reader gets EOF.
  - On the error path the pipe reader is closed before `p.wait`, so the copy never blocks writing into a pipe nobody reads. Without this the existing `TestMaterializeTree` hung.
- `internal/jobs/git_timeout_test.go`: new `TestMaterializeTreeKillsChildHoldingStdout`. A fake `cat-file` starts `sleep 60` inheriting stdout, records its PID and exits. The test asserts `errGitTimeout` and that the child has ended. Other git calls go to the real git.

## Evidence
- Before the fix the new test failed: `cat-file's child 3323473 still runs after the deadline` (FAIL, 6.14s).
- After the fix, the targeted run `go test ./internal/jobs -run 'TestMaterializeTree|TestDaemonGit|TestGit'` printed `ok` (the exit code was not captured).
- `go vet ./...` exited 0, and `gofmt -l internal` printed nothing.
- `go test ./internal/jobs -count=1 -timeout 1500s` printed `ok ... 1007.353s` with `test=0`. An earlier run with `-timeout 500s` hit that limit because the suite takes about 17 minutes, not because of a failure.
- I only touched `internal/jobs`, so no other package was tested.
